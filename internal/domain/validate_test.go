package domain

import "testing"

func TestOriginHTTPVersionValidation(t *testing.T) {
	for _, test := range []struct {
		url     string
		version OriginHTTPVersion
		want    OriginHTTPVersion
	}{
		{url: "http://origin.example.test", want: OriginHTTPVersionHTTP1},
		{url: "https://origin.example.test", version: OriginHTTPVersionHTTP2, want: OriginHTTPVersionHTTP2},
		{url: "http://origin.example.test", version: OriginHTTPVersionH2C, want: OriginHTTPVersionH2C},
		{url: "wss://origin.example.test", want: OriginHTTPVersionHTTP1},
		{url: "grpcs://origin.example.test", want: ""},
	} {
		origin := Origin{URL: test.url, HTTPVersion: test.version}
		if err := ValidateOrigin(&origin); err != nil {
			t.Fatalf("ValidateOrigin(%q, %q): %v", test.url, test.version, err)
		}
		if origin.HTTPVersion != test.want {
			t.Fatalf("ValidateOrigin(%q, %q) version = %q, want %q", test.url, test.version, origin.HTTPVersion, test.want)
		}
	}
	for _, origin := range []Origin{
		{URL: "http://origin.example.test", HTTPVersion: OriginHTTPVersionHTTP2},
		{URL: "https://origin.example.test", HTTPVersion: OriginHTTPVersionH2C},
		{URL: "ws://origin.example.test", HTTPVersion: OriginHTTPVersionH2C},
		{URL: "grpc://origin.example.test", HTTPVersion: OriginHTTPVersionHTTP2},
	} {
		if err := ValidateOrigin(&origin); err == nil {
			t.Fatalf("invalid origin protocol was accepted: %#v", origin)
		}
	}
}

func TestClientMaxBodySizeDefaultsAndPresetValidation(t *testing.T) {
	newSite := func(value int) Site {
		return Site{
			Name:                "example",
			Domains:             []string{"example.test"},
			Nodes:               []string{"node-1"},
			PrimaryOrigin:       Origin{URL: "https://origin.example.test"},
			ClientMaxBodySizeMB: value,
		}
	}

	defaultSite := newSite(0)
	if err := NormalizeAndValidateSite(&defaultSite); err != nil {
		t.Fatal(err)
	}
	if defaultSite.ClientMaxBodySizeMB != DefaultClientMaxBodySizeMB {
		t.Fatalf("default client max body size = %d", defaultSite.ClientMaxBodySizeMB)
	}

	for _, value := range []int{128, 256, 512, 1024} {
		site := newSite(value)
		if err := NormalizeAndValidateSite(&site); err != nil {
			t.Fatalf("expected %d MiB to be accepted: %v", value, err)
		}
	}
	for _, value := range []int{-1, 1, 127, 129, 1025} {
		site := newSite(value)
		if err := NormalizeAndValidateSite(&site); err == nil {
			t.Fatalf("expected %d MiB to be rejected", value)
		}
	}
}

func TestReadWriteTimeoutDefaultsAndPresetValidation(t *testing.T) {
	newSite := func(value int) Site {
		return Site{
			Name:                    "example",
			Domains:                 []string{"example.test"},
			Nodes:                   []string{"node-1"},
			PrimaryOrigin:           Origin{URL: "https://origin.example.test"},
			ReadWriteTimeoutSeconds: value,
		}
	}

	defaultSite := newSite(0)
	if err := NormalizeAndValidateSite(&defaultSite); err != nil {
		t.Fatal(err)
	}
	if defaultSite.ReadWriteTimeoutSeconds != DefaultReadWriteTimeoutSeconds {
		t.Fatalf("default read/write timeout = %d", defaultSite.ReadWriteTimeoutSeconds)
	}

	for _, value := range []int{120, 360, 900, 1800, 3600} {
		site := newSite(value)
		if err := NormalizeAndValidateSite(&site); err != nil {
			t.Fatalf("expected %d seconds to be accepted: %v", value, err)
		}
	}
	for _, value := range []int{-1, 1, 119, 121, 359, 361, 7200} {
		site := newSite(value)
		if err := NormalizeAndValidateSite(&site); err == nil {
			t.Fatalf("expected %d seconds to be rejected", value)
		}
	}
}

