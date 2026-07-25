package nginx

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestRenderedCacheUsesOnlyStaticSuffixes(t *testing.T) {
	binary, err := exec.LookPath("nginx")
	if err != nil {
		t.Skip("nginx is not installed")
	}
	type originRequest struct {
		Path          string
		Cookie        string
		Authorization string
	}
	var originMu sync.Mutex
	var originRequests []originRequest
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		originMu.Lock()
		originRequests = append(originRequests, originRequest{
			Path: request.URL.Path, Cookie: request.Header.Get("Cookie"), Authorization: request.Header.Get("Authorization"),
		})
		sequence := len(originRequests)
		originMu.Unlock()
		response.Header().Set("Cache-Control", "public, max-age=3600")
		if request.URL.Path == "/assets/session.js" {
			response.Header().Set("Set-Cookie", "origin_session=changed; Path=/")
		}
		response.Header().Set("X-Origin-Sequence", strconv.Itoa(sequence))
		_, _ = fmt.Fprintf(response, "origin-%d", sequence)
	}))
	defer origin.Close()

	configuration, err := Render([]domain.Site{{
		ID: "cache-test", Name: "cache-test", Domains: []string{"cache.test"},
		PrimaryOrigin: domain.Origin{URL: origin.URL, Enabled: true}, CacheGeneration: 1, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	cacheDirectory := filepath.Join(directory, "cache")
	cacheSitesDirectory := filepath.Join(cacheDirectory, "sites")
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheSitesDirectory, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cacheDirectory, cacheSitesDirectory} {
		if err := os.Chmod(path, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	configuration, nginxPort := prepareCacheIntegrationConfiguration(t, configuration, directory)
	path := filepath.Join(directory, "nginx.conf")
	nginxConfiguration := buildIsolatedNginxConfiguration(
		t, directory, "", filepath.Join(directory, "error.log"), "access_log off;\n"+configuration,
	)
	if err := os.WriteFile(path, []byte(nginxConfiguration), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(binary, "-t", "-c", path, "-p", directory).CombinedOutput(); err != nil {
		t.Fatalf("nginx -t: %v\n%s\n%s", err, output, nginxConfiguration)
	}

	command := exec.Command(binary, "-c", path, "-p", directory, "-g", "daemon off;")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	stop := func() {
		if command.Process != nil {
			_ = command.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if command.Process != nil {
				_ = command.Process.Kill()
			}
			<-done
		}
	}
	defer stop()

	client := &http.Client{Timeout: 2 * time.Second}
	baseURL := "http://127.0.0.1:" + strconv.Itoa(nginxPort)
	waitForCacheNginx(t, client, baseURL, filepath.Join(directory, "error.log"))
	request := func(path, cookie, authorization string) (string, string, string) {
		t.Helper()
		httpRequest, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		httpRequest.Host = "cache.test"
		if cookie != "" {
			httpRequest.Header.Set("Cookie", cookie)
		}
		if authorization != "" {
			httpRequest.Header.Set("Authorization", authorization)
		}
		response, err := client.Do(httpRequest)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			errorLog, _ := os.ReadFile(filepath.Join(directory, "error.log"))
			t.Fatalf("GET %s returned %d: %s\nerror log:\n%s", path, response.StatusCode, body, errorLog)
		}
		return string(body), response.Header.Get("X-Test-Cache"), response.Header.Get("X-Origin-Sequence")
	}

	if body, cache, sequence := request("/assets/app.js", "session=first", "Bearer first"); body != "origin-1" || cache != "MISS" || sequence != "1" {
		t.Fatalf("first static response body=%q cache=%q sequence=%q", body, cache, sequence)
	}
	if body, cache, sequence := request("/assets/app.js", "session=second", "Bearer second"); body != "origin-1" || cache != "HIT" || sequence != "1" {
		t.Fatalf("cached static response body=%q cache=%q sequence=%q", body, cache, sequence)
	}
	if body, cache, sequence := request("/api/data", "session=dynamic", "Bearer dynamic"); body != "origin-2" || cache != "" || sequence != "2" {
		t.Fatalf("first dynamic response body=%q cache=%q sequence=%q", body, cache, sequence)
	}
	if body, cache, sequence := request("/api/data", "", ""); body != "origin-3" || cache != "" || sequence != "3" {
		t.Fatalf("repeated dynamic response body=%q cache=%q sequence=%q", body, cache, sequence)
	}
	if body, cache, sequence := request("/assets/session.js", "session=first", ""); body != "origin-4" || cache != "MISS" || sequence != "4" {
		t.Fatalf("first Set-Cookie static response body=%q cache=%q sequence=%q", body, cache, sequence)
	}
	if body, cache, sequence := request("/assets/session.js", "session=second", ""); body != "origin-5" || cache != "MISS" || sequence != "5" {
		t.Fatalf("repeated Set-Cookie static response body=%q cache=%q sequence=%q", body, cache, sequence)
	}

	originMu.Lock()
	defer originMu.Unlock()
	if len(originRequests) != 5 {
		t.Fatalf("origin requests = %#v", originRequests)
	}
	if originRequests[0].Path != "/assets/app.js" || originRequests[0].Cookie != "session=first" || originRequests[0].Authorization != "Bearer first" {
		t.Fatalf("public static origin request = %#v", originRequests[0])
	}
	if originRequests[1].Path != "/api/data" || originRequests[1].Cookie != "session=dynamic" || originRequests[1].Authorization != "Bearer dynamic" {
		t.Fatalf("first dynamic origin request = %#v", originRequests[1])
	}
	if originRequests[2].Path != "/api/data" || originRequests[2].Cookie != "" || originRequests[2].Authorization != "" {
		t.Fatalf("repeated dynamic origin request = %#v", originRequests[2])
	}
	for _, request := range originRequests[3:] {
		if request.Path != "/assets/session.js" || request.Cookie == "" {
			t.Fatalf("Set-Cookie static origin request = %#v", request)
		}
	}
}

func prepareCacheIntegrationConfiguration(t *testing.T, configuration, directory string) (string, int) {
	t.Helper()
	ports := reserveCacheIntegrationPorts(t, 4)
	configuration = strings.Replace(configuration, "listen 80 default_server;", fmt.Sprintf("listen 127.0.0.1:%d default_server;", ports[0]), 1)
	configuration = strings.Replace(configuration, "listen 443 ssl default_server;", fmt.Sprintf("listen 127.0.0.1:%d default_server;", ports[1]), 1)
	configuration = strings.Replace(configuration, "ssl_reject_handshake on;", "return 444;", 1)
	configuration = strings.Replace(configuration, "listen 80;", fmt.Sprintf("listen 127.0.0.1:%d;", ports[2]), 1)
	configuration = strings.Replace(configuration, "listen 443 ssl;", fmt.Sprintf("listen 127.0.0.1:%d; # cache-integration-port", ports[3]), 1)
	configuration = strings.ReplaceAll(configuration, "server_name cache.test;", "server_name cache.test;\n    add_header X-Test-Cache $upstream_cache_status always;")
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/cache", filepath.Join(directory, "cache"))
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/logs/access.json", filepath.Join(directory, "access.json"))
	lines := strings.Split(configuration, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "http2 on;" || strings.HasPrefix(trimmed, "ssl_certificate ") ||
			strings.HasPrefix(trimmed, "ssl_certificate_key ") || strings.HasPrefix(trimmed, "ssl_protocols ") ||
			strings.HasPrefix(trimmed, "ssl_session_cache ") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n"), ports[3]
}

func reserveCacheIntegrationPorts(t *testing.T, count int) []int {
	t.Helper()
	listeners := make([]net.Listener, 0, count)
	ports := make([]int, 0, count)
	for range count {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, opened := range listeners {
				opened.Close()
			}
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		ports = append(ports, listener.Addr().(*net.TCPAddr).Port)
	}
	for _, listener := range listeners {
		listener.Close()
	}
	return ports
}

func waitForCacheNginx(t *testing.T, client *http.Client, baseURL, errorLogPath string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(http.MethodGet, baseURL+"/__cdn_health", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = "cache.test"
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	errorLog, _ := os.ReadFile(errorLogPath)
	t.Fatalf("nginx did not become ready:\n%s", errorLog)
}
