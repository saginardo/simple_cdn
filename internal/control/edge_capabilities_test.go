package control

import (
	"path/filepath"
	"strings"
	"testing"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/nginx"
	"simple_cdn/internal/store"
)

func TestReconcileEdgeRuntimeCapabilitiesTracksHTTP3Support(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-http3-reconcile", "203.0.113.20")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "reconcile", Domains: []string{"reconcile.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, HTTP3Enabled: true, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	certificate, privateKey, notAfter := testCertificate(t, site.Domains...)
	if err := publisher.StoreCertificate(site.ID, certificate, privateKey, notAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(site.ID); err != nil {
		t.Fatal(err)
	}
	server := Server{Store: database, Publisher: publisher}
	capabilities := []string{domain.EdgeCapabilityHTTP3}
	if err := database.SetNodeCapabilities(node.ID, capabilities); err != nil {
		t.Fatal(err)
	}
	if err := server.reconcileEdgeRuntimeCapabilities(node.ID, capabilities); err != nil {
		t.Fatal(err)
	}
	http3State, encryptedCertificates, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(http3State.NginxConfig, "listen 443 quic") || len(http3State.PublicUDPPorts) != 1 || http3State.PublicUDPPorts[0] != 443 {
		t.Fatalf("reconciled HTTP/3 state = %#v\n%s", http3State.PublicUDPPorts, http3State.NginxConfig)
	}
	if http3StateNeedsRebuild(http3State, true) {
		t.Fatal("reconciled HTTP/3 state still requests a rebuild")
	}

	legacyRuntime := http3State
	legacyRuntime.NginxMainConfig = strings.ReplaceAll(legacyRuntime.NginxMainConfig, "pcre_jit on;\n", "")
	legacyRuntime.NginxMainConfig = strings.ReplaceAll(legacyRuntime.NginxMainConfig, "worker_shutdown_timeout 1h;\n", "")
	legacyRuntime.NginxConfig = strings.ReplaceAll(legacyRuntime.NginxConfig, "    ssl_session_timeout 30m;\n", "")
	legacyRuntime.NginxConfig = strings.ReplaceAll(legacyRuntime.NginxConfig, "quic_host_key /opt/cdn-edge/config/nginx/quic-host.key;\n", "")
	if err := database.SaveNodeState(node.ID, legacyRuntime, encryptedCertificates); err != nil {
		t.Fatal(err)
	}
	if err := server.reconcileEdgeRuntimeCapabilities(node.ID, capabilities); err != nil {
		t.Fatal(err)
	}
	optimizedState, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if optimizedState.Version <= legacyRuntime.Version || runtimeOptimizationStateNeedsRebuild(optimizedState) {
		t.Fatalf("runtime optimization state was not rebuilt: version=%d\nmain:\n%s\nhttp:\n%s", optimizedState.Version, optimizedState.NginxMainConfig, optimizedState.NginxConfig)
	}

	if err := database.SetNodeCapabilities(node.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := server.reconcileEdgeRuntimeCapabilities(node.ID, nil); err != nil {
		t.Fatal(err)
	}
	legacyState, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(legacyState.NginxConfig, "listen 443 quic") || len(legacyState.PublicUDPPorts) != 0 {
		t.Fatalf("HTTP/3 state survived capability removal = %#v\n%s", legacyState.PublicUDPPorts, legacyState.NginxConfig)
	}
}

func TestHTTP3RebuildTracksPublishedSiteOptIn(t *testing.T) {
	state := domain.DesiredState{NginxConfig: "server { listen 80 default_server; }", PublicPorts: []int{80}}
	if http3StateNeedsRebuild(state, false) {
		t.Fatal("default-off site requested an HTTP/3 rebuild")
	}
	legacyEnabled := domain.DesiredState{
		NginxConfig:    "server { listen 443 ssl; listen 443 quic; }",
		PublicUDPPorts: []int{443},
	}
	if !http3StateNeedsRebuild(legacyEnabled, false) {
		t.Fatal("old global HTTP/3 state was not scheduled for removal")
	}
	if http3StateNeedsRebuild(legacyEnabled, true) {
		t.Fatal("opted-in HTTP/3 state requested an unnecessary rebuild")
	}
}

func TestOriginHTTP2StateRebuildsAfterCapabilityRemoval(t *testing.T) {
	state := domain.DesiredState{NginxConfig: "location / { proxy_http_version 2; proxy_pass http://origin; }"}
	if originHTTP2StateNeedsRebuild(state, []string{domain.EdgeCapabilityOriginHTTP2}) {
		t.Fatal("HTTP/2 state rebuilt while the capability remained present")
	}
	if !originHTTP2StateNeedsRebuild(state, nil) {
		t.Fatal("HTTP/2 state did not rebuild after capability removal")
	}
	if originHTTP2StateNeedsRebuild(domain.DesiredState{NginxConfig: "proxy_http_version 1.1;"}, nil) {
		t.Fatal("HTTP/1.1 state incorrectly requires an HTTP/2 capability")
	}
}

func TestRequestTracingStateRebuildsOnlyForCapableHTTPNodes(t *testing.T) {
	legacy := domain.DesiredState{NginxConfig: "location / { proxy_pass https://origin; }"}
	capabilities := []string{domain.EdgeCapabilityRequestTracing}
	if !requestTracingStateNeedsRebuild(legacy, capabilities) {
		t.Fatal("capable node did not request a legacy HTTP state rebuild")
	}
	if requestTracingStateNeedsRebuild(legacy, nil) {
		t.Fatal("legacy agent requested tracing state before advertising support")
	}
	tracedConfig, err := nginx.Render([]domain.Site{{
		ID: "traced", Name: "traced", Domains: []string{"traced.example.test"},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	traced := domain.DesiredState{NginxConfig: tracedConfig}
	if requestTracingStateNeedsRebuild(traced, capabilities) {
		t.Fatal("traced HTTP state requested another rebuild")
	}
	if requestTracingStateNeedsRebuild(domain.DesiredState{NginxConfig: "server { listen 80; }"}, capabilities) {
		t.Fatal("pure TCP state requested an HTTP tracing rebuild")
	}
}

func TestRuntimeOptimizationStateNeedsRebuild(t *testing.T) {
	legacy := domain.DesiredState{
		NginxMainConfig: "worker_processes auto;\nworker_rlimit_nofile 65536;\n",
		NginxConfig:     "server { listen 443 ssl; listen 443 quic; }",
	}
	if !runtimeOptimizationStateNeedsRebuild(legacy) {
		t.Fatal("legacy state did not request runtime optimization rebuild")
	}
	optimized := domain.DesiredState{
		NginxMainConfig: "pcre_jit on;\nworker_processes auto;\nworker_shutdown_timeout 1h;\n",
		NginxConfig:     "server { listen 443 ssl; ssl_session_timeout 30m; listen 443 quic; }\nquic_host_key /opt/cdn-edge/config/nginx/quic-host.key;",
	}
	if runtimeOptimizationStateNeedsRebuild(optimized) {
		t.Fatal("optimized state unexpectedly requested a rebuild")
	}
	tcpOnly := domain.DesiredState{NginxMainConfig: "pcre_jit on;\nworker_shutdown_timeout 1h;\n"}
	if runtimeOptimizationStateNeedsRebuild(tcpOnly) {
		t.Fatal("TCP-only optimized state unexpectedly requested a rebuild")
	}
}

func TestReconcileRemovesLegacyHTTP3WhenPublishedSiteIsOptedOut(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-http3-default-off-reconcile", "203.0.113.21")
	if err != nil {
		t.Fatal(err)
	}
	capabilities := []string{domain.EdgeCapabilityHTTP3}
	if err := database.SetNodeCapabilities(node.ID, capabilities); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "default-off-reconcile", Domains: []string{"default-off-reconcile.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	certificate, privateKey, notAfter := testCertificate(t, site.Domains...)
	if err := publisher.StoreCertificate(site.ID, certificate, privateKey, notAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(site.ID); err != nil {
		t.Fatal(err)
	}
	legacyState, encryptedCertificates, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacyState.NginxConfig += "\nlisten 443 quic;\n"
	legacyState.PublicUDPPorts = []int{443}
	if err := database.SaveNodeState(node.ID, legacyState, encryptedCertificates); err != nil {
		t.Fatal(err)
	}

	server := Server{Store: database, Publisher: publisher}
	if err := server.reconcileEdgeRuntimeCapabilities(node.ID, capabilities); err != nil {
		t.Fatal(err)
	}
	reconciled, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reconciled.NginxConfig, "listen 443 quic") || len(reconciled.PublicUDPPorts) != 0 {
		t.Fatalf("legacy HTTP/3 state survived site opt-out = %#v\n%s", reconciled.PublicUDPPorts, reconciled.NginxConfig)
	}
}

func TestOriginPoolCapabilityRebuildsOnlyHTTPOriginStates(t *testing.T) {
	legacy := domain.DesiredState{NginxConfig: "location / { proxy_pass https://origin_site; }"}
	capabilities := []string{domain.EdgeCapabilityOriginConnection}
	if !originPoolStateNeedsRebuild(legacy, capabilities) {
		t.Fatal("origin connection capability did not request a legacy state rebuild")
	}
	managed := legacy
	managed.OriginPools = []domain.OriginPool{{ID: "0123456789abcdef01234567"}}
	if originPoolStateNeedsRebuild(managed, capabilities) {
		t.Fatal("managed origin state still requested a rebuild")
	}
	if !originPoolStateNeedsRebuild(managed, nil) {
		t.Fatal("capability removal did not request a legacy state rebuild")
	}
	if originPoolStateNeedsRebuild(domain.DesiredState{NginxConfig: "server { listen 80; }"}, capabilities) {
		t.Fatal("node without an HTTP origin requested a pool rebuild")
	}
}
