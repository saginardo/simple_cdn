package nginx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"simple_cdn/internal/domain"
)

func TestManagedOriginPoolPassesNginxValidation(t *testing.T) {
	if os.Getenv("NGINX_DOCKER_TEST") != "1" {
		t.Skip("NGINX_DOCKER_TEST is not enabled")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	poolDirectory := filepath.Join(directory, "origin-pools")
	cacheDirectory := filepath.Join(directory, "cache")
	if err := os.MkdirAll(poolDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheDirectory, 0o777); err != nil {
		t.Fatal(err)
	}
	const siteID = "origin-pool-integration"
	rendered, err := RenderNodeWithRuntimeOptions([]domain.Site{{
		ID: siteID, Name: "origin pool", Domains: []string{"origin-pool.test"},
		PrimaryOrigin: domain.Origin{URL: "http://127.0.0.1:8080", Enabled: true}, Enabled: true,
	}}, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, ManagedOriginPools: true,
		NginxWorkerConnections: 4096, OriginPoolConfigDirectory: poolDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.OriginPools) != 1 {
		t.Fatalf("origin pools = %#v", rendered.OriginPools)
	}
	pool := rendered.OriginPools[0]
	certificatePath := filepath.Join(directory, siteID+".crt")
	privateKeyPath := filepath.Join(directory, siteID+".key")
	writeHTTP3TestCertificate(t, certificatePath, privateKeyPath)
	httpConfiguration := strings.ReplaceAll(rendered.NginxConfig, DefaultCachePath, cacheDirectory)
	httpConfiguration = strings.ReplaceAll(httpConfiguration, "/opt/cdn-edge/logs/access.json", filepath.Join(directory, "access.json"))
	httpConfiguration = strings.ReplaceAll(httpConfiguration, "/opt/cdn-edge/config/certs/"+siteID+".crt", certificatePath)
	httpConfiguration = strings.ReplaceAll(httpConfiguration, "/opt/cdn-edge/config/certs/"+siteID+".key", privateKeyPath)
	configurationPath := filepath.Join(directory, "nginx.conf")
	nginxConfiguration := buildIsolatedNginxConfiguration(t, directory, "", filepath.Join(directory, "error.log"), httpConfiguration)
	if err := os.WriteFile(configurationPath, []byte(nginxConfiguration), 0o644); err != nil {
		t.Fatal(err)
	}
	validate := func(contents string) {
		t.Helper()
		if err := os.WriteFile(pool.ConfigPath, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("docker", "run", "--rm", "-v", directory+":"+directory+":rw", "nginx:1.28-alpine", "nginx", "-t", "-c", configurationPath, "-p", directory)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("nginx -t: %v\n%s\n%s", err, output, nginxConfiguration)
		}
	}
	validate("server " + pool.Address + " max_fails=1 fail_timeout=5s;\n")
	validate("server " + pool.Address + " down;\n")
}
