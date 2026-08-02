package nginx

import (
	"strings"
	"testing"

	"simple_cdn/internal/domain"
)

func TestRenderStaticAssetExactLocation(t *testing.T) {
	site := domain.Site{
		ID: "site-static", Name: "static", Domains: []string{"static.example.test"}, Enabled: true,
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
	}
	digest := strings.Repeat("a", 64)
	configuration, err := RenderWithRuntimeOptions([]domain.Site{site}, nil, []domain.RateLimitPolicy{{
		ID: "rate", Name: "rate", Enabled: true, RequestsPerSecond: 100,
	}}, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB,
		StaticAssets: []domain.StaticAssetReference{{
			AssetID: "asset", BindingID: "binding", SiteID: site.ID, URLPath: "/assets/app.js",
			SHA256: digest, SizeBytes: 12, ContentType: "application/javascript",
			CacheControl: domain.StaticAssetCacheImmutable,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`location = "/assets/app.js"`,
		`alias "/opt/cdn-edge/static/objects/` + digest + `";`,
		`default_type "application/javascript";`,
		`ngx.header["Cache-Control"] = "public, max-age=31536000, immutable"`,
		`ngx.ctx.cdn_static_asset_allowed = true`,
		`package.loaded.simple_cdn_rate_limit.access()`,
		`limit_except GET HEAD { deny all; }`,
	} {
		if !strings.Contains(configuration, expected) {
			t.Errorf("rendered configuration is missing %q", expected)
		}
	}
}

func TestRenderStaticAssetHeadersDoNotCacheErrorResponses(t *testing.T) {
	site := domain.Site{
		ID: "site-static", Name: "static", Domains: []string{"static.example.test"}, Enabled: true,
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
	}
	configuration, err := RenderWithRuntimeOptions([]domain.Site{site}, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB,
		StaticAssets: []domain.StaticAssetReference{{
			AssetID: "asset", BindingID: "binding", SiteID: site.ID, URLPath: "/app.js",
			SHA256: strings.Repeat("a", 64), SizeBytes: 12, ContentType: "application/javascript",
			CacheControl: domain.StaticAssetCacheImmutable,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`add_header Cache-Control "public, max-age=31536000, immutable";`,
		`add_header X-Content-Type-Options nosniff;`,
	} {
		if !strings.Contains(configuration, expected) {
			t.Errorf("rendered configuration is missing %q", expected)
		}
	}
	if strings.Contains(configuration, `add_header Cache-Control "public, max-age=31536000, immutable" always;`) {
		t.Fatal("static resource error responses can inherit the long-lived cache policy")
	}
}

func TestRenderStaticAssetRejectsUnknownSiteAndDuplicatePath(t *testing.T) {
	site := domain.Site{
		ID: "site-static", Name: "static", Domains: []string{"static.example.test"}, Enabled: true,
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
	}
	reference := domain.StaticAssetReference{
		AssetID: "asset", BindingID: "binding", SiteID: "unknown", URLPath: "/app.js",
		SHA256: strings.Repeat("b", 64), SizeBytes: 1, ContentType: "application/javascript",
		CacheControl: domain.StaticAssetCacheHour,
	}
	if _, err := RenderWithRuntimeOptions([]domain.Site{site}, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, StaticAssets: []domain.StaticAssetReference{reference},
	}); err == nil {
		t.Fatal("accepted a static asset for an unknown site")
	}
	reference.SiteID = site.ID
	duplicate := reference
	duplicate.BindingID = "binding-2"
	if _, err := RenderWithRuntimeOptions([]domain.Site{site}, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB,
		StaticAssets:       []domain.StaticAssetReference{reference, duplicate},
	}); err == nil {
		t.Fatal("accepted duplicate static asset paths")
	}
}
