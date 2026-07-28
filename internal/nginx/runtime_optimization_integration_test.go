package nginx

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"simple_cdn/internal/domain"
)

func TestRuntimeOptimizationsPassNginx1304Validation(t *testing.T) {
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
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	cacheDirectory := filepath.Join(directory, "cache")
	if err := os.MkdirAll(cacheDirectory, 0o777); err != nil {
		t.Fatal(err)
	}
	certificatePath := filepath.Join(directory, "runtime.crt")
	privateKeyPath := filepath.Join(directory, "runtime.key")
	writeHTTP3TestCertificate(t, certificatePath, privateKeyPath)
	quicHostKeyPath := filepath.Join(directory, "quic-host.key")
	quicHostKey := make([]byte, 32)
	if _, err := rand.Read(quicHostKey); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(quicHostKeyPath, quicHostKey, 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := RenderWithRuntimeOptions([]domain.Site{{
		ID: "runtime", Name: "runtime", Domains: []string{"runtime.test"},
		PrimaryOrigin: domain.Origin{URL: "http://127.0.0.1:8080", Enabled: true}, HTTP3Enabled: true, Enabled: true,
	}}, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, HTTP3Capable: true, QUICHostKeyPath: quicHostKeyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	ports := reserveHTTP3TCPPorts(t, 3)
	quicPort := reserveHTTP3DualProtocolPort(t)
	configuration = strings.Replace(configuration, "listen 80 default_server;", fmt.Sprintf("listen 127.0.0.1:%d default_server;", ports[0]), 1)
	configuration = strings.Replace(configuration, "listen 80;", fmt.Sprintf("listen 127.0.0.1:%d;", ports[1]), 1)
	configuration = strings.Replace(configuration, "listen 443 ssl default_server;", fmt.Sprintf("listen 127.0.0.1:%d ssl default_server;", ports[2]), 1)
	configuration = strings.Replace(configuration, "listen 443 quic reuseport default_server;", fmt.Sprintf("listen 127.0.0.1:%d quic reuseport default_server;", quicPort), 1)
	configuration = strings.Replace(configuration, "listen 443 ssl;", fmt.Sprintf("listen 127.0.0.1:%d ssl;", quicPort), 1)
	configuration = strings.Replace(configuration, "listen 443 quic;", fmt.Sprintf("listen 127.0.0.1:%d quic;", quicPort), 1)
	configuration = strings.ReplaceAll(configuration, DefaultCachePath, cacheDirectory)
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/logs/access.json", filepath.Join(directory, "access.json"))
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/config/certs/runtime.crt", certificatePath)
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/config/certs/runtime.key", privateKeyPath)
	statusSocketPath := filepath.Join(directory, "status.sock")
	configuration += fmt.Sprintf("\nserver { listen unix:%s; access_log off; location = /stub_status { stub_status; } }\n", statusSocketPath)
	nginxConfiguration := buildIsolatedNginxConfiguration(
		t, directory, "pcre_jit on;\nworker_shutdown_timeout 1h;", filepath.Join(directory, "error.log"), configuration,
	)
	configurationPath := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(configurationPath, []byte(nginxConfiguration), 0o644); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(docker, "run", "--rm", "--network", "host", "-v", directory+":"+directory+":rw", image, binary, "-t", "-c", configurationPath, "-p", directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("nginx -t: %v\n%s\n%s", err, output, nginxConfiguration)
	}
	for _, expected := range []string{
		"pcre_jit on;", "worker_shutdown_timeout 1h;", "quic_host_key " + quicHostKeyPath + ";",
		"ssl_session_timeout 30m;", "listen unix:" + statusSocketPath + ";", "stub_status;",
	} {
		if !strings.Contains(nginxConfiguration, expected) {
			t.Fatalf("validated configuration is missing %q", expected)
		}
	}
}
