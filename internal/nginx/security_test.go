package nginx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func defaultSecurityPoliciesForTest() []domain.SecurityPolicy {
	return []domain.SecurityPolicy{
		{
			ID: domain.DefaultSecurityPolicyID, Name: "sensitive", Enabled: true,
			Pattern: domain.DefaultSecurityPolicyPattern, Action: domain.SecurityActionBan,
			BanDurationSeconds: 21600, Priority: 100,
		},
		{
			ID: domain.DefaultPHPSecurityPolicyID, Name: "PHP probes", Enabled: true,
			Pattern: domain.DefaultPHPSecurityPolicyPattern, Action: domain.SecurityActionBlock, Priority: 200,
		},
	}
}

func rateLimitPoliciesForTest() []domain.RateLimitPolicy {
	return []domain.RateLimitPolicy{
		{
			ID: "11111111-1111-4111-8111-111111111111", Name: "all requests",
			Enabled: true, RequestsPerSecond: 20,
		},
		{
			ID: "22222222-2222-4222-8222-222222222222", Name: "error responses",
			Enabled: true, RequestsPerSecond: 5, ResponseConditionEnabled: true,
			ResponseStatusClasses: []int{5, 4}, BanEnabled: true,
			BanAfterConsecutive429: 3, BanDurationSeconds: 3600,
		},
	}
}