func TestClientKeepaliveTimeoutDefaultsAndValidation(t *testing.T) {
	newSite := func(value int) Site {
		return Site{Name: "example", Domains: []string{"example.test"}, Nodes: []string{"node-1"}, PrimaryOrigin: Origin{URL: "https://origin.example.test"}, ClientKeepaliveTimeoutSeconds: value}
	}
	defaultSite := newSite(0)
	if err := NormalizeAndValidateSite(&defaultSite); err != nil {
		t.Fatal(err)
	}
	if defaultSite.ClientKeepaliveTimeoutSeconds != DefaultClientKeepaliveTimeoutSeconds {
		t.Fatalf("default client keepalive timeout = %d", defaultSite.ClientKeepaliveTimeoutSeconds)
	}
	for _, value := range []int{15, 120, 300, 3600} {
		site := newSite(value)
		if err := NormalizeAndValidateSite(&site); err != nil {
			t.Fatalf("expected %d seconds to be accepted: %v", value, err)
		}
	}
	for _, value := range []int{-1, 1, 14, 3601} {
		site := newSite(value)
		if err := NormalizeAndValidateSite(&site); err == nil {
			t.Fatalf("expected %d seconds to be rejected", value)
		}
	}
}

func TestNginxCapacityDefaultsAndValidation(t *testing.T) {
	capacity, err := NormalizeNginxCapacity(NginxCapacity{})
	if err != nil {
		t.Fatal(err)
	}
	if capacity != DefaultNginxCapacity() {
		t.Fatalf("default Nginx capacity = %#v", capacity)
	}
	for _, invalid := range []NginxCapacity{
		{WorkerProcesses: -1},
		{WorkerProcesses: MaxNginxWorkerProcesses + 1},
		{WorkerConnections: MinNginxWorkerConnections - 1},
		{WorkerConnections: 8192, WorkerRlimitNoFile: 4096},
		{WorkerRlimitNoFile: MaxNginxWorkerRlimitNoFile + 1},
	} {
		if _, err := NormalizeNginxCapacity(invalid); err == nil {
			t.Fatalf("invalid Nginx capacity accepted: %#v", invalid)
		}
	}
}

func TestSiteDNSTTLValidation(t *testing.T) {
	newSite := func(value *int) Site {
		return Site{Name: "example", Domains: []string{"example.test"}, Nodes: []string{"node-1"}, PrimaryOrigin: Origin{URL: "https://origin.example.test"}, DNSTTLSeconds: value}
	}
	inherited := newSite(nil)
	if err := NormalizeAndValidateSite(&inherited); err != nil {
		t.Fatal(err)
	}
	for _, value := range []int{60, 61, 180, 300} {
		ttl := value
		site := newSite(&ttl)
		if err := NormalizeAndValidateSite(&site); err != nil {
			t.Fatalf("expected TTL %d to be accepted: %v", value, err)
		}
	}
	for _, value := range []int{-1, 0, 59, 301} {
		ttl := value
		site := newSite(&ttl)
		if err := NormalizeAndValidateSite(&site); err == nil {
			t.Fatalf("expected TTL %d to be rejected", value)
		}
	}
}

