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
	if http3StateNeedsRebuild(http3State, capabilities) {
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

func TestHTTP3CapabilityDoesNotOpenUDPWithoutAnHTTPSSite(t *testing.T) {
	state := domain.DesiredState{NginxConfig: "server { listen 80 default_server; }", PublicPorts: []int{80}}
	if http3StateNeedsRebuild(state, []string{domain.EdgeCapabilityHTTP3}) {
		t.Fatal("HTTP/3 capability requested a rebuild for a node without an HTTPS site")
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
