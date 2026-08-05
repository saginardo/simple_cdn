package nginx

import (
	"strings"
	"testing"

	"simple_cdn/internal/domain"
)

func TestRenderCompressionCapabilityAndMIMEExclusions(t *testing.T) {
	site := domain.Site{
		ID: "site-compression", Name: "compression", Domains: []string{"cdn.example.test"},
		PrimaryOrigin:                domain.Origin{URL: "https://origin.example.test", Enabled: true},
		DynamicCompressionEnabled:    true,
		CompressionExcludedMIMETypes: []string{"application/json", "text/event-stream"},
		OriginResponseBuffering:      true, Enabled: true,
	}
	configuration, err := RenderWithRuntimeOptions([]domain.Site{site}, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, CompressionCapable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		CompressionRuntimeMarker, "gzip on;", "gzip_static on;", "brotli on;", "brotli_static on;",
		"zstd on;", "zstd_static on;", `"content_encoding":"$sent_http_content_encoding"`,
		`"brotli_ratio":"$brotli_ratio"`, `"zstd_ratio":"$zstd_ratio"`,
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("compression configuration is missing %q:\n%s", expected, configuration)
		}
	}
	for _, prefix := range []string{"gzip_types ", "brotli_types ", "zstd_types "} {
		start := strings.Index(configuration, prefix)
		if start < 0 {
			t.Fatalf("compression types directive %q is missing", prefix)
		}
		end := strings.Index(configuration[start:], ";")
		if end < 0 {
			t.Fatalf("compression types directive %q is unterminated", prefix)
		}
		directive := configuration[start : start+end]
		if strings.Contains(directive, "application/json") || strings.Contains(directive, "text/event-stream") || !strings.Contains(directive, "text/css") {
			t.Fatalf("unexpected %s directive: %s", prefix, directive)
		}
	}

	site.DynamicCompressionEnabled = false
	disabled, err := RenderWithRuntimeOptions([]domain.Site{site}, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, CompressionCapable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"gzip off;", "brotli off;", "zstd off;", "gzip_static on;", "brotli_static on;", "zstd_static on;"} {
		if !strings.Contains(disabled, expected) {
			t.Fatalf("disabled dynamic compression is missing %q", expected)
		}
	}
}

func TestRenderLegacyCompressionAndScopedCacheInvalidation(t *testing.T) {
	site := domain.Site{
		ID: "site-cache", Name: "cache", Domains: []string{"cdn.example.test"},
		PrimaryOrigin:             domain.Origin{URL: "https://origin.example.test", Enabled: true},
		DynamicCompressionEnabled: true, OriginResponseBuffering: true, CacheGeneration: 7,
		CacheInvalidations: []domain.CacheInvalidationRule{
			{Scope: domain.CacheInvalidationPrefix, Value: "/assets/v1.2/", Generation: 8},
			{Scope: domain.CacheInvalidationURL, Value: "/index.html?v=2", Generation: 9},
		},
		Enabled: true,
	}
	configuration, err := RenderWithRuntimeOptions([]domain.Site{site}, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"gzip on;", "gzip_static on;", `set $cdn_cache_scope_generation "0";`,
		`if ($uri ~ "^/assets/v1\.2/") { set $cdn_cache_scope_generation "8"; }`,
		`if ($request_uri = "/index.html?v=2") { set $cdn_cache_scope_generation "9"; }`,
		`site-cache:7:$cdn_cache_scope_generation:$scheme$host$request_uri`,
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("legacy/scoped configuration is missing %q:\n%s", expected, configuration)
		}
	}
	for _, unexpected := range []string{CompressionRuntimeMarker, "brotli on;", "zstd on;", "$brotli_ratio", "$zstd_ratio"} {
		if strings.Contains(configuration, unexpected) {
			t.Fatalf("legacy node configuration contains %q:\n%s", unexpected, configuration)
		}
	}
}