func TestTCPOnlySiteValidationAndDefaults(t *testing.T) {
	site := Site{
		Name: "mail", Domains: []string{"mail.example.test"}, Nodes: []string{"node-1"}, TCPOnly: true, Enabled: true,
		TCPForwards: []TCPForward{{
			Name: "IMAPS", ListenPort: 9993, ListenTLS: true,
			UpstreamHost: "US1.Workspace.org.", UpstreamPort: 993, UpstreamTLS: true,
		}},
	}
	if err := NormalizeAndValidateSite(&site); err != nil {
		t.Fatal(err)
	}
	forward := site.TCPForwards[0]
	if forward.UpstreamHost != "us1.workspace.org" || forward.UpstreamTLSServerName != "us1.workspace.org" {
		t.Fatalf("upstream normalization = %#v", forward)
	}
	if forward.ConnectTimeoutSeconds != DefaultTCPConnectTimeoutSeconds || forward.IdleTimeoutSeconds != DefaultTCPIdleTimeoutSeconds {
		t.Fatalf("timeout defaults = %#v", forward)
	}
	if !SiteNeedsCertificate(site) {
		t.Fatal("TLS listener did not require a certificate")
	}
}

func TestTCPForwardValidationRejectsUnsafeInputs(t *testing.T) {
	base := func() Site {
		return Site{Name: "mail", Domains: []string{"mail.example.test"}, Nodes: []string{"node-1"}, TCPOnly: true, TCPForwards: []TCPForward{{Name: "mail", ListenPort: 9465, UpstreamHost: "mail.example.test", UpstreamPort: 465}}}
	}
	tests := []struct {
		name   string
		mutate func(*Site)
	}{
		{"reserved HTTP port", func(site *Site) { site.TCPForwards[0].ListenPort = 443 }},
		{"duplicate port", func(site *Site) { site.TCPForwards = append(site.TCPForwards, site.TCPForwards[0]) }},
		{"invalid host", func(site *Site) { site.TCPForwards[0].UpstreamHost = "mail.example.test; return 200" }},
		{"IP TLS without SNI", func(site *Site) {
			site.TCPForwards[0].UpstreamHost = "203.0.113.8"
			site.TCPForwards[0].UpstreamTLS = true
		}},
		{"SNI without TLS", func(site *Site) { site.TCPForwards[0].UpstreamTLSServerName = "mail.example.test" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			site := base()
			test.mutate(&site)
			if err := NormalizeAndValidateSite(&site); err == nil {
				t.Fatalf("expected validation failure for %#v", site.TCPForwards)
			}
		})
	}
}

func TestRawTCPOnlySiteDoesNotRequireCertificate(t *testing.T) {
	site := Site{Name: "tcp", Domains: []string{"tcp.example.test"}, Nodes: []string{"node-1"}, TCPOnly: true, TCPForwards: []TCPForward{{Name: "raw", ListenPort: 2525, UpstreamHost: "mail.example.test", UpstreamPort: 25}}}
	if err := NormalizeAndValidateSite(&site); err != nil {
		t.Fatal(err)
	}
	if SiteNeedsCertificate(site) {
		t.Fatal("raw TCP-only site unexpectedly requires a certificate")
	}
}

func TestSiteDomainRejectsIPAddress(t *testing.T) {
	site := Site{
		Name:    "example",
		Domains: []string{"203.0.113.7"},
		Nodes:   []string{"node-1"},
		PrimaryOrigin: Origin{
			URL:     "https://203.0.113.8",
			Enabled: true,
		},
	}
	if err := NormalizeAndValidateSite(&site); err == nil {
		t.Fatal("expected an IP address to be rejected as a site domain")
	}
}

func TestOriginMayUseIPAddress(t *testing.T) {
	origin := Origin{URL: "https://203.0.113.8:8443", Enabled: true}
	if err := ValidateOrigin(&origin); err != nil {
		t.Fatalf("expected an IP origin to remain supported: %v", err)
	}
}

