package nginx

import (
	"regexp"
	"strings"
	"testing"

	"simple_cdn/internal/domain"
)

func TestRenderIncludesCacheAndFailoverPolicy(t *testing.T) {
	backup := domain.Origin{URL: "https://backup.example.test", Enabled: true}
	configuration, err := Render([]domain.Site{{ID: "site-1", Name: "site", Domains: []string{"cdn.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, BackupOrigin: &backup, CacheGeneration: 7, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"proxy_cache_path /opt/cdn-edge/cache levels=1:2 keys_zone=cdn_cache:16m inactive=7d max_size=1g use_temp_path=off", `map $uri $cdn_static_cache_zone { default off; ~*\.(?:css|js|mjs|map|wasm|woff|woff2|ttf|otf|eot|jpg|jpeg|png|apng|gif|webp|avif|svg|ico|bmp|tif|tiff|webmanifest)$ cdn_cache; }`, `map "$http_authorization:$http_cookie" $cdn_private_cache_bypass { default 1; ":" 0; }`, "proxy_cache $cdn_static_cache_zone", "proxy_cache_bypass $cdn_private_cache_bypass;", "proxy_no_cache $cdn_private_cache_bypass;", "listen 443 ssl default_server;", "ssl_reject_handshake on;", "client_max_body_size 128m;", "keepalive_timeout 120s;", "keepalive_requests 1000;", "keepalive 30;", "proxy_connect_timeout 10s;", "recursive_error_pages on;", "ssl_certificate /opt/cdn-edge/config/certs/site-1.crt", "access_log /opt/cdn-edge/logs/access.json cdn_json", `"request_id":"$request_id"`, `"upstream_connect_time":"$upstream_connect_time"`, `"upstream_header_time":"$upstream_header_time"`, `"user_agent":"$http_user_agent"`, "proxy_cache_lock on", "proxy_cache_background_update on", "proxy_cache_use_stale error timeout", "upstream origin_site-1_primary", "upstream origin_site-1_backup", "proxy_ssl_name origin.example.test", "proxy_ssl_name backup.example.test", "proxy_set_header Host backup.example.test", "proxy_set_header Upgrade \"\";", "proxy_set_header Connection \"\";", "location @cdn_http_site-1", "location @cdn_stream_site-1", "location @cdn_backup_site-1", "location @cdn_stream_backup_site-1", "site-1:7:$scheme$host$request_uri", "location = /__cdn_health", `return 200 "site=site-1\n";`} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("missing %q from config:\n%s", expected, configuration)
		}
	}
	if !HasSiteHealth(configuration, "site-1") || HasSiteHealth(configuration, "other-site") {
		t.Fatalf("site health capability detection is incorrect:\n%s", configuration)
	}
	if got := strings.Count(configuration, "proxy_set_header Connection \"\";"); got != 2 {
		t.Fatalf("expected Connection header to be cleared in both regular proxy locations, got %d:\n%s", got, configuration)
	}
	if got := strings.Count(configuration, "proxy_set_header Upgrade \"\";"); got != 2 {
		t.Fatalf("expected Upgrade header to be cleared in both regular proxy locations, got %d:\n%s", got, configuration)
	}
	for _, retired := range []string{
		"$cdn_has_auth",
		"$cdn_has_cookie",
		"$cdn_cookie_cache_bypass",
		"$cdn_upstream_cookie",
		"proxy_set_header Cookie",
	} {
		if strings.Contains(configuration, retired) {
			t.Fatalf("configuration still contains retired auth/cookie cache logic %q:\n%s", retired, configuration)
		}
	}
	if got := strings.Count(configuration, "keepalive 30;"); got != 2 {
		t.Fatalf("expected one 30-connection pool for each upstream, got %d:\n%s", got, configuration)
	}
	if got := strings.Count(configuration, "proxy_ssl_verify_depth 3;"); got != 4 {
		t.Fatalf("expected TLS verification depth on normal/stream primary/backup paths, got %d:\n%s", got, configuration)
	}
	for _, retired := range []string{"keepalive 32;", "proxy_connect_timeout 5s;", "grpc_connect_timeout 5s;", "proxy_read_timeout 60s;"} {
		if strings.Contains(configuration, retired) {
			t.Fatalf("configuration still contains retired connection setting %q:\n%s", retired, configuration)
		}
	}
	if strings.Contains(configuration, "max_size=50g") {
		t.Fatalf("configuration still uses the retired 50g default:\n%s", configuration)
	}
}

func TestRenderHTTP3RequiresSiteOptInAndCapability(t *testing.T) {
	site := domain.Site{
		ID: "site-h3", Name: "HTTP/3 site", Domains: []string{"h3.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}
	legacyConfiguration, err := Render([]domain.Site{site})
	if err != nil {
		t.Fatal(err)
	}
	for _, unexpected := range []string{"listen 443 quic", "http3 on;", "quic_retry on;", "Alt-Svc"} {
		if strings.Contains(legacyConfiguration, unexpected) {
			t.Fatalf("legacy configuration unexpectedly contains %q:\n%s", unexpected, legacyConfiguration)
		}
	}

	capableButDisabled, err := RenderWithRuntimeOptions([]domain.Site{site}, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB,
		HTTP3Capable:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, unexpected := range []string{"listen 443 quic", "http3 on;", "quic_retry on;", "Alt-Svc"} {
		if strings.Contains(capableButDisabled, unexpected) {
			t.Fatalf("site without HTTP/3 opt-in unexpectedly contains %q:\n%s", unexpected, capableButDisabled)
		}
	}

	site.HTTP3Enabled = true
	http2Site := domain.Site{
		ID: "site-http2", Name: "HTTP/2 site", Domains: []string{"h2.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}
	configuration, err := RenderWithRuntimeOptions([]domain.Site{site, http2Site}, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB,
		HTTP3Capable:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"listen 443 ssl default_server;",
		"listen 443 quic reuseport default_server;",
		"listen 443 ssl;",
		"listen 443 quic;",
		"http2 on;",
		"http3 on;",
		"quic_host_key /opt/cdn-edge/config/nginx/quic-host.key;",
		"quic_retry on;",
		`add_header Alt-Svc 'h3=":443"; ma=86400' always;`,
		"ssl_protocols TLSv1.2 TLSv1.3;",
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("HTTP/3 configuration is missing %q:\n%s", expected, configuration)
		}
	}
	if count := strings.Count(configuration, "    listen 443 quic;\n"); count != 1 {
		t.Fatalf("site QUIC listeners = %d, want one opted-in site:\n%s", count, configuration)
	}
	if count := strings.Count(configuration, "add_header Alt-Svc"); count != 1 {
		t.Fatalf("Alt-Svc headers = %d, want one opted-in site:\n%s", count, configuration)
	}
	if strings.Contains(configuration, "ssl_early_data") {
		t.Fatalf("HTTP/3 configuration unexpectedly enables replayable 0-RTT data:\n%s", configuration)
	}
}

func TestRenderManagedOriginPoolsSharesCompatibleConnections(t *testing.T) {
	sites := []domain.Site{
		{ID: "site-a", Name: "a", Domains: []string{"a.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true},
		{ID: "site-b", Name: "b", Domains: []string{"b.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true},
	}
	rendered, err := RenderNodeWithRuntimeOptions(sites, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, ManagedOriginPools: true,
		NginxWorkerConnections: 4096, OriginPoolConfigDirectory: "/tmp/cdn-origin-pools",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.OriginPools) != 1 {
		t.Fatalf("origin pools = %#v, want one shared pool", rendered.OriginPools)
	}
	pool := rendered.OriginPools[0]
	if pool.Address != "origin.example.test:443" || pool.KeepaliveConnections != 64 || len(pool.References) != 2 || pool.ConfigPath != "/tmp/cdn-origin-pools/"+pool.ID+".conf" {
		t.Fatalf("shared pool = %#v", pool)
	}
	for _, expected := range []string{
		"upstream origin_pool_" + pool.ID,
		"include " + pool.ConfigPath + ";",
		"keepalive 64;",
		"keepalive_timeout 45s;",
		"keepalive_requests 1000;",
		"keepalive_time 1h;",
		"proxy_pass https://origin_pool_" + pool.ID,
		"proxy_ssl_session_reuse on;",
	} {
		if !strings.Contains(rendered.NginxConfig, expected) {
			t.Fatalf("managed configuration is missing %q:\n%s", expected, rendered.NginxConfig)
		}
	}
	if strings.Count(rendered.NginxConfig, "upstream origin_pool_") != 1 || strings.Contains(rendered.NginxConfig, "server origin.example.test:443;") {
		t.Fatalf("compatible origins were not fully shared:\n%s", rendered.NginxConfig)
	}
}

func TestRenderManagedOriginPoolsIsolatesVirtualHostsAndWeightsCapacity(t *testing.T) {
	sites := []domain.Site{
		{ID: "site-a", Name: "a", Domains: []string{"a.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://203.0.113.10:443", HostHeader: "shared.example.test", TLSServerName: "shared.example.test", Enabled: true}, Enabled: true},
		{ID: "site-b", Name: "b", Domains: []string{"b.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://203.0.113.10:443", HostHeader: "shared.example.test", TLSServerName: "shared.example.test", Enabled: true}, Enabled: true},
		{ID: "site-c", Name: "c", Domains: []string{"c.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://203.0.113.10:443", HostHeader: "shared.example.test", TLSServerName: "shared.example.test", Enabled: true}, Enabled: true},
		{ID: "site-d", Name: "d", Domains: []string{"d.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://203.0.113.10:443", HostHeader: "isolated.example.test", TLSServerName: "isolated.example.test", Enabled: true}, Enabled: true},
	}
	rendered, err := RenderNodeWithRuntimeOptions(sites, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, ManagedOriginPools: true,
		NginxWorkerConnections: 1024, OriginPoolConfigDirectory: "/tmp/cdn-origin-pools",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.OriginPools) != 2 {
		t.Fatalf("origin pools = %#v, want Host/SNI-isolated pools", rendered.OriginPools)
	}
	var shared, isolated domain.OriginPool
	for _, pool := range rendered.OriginPools {
		if pool.HostHeader == "shared.example.test" {
			shared = pool
		} else if pool.HostHeader == "isolated.example.test" {
			isolated = pool
		}
	}
	if shared.KeepaliveConnections != 48 || isolated.KeepaliveConnections != 16 {
		t.Fatalf("weighted pool sizes = shared %d, isolated %d", shared.KeepaliveConnections, isolated.KeepaliveConnections)
	}
	if shared.ID == isolated.ID || shared.TLSServerName == isolated.TLSServerName {
		t.Fatalf("virtual-host isolation was lost: shared=%#v isolated=%#v", shared, isolated)
	}
}

func TestRenderUsesOneNodeCacheLimitAcrossSites(t *testing.T) {
	override := 7
	sites := []domain.Site{
		{ID: "site-a", Name: "a", Domains: []string{"a.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true},
		{ID: "site-b", Name: "b", Domains: []string{"b.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, CacheMaxSizeGB: &override, Enabled: true},
	}
	configuration, err := RenderWithOptions(sites, nil, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"proxy_cache_path /opt/cdn-edge/cache levels=1:2 keys_zone=cdn_cache:32m inactive=7d max_size=3g",
		"proxy_cache $cdn_static_cache_zone;",
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("missing %q from config:\n%s", expected, configuration)
		}
	}
	if strings.Count(configuration, "proxy_cache_path ") != 1 || strings.Contains(configuration, "/cache/sites/") || strings.Contains(configuration, "max_size=7g") {
		t.Fatalf("configuration did not enforce one node cache limit:\n%s", configuration)
	}
}

func TestRenderWithLegacyCacheUsesTheSameNodeLimit(t *testing.T) {
	override := 7
	sites := []domain.Site{
		{ID: "site-a", Name: "a", Domains: []string{"a.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true},
		{ID: "site-b", Name: "b", Domains: []string{"b.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, CacheMaxSizeGB: &override, Enabled: true},
	}
	configuration, err := RenderWithLegacyCache(sites, nil, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"proxy_cache_path /opt/cdn-edge/cache levels=1:2 keys_zone=cdn_cache:32m inactive=7d max_size=3g",
		"proxy_cache $cdn_static_cache_zone;",
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("missing %q from legacy-compatible config:\n%s", expected, configuration)
		}
	}
	if strings.Count(configuration, "proxy_cache_path ") != 1 || strings.Contains(configuration, "/cache/sites/") {
		t.Fatalf("legacy-compatible config contains an independent cache path:\n%s", configuration)
	}
}

func TestStaticAssetPathPattern(t *testing.T) {
	matcher := regexp.MustCompile(`(?i)` + strings.ReplaceAll(staticAssetPathPattern, "(?:", "("))
	for _, path := range []string{
		"/assets/app.css", "/assets/app.JS", "/assets/module.mjs", "/assets/app.js.map",
		"/fonts/inter.woff", "/fonts/inter.woff2", "/fonts/inter.ttf", "/fonts/inter.otf",
		"/images/photo.jpg", "/images/photo.jpeg", "/images/photo.png", "/images/photo.webp",
		"/images/photo.avif", "/images/logo.svg", "/favicon.ico", "/app.webmanifest", "/module.wasm",
	} {
		if !matcher.MatchString(path) {
			t.Errorf("static asset pattern did not match %q", path)
		}
	}
	for _, path := range []string{
		"/api/data", "/api/report.json", "/download/image.png.exe", "/assets/app.js/extra",
		"/images/avatar", "/assets/javascript", "/documents/report.pdf",
	} {
		if matcher.MatchString(path) {
			t.Errorf("static asset pattern unexpectedly matched %q", path)
		}
	}
}

func TestRenderUsesConfiguredReadWriteTimeout(t *testing.T) {
	backup := domain.Origin{URL: "https://backup.example.test", Enabled: true}
	configuration, err := Render([]domain.Site{{
		ID: "site-1", Name: "site", Domains: []string{"cdn.example.test"},
		PrimaryOrigin:           domain.Origin{URL: "https://origin.example.test", Enabled: true},
		BackupOrigin:            &backup,
		ReadWriteTimeoutSeconds: 1800,
		Enabled:                 true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(configuration, "proxy_read_timeout 1800s;"); got != 4 {
		t.Fatalf("expected configured read timeout in normal/stream primary/backup locations, got %d:\n%s", got, configuration)
	}
	if got := strings.Count(configuration, "proxy_send_timeout 1800s;"); got != 4 {
		t.Fatalf("expected configured send timeout in normal/stream primary/backup locations, got %d:\n%s", got, configuration)
	}
	for _, retired := range []string{"proxy_read_timeout 60s;", "proxy_read_timeout 1h;", "proxy_send_timeout 1h;"} {
		if strings.Contains(configuration, retired) {
			t.Fatalf("configuration still contains retired HTTP timeout %q:\n%s", retired, configuration)
		}
	}
	defaultConfiguration, err := Render([]domain.Site{{
		ID: "default", Name: "default", Domains: []string{"default.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(defaultConfiguration, "proxy_read_timeout 120s;"); got != 2 {
		t.Fatalf("expected the default timeout in regular and stream locations, got %d:\n%s", got, defaultConfiguration)
	}
	if _, err := Render([]domain.Site{{
		ID: "invalid", Name: "invalid", Domains: []string{"invalid.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, ReadWriteTimeoutSeconds: 901, Enabled: true,
	}}); err == nil {
		t.Fatal("expected an unsupported read/write timeout to be rejected")
	}
}

func TestRenderUsesConfiguredClientKeepaliveTimeout(t *testing.T) {
	configuration, err := Render([]domain.Site{{
		ID: "keepalive", Name: "keepalive", Domains: []string{"keepalive.example.test"},
		PrimaryOrigin:                 domain.Origin{URL: "https://origin.example.test", Enabled: true},
		ClientKeepaliveTimeoutSeconds: 240, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(configuration, "keepalive_timeout 240s;"); got != 2 {
		t.Fatalf("expected configured client keepalive in HTTP and HTTPS servers, got %d:\n%s", got, configuration)
	}
	if got := strings.Count(configuration, "keepalive_timeout 120s;"); got != 2 {
		t.Fatalf("expected default client keepalive for HTTP and HTTPS default servers, got %d:\n%s", got, configuration)
	}
}

func TestRenderOptionalHTTP2AndH2COrigins(t *testing.T) {
	h2cSite := domain.Site{
		ID: "h2c-site", Name: "h2c", Domains: []string{"h2c.example.test"},
		PrimaryOrigin: domain.Origin{URL: "http://origin.example.test:8080", HTTPVersion: domain.OriginHTTPVersionH2C, Enabled: true}, Enabled: true,
	}
	if _, err := RenderWithRuntimeOptions([]domain.Site{h2cSite}, nil, nil, RenderRuntimeOptions{DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB}); err == nil {
		t.Fatal("H2C origin rendered without an HTTP/2-capable edge")
	}
	httpsSite := domain.Site{
		ID: "h2-site", Name: "h2", Domains: []string{"h2.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", HTTPVersion: domain.OriginHTTPVersionHTTP2, Enabled: true}, Enabled: true,
	}
	rendered, err := RenderNodeWithRuntimeOptions([]domain.Site{h2cSite, httpsSite}, nil, nil, RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, OriginHTTP2Capable: true, ManagedOriginPools: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"proxy_http_version 2;", "error_page 417 = @cdn_websocket_", "if ($cdn_is_websocket) { return 417; }",
		"proxy_http_version 1.1;", "_http1 {", "proxy_set_header Connection upgrade;",
	} {
		if !strings.Contains(rendered.NginxConfig, expected) {
			t.Fatalf("HTTP/2 origin configuration is missing %q:\n%s", expected, rendered.NginxConfig)
		}
	}
	if len(rendered.OriginPools) != 2 {
		t.Fatalf("origin pools = %#v", rendered.OriginPools)
	}
	versions := map[domain.OriginHTTPVersion]bool{}
	for _, pool := range rendered.OriginPools {
		versions[pool.HTTPVersion] = true
	}
	if !versions[domain.OriginHTTPVersionHTTP2] || !versions[domain.OriginHTTPVersionH2C] {
		t.Fatalf("origin pool protocols = %#v", versions)
	}

	legacy, err := Render([]domain.Site{{
		ID: "h1-site", Name: "h1", Domains: []string{"h1.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(legacy, "error_page 417") || strings.Contains(legacy, "_http1 {") || strings.Contains(legacy, "proxy_http_version 2;") {
		t.Fatalf("default HTTP/1.1 origin gained HTTP/2 routing:\n%s", legacy)
	}
}

func TestRenderWebSocketHeadersRemainCorrectForHTTP1AndHTTP2Origins(t *testing.T) {
	http1Backup := domain.Origin{URL: "https://backup.example.test", Enabled: true}
	http1, err := Render([]domain.Site{{
		ID: "http1-site", Name: "http1", Domains: []string{"http1.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, BackupOrigin: &http1Backup, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(http1, "proxy_set_header Upgrade $cdn_upstream_upgrade;"); got != 2 {
		t.Fatalf("HTTP/1 origin should forward Upgrade only through its stream route, got %d:\n%s", got, http1)
	}
	if got := strings.Count(http1, "proxy_set_header Upgrade \"\";"); got != 2 {
		t.Fatalf("HTTP/1 origin should clear Upgrade only on its normal route, got %d:\n%s", got, http1)
	}

	http2Backup := domain.Origin{URL: "https://backup.example.test", HTTPVersion: domain.OriginHTTPVersionHTTP2, Enabled: true}
	http2, err := RenderWithRuntimeOptions([]domain.Site{{
		ID: "http2-site", Name: "http2", Domains: []string{"http2.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", HTTPVersion: domain.OriginHTTPVersionHTTP2, Enabled: true}, BackupOrigin: &http2Backup, Enabled: true,
	}}, nil, nil, RenderRuntimeOptions{DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, OriginHTTP2Capable: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(http2, "proxy_set_header Upgrade $cdn_upstream_upgrade;") {
		t.Fatalf("HTTP/2 origin must not send HTTP/1 Upgrade headers to its normal upstream:\n%s", http2)
	}
	if got := strings.Count(http2, "proxy_set_header Upgrade \"\";"); got != 4 {
		t.Fatalf("HTTP/2 origin should clear Upgrade on normal and stream routes, got %d:\n%s", got, http2)
	}
	if got := strings.Count(http2, "proxy_set_header Upgrade $http_upgrade;"); got != 2 {
		t.Fatalf("HTTP/2 origin should use the dedicated HTTP/1 WebSocket route once, got %d:\n%s", got, http2)
	}
}

func TestRenderCapacity(t *testing.T) {
	mainConfig, eventsConfig, err := RenderCapacity(domain.NginxCapacity{
		WorkerProcesses: 8, WorkerConnections: 8192, WorkerRlimitNoFile: 16384,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mainConfig != "# Generated by cdn-edge-agent. Do not edit.\npcre_jit on;\nworker_processes 8;\nworker_rlimit_nofile 16384;\nworker_shutdown_timeout 1h;\n" ||
		eventsConfig != "# Generated by cdn-edge-agent. Do not edit.\nworker_connections 8192;\n" {
		t.Fatalf("rendered capacity = main=%q events=%q", mainConfig, eventsConfig)
	}
	mainConfig, eventsConfig, err = RenderCapacity(domain.NginxCapacity{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mainConfig, "worker_processes auto;") || !strings.Contains(eventsConfig, "worker_connections 4096;") {
		t.Fatalf("default capacity = main=%q events=%q", mainConfig, eventsConfig)
	}
	for _, expected := range []string{"pcre_jit on;", "worker_shutdown_timeout 1h;"} {
		if !strings.Contains(mainConfig, expected) {
			t.Fatalf("main configuration is missing %q: %s", expected, mainConfig)
		}
	}
}

func TestRenderUsesConfiguredClientMaxBodySize(t *testing.T) {
	configuration, err := Render([]domain.Site{{
		ID: "site-1", Name: "site", Domains: []string{"api.example.test"},
		PrimaryOrigin:       domain.Origin{URL: "https://origin.example.test", Enabled: true},
		ClientMaxBodySizeMB: 1024, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(configuration, "client_max_body_size 1024m;") {
		t.Fatalf("configured client max body size is missing:\n%s", configuration)
	}
	if strings.Contains(configuration, "client_max_body_size 0m;") {
		t.Fatalf("configuration disabled the client body limit:\n%s", configuration)
	}
	if _, err := Render([]domain.Site{{
		ID: "invalid", Name: "invalid", Domains: []string{"invalid.example.test"},
		PrimaryOrigin:       domain.Origin{URL: "https://origin.example.test", Enabled: true},
		ClientMaxBodySizeMB: 129, Enabled: true,
	}}); err == nil {
		t.Fatal("expected an unsupported client max body size to be rejected")
	}
}

func TestRenderEmptyNodeConfigurationDoesNotReferenceSiteVariables(t *testing.T) {
	configuration, err := Render(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(configuration, "location = /__cdn_health") {
		t.Fatalf("empty node configuration lost the health endpoint:\n%s", configuration)
	}
	for _, unexpected := range []string{"$cdn_site_id", "$cdn_static_cache_zone", "proxy_cache_path", "log_format cdn_json", "client_max_body_size"} {
		if strings.Contains(configuration, unexpected) {
			t.Fatalf("empty node configuration contains %q:\n%s", unexpected, configuration)
		}
	}
	if strings.Contains(configuration, "listen 443") || strings.Contains(configuration, "ssl_reject_handshake") {
		t.Fatalf("empty node configuration unexpectedly listens on HTTPS:\n%s", configuration)
	}
}

func TestRenderTCPOnlySiteOmitsHTTPListeners(t *testing.T) {
	site := domain.Site{ID: "11111111-1111-4111-8111-111111111111", Name: "mail", Domains: []string{"mail.example.test"}, TCPOnly: true, Enabled: true, TCPForwards: []domain.TCPForward{
		{Name: "SMTPS", ListenPort: 9465, ListenTLS: true, UpstreamHost: "us1.workspace.org", UpstreamPort: 465, UpstreamTLS: true, UpstreamTLSServerName: "us1.workspace.org", ConnectTimeoutSeconds: 10, IdleTimeoutSeconds: 300},
		{Name: "IMAPS", ListenPort: 9993, ListenTLS: true, UpstreamHost: "us1.workspace.org", UpstreamPort: 993, UpstreamTLS: true, UpstreamTLSServerName: "us1.workspace.org", ConnectTimeoutSeconds: 10, IdleTimeoutSeconds: 300},
	}}
	httpConfiguration, err := Render([]domain.Site{site})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(httpConfiguration, "listen 80") || strings.Contains(httpConfiguration, "listen 443") {
		t.Fatalf("TCP-only HTTP config contains public HTTP listeners:\n%s", httpConfiguration)
	}
	streamConfiguration, err := RenderStream([]domain.Site{site})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"listen 9465 ssl;", "listen 9993 ssl;", "9465 \"us1.workspace.org:465\";", "9993 \"us1.workspace.org:993\";",
		"proxy_pass $cdn_tcp_upstream;", "resolver 1.1.1.1 8.8.8.8 valid=1h ipv6=off;", "proxy_ssl_name $cdn_tcp_upstream_sni;",
		"ssl_certificate /opt/cdn-edge/config/certs/11111111-1111-4111-8111-111111111111.crt;", "proxy_timeout 300s;",
		"access_log /opt/cdn-edge/logs/tcp-access.json cdn_tcp_json;", "error_log /opt/cdn-edge/logs/tcp-error.log warn;",
	} {
		if !strings.Contains(streamConfiguration, expected) {
			t.Fatalf("stream config is missing %q:\n%s", expected, streamConfiguration)
		}
	}
}

func TestRenderStreamRejectsPortAndNodeModeConflicts(t *testing.T) {
	tcpSite := domain.Site{ID: "tcp", Name: "tcp", TCPOnly: true, Enabled: true, TCPForwards: []domain.TCPForward{{Name: "mail", ListenPort: 9465, UpstreamHost: "mail.example.test", UpstreamPort: 465}}}
	httpSite := domain.Site{ID: "http", Name: "http", Enabled: true, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test"}}
	if _, err := RenderStream([]domain.Site{tcpSite, httpSite}); err == nil {
		t.Fatal("expected TCP-only/HTTP node sharing to fail")
	}
	second := tcpSite
	second.ID, second.Name = "tcp-2", "tcp-2"
	if _, err := RenderStream([]domain.Site{tcpSite, second}); err == nil {
		t.Fatal("expected duplicate node listen port to fail")
	}
}

func TestRenderRejectsOriginPath(t *testing.T) {
	_, err := Render([]domain.Site{{ID: "site-1", Name: "site", Domains: []string{"cdn.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test/not-allowed", Enabled: true}, Enabled: true}})
	if err == nil {
		t.Fatal("expected origin path validation error")
	}
}

func TestRenderOnlyUsesTLSUpstreamDirectivesForHTTPSOrigins(t *testing.T) {
	configuration, err := Render([]domain.Site{{ID: "site-1", Name: "site", Domains: []string{"cdn.example.test"}, PrimaryOrigin: domain.Origin{URL: "http://origin.example.test", Enabled: true}, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(configuration, "proxy_ssl_verify on") {
		t.Fatalf("HTTP origin should not emit TLS upstream directives:\n%s", configuration)
	}
}

func TestRenderUsesIndependentTLSServerNamesForIPOrigins(t *testing.T) {
	backup := domain.Origin{URL: "https://203.0.113.21:443", HostHeader: "backup.dustvm.de", TLSServerName: "backup.dustvm.de", Enabled: true}
	configuration, err := Render([]domain.Site{{
		ID: "ip-origin", Name: "ip-origin", Domains: []string{"lax.dustvm.de"},
		PrimaryOrigin: domain.Origin{URL: "https://203.0.113.20:443", HostHeader: "lax.dustvm.de", TLSServerName: "lax.dustvm.de", Enabled: true},
		BackupOrigin:  &backup, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"server 203.0.113.20:443", "server 203.0.113.21:443",
		"proxy_set_header Host lax.dustvm.de", "proxy_set_header Host backup.dustvm.de",
		"proxy_ssl_name lax.dustvm.de", "proxy_ssl_name backup.dustvm.de",
		"proxy_ssl_verify on", "proxy_ssl_trusted_certificate /etc/ssl/certs/ca-certificates.crt",
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("missing %q from IP origin config:\n%s", expected, configuration)
		}
	}
	if strings.Contains(configuration, "proxy_ssl_name 203.0.113.") {
		t.Fatalf("IP connection address leaked into TLS certificate name:\n%s", configuration)
	}
	if got := strings.Count(configuration, "proxy_ssl_name lax.dustvm.de;"); got != 2 {
		t.Fatalf("expected primary SNI in regular and stream locations, got %d:\n%s", got, configuration)
	}
	if got := strings.Count(configuration, "proxy_ssl_name backup.dustvm.de;"); got != 2 {
		t.Fatalf("expected backup SNI in regular and stream locations, got %d:\n%s", got, configuration)
	}
}

func TestRenderUsesIndependentTLSServerNameForWSSOrigin(t *testing.T) {
	configuration, err := Render([]domain.Site{{
		ID: "wss-ip", Name: "wss-ip", Domains: []string{"ws.dustvm.de"},
		PrimaryOrigin: domain.Origin{URL: "wss://203.0.113.20:443", HostHeader: "ws.dustvm.de", TLSServerName: "ws.dustvm.de", Enabled: true}, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"server 203.0.113.20:443", "proxy_set_header Host ws.dustvm.de", "proxy_ssl_name ws.dustvm.de", "proxy_ssl_verify on"} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("missing %q from WSS IP origin config:\n%s", expected, configuration)
		}
	}
}

func TestRenderAutomaticallyRoutesWebSocketAndSSEWithoutPaths(t *testing.T) {
	configuration, err := Render([]domain.Site{{
		ID: "site-1", Name: "streaming", Domains: []string{"stream.example.test"},
		PrimaryOrigin:           domain.Origin{URL: "https://origin.example.test", Enabled: true},
		StreamPaths:             []string{"/events", "/ws"},
		ReadWriteTimeoutSeconds: 900,
		Enabled:                 true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"map $http_upgrade $cdn_is_websocket { default 0; ~*^websocket$ 1; }",
		"map $http_accept $cdn_accepts_event_stream",
		"map $http_x_cdn_stream $cdn_forced_stream",
		`map "$request_method:$cdn_is_websocket:$cdn_accepts_event_stream:$cdn_forced_stream" $cdn_auto_stream`,
		"~^POST: 1;",
		"~^[^:]+:1: 1;",
		"~^[^:]+:[01]:1: 1;",
		"~^[^:]+:[01]:[01]:1$ 1;",
		"error_page 418 = @cdn_stream_site-1",
		"error_page 419 = @cdn_http_site-1",
		"if ($cdn_auto_stream) { return 418; }",
		"return 419;",
		"location @cdn_http_site-1",
		"location @cdn_stream_site-1",
		"proxy_set_header Upgrade $cdn_upstream_upgrade",
		"proxy_set_header Connection $cdn_connection_upgrade",
		`proxy_set_header X-CDN-Stream ""`,
		"proxy_cache off",
		"proxy_buffering off",
		"proxy_cache_methods GET HEAD",
		"proxy_connect_timeout 10s",
		"proxy_read_timeout 900s",
		"proxy_send_timeout 900s",
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("missing %q from streaming config:\n%s", expected, configuration)
		}
	}
	for _, retired := range []string{"location = /events", "location ^~ /events/", "location = /ws", "location ^~ /ws/", "proxy_request_buffering off;", "proxy_read_timeout 1h;"} {
		if strings.Contains(configuration, retired) {
			t.Fatalf("automatic streaming config contains retired directive %q:\n%s", retired, configuration)
		}
	}
}

func TestRenderPassthroughDisablesCacheAndForwardsRanges(t *testing.T) {
	backup := domain.Origin{URL: "https://backup.example.test", Enabled: true}
	configuration, err := Render([]domain.Site{{
		ID: "site-1", Name: "passthrough", Domains: []string{"stream.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		BackupOrigin:  &backup, Passthrough: true, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"location / {",
		"location @cdn_http_site-1 {",
		"location @cdn_stream_site-1 {",
		"location @cdn_backup_site-1 {",
		"location @cdn_stream_backup_site-1 {",
		"proxy_cache off;",
		"proxy_buffering off;",
		"proxy_request_buffering off;",
		"proxy_connect_timeout 10s;",
		"proxy_read_timeout 120s;",
		"proxy_send_timeout 120s;",
		"proxy_set_header Range $http_range;",
		"proxy_set_header If-Range $http_if_range;",
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("missing %q from passthrough config:\n%s", expected, configuration)
		}
	}
	if strings.Contains(configuration, "proxy_cache $cdn_static_cache_zone;") || strings.Contains(configuration, "proxy_cache_key \"site-1:") {
		t.Fatalf("passthrough site inherited cache configuration:\n%s", configuration)
	}
	if strings.Contains(configuration, "$cdn_static_cache_zone") {
		t.Fatalf("passthrough-only configuration unexpectedly declares a cache selector:\n%s", configuration)
	}
	if got := strings.Count(configuration, "proxy_cache off;"); got != 4 {
		t.Fatalf("expected cache to be disabled in normal/stream primary/backup locations, got %d:\n%s", got, configuration)
	}
	if got := strings.Count(configuration, "proxy_set_header Range $http_range;"); got != 4 {
		t.Fatalf("expected Range forwarding in normal/stream primary/backup locations, got %d:\n%s", got, configuration)
	}
}

func TestRenderWebSocketOriginRemainsFullyUnbuffered(t *testing.T) {
	configuration, err := Render([]domain.Site{{
		ID: "ws-site", Name: "websocket", Domains: []string{"ws.example.test"},
		PrimaryOrigin: domain.Origin{URL: "wss://origin.example.test", Enabled: true}, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(configuration, "$cdn_static_cache_zone") {
		t.Fatalf("WebSocket origin inherited HTTP cache configuration:\n%s", configuration)
	}
	for _, expected := range []string{"proxy_pass https://origin_ws-site", "proxy_cache off;", "proxy_buffering off;", "proxy_request_buffering off;", "proxy_read_timeout 120s;"} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("WebSocket origin is missing %q:\n%s", expected, configuration)
		}
	}
}

func TestRenderUsesGRPCPassForGRPCOrigin(t *testing.T) {
	configuration, err := Render([]domain.Site{{
		ID: "grpc-site", Name: "grpc", Domains: []string{"grpc.example.test"},
		PrimaryOrigin: domain.Origin{URL: "grpcs://origin.example.test:443", Enabled: true}, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"listen 443 ssl;",
		"http2 on;",
		"grpc_pass grpcs://origin_grpc-site",
		"grpc_set_header TE trailers",
		"grpc_connect_timeout 10s",
		"grpc_read_timeout 1h",
		"grpc_ssl_server_name on",
		"grpc_ssl_name origin.example.test",
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("missing %q from gRPC config:\n%s", expected, configuration)
		}
	}
	if strings.Contains(configuration, "proxy_pass grpcs://") {
		t.Fatalf("gRPC origin must not use proxy_pass:\n%s", configuration)
	}
	if strings.Contains(configuration, "listen 443 ssl http2") {
		t.Fatalf("configuration still uses the deprecated HTTP/2 listen parameter:\n%s", configuration)
	}
}

func TestRenderUsesIndependentTLSServerNamesForGRPCSIPOrigins(t *testing.T) {
	backup := domain.Origin{URL: "grpcs://203.0.113.31:443", HostHeader: "grpc-backup.dustvm.de", TLSServerName: "grpc-backup.dustvm.de", Enabled: true}
	configuration, err := Render([]domain.Site{{
		ID: "grpc-ip", Name: "grpc-ip", Domains: []string{"grpc.dustvm.de"},
		PrimaryOrigin: domain.Origin{URL: "grpcs://203.0.113.30:443", HostHeader: "grpc.dustvm.de", TLSServerName: "grpc.dustvm.de", Enabled: true},
		BackupOrigin:  &backup, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"server 203.0.113.30:443", "server 203.0.113.31:443",
		"grpc_set_header Host grpc.dustvm.de", "grpc_set_header Host grpc-backup.dustvm.de",
		"grpc_ssl_name grpc.dustvm.de", "grpc_ssl_name grpc-backup.dustvm.de",
		"grpc_ssl_verify on", "grpc_ssl_trusted_certificate /etc/ssl/certs/ca-certificates.crt",
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("missing %q from GRPCS IP origin config:\n%s", expected, configuration)
		}
	}
	if strings.Contains(configuration, "grpc_ssl_name 203.0.113.") {
		t.Fatalf("IP connection address leaked into gRPC TLS certificate name:\n%s", configuration)
	}
	if got := strings.Count(configuration, "grpc_ssl_verify_depth 3;"); got != 2 {
		t.Fatalf("expected TLS verification depth on primary and backup gRPC paths, got %d:\n%s", got, configuration)
	}
}