func TestRenderWithSecurityPolicies(t *testing.T) {
	site := domain.Site{
		ID: "site-a", Name: "site-a", Domains: []string{"cdn.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}
	configuration, err := RenderWithSecurity([]domain.Site{site}, defaultSecurityPoliciesForTest())
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{
		"# CDN security revision:", "map $uri $cdn_security_policy_id", "log_format cdn_security_json", "security.json cdn_security_json",
		"if ($cdn_security_policy_id) { return 444; }", `"ban"`, `"block"`, "21600", `"~(?i)^/+`, `\\.env`, "php[-_]?info",
	} {
		if !strings.Contains(configuration, wanted) {
			t.Errorf("security configuration lacks %q:\n%s", wanted, configuration)
		}
	}
}

func TestRenderInitializesSiteIDBeforeSecurityRejection(t *testing.T) {
	site := domain.Site{
		ID: "site-a", Name: "site-a", Domains: []string{"cdn.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}
	configuration, err := RenderWithSecurity([]domain.Site{site}, defaultSecurityPoliciesForTest())
	if err != nil {
		t.Fatal(err)
	}
	base, fragments, err := SplitHTTPConfig(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(fragments) != 1 {
		t.Fatalf("HTTP site fragments = %d, want 1", len(fragments))
	}

	const rejection = `if ($cdn_security_policy_id) { return 444; }`
	const defaultInitialization = `set $cdn_site_id "";`
	if count := strings.Count(base, defaultInitialization); count != 2 {
		t.Fatalf("default server site ID initializations = %d, want 2:\n%s", count, base)
	}
	if initialization, rejectionIndex := strings.Index(base, defaultInitialization), strings.Index(base, rejection); initialization < 0 || rejectionIndex < 0 || initialization > rejectionIndex {
		t.Fatalf("default server initializes the site ID after security rejection:\n%s", base)
	}

	const siteInitialization = `set $cdn_site_id "site-a";`
	fragment := fragments[0].Content
	if count := strings.Count(fragment, siteInitialization); count != 2 {
		t.Fatalf("site server site ID initializations = %d, want 2:\n%s", count, fragment)
	}
	remaining := fragment
	for server := 1; server <= 2; server++ {
		initialization := strings.Index(remaining, siteInitialization)
		rejectionIndex := strings.Index(remaining, rejection)
		if initialization < 0 || rejectionIndex < 0 || initialization > rejectionIndex {
			t.Fatalf("site server %d initializes the site ID after security rejection:\n%s", server, fragment)
		}
		remaining = remaining[rejectionIndex+len(rejection):]
	}
}

func TestDisabledSecurityPoliciesRetainRevisionMarker(t *testing.T) {
	policies := []domain.SecurityPolicy{{ID: domain.DefaultSecurityPolicyID, Enabled: false}}
	configuration, err := RenderWithSecurity(nil, policies)
	if err != nil {
		t.Fatal(err)
	}
	if !HasSecurityRevision(configuration, policies) || strings.Contains(configuration, "cdn_security_policy_id") {
		t.Fatalf("disabled security policy configuration is not revision-marked:\n%s", configuration)
	}
}

func TestRenderWithRateLimitPolicies(t *testing.T) {
	site := domain.Site{
		ID: "site-a", Name: "site-a", Domains: []string{"cdn.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}
	policies := rateLimitPoliciesForTest()
	configuration, err := RenderWithSecurityAndRateLimit([]domain.Site{site}, nil, policies)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{
		"# CDN rate limit revision:", "lua_shared_dict cdn_rate_limit 20m;", "init_by_lua_block",
		`id = "11111111-1111-4111-8111-111111111111", limit = 20`,
		`id = "22222222-2222-4222-8222-222222222222", limit = 5, statuses = { [4] = true, [5] = true }`,
		"ban_after = 3, ban_seconds = 3600", "lua_shared_dict cdn_rate_limit_escalation 10m;",
		"log_format cdn_rate_limit_ban_json", "cdn_rate_limit_ban_policy_id",
		"dict:incr(current_key, 1, 0, key_ttl)", "count_requests and rate > policy.limit",
		"not count_requests and rate >= policy.limit", "policy.statuses[status_class]",
		"record_rejection(policy, client_ip)", "ngx.ctx.cdn_rate_limit_rejected",
		`ngx.header["Retry-After"] = "1"`, "ngx.exit(429)",
		"access_by_lua_block", "header_filter_by_lua_block",
	} {
		if !strings.Contains(configuration, wanted) {
			t.Errorf("rate limit configuration lacks %q:\n%s", wanted, configuration)
		}
	}
	if !HasRateLimitRevision(configuration, policies) {
		t.Fatal("rendered rate limit revision was not detected")
	}
	changed := append([]domain.RateLimitPolicy(nil), policies...)
	changed[0].RequestsPerSecond++
	if HasRateLimitRevision(configuration, changed) {
		t.Fatal("rate limit revision ignored a threshold change")
	}
	changed = append([]domain.RateLimitPolicy(nil), policies...)
	changed[1].BanAfterConsecutive429++
	if HasRateLimitRevision(configuration, changed) {
		t.Fatal("rate limit revision ignored a ban threshold change")
	}
	if strings.Index(configuration, "[4] = true") > strings.Index(configuration, "[5] = true") {
		t.Fatal("response status classes were not normalized before rendering")
	}
}

func TestDisabledRateLimitPoliciesRetainRevisionMarker(t *testing.T) {
	policies := []domain.RateLimitPolicy{{
		ID: "11111111-1111-4111-8111-111111111111", Name: "disabled",
		RequestsPerSecond: 10,
	}}
	configuration, err := RenderWithSecurityAndRateLimit(nil, nil, policies)
	if err != nil {
		t.Fatal(err)
	}
	if !HasRateLimitRevision(configuration, policies) || strings.Contains(configuration, "lua_shared_dict cdn_rate_limit") {
		t.Fatalf("disabled rate limit policy configuration is not revision-only:\n%s", configuration)
	}
	if _, err := RenderWithSecurityAndRateLimit(nil, nil, []domain.RateLimitPolicy{{
		Name: "invalid", Enabled: true, RequestsPerSecond: 10,
		ResponseConditionEnabled: true, ResponseStatusClasses: []int{1},
	}}); err == nil {
		t.Fatal("invalid response status class was rendered")
	}
}

func TestRenderWithoutSecurityRetainsLegacyShape(t *testing.T) {
	configuration, err := Render(nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(configuration, "cdn_security") || strings.Contains(configuration, "security.json") || strings.Contains(configuration, "cdn_rate_limit") {
		t.Fatalf("legacy render unexpectedly contains security configuration:\n%s", configuration)
	}
}

func TestRenderedSecurityConfigurationPassesNginxSyntaxCheck(t *testing.T) {
	binary, err := exec.LookPath("nginx")
	if err != nil {
		t.Skip("nginx is not installed")
	}
	configuration, err := RenderWithSecurity(nil, defaultSecurityPoliciesForTest())
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/logs/security.json", filepath.Join(directory, "security.json"))
	configuration = strings.Replace(configuration, "listen 80 default_server;", "listen unix:"+filepath.Join(directory, "nginx.sock")+" default_server;", 1)
	nginxConfiguration := buildIsolatedNginxConfiguration(t, directory, "", "stderr", configuration)
	path := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(path, []byte(nginxConfiguration), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-t", "-c", path, "-p", directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("nginx -t: %v\n%s\n%s", err, output, nginxConfiguration)
	}
}

func TestRenderedSecurityConfigurationRuntime(t *testing.T) {
	binary, err := exec.LookPath("nginx")
	if err != nil {
		t.Skip("nginx is not installed")
	}
	configuration, err := RenderWithSecurity(nil, defaultSecurityPoliciesForTest())
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	socketPath := filepath.Join(directory, "nginx.sock")
	securityLogPath := filepath.Join(directory, "security.json")
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/logs/security.json", securityLogPath)
	configuration = strings.Replace(configuration, "listen 80 default_server;", "listen unix:"+socketPath+" default_server;", 1)
	nginxConfiguration := buildIsolatedNginxConfiguration(
		t, directory, "", filepath.Join(directory, "error.log"), configuration,
	)
	path := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(path, []byte(nginxConfiguration), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-t", "-c", path, "-p", directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("nginx -t: %v\n%s\n%s", err, output, nginxConfiguration)
	}
	command = exec.Command(binary, "-c", path, "-p", directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("start nginx: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		command := exec.Command(binary, "-s", "quit", "-c", path, "-p", directory)
		if output, err := command.CombinedOutput(); err != nil {
			t.Logf("stop nginx: %v: %s", err, output)
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		connection, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporary nginx did not listen on %s: %v", socketPath, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	connection, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(connection, "GET /.env HTTP/1.0\r\nHost: localhost\r\n\r\n"); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	response, err := io.ReadAll(connection)
	_ = connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(response) != 0 {
		t.Fatalf("sensitive path was not closed with status 444:\n%s", response)
	}
	var contents []byte
	deadline = time.Now().Add(2 * time.Second)
	for {
		contents, err = os.ReadFile(securityLogPath)
		if err == nil && len(strings.TrimSpace(string(contents))) > 0 {
			break
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("security event was not logged: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var event struct {
		PolicyID   string                      `json:"policy_id"`
		Action     domain.SecurityPolicyAction `json:"action"`
		BanSeconds int                         `json:"ban_seconds"`
		Path       string                      `json:"path"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(contents))), &event); err != nil {
		t.Fatal(err)
	}
	if event.PolicyID != domain.DefaultSecurityPolicyID || event.Action != domain.SecurityActionBan ||
		event.BanSeconds != 21600 || event.Path != "/.env" {
		t.Fatalf("unexpected security event: %#v", event)
	}
}

func TestRenderedRateLimitConfigurationRuntime(t *testing.T) {
	luaModule := os.Getenv("NGINX_LUA_MODULE_PATH")
	ndkModule := os.Getenv("NGINX_NDK_MODULE_PATH")
	if luaModule == "" || ndkModule == "" {
		t.Skip("Nginx Lua module paths are not configured")
	}
	for _, module := range []string{luaModule, ndkModule} {
		if _, err := os.Stat(module); err != nil {
			t.Fatalf("rate limit test module %s: %v", module, err)
		}
	}
	tests := []struct {
		name           string
		policy         domain.RateLimitPolicy
		wantLimited    bool
		responseStatus int
		responseClass  int
	}{
		{
			name: "all requests",
			policy: domain.RateLimitPolicy{
				ID: "11111111-1111-4111-8111-111111111111", Name: "all", Enabled: true, RequestsPerSecond: 2,
			},
			wantLimited: true, responseStatus: http.StatusNotFound, responseClass: 4,
		},
		{
			name: "matching response condition",
			policy: domain.RateLimitPolicy{
				ID: "22222222-2222-4222-8222-222222222222", Name: "4xx", Enabled: true, RequestsPerSecond: 2,
				ResponseConditionEnabled: true, ResponseStatusClasses: []int{4},
			},
			wantLimited: true, responseStatus: http.StatusNotFound, responseClass: 4,
		},
		{
			name: "matching 5xx response condition",
			policy: domain.RateLimitPolicy{
				ID: "55555555-5555-4555-8555-555555555555", Name: "5xx", Enabled: true, RequestsPerSecond: 2,
				ResponseConditionEnabled: true, ResponseStatusClasses: []int{5},
			},
			wantLimited: true, responseStatus: http.StatusInternalServerError, responseClass: 5,
		},
		{
			name: "non-matching response condition",
			policy: domain.RateLimitPolicy{
				ID: "33333333-3333-4333-8333-333333333333", Name: "5xx", Enabled: true, RequestsPerSecond: 2,
				ResponseConditionEnabled: true, ResponseStatusClasses: []int{5},
			},
			wantLimited: false, responseStatus: http.StatusNotFound, responseClass: 4,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			statuses := runRateLimitNginx(t, luaModule, ndkModule, test.policy, test.responseStatus)
			limited := false
			for _, status := range statuses {
				limited = limited || status == http.StatusTooManyRequests
				if status != http.StatusTooManyRequests && status/100 != test.responseClass {
					t.Fatalf("unexpected response statuses %v", statuses)
				}
			}
			if limited != test.wantLimited {
				t.Fatalf("limited=%t, statuses=%v", limited, statuses)
			}
		})
	}
	t.Run("named proxy location", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.WriteHeader(http.StatusNoContent)
		}))
		defer origin.Close()
		policy := domain.RateLimitPolicy{
			ID: "44444444-4444-4444-8444-444444444444", Name: "2xx proxy responses",
			Enabled: true, RequestsPerSecond: 2, ResponseConditionEnabled: true,
			ResponseStatusClasses: []int{2},
		}
		policy, err := domain.NormalizeRateLimitPolicy(policy)
		if err != nil {
			t.Fatal(err)
		}
		configuration := renderRateLimitConfig([]domain.RateLimitPolicy{policy}, true) + `
server {
    listen __RATE_LIMIT_TEST_LISTEN__;
    location / {
        error_page 419 = @origin;
        return 419;
    }
    location @origin {
        internal;
        access_by_lua_block { package.loaded.simple_cdn_rate_limit.access() }
        header_filter_by_lua_block { package.loaded.simple_cdn_rate_limit.response() }
        proxy_pass ` + origin.URL + `;
    }
}
`
		statuses := runRateLimitNginxConfiguration(t, luaModule, ndkModule, configuration).Statuses
		limited := false
		for _, status := range statuses {
			limited = limited || status == http.StatusTooManyRequests
			if status != http.StatusNoContent && status != http.StatusTooManyRequests {
				t.Fatalf("unexpected named proxy statuses %v", statuses)
			}
		}
		if !limited {
			t.Fatalf("named proxy rate limit did not run: %v", statuses)
		}
	})
	t.Run("consecutive 429 escalates once", func(t *testing.T) {
		policy := domain.RateLimitPolicy{
			ID: "66666666-6666-4666-8666-666666666666", Name: "error burst", Enabled: true,
			RequestsPerSecond: 1, ResponseConditionEnabled: true, ResponseStatusClasses: []int{4, 5},
			BanEnabled: true, BanAfterConsecutive429: 3, BanDurationSeconds: 3600,
		}
		configuration, err := RenderWithSecurityAndRateLimit(nil, nil, []domain.RateLimitPolicy{policy})
		if err != nil {
			t.Fatal(err)
		}
		configuration = strings.Replace(configuration, "listen 80 default_server;", "listen __RATE_LIMIT_TEST_LISTEN__;", 1)
		result := runRateLimitNginxConfiguration(t, luaModule, ndkModule, configuration)
		if len(result.Statuses) != 10 || result.Statuses[0] != http.StatusNotFound {
			t.Fatalf("unexpected escalation statuses %v", result.Statuses)
		}
		for index, status := range result.Statuses[1:] {
			if status != http.StatusTooManyRequests {
				t.Fatalf("request %d status = %d, want 429: %v", index+2, status, result.Statuses)
			}
			if result.RetryAfter[index+1] != "1" || result.CacheControl[index+1] != "no-store" {
				t.Fatalf("request %d limit headers retry=%q cache=%q", index+2,
					result.RetryAfter[index+1], result.CacheControl[index+1])
			}
		}
		if !slices.Equal(result.BanEventCounts[:4], []int{0, 0, 0, 1}) {
			t.Fatalf("ban event counts through third 429 = %v, want [0 0 0 1]", result.BanEventCounts[:4])
		}
		for request, count := range result.BanEventCounts[4:] {
			if count != 1 {
				t.Fatalf("request %d ban event count = %d, want 1", request+5, count)
			}
		}
		contents, err := os.ReadFile(result.SecurityLogPath)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
		if len(lines) != 1 {
			t.Fatalf("rate limit ban events = %d, want 1:\n%s", len(lines), contents)
		}
		var event struct {
			PolicyID   string `json:"policy_id"`
			Action     string `json:"action"`
			BanSeconds int    `json:"ban_seconds"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
			t.Fatal(err)
		}
		if event.PolicyID != policy.ID || event.Action != "ban" || event.BanSeconds != 3600 {
			t.Fatalf("rate limit ban event = %#v", event)
		}
	})
}

func runRateLimitNginx(t *testing.T, luaModule, ndkModule string, policy domain.RateLimitPolicy, responseStatus int) []int {
	t.Helper()
	configuration, err := RenderWithSecurityAndRateLimit(nil, nil, []domain.RateLimitPolicy{policy})
	if err != nil {
		t.Fatal(err)
	}
	configuration = strings.Replace(configuration, "content_by_lua_block { return ngx.exit(404) }",
		fmt.Sprintf("content_by_lua_block { return ngx.exit(%d) }", responseStatus), 1)
	configuration = strings.Replace(configuration, "listen 80 default_server;", "listen __RATE_LIMIT_TEST_LISTEN__;", 1)
	return runRateLimitNginxConfiguration(t, luaModule, ndkModule, configuration).Statuses
}

type rateLimitNginxResult struct {
	Statuses        []int
	RetryAfter      []string
	CacheControl    []string
	BanEventCounts  []int
	SecurityLogPath string
}

func runRateLimitNginxConfiguration(t *testing.T, luaModule, ndkModule, configuration string) rateLimitNginxResult {
	t.Helper()
	binary, err := exec.LookPath("nginx")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	configuration = strings.Replace(configuration, "__RATE_LIMIT_TEST_LISTEN__", "127.0.0.1:"+strconv.Itoa(port), 1)
	directory := t.TempDir()
	securityLogPath := filepath.Join(directory, "security.json")
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/logs/security.json", securityLogPath)
	packagePath := os.Getenv("NGINX_LUA_PACKAGE_PATH")
	var luaPathDirective string
	if packagePath != "" {
		luaPathDirective = "lua_package_path '" + strings.ReplaceAll(packagePath, "'", "\\'") + "';\n"
	}
	preamble := fmt.Sprintf("load_module %s;\nload_module %s;\n", strconv.Quote(ndkModule), strconv.Quote(luaModule))
	nginxConfiguration := buildIsolatedNginxConfiguration(
		t, directory, preamble, filepath.Join(directory, "error.log"), luaPathDirective+configuration,
	)
	path := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(path, []byte(nginxConfiguration), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-t", "-c", path, "-p", directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("nginx -t: %v\n%s\n%s", err, output, nginxConfiguration)
	}
	command = exec.Command(binary, "-c", path, "-p", directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("start nginx: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		command := exec.Command(binary, "-s", "quit", "-c", path, "-p", directory)
		if output, err := command.CombinedOutput(); err != nil {
			t.Logf("stop nginx: %v: %s", err, output)
		}
	})
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(3 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporary nginx did not listen on %s: %v", address, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	result := rateLimitNginxResult{
		Statuses: make([]int, 0, 10), RetryAfter: make([]string, 0, 10),
		CacheControl: make([]string, 0, 10), BanEventCounts: make([]int, 0, 10),
		SecurityLogPath: securityLogPath,
	}
	for range 10 {
		response, err := client.Get("http://" + address + "/test")
		if err != nil {
			t.Fatal(err)
		}
		result.Statuses = append(result.Statuses, response.StatusCode)
		result.RetryAfter = append(result.RetryAfter, response.Header.Get("Retry-After"))
		result.CacheControl = append(result.CacheControl, response.Header.Get("Cache-Control"))
		_ = response.Body.Close()
		contents, err := os.ReadFile(securityLogPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		count := 0
		if len(bytes.TrimSpace(contents)) > 0 {
			count = bytes.Count(bytes.TrimSpace(contents), []byte("\n")) + 1
		}
		result.BanEventCounts = append(result.BanEventCounts, count)
	}
	return result
}