func TestOriginSupportsIndependentTLSServerName(t *testing.T) {
	for _, scheme := range []string{"https", "wss", "grpcs"} {
		origin := Origin{URL: scheme + "://203.0.113.8:443", HostHeader: "lax.dustvm.de", TLSServerName: " LAX.DUSTVM.DE ", Enabled: true}
		if err := ValidateOrigin(&origin); err != nil {
			t.Fatalf("expected %s origin TLS server name to be accepted: %v", scheme, err)
		}
		if origin.TLSServerName != "lax.dustvm.de" {
			t.Fatalf("TLS server name was not normalized: %q", origin.TLSServerName)
		}
	}
}

func TestOriginRejectsInvalidTLSServerName(t *testing.T) {
	for _, value := range []string{"203.0.113.8", "lax.dustvm.de:443", "https://lax.dustvm.de", "*.dustvm.de", "bad name"} {
		origin := Origin{URL: "https://203.0.113.8:443", TLSServerName: value, Enabled: true}
		if err := ValidateOrigin(&origin); err == nil {
			t.Fatalf("expected TLS server name %q to be rejected", value)
		}
	}
	for _, scheme := range []string{"http", "ws", "grpc"} {
		origin := Origin{URL: scheme + "://origin.example.test", TLSServerName: "origin.example.test", Enabled: true}
		if err := ValidateOrigin(&origin); err == nil {
			t.Fatalf("expected TLS server name on %s origin to be rejected", scheme)
		}
	}
}

func TestOriginSupportsWebSocketAndGRPCSchemes(t *testing.T) {
	for _, value := range []string{"ws://origin.example.test:8080", "wss://origin.example.test", "grpc://grpc.example.test:50051", "grpcs://grpc.example.test"} {
		origin := Origin{URL: value}
		if err := ValidateOrigin(&origin); err != nil {
			t.Fatalf("expected %q to be accepted: %v", value, err)
		}
	}
}

func TestStreamPathsAreRetiredForAPICompatibility(t *testing.T) {
	site := Site{
		Name:          "streaming",
		Domains:       []string{"stream.example.test"},
		Nodes:         []string{"node-1"},
		PrimaryOrigin: Origin{URL: "https://origin.example.test"},
		StreamPaths:   []string{"/bad path", "/../ws"},
	}
	if err := NormalizeAndValidateSite(&site); err != nil {
		t.Fatal(err)
	}
	if site.StreamPaths == nil || len(site.StreamPaths) != 0 {
		t.Fatalf("retired stream paths should normalize to an empty array: %#v", site.StreamPaths)
	}
}

func TestPassthroughIsRestrictedToHTTPOrigins(t *testing.T) {
	site := Site{
		Name:          "passthrough",
		Domains:       []string{"stream.example.test"},
		Nodes:         []string{"node-1"},
		PrimaryOrigin: Origin{URL: "https://origin.example.test"},
		Passthrough:   true,
	}
	if err := NormalizeAndValidateSite(&site); err != nil {
		t.Fatalf("HTTP passthrough should be valid: %v", err)
	}
	for _, origin := range []string{"ws://origin.example.test", "grpcs://origin.example.test"} {
		site.PrimaryOrigin = Origin{URL: origin}
		if err := NormalizeAndValidateSite(&site); err == nil {
			t.Fatalf("expected passthrough origin %q to be rejected", origin)
		}
	}
}

func TestEffectiveNodeCacheMaxSizeUsesOverrideOrGlobalDefault(t *testing.T) {
	size, err := EffectiveNodeCacheMaxSizeGB(Node{}, 4)
	if err != nil || size != 4 {
		t.Fatalf("inherited node cache size = %d, err=%v", size, err)
	}
	override := 2
	size, err = EffectiveNodeCacheMaxSizeGB(Node{CacheMaxSizeGB: &override}, 4)
	if err != nil || size != 2 {
		t.Fatalf("overridden node cache size = %d, err=%v", size, err)
	}
	invalid := MaxCacheMaxSizeGB + 1
	if _, err := EffectiveNodeCacheMaxSizeGB(Node{CacheMaxSizeGB: &invalid}, 4); err == nil {
		t.Fatal("invalid node cache override was accepted")
	}
}
