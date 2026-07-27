package control

import (
	"path/filepath"
	"strings"
	"testing"

	"simple_cdn/internal/domain"
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
	http3State, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(http3State.NginxConfig, "listen 443 quic") || len(http3State.PublicUDPPorts) != 1 || http3State.PublicUDPPorts[0] != 443 {
		t.Fatalf("reconciled HTTP/3 state = %#v\n%s", http3State.PublicUDPPorts, http3State.NginxConfig)
	}
	if http3StateNeedsRebuild(http3State, true) {
		t.Fatal("reconciled HTTP/3 state still requests a rebuild")
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
