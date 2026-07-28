package nginx

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestRenderedH2COriginServesAndReusesARealHTTP2Connection(t *testing.T) {
	image := strings.TrimSpace(os.Getenv("NGINX_1304_DOCKER_IMAGE"))
	if os.Getenv("NGINX_DOCKER_TEST") != "1" || image == "" {
		t.Skip("NGINX_DOCKER_TEST and NGINX_1304_DOCKER_IMAGE are not enabled")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker is not installed")
	}
	binary := strings.TrimSpace(os.Getenv("NGINX_1304_DOCKER_BINARY"))
	if binary == "" {
		binary = "/out/nginx"
	}

	var accepted atomic.Int64
	originHandler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.ProtoMajor != 2 {
			t.Errorf("origin protocol = %s", request.Proto)
		}
		response.Header().Set("X-Origin-Protocol", request.Proto)
		_, _ = io.WriteString(response, "h2c-ok")
	})
	originListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	origin := &http.Server{
		Handler: originHandler, Protocols: protocols,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				accepted.Add(1)
			}
		},
	}
	go func() { _ = origin.Serve(originListener) }()
	defer origin.Close()
	originURL := "http://" + originListener.Addr().String()

	configuration, err := RenderWithRuntimeOptions([]domain.Site{{
		ID: "h2c-integration", Name: "h2c integration", Domains: []string{"h2c.test"},
		PrimaryOrigin: domain.Origin{URL: originURL, HTTPVersion: domain.OriginHTTPVersionH2C, Enabled: true}, Enabled: true,
	}}, nil, nil, RenderRuntimeOptions{DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, OriginHTTP2Capable: true})
	if err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	cacheDirectory := filepath.Join(directory, "cache")
	if err := os.MkdirAll(cacheDirectory, 0o777); err != nil {
		t.Fatal(err)
	}
	ports := reserveHTTP3TCPPorts(t, 4)
	configuration = strings.Replace(configuration, "listen 80 default_server;", fmt.Sprintf("listen 127.0.0.1:%d default_server;", ports[0]), 1)
	configuration = strings.Replace(configuration, "listen 443 ssl default_server;", fmt.Sprintf("listen 127.0.0.1:%d default_server;", ports[1]), 1)
	configuration = strings.Replace(configuration, "ssl_reject_handshake on;", "return 444;", 1)
	configuration = strings.Replace(configuration, "listen 80;", fmt.Sprintf("listen 127.0.0.1:%d;", ports[2]), 1)
	configuration = strings.Replace(configuration, "listen 443 ssl;", fmt.Sprintf("listen 127.0.0.1:%d;", ports[3]), 1)
	configuration = strings.ReplaceAll(configuration, DefaultCachePath, cacheDirectory)
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/logs/access.json", filepath.Join(directory, "access.json"))
	lines := strings.Split(configuration, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "http2 on;" || strings.HasPrefix(trimmed, "ssl_certificate ") ||
			strings.HasPrefix(trimmed, "ssl_certificate_key ") || strings.HasPrefix(trimmed, "ssl_protocols ") ||
			strings.HasPrefix(trimmed, "ssl_session_cache ") || strings.HasPrefix(trimmed, "ssl_session_timeout ") {
			continue
		}
		filtered = append(filtered, line)
	}
	configuration = strings.Join(filtered, "\n")
	configurationPath := filepath.Join(directory, "nginx.conf")
	errorLogPath := filepath.Join(directory, "error.log")
	nginxConfiguration := buildIsolatedNginxConfiguration(t, directory, "", errorLogPath, configuration)
	if err := os.WriteFile(configurationPath, []byte(nginxConfiguration), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerArgs := []string{"run", "--rm", "--network", "host", "-v", directory + ":" + directory + ":rw", image, binary}
	if output, err := exec.Command(docker, append(dockerArgs, "-t", "-c", configurationPath, "-p", directory)...).CombinedOutput(); err != nil {
		t.Fatalf("nginx -t: %v\n%s\n%s", err, output, nginxConfiguration)
	}

	var processOutput bytes.Buffer
	command := exec.Command(docker, append(dockerArgs, "-c", configurationPath, "-p", directory, "-g", "daemon off;")...)
	command.Stdout = &processOutput
	command.Stderr = &processOutput
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
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
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	requestURL := "http://127.0.0.1:" + strconv.Itoa(ports[3]) + "/probe"
	request := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			httpRequest, requestErr := http.NewRequest(http.MethodGet, requestURL, nil)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			httpRequest.Host = "h2c.test"
			response, requestErr := client.Do(httpRequest)
			if requestErr == nil {
				body, readErr := io.ReadAll(response.Body)
				response.Body.Close()
				if readErr == nil && response.StatusCode == http.StatusOK && string(body) == "h2c-ok" && response.Header.Get("X-Origin-Protocol") == "HTTP/2.0" {
					return
				}
			}
			if time.Now().After(deadline) {
				errorLog, _ := os.ReadFile(errorLogPath)
				t.Fatalf("H2C request failed: %v\nprocess output:\n%s\nerror log:\n%s", requestErr, processOutput.String(), errorLog)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	request()
	request()
	if accepted.Load() != 1 {
		t.Fatalf("origin accepted %d connections for two sequential requests, want one reused HTTP/2 connection", accepted.Load())
	}
}
