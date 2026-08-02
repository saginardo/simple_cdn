package nginx

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

func TestRenderWAFChainAndProofOfWork(t *testing.T) {
	site := domain.Site{
		ID: "site-a", Name: "site-a", Domains: []string{"cdn.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}
	policies := []domain.SecurityPolicy{{
		ID: "11111111-1111-4111-8111-111111111111", Name: "scanner chain", Enabled: true,
		SiteIDs: []string{site.ID}, Conditions: []domain.SecurityCondition{
			{Field: domain.SecurityFieldPath, Operator: domain.SecurityOperatorPrefix, Value: "/api/"},
			{Field: domain.SecurityFieldUserAgent, Operator: domain.SecurityOperatorRegex, Value: "scanner"},
		},
		Action: domain.SecurityActionBlock, ResponseStatus: 403, Priority: 100,
	}}
	powPolicy := domain.POWPolicy{
		ID: "22222222-2222-4222-8222-222222222222", Name: "browser check", Enabled: true,
		SiteIDs: []string{site.ID}, PathPattern: `^/`, DifficultyBits: 18,
		ChallengeTTLSeconds: 120, PassTTLSeconds: 1800, Priority: 100,
	}
	powRuntime := []domain.POWPolicyRuntime{{
		Policy: powPolicy, Secret: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32)),
	}}
	configuration, err := RenderWithRuntimeOptions([]domain.Site{site}, policies, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, WAFChainCapable: true, POWCapable: true, POWPolicies: powRuntime,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{
		"# CDN security revision:", "# CDN proof-of-work revision:", "init_worker_by_lua_block",
		"package.loaded.simple_cdn_security", "cdn_security_matched_field", "run_waf()", "run_pow()",
		powVerifyPath, "crypto.subtle.digest", "Set-Cookie", "difficulty = 18", "hmac_sha256",
		"local stopped, bypass_pow = run_waf()", "string.sub(ngx.req.get_body_data() or \"\", 1, MAX_BODY_BYTES)",
		"ngx.ctx.cdn_security_body_loaded", "ngx.ctx.cdn_security_request_body",
	} {
		if !strings.Contains(configuration, wanted) {
			t.Errorf("WAF/PoW configuration lacks %q:\n%s", wanted, configuration)
		}
	}
	if strings.Contains(configuration, "map $uri $cdn_security_policy_id") ||
		strings.Contains(configuration, `if ($cdn_security_policy_id) { return 444; }`) ||
		strings.Contains(configuration, "ngx.hmac_sha1") ||
		strings.Contains(configuration, "if length > MAX_BODY_BYTES") ||
		strings.Contains(configuration, `local body_loaded, request_body`) {
		t.Fatalf("runtime WAF configuration contains the legacy rejection path:\n%s", configuration)
	}
	if !HasSecurityRevision(configuration, policies) || !HasPOWRevision(configuration, []domain.POWPolicy{powPolicy}) {
		t.Fatal("rendered WAF/PoW revisions were not detected")
	}
	changed := append([]domain.SecurityPolicy(nil), policies...)
	changed[0].Conditions = append([]domain.SecurityCondition(nil), policies[0].Conditions...)
	changed[0].Conditions[0].Value = "/admin/"
	if HasSecurityRevision(configuration, changed) {
		t.Fatal("WAF revision ignored a condition change")
	}
}

func TestRuntimeWAFLogsEachMatchingRuleWithoutLegacyDuplicates(t *testing.T) {
	site := domain.Site{
		ID: "site-a", Name: "site-a", Domains: []string{"cdn.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}
	policies := []domain.SecurityPolicy{
		{
			ID: "11111111-1111-4111-8111-111111111111", Name: "record path", Enabled: true,
			Conditions: []domain.SecurityCondition{{Field: domain.SecurityFieldPath, Operator: domain.SecurityOperatorPrefix, Value: "/"}},
			Action:     domain.SecurityActionLog, Priority: 100,
		},
		{
			ID: "22222222-2222-4222-8222-222222222222", Name: "block GET", Enabled: true,
			Conditions: []domain.SecurityCondition{{Field: domain.SecurityFieldMethod, Operator: domain.SecurityOperatorEquals, Value: "GET"}},
			Action:     domain.SecurityActionBlock, ResponseStatus: 403, Priority: 200,
		},
	}
	configuration, err := RenderWithRuntimeOptions([]domain.Site{site}, policies, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, WAFChainCapable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 2; index++ {
		format := fmt.Sprintf("log_format cdn_security_json_%d ", index)
		if strings.Count(configuration, format) != 1 {
			t.Fatalf("WAF rule %d has %d log formats, want one", index, strings.Count(configuration, format))
		}
		accessLog := fmt.Sprintf("security.json cdn_security_json_%d if=$cdn_security_match_%d;", index, index)
		if strings.Count(configuration, accessLog) != 3 {
			t.Fatalf("WAF rule %d has %d server loggers, want one per HTTP/HTTPS server", index, strings.Count(configuration, accessLog))
		}
	}
	if strings.Contains(configuration, "security.json cdn_security_json if=$cdn_security_policy_id") ||
		strings.Contains(configuration, `if ($cdn_security_policy_id) { return 444; }`) {
		t.Fatalf("modern WAF configuration retained the legacy security logger/rejection path:\n%s", configuration)
	}
	if !strings.Contains(configuration, `ngx.var["cdn_security_match_" .. tostring(index)] = "1"`) {
		t.Fatal("runtime WAF does not mark every matching policy")
	}
}

func TestRuntimeWAFProtectsHTTPRedirectWhenRateLimitDisabled(t *testing.T) {
	site := domain.Site{
		ID: "site-a", Name: "site-a", Domains: []string{"cdn.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}
	policy := domain.SecurityPolicy{
		ID: "11111111-1111-4111-8111-111111111111", Name: "scanner", Enabled: true,
		Conditions: []domain.SecurityCondition{{Field: domain.SecurityFieldUserAgent, Operator: domain.SecurityOperatorContains, Value: "scanner"}},
		Action:     domain.SecurityActionBlock, ResponseStatus: 403, Priority: 100,
	}
	configuration, err := RenderWithRuntimeOptions([]domain.Site{site}, []domain.SecurityPolicy{policy}, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, WAFChainCapable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fragment := configuration[strings.Index(configuration, "# CDN HTTP site fragment "):]
	if !strings.Contains(fragment, "access_by_lua_block") || !strings.Contains(fragment, "content_by_lua_block { return ngx.redirect") {
		t.Fatalf("HTTP site does not run runtime security before redirect:\n%s", fragment)
	}
	if strings.Contains(fragment, "return 301 https://$host$request_uri;") {
		t.Fatalf("modern WAF HTTP site still uses a rewrite-phase redirect:\n%s", fragment)
	}
}

func TestRuntimeWAFWithoutSitesInitializesSiteID(t *testing.T) {
	policy := domain.SecurityPolicy{
		ID: "11111111-1111-4111-8111-111111111111", Name: "global scanner", Enabled: true,
		Conditions: []domain.SecurityCondition{{
			Field: domain.SecurityFieldUserAgent, Operator: domain.SecurityOperatorContains, Value: "scanner",
		}},
		Action: domain.SecurityActionBlock, ResponseStatus: 403, Priority: 100,
	}
	configuration, err := RenderWithRuntimeOptions(nil, []domain.SecurityPolicy{policy}, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, WAFChainCapable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(configuration, `set $cdn_site_id "";`) ||
		!strings.Contains(configuration, `"site_id":"$cdn_site_id"`) {
		t.Fatalf("site-free runtime WAF does not initialize its log variable:\n%s", configuration)
	}
}

func TestRenderedRuntimeSecurityConfiguration(t *testing.T) {
	binary, err := exec.LookPath("nginx")
	if err != nil {
		t.Skip("nginx is not installed")
	}
	version, err := exec.Command(binary, "-V").CombinedOutput()
	if err != nil || !strings.Contains(string(version), "lua-nginx-module") {
		t.Skip("nginx was not built with lua-nginx-module")
	}
	directory := t.TempDir()
	certificatePath, keyPath := writeRuntimeTestCertificate(t, directory)
	objectDirectory := filepath.Join(directory, "objects")
	if err := os.MkdirAll(objectDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(objectDirectory, digest), []byte("allowed"), 0o600); err != nil {
		t.Fatal(err)
	}
	site := domain.Site{
		ID: "site-a", Name: "runtime security", Domains: []string{"cdn.example.test"}, Enabled: true,
		PrimaryOrigin: domain.Origin{URL: "http://127.0.0.1:9", Enabled: true},
	}
	policies := []domain.SecurityPolicy{
		{
			ID: "11111111-1111-4111-8111-111111111111", Name: "trusted path", Enabled: true,
			Conditions: []domain.SecurityCondition{{
				Field: domain.SecurityFieldPath, Operator: domain.SecurityOperatorEquals, Value: "/allow",
			}},
			Action: domain.SecurityActionAllow, ResponseStatus: 403, Priority: 100,
		},
		{
			ID: "66666666-6666-4666-8666-666666666666", Name: "record scanner", Enabled: true,
			Conditions: []domain.SecurityCondition{{
				Field: domain.SecurityFieldUserAgent, Operator: domain.SecurityOperatorContains, Value: "scanner",
			}},
			Action: domain.SecurityActionLog, Priority: 150,
		},
		{
			ID: "22222222-2222-4222-8222-222222222222", Name: "scanner", Enabled: true,
			Conditions: []domain.SecurityCondition{{
				Field: domain.SecurityFieldUserAgent, Operator: domain.SecurityOperatorContains, Value: "scanner",
			}},
			Action: domain.SecurityActionBlock, ResponseStatus: 403, Priority: 200,
		},
		{
			ID: "33333333-3333-4333-8333-333333333333", Name: "body SQL injection", Enabled: true,
			Conditions: []domain.SecurityCondition{{
				Field: domain.SecurityFieldBody, Operator: domain.SecurityOperatorContains, Value: "union select",
			}},
			Action: domain.SecurityActionBlock, ResponseStatus: 403, Priority: 300,
		},
	}
	powPolicy := domain.POWPolicy{
		ID: "44444444-4444-4444-8444-444444444444", Name: "protected paths", Enabled: true,
		SiteIDs: []string{site.ID}, PathPattern: `^/(?:protected|allow)$`, DifficultyBits: 16,
		ChallengeTTLSeconds: 120, PassTTLSeconds: 300, Priority: 100,
	}
	configuration, err := RenderWithRuntimeOptions([]domain.Site{site}, policies, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB,
		WAFChainCapable:    true,
		POWCapable:         true,
		POWPolicies: []domain.POWPolicyRuntime{{
			Policy: powPolicy, Secret: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32)),
		}},
		StaticAssetDirectory: objectDirectory,
		StaticAssets: []domain.StaticAssetReference{
			{
				AssetID: "asset-a", BindingID: "binding-a", SiteID: site.ID, URLPath: "/allow",
				SHA256: digest, SizeBytes: 7, ContentType: "text/plain", CacheControl: domain.StaticAssetCacheHour,
			},
			{
				AssetID: "asset-a", BindingID: "binding-protected", SiteID: site.ID, URLPath: "/protected",
				SHA256: digest, SizeBytes: 7, ContentType: "text/plain", CacheControl: domain.StaticAssetCacheHour,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	httpSocketPath := filepath.Join(directory, "http.sock")
	tlsSocketPath := filepath.Join(directory, "https.sock")
	securityLogPath := filepath.Join(directory, "security.json")
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/logs/security.json", securityLogPath)
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/logs/access.json", filepath.Join(directory, "access.json"))
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/config/certs/"+site.ID+".crt", certificatePath)
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/config/certs/"+site.ID+".key", keyPath)
	configuration = strings.ReplaceAll(configuration, "listen 80 default_server;", "listen unix:"+httpSocketPath+" default_server;")
	configuration = strings.ReplaceAll(configuration, "listen 80;", "listen unix:"+httpSocketPath+";")
	configuration = strings.ReplaceAll(configuration, "listen 443 ssl default_server;", "listen unix:"+tlsSocketPath+" ssl default_server;")
	configuration = strings.ReplaceAll(configuration, "listen 443 ssl;", "listen unix:"+tlsSocketPath+" ssl;")
	configuration = "access_log off;\n" + configuration
	packagePath := os.Getenv("NGINX_LUA_PACKAGE_PATH")
	if packagePath != "" {
		configuration = "lua_package_path '" + strings.ReplaceAll(packagePath, "'", "\\'") + "';\n" + configuration
	}
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
		connection, dialErr := net.DialTimeout("unix", tlsSocketPath, 100*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporary nginx did not listen on %s: %v", tlsSocketPath, dialErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	request := func(method, path, userAgent, body, cookie string) string {
		t.Helper()
		rawConnection, err := net.DialTimeout("unix", tlsSocketPath, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		connection := tls.Client(rawConnection, &tls.Config{
			ServerName: "cdn.example.test", MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
		})
		defer connection.Close()
		if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := connection.Handshake(); err != nil {
			t.Fatal(err)
		}
		cookieHeader := ""
		if cookie != "" {
			cookieHeader = "Cookie: " + cookie + "\r\n"
		}
		if _, err := fmt.Fprintf(connection,
			"%s %s HTTP/1.0\r\nHost: cdn.example.test\r\nUser-Agent: %s\r\n%sContent-Length: %d\r\n\r\n%s",
			method, path, userAgent, cookieHeader, len(body), body); err != nil {
			t.Fatal(err)
		}
		response, err := io.ReadAll(connection)
		if err != nil {
			t.Fatal(err)
		}
		return string(response)
	}
	httpRequest := func(path, userAgent string) string {
		t.Helper()
		connection, err := net.DialTimeout("unix", httpSocketPath, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(connection,
			"GET %s HTTP/1.0\r\nHost: cdn.example.test\r\nUser-Agent: %s\r\n\r\n", path, userAgent); err != nil {
			t.Fatal(err)
		}
		response, err := io.ReadAll(connection)
		if err != nil {
			t.Fatal(err)
		}
		return string(response)
	}
	if response := httpRequest("/blocked", "scanner-client"); !strings.Contains(response, " 403 ") {
		t.Fatalf("HTTP scanner request bypassed WAF before redirect:\n%s", response)
	}
	if response := httpRequest("/redirect", "browser"); !strings.Contains(response, " 301 ") {
		t.Fatalf("clean HTTP request did not redirect after WAF:\n%s", response)
	}
	if response := request("GET", "/blocked", "scanner-client", "", ""); !strings.Contains(response, " 403 ") {
		t.Fatalf("scanner request was not blocked:\n%s", response)
	}
	if response := request("POST", "/submit", "browser", "id=1 union select password", ""); !strings.Contains(response, " 403 ") {
		t.Fatalf("SQL injection body was not blocked:\n%s", response)
	}
	challenge := request("GET", "/protected", "browser", "", "")
	if !strings.Contains(challenge, " 200 ") || !strings.Contains(challenge, "crypto.subtle.digest") {
		t.Fatalf("protected request did not receive a PoW challenge:\n%s", challenge)
	}
	if strings.Contains(challenge, "Cache-Control: public") || strings.Count(challenge, "Cache-Control:") != 1 {
		t.Fatalf("PoW challenge inherited the static resource cache policy:\n%s", challenge)
	}
	if strings.Count(challenge, "X-Content-Type-Options:") != 1 {
		t.Fatalf("PoW challenge inherited duplicate static response headers:\n%s", challenge)
	}
	tokenMatch := regexp.MustCompile(`const token="([A-Za-z0-9_.-]+)"`).FindStringSubmatch(challenge)
	if len(tokenMatch) != 2 {
		t.Fatalf("PoW challenge token was not found:\n%s", challenge)
	}
	token := tokenMatch[1]
	nonce := uint64(0)
	for {
		digest := sha256.Sum256([]byte(token + ":" + strconv.FormatUint(nonce, 10)))
		if digest[0] == 0 && digest[1] == 0 {
			break
		}
		nonce++
	}
	verificationPath := powVerifyPath + "?token=" + url.QueryEscape(token) + "&nonce=" + strconv.FormatUint(nonce, 10)
	verification := request("POST", verificationPath, "browser", "", "")
	if !strings.Contains(verification, " 204 ") {
		errorLog, _ := os.ReadFile(filepath.Join(directory, "error.log"))
		t.Fatalf("valid PoW proof was rejected (token %s, nonce %d):\n%s\n%s", token, nonce, verification, errorLog)
	}
	cookieMatch := regexp.MustCompile(`(?mi)^Set-Cookie:\s*([^;\r\n]+)`).FindStringSubmatch(verification)
	if len(cookieMatch) != 2 {
		t.Fatalf("PoW verification did not return a pass cookie:\n%s", verification)
	}
	if response := request("GET", "/protected", "browser", "", cookieMatch[1]); !strings.Contains(response, " 200 ") || !strings.Contains(response, "Cache-Control: public, max-age=3600") || !strings.HasSuffix(response, "allowed") {
		t.Fatalf("valid PoW pass did not reach the protected resource:\n%s", response)
	}
	if response := request("GET", "/allow", "browser", "", ""); !strings.Contains(response, " 200 ") || !strings.HasSuffix(response, "allowed") {
		t.Fatalf("allow rule did not bypass the PoW challenge:\n%s", response)
	}
	contents, err := os.ReadFile(securityLogPath)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, line := range bytes.Split(bytes.TrimSpace(contents), []byte("\n")) {
		var event struct {
			PolicyID string `json:"policy_id"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode runtime security log %q: %v", line, err)
		}
		counts[event.PolicyID]++
	}
	for _, policyID := range []string{
		"66666666-6666-4666-8666-666666666666",
		"22222222-2222-4222-8222-222222222222",
	} {
		if counts[policyID] != 2 {
			t.Fatalf("policy %s security log count = %d, want one HTTP and one HTTPS event; all=%#v", policyID, counts[policyID], counts)
		}
	}
}

func writeRuntimeTestCertificate(t *testing.T, directory string) (string, string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cdn.example.test"},
		DNSNames: []string{"cdn.example.test"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePath := filepath.Join(directory, "site.crt")
	keyPath := filepath.Join(directory, "site.key")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certificatePath, keyPath
}

func TestRuntimeWAFRunsBeforeEveryRateLimitAccessHandler(t *testing.T) {
	site := domain.Site{
		ID: "site-a", Name: "site-a", Domains: []string{"cdn.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}
	policy := domain.SecurityPolicy{
		ID: "11111111-1111-4111-8111-111111111111", Name: "scanner", Enabled: true,
		Conditions: []domain.SecurityCondition{{
			Field: domain.SecurityFieldUserAgent, Operator: domain.SecurityOperatorContains, Value: "scanner",
		}},
		Action: domain.SecurityActionBlock, ResponseStatus: 403, Priority: 100,
	}
	ratePolicy := domain.RateLimitPolicy{
		ID: "22222222-2222-4222-8222-222222222222", Name: "all", Enabled: true, RequestsPerSecond: 20,
	}
	reference := domain.StaticAssetReference{
		AssetID: "asset-a", BindingID: "binding-a", SiteID: site.ID, URLPath: "/app.js",
		SHA256: strings.Repeat("a", 64), SizeBytes: 10, ContentType: "text/javascript",
		CacheControl: domain.StaticAssetCacheHour,
	}
	configuration, err := RenderWithRuntimeOptions([]domain.Site{site}, []domain.SecurityPolicy{policy},
		[]domain.RateLimitPolicy{ratePolicy}, RenderRuntimeOptions{
			DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, WAFChainCapable: true,
			StaticAssets: []domain.StaticAssetReference{reference},
		})
	if err != nil {
		t.Fatal(err)
	}
	rateCalls := strings.Count(configuration, "package.loaded.simple_cdn_rate_limit.access()")
	combined := regexp.MustCompile(`(?s)access_by_lua_block\s*\{\s*package\.loaded\.simple_cdn_security\.access\(\)\s*package\.loaded\.simple_cdn_rate_limit\.access\(\)(?:\s*ngx\.ctx\.cdn_static_asset_allowed\s*=\s*true)?\s*\}`)
	if rateCalls == 0 || len(combined.FindAllString(configuration, -1)) != rateCalls {
		t.Fatalf("WAF does not precede every rate limit access handler: rate=%d combined=%d\n%s",
			rateCalls, len(combined.FindAllString(configuration, -1)), configuration)
	}
}

func TestMixedHTTPAndTCPNodeKeepsHTTPSecurityRuntime(t *testing.T) {
	httpSite := domain.Site{
		ID: "site-http", Name: "HTTP", Domains: []string{"cdn.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}
	tcpSite := domain.Site{ID: "site-tcp", Name: "TCP", TCPOnly: true, Enabled: true}
	policy := domain.SecurityPolicy{
		ID: "33333333-3333-4333-8333-333333333333", Name: "HTTP WAF", Enabled: true,
		Conditions: []domain.SecurityCondition{{
			Field: domain.SecurityFieldPath, Operator: domain.SecurityOperatorPrefix, Value: "/admin",
		}},
		Action: domain.SecurityActionBlock, ResponseStatus: 403, Priority: 100,
	}
	ratePolicy := domain.RateLimitPolicy{
		ID: "44444444-4444-4444-8444-444444444444", Name: "HTTP rate", Enabled: true, RequestsPerSecond: 10,
	}
	configuration, err := RenderWithRuntimeOptions([]domain.Site{httpSite, tcpSite}, []domain.SecurityPolicy{policy},
		[]domain.RateLimitPolicy{ratePolicy}, RenderRuntimeOptions{
			DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, WAFChainCapable: true,
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{
		"package.loaded.simple_cdn_security", "package.loaded.simple_cdn_rate_limit", `server_name cdn.example.test;`,
	} {
		if !strings.Contains(configuration, wanted) {
			t.Fatalf("mixed HTTP/TCP configuration lacks %q:\n%s", wanted, configuration)
		}
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
