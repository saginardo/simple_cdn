package edge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple_cdn/internal/domain"
)

func TestNewLoadsManagedNginxMetadataAndCapability(t *testing.T) {
	directory := t.TempDir()
	versionPath := filepath.Join(directory, "VERSION")
	digestPath := filepath.Join(directory, ".bundle-sha256")
	digest := strings.Repeat("a", 64)
	if err := os.WriteFile(versionPath, []byte("1.30.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(digestPath, []byte(strings.ToUpper(digest)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "BUILD.json"), []byte(`{"ngx_brotli_commit":"`+strings.Repeat("c", 40)+`","brotli_commit":"`+strings.Repeat("d", 40)+`","zstd_nginx_commit":"`+strings.Repeat("e", 40)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(directory, "config")
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: filepath.Join(directory, "data"),
		CertificateDir: filepath.Join(directory, "certs"), AgentSHA256: strings.Repeat("b", 64),
		NginxConfigPath:  filepath.Join(configDir, "cdn-platform.conf"),
		NginxVersionPath: versionPath, NginxSHA256Path: digestPath, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Config.NginxVersion != "1.30.4" || agent.Config.NginxSHA256 != digest {
		t.Fatalf("managed Nginx metadata = version %q digest %q", agent.Config.NginxVersion, agent.Config.NginxSHA256)
	}
	found, foundOriginHTTP2, foundCompression, foundCacheControl := false, false, false, false
	for _, capability := range agent.Config.Capabilities {
		if capability == domain.EdgeCapabilityNginxBundle {
			found = true
		}
		if capability == domain.EdgeCapabilityOriginHTTP2 {
			foundOriginHTTP2 = true
		}
		if capability == domain.EdgeCapabilityCompression {
			foundCompression = true
		}
		if capability == domain.EdgeCapabilityCacheControl {
			foundCacheControl = true
		}
	}
	if !found || !foundOriginHTTP2 || !foundCompression || !foundCacheControl {
		t.Fatalf("managed Nginx capability missing from %#v", agent.Config.Capabilities)
	}
}

func TestManagedNginxHTTP2OriginVersionGate(t *testing.T) {
	for version, wanted := range map[string]bool{
		"": false, "1.29.3": false, "1.29.4": true, "1.30.4": true, "2.0.0": true,
	} {
		if got := managedNginxVersionAtLeast(version, 1, 29, 4); got != wanted {
			t.Fatalf("managedNginxVersionAtLeast(%q) = %t, want %t", version, got, wanted)
		}
	}
}

func TestLoadManagedNginxMetadataRejectsPartialOrSignedVersion(t *testing.T) {
	validDigest := strings.Repeat("c", 64)
	for name, config := range map[string]Config{
		"missing digest":   {NginxVersion: "1.30.4"},
		"negative segment": {NginxVersion: "1.-30.4", NginxSHA256: validDigest},
		"positive segment": {NginxVersion: "1.+30.4", NginxSHA256: validDigest},
		"invalid digest":   {NginxVersion: "1.30.4", NginxSHA256: strings.Repeat("z", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := loadManagedNginxMetadata(&config); err == nil {
				t.Fatal("invalid managed Nginx metadata was accepted")
			}
		})
	}
}
