package control

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/nginx"
	"simple_cdn/internal/store"
)

func TestPublishRequiresCertificateAndThenMarksPublished(t *testing.T) {
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
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{Name: "site", Domains: []string{"cdn.example.test"}, Nodes: []string{node.ID}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	if _, err := publisher.PublishSite(site.ID); err == nil {
		t.Fatal("expected certificate gate")
	}
	certificate, privateKey, notAfter := testCertificate(t, "cdn.example.test")
	if err := publisher.StoreCertificate(site.ID, certificate, privateKey, notAfter); err != nil {
		t.Fatal(err)
	}
	task, err := publisher.PublishSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskSucceeded || !strings.Contains(task.Detail, "no active") {
		t.Fatalf("unexpected task: %#v", task)
	}
	published, _, err := database.GetSite(site.ID)
	if err != nil || !published.Published {
		t.Fatalf("site did not become published: %#v %v", published, err)
	}
	state, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != 1 || state.NginxConfig == "" {
		t.Fatalf("unexpected node state: %#v", state)
	}
	if _, _, _, err := database.Certificate("missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected missing certificate: %v", err)
	}
}

func TestPublishUsesEachNodesEffectiveCacheLimit(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, _ := NewEncryptionKey()
	cipher, _ := NewCipher(key)
	defaultNode, err := database.CreateNode("default-cache-edge", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	overriddenNode, err := database.CreateNode("overridden-cache-edge", "203.0.113.11")
	if err != nil {
		t.Fatal(err)
	}
	override := 3
	if _, err := database.SetNodeCacheMaxSizeGB(overriddenNode.ID, &override); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "site", Domains: []string{"cdn.example.test"}, Nodes: []string{defaultNode.ID, overriddenNode.ID},
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
	defaultState, _, err := database.NodeState(defaultNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	overriddenState, _, err := database.NodeState(overriddenNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(defaultState.NginxConfig, "max_size=1g") || defaultState.CacheMaxBytes != 1<<30 || strings.Contains(defaultState.NginxConfig, "/cache/sites/") {
		t.Fatalf("default node did not receive the global cache limit:\n%s", defaultState.NginxConfig)
	}
	if !strings.Contains(overriddenState.NginxConfig, "proxy_cache_path /opt/cdn-edge/cache ") || !strings.Contains(overriddenState.NginxConfig, "max_size=3g") || overriddenState.CacheMaxBytes != 3<<30 || strings.Contains(overriddenState.NginxConfig, "/cache/sites/") {
		t.Fatalf("overridden node did not receive its total cache limit:\n%s", overriddenState.NginxConfig)
	}
}

func TestPublishCapabilityGatesSharedOriginPools(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, _ := NewEncryptionKey()
	cipher, _ := NewCipher(key)
	legacyNode, err := database.CreateNode("legacy-origin-edge", "203.0.113.30")
	if err != nil {
		t.Fatal(err)
	}
	managedNode, err := database.CreateNode("managed-origin-edge", "203.0.113.31")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(managedNode.ID, []string{domain.EdgeCapabilityOriginConnection}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetNodeNginxCapacity(managedNode.ID, domain.NginxCapacity{WorkerConnections: 1024, WorkerRlimitNoFile: 65536}); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "shared-origin", Domains: []string{"cdn.example.test"}, Nodes: []string{legacyNode.ID, managedNode.ID},
		PrimaryOrigin: domain.Origin{URL: "http://origin.example.test:8080", Enabled: true}, Enabled: true,
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
	legacyState, _, err := database.NodeState(legacyNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	managedState, _, err := database.NodeState(managedNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyState.OriginPools) != 0 || !strings.Contains(legacyState.NginxConfig, "upstream origin_"+site.ID) {
		t.Fatalf("legacy node received managed origin state: %#v\n%s", legacyState.OriginPools, legacyState.NginxConfig)
	}
	if len(managedState.OriginPools) != 1 || managedState.OriginPools[0].KeepaliveConnections != 64 ||
		!strings.Contains(managedState.NginxConfig, "include "+managedState.OriginPools[0].ConfigPath+";") ||
		strings.Contains(managedState.NginxConfig, "server origin.example.test:8080;") {
		t.Fatalf("managed node origin state = %#v\n%s", managedState.OriginPools, managedState.NginxConfig)
	}
	version := managedState.Version
	if err := publisher.PublishNode(managedNode.ID); err != nil {
		t.Fatal(err)
	}
	unchanged, _, err := database.NodeState(managedNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Version != version {
		t.Fatalf("unchanged managed pool advanced version from %d to %d", version, unchanged.Version)
	}
}

func TestPublishNodeUpdatesNginxCapacityState(t *testing.T) {
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
	node, err := database.CreateNode("capacity-edge", "203.0.113.19")
	if err != nil {
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	if err := publisher.PublishNode(node.ID); err != nil {
		t.Fatal(err)
	}
	initial, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(initial.NginxMainConfig, "worker_processes auto;") || !strings.Contains(initial.NginxEventsConfig, "worker_connections 4096;") {
		t.Fatalf("default capacity state = %#v", initial)
	}
	if _, err := database.SetNodeNginxCapacity(node.ID, domain.NginxCapacity{
		WorkerProcesses: 4, WorkerConnections: 8192, WorkerRlimitNoFile: 16384,
	}); err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishNode(node.ID); err != nil {
		t.Fatal(err)
	}
	updated, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != initial.Version+1 || !strings.Contains(updated.NginxMainConfig, "worker_processes 4;") ||
		!strings.Contains(updated.NginxMainConfig, "worker_rlimit_nofile 16384;") || !strings.Contains(updated.NginxEventsConfig, "worker_connections 8192;") {
		t.Fatalf("updated capacity state = %#v", updated)
	}
	if err := publisher.PublishNode(node.ID); err != nil {
		t.Fatal(err)
	}
	unchanged, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Version != updated.Version {
		t.Fatalf("unchanged capacity state advanced version from %d to %d", updated.Version, unchanged.Version)
	}
}

func TestPublishTCPOnlySiteBuildsStreamStateWithoutHTTPPorts(t *testing.T) {
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
	node, err := database.CreateNode("mail-edge", "203.0.113.15")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilityTCPStream}); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "mail", Domains: []string{"mail.example.test"}, Nodes: []string{node.ID}, TCPOnly: true, Enabled: true,
		TCPForwards: []domain.TCPForward{{Name: "SMTP", ListenPort: 2525, UpstreamHost: "mail.example.test", UpstreamPort: 25}},
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	if _, err := publisher.PublishSite(site.ID); err != nil {
		t.Fatal(err)
	}
	state, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(state.PublicPorts, []int{2525}) {
		t.Fatalf("public ports = %#v", state.PublicPorts)
	}
	if strings.Contains(state.NginxConfig, "listen 80") || strings.Contains(state.NginxConfig, "listen 443") {
		t.Fatalf("TCP-only state contains HTTP listeners:\n%s", state.NginxConfig)
	}
	if !strings.Contains(state.NginxStreamConfig, "listen 2525;") || strings.Contains(state.NginxStreamConfig, "ssl_certificate") {
		t.Fatalf("unexpected stream state:\n%s", state.NginxStreamConfig)
	}
	newNode, err := database.CreateNode("mail-edge-new", "203.0.113.17")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(newNode.ID, []string{domain.EdgeCapabilityTCPStream}); err != nil {
		t.Fatal(err)
	}
	draft, zoneID, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft.Nodes = []string{newNode.ID}
	if _, err := database.UpdateSite(draft, zoneID); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(site.ID); err != nil {
		t.Fatal(err)
	}
	oldState, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldState.PublicPorts) != 0 || strings.Contains(oldState.NginxConfig, "listen 80") || strings.Contains(oldState.NginxConfig, "listen 443") {
		t.Fatalf("former TCP-only node reopened HTTP ports: %#v\n%s", oldState.PublicPorts, oldState.NginxConfig)
	}
}

func TestPublishEnablesHTTP3OnlyForCapableNodes(t *testing.T) {
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
	http3Node, err := database.CreateNode("edge-http3", "203.0.113.18")
	if err != nil {
		t.Fatal(err)
	}
	legacyNode, err := database.CreateNode("edge-http2", "203.0.113.19")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(http3Node.ID, []string{domain.EdgeCapabilityHTTP3}); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "protocols", Domains: []string{"protocols.example.test"}, Nodes: []string{http3Node.ID, legacyNode.ID},
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
	http3State, _, err := database.NodeState(http3Node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(http3State.PublicUDPPorts, []int{443}) || !strings.Contains(http3State.NginxConfig, "listen 443 quic") || !strings.Contains(http3State.NginxConfig, "Alt-Svc") {
		t.Fatalf("HTTP/3 node state = %#v\n%s", http3State.PublicUDPPorts, http3State.NginxConfig)
	}
	legacyState, _, err := database.NodeState(legacyNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyState.PublicUDPPorts) != 0 || strings.Contains(legacyState.NginxConfig, "listen 443 quic") || strings.Contains(legacyState.NginxConfig, "Alt-Svc") {
		t.Fatalf("legacy node received HTTP/3 state = %#v\n%s", legacyState.PublicUDPPorts, legacyState.NginxConfig)
	}
}

func TestPublishTCPForwardRequiresUpgradedAgentCapability(t *testing.T) {
	if !siteRequiresTCPStream([]domain.Site{{TCPOnly: true}}) {
		t.Fatal("disabled TCP-only state did not require the stream-capable agent")
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, _ := NewEncryptionKey()
	cipher, _ := NewCipher(key)
	node, err := database.CreateNode("old-edge", "203.0.113.16")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "mail", Domains: []string{"mail.example.test"}, Nodes: []string{node.ID}, TCPOnly: true, Enabled: true,
		TCPForwards: []domain.TCPForward{{Name: "SMTP", ListenPort: 2525, UpstreamHost: "mail.example.test", UpstreamPort: 25}},
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	_, err = (Publisher{Store: database, Cipher: cipher}).PublishSite(site.ID)
	if err == nil || !strings.Contains(err.Error(), "must be upgraded") {
		t.Fatalf("publish error = %v", err)
	}
	if _, _, err := database.NodeState(node.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unsupported node received state: %v", err)
	}
}

func TestPublishingOneSitePreservesAnotherSitesPublishedSnapshot(t *testing.T) {
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
	publisher := Publisher{Store: database, Cipher: cipher}
	sharedNode, err := database.CreateNode("edge-shared", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	apiOnlyNode, err := database.CreateNode("edge-api", "203.0.113.11")
	if err != nil {
		t.Fatal(err)
	}
	apiSite, err := database.CreateSite(domain.Site{
		Name: "api", Domains: []string{"api.example.test"}, Nodes: []string{sharedNode.ID, apiOnlyNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://api-old-origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-api")
	if err != nil {
		t.Fatal(err)
	}
	apiOldCertificate, apiOldKey, apiOldNotAfter := testCertificate(t, apiSite.Domains...)
	if err := publisher.StoreCertificate(apiSite.ID, apiOldCertificate, apiOldKey, apiOldNotAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(apiSite.ID); err != nil {
		t.Fatal(err)
	}
	nodeSite, err := database.CreateSite(domain.Site{
		Name: "node", Domains: []string{"node.example.test"}, Nodes: []string{sharedNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://node-old-origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-node")
	if err != nil {
		t.Fatal(err)
	}
	nodeCertificate, nodeKey, nodeNotAfter := testCertificate(t, nodeSite.Domains...)
	if err := publisher.StoreCertificate(nodeSite.ID, nodeCertificate, nodeKey, nodeNotAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(nodeSite.ID); err != nil {
		t.Fatal(err)
	}
	sharedBefore, _, err := database.NodeState(sharedNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	apiOnlyBefore, apiOnlyCertificatesBefore, err := database.NodeState(apiOnlyNode.ID)
	if err != nil {
		t.Fatal(err)
	}

	apiDraft, zoneID, err := database.GetSite(apiSite.ID)
	if err != nil {
		t.Fatal(err)
	}
	apiDraft.PrimaryOrigin.URL = "https://api-draft-origin.example.test"
	if _, err := database.UpdateSite(apiDraft, zoneID); err != nil {
		t.Fatal(err)
	}
	apiDraftCertificate, apiDraftKey, apiDraftNotAfter := testCertificate(t, apiSite.Domains...)
	if err := publisher.StoreCertificate(apiSite.ID, apiDraftCertificate, apiDraftKey, apiDraftNotAfter); err != nil {
		t.Fatal(err)
	}
	nodeDraft, nodeZoneID, err := database.GetSite(nodeSite.ID)
	if err != nil {
		t.Fatal(err)
	}
	nodeDraft.PrimaryOrigin.URL = "https://node-new-origin.example.test"
	if _, err := database.UpdateSite(nodeDraft, nodeZoneID); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(nodeSite.ID); err != nil {
		t.Fatal(err)
	}

	apiAfter, _, err := database.GetSite(apiSite.ID)
	if err != nil {
		t.Fatal(err)
	}
	if apiAfter.Published {
		t.Fatal("publishing node unexpectedly promoted the API draft")
	}
	sharedAfter, sharedCertificatesCiphertext, err := database.NodeState(sharedNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sharedAfter.Version != sharedBefore.Version+1 {
		t.Fatalf("shared node version = %d, want %d", sharedAfter.Version, sharedBefore.Version+1)
	}
	if !strings.Contains(sharedAfter.NginxConfig, "api-old-origin.example.test:443") || strings.Contains(sharedAfter.NginxConfig, "api-draft-origin.example.test:443") {
		t.Fatalf("shared node did not retain the published API origin:\n%s", sharedAfter.NginxConfig)
	}
	if !strings.Contains(sharedAfter.NginxConfig, "node-new-origin.example.test:443") {
		t.Fatalf("shared node did not receive the node draft:\n%s", sharedAfter.NginxConfig)
	}
	encodedCertificates, err := cipher.Decrypt(sharedCertificatesCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	var sharedCertificates map[string]domain.TLSBundle
	if err := json.Unmarshal(encodedCertificates, &sharedCertificates); err != nil {
		t.Fatal(err)
	}
	if got := sharedCertificates[apiSite.ID].CertificatePEM; got != string(apiOldCertificate) || got == string(apiDraftCertificate) {
		t.Fatal("shared node did not retain the published API certificate")
	}
	apiOnlyAfter, apiOnlyCertificatesAfter, err := database.NodeState(apiOnlyNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if apiOnlyAfter.Version != apiOnlyBefore.Version || apiOnlyAfter.NginxConfig != apiOnlyBefore.NginxConfig || !bytes.Equal(apiOnlyCertificatesAfter, apiOnlyCertificatesBefore) {
		t.Fatalf("unaffected API-only node was rewritten: before=%#v after=%#v", apiOnlyBefore, apiOnlyAfter)
	}
}

func TestPublishUpdatesOldAndNewNodesButNotUnrelatedNodes(t *testing.T) {
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
	publisher := Publisher{Store: database, Cipher: cipher}
	oldNode, err := database.CreateNode("edge-old", "203.0.113.20")
	if err != nil {
		t.Fatal(err)
	}
	newNode, err := database.CreateNode("edge-new", "203.0.113.21")
	if err != nil {
		t.Fatal(err)
	}
	unrelatedNode, err := database.CreateNode("edge-unrelated", "203.0.113.22")
	if err != nil {
		t.Fatal(err)
	}
	unrelatedSite, err := database.CreateSite(domain.Site{
		Name: "unrelated", Domains: []string{"unrelated.example.test"}, Nodes: []string{unrelatedNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://unrelated-origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-unrelated")
	if err != nil {
		t.Fatal(err)
	}
	unrelatedCertificate, unrelatedKey, unrelatedNotAfter := testCertificate(t, unrelatedSite.Domains...)
	if err := publisher.StoreCertificate(unrelatedSite.ID, unrelatedCertificate, unrelatedKey, unrelatedNotAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(unrelatedSite.ID); err != nil {
		t.Fatal(err)
	}
	unrelatedBefore, unrelatedCertificatesBefore, err := database.NodeState(unrelatedNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	movingSite, err := database.CreateSite(domain.Site{
		Name: "moving", Domains: []string{"moving.example.test"}, Nodes: []string{oldNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://moving-origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-moving")
	if err != nil {
		t.Fatal(err)
	}
	movingCertificate, movingKey, movingNotAfter := testCertificate(t, movingSite.Domains...)
	if err := publisher.StoreCertificate(movingSite.ID, movingCertificate, movingKey, movingNotAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(movingSite.ID); err != nil {
		t.Fatal(err)
	}
	draft, zoneID, err := database.GetSite(movingSite.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft.Nodes = []string{newNode.ID}
	if _, err := database.UpdateSite(draft, zoneID); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(oldNode.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(newNode.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(movingSite.ID); err != nil {
		t.Fatal(err)
	}
	status, err := database.PublishStatus(movingSite.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Task == nil || status.Task.Status != domain.TaskApplying || len(status.Nodes) != 2 {
		t.Fatalf("publish did not wait for both old and new active nodes: %#v", status)
	}
	oldState, _, err := database.NodeState(oldNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if nginx.HasSiteHealth(oldState.NginxConfig, movingSite.ID) {
		t.Fatalf("old node retained moving site:\n%s", oldState.NginxConfig)
	}
	newState, _, err := database.NodeState(newNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !nginx.HasSiteHealth(newState.NginxConfig, movingSite.ID) {
		t.Fatalf("new node did not receive moving site:\n%s", newState.NginxConfig)
	}
	unrelatedAfter, unrelatedCertificatesAfter, err := database.NodeState(unrelatedNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unrelatedAfter.Version != unrelatedBefore.Version || unrelatedAfter.NginxConfig != unrelatedBefore.NginxConfig || !bytes.Equal(unrelatedCertificatesAfter, unrelatedCertificatesBefore) {
		t.Fatalf("unrelated node was rewritten: before=%#v after=%#v", unrelatedBefore, unrelatedAfter)
	}
}

func TestRepublishSkipsUnchangedNodeState(t *testing.T) {
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
	node, err := database.CreateNode("edge-1", "203.0.113.30")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "unchanged", Domains: []string{"unchanged.example.test"}, Nodes: []string{node.ID},
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
	before, beforeCertificates, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := publisher.PublishSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskSucceeded {
		t.Fatalf("unchanged republish task = %#v", task)
	}
	after, afterCertificates, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || after.NginxConfig != before.NginxConfig || !bytes.Equal(afterCertificates, beforeCertificates) {
		t.Fatalf("unchanged node state was rewritten: before=%#v after=%#v", before, after)
	}
}

func TestLegacyPendingSiteMustBeRepublishedBeforeRemoval(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-legacy", "203.0.113.31")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "legacy-pending", Domains: []string{"legacy-pending.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	task, err := database.CreateTask("publish_site", site.ID, "legacy publication")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateTask(task.ID, domain.TaskSucceeded, task.Detail); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Publisher{Store: database}).prepareNodeStates(site.ID, true); err == nil || !strings.Contains(err.Error(), "without a published snapshot") {
		t.Fatalf("legacy pending removal was not blocked: %v", err)
	}
}

func TestLegacyPendingSitePublicationSweepsUnknownOldNodes(t *testing.T) {
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
	publisher := Publisher{Store: database, Cipher: cipher}
	oldNode, err := database.CreateNode("edge-legacy-old", "203.0.113.32")
	if err != nil {
		t.Fatal(err)
	}
	newNode, err := database.CreateNode("edge-legacy-new", "203.0.113.33")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "legacy-move", Domains: []string{"legacy-move.example.test"}, Nodes: []string{oldNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	certificate, privateKey, notAfter := testCertificate(t, site.Domains...)
	if err := publisher.StoreCertificate(site.ID, certificate, privateKey, notAfter); err != nil {
		t.Fatal(err)
	}
	oldConfiguration, err := nginx.Render([]domain.Site{site})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SaveNodeState(oldNode.ID, domain.DesiredState{Version: 7, NginxConfig: oldConfiguration, PublicPorts: []int{80, 443}}, nil); err != nil {
		t.Fatal(err)
	}
	legacyTask, err := database.CreateTask("publish_site", site.ID, "legacy publication")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateTask(legacyTask.ID, domain.TaskSucceeded, legacyTask.Detail); err != nil {
		t.Fatal(err)
	}
	draft, zoneID, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft.Nodes = []string{newNode.ID}
	if _, err := database.UpdateSite(draft, zoneID); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(site.ID); err != nil {
		t.Fatal(err)
	}
	oldState, _, err := database.NodeState(oldNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if nginx.HasSiteHealth(oldState.NginxConfig, site.ID) {
		t.Fatalf("legacy old node retained the site:\n%s", oldState.NginxConfig)
	}
	newState, _, err := database.NodeState(newNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !nginx.HasSiteHealth(newState.NginxConfig, site.ID) {
		t.Fatalf("legacy new node did not receive the site:\n%s", newState.NginxConfig)
	}
}

func TestPublishWaitsForActiveEdgeConfirmation(t *testing.T) {
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
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(node.ID, 0, "", nil); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{Name: "site", Domains: []string{"cdn.example.test"}, Nodes: []string{node.ID}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	certificate, privateKey, notAfter := testCertificate(t, "cdn.example.test")
	if err := publisher.StoreCertificate(site.ID, certificate, privateKey, notAfter); err != nil {
		t.Fatal(err)
	}
	task, err := publisher.PublishSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskApplying || task.DeadlineAt == nil {
		t.Fatalf("publish should wait for edge: %#v", task)
	}
	state, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(node.ID, state.Version, "", &domain.ApplyReport{Version: state.Version, Status: domain.ApplySucceeded, Detail: "Nginx is listening"}); err != nil {
		t.Fatal(err)
	}
	status, err := database.PublishStatus(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Task == nil || status.Task.Status != domain.TaskSucceeded || len(status.Nodes) != 1 || status.Nodes[0].Status != domain.PublishNodeSucceeded || status.Nodes[0].Detail != "Nginx is listening" {
		t.Fatalf("unexpected published status: %#v", status)
	}
}

func TestPublishRecordsPortConflictFromEdge(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	task, created, err := database.CreateOrGetActivePublishTask("site", time.Now().Add(time.Minute))
	if err != nil || !created {
		t.Fatalf("create task: %#v %t %v", task, created, err)
	}
	if err := database.UpdateTask(task.ID, domain.TaskApplying, "waiting for edge"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreatePublishTaskNodes(task.ID, []store.PublishTaskNode{{NodeID: node.ID, TargetVersion: 5}}); err != nil {
		t.Fatal(err)
	}
	report := &domain.ApplyReport{Version: 5, Status: domain.ApplyFailed, Code: "port_conflict", Detail: "TCP 80 is in use", PortConflicts: []domain.PortConflict{{Port: 80, PID: 1234, Process: "caddy"}}}
	if err := database.Heartbeat(node.ID, 0, report.Detail, report); err != nil {
		t.Fatal(err)
	}
	status, err := database.PublishStatus("site")
	if err != nil {
		t.Fatal(err)
	}
	if status.Task == nil || status.Task.Status != domain.TaskFailed || len(status.Nodes) != 1 || status.Nodes[0].ErrorCode != "port_conflict" || len(status.Nodes[0].PortConflicts) != 1 {
		t.Fatalf("unexpected conflict status: %#v", status)
	}
}

func testCertificate(t *testing.T, domains ...string) ([]byte, []byte, time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domains[0]}, DNSNames: domains, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}), certificate.NotAfter
}

func TestLoginAndEnrollmentCommandGuard(t *testing.T) {
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "control.db"))
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
	tokenPath := filepath.Join(directory, "initialization-token")
	if _, err := EnsureInitializationToken(tokenPath); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, Cipher: cipher, ControlURL: "https://control.example.test", EdgeControlURL: "https://edge-control.example.test:8443", InitializationTokenPath: tokenPath}
	setup := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewBufferString(`{"initialization_token":"`+strings.TrimSpace(string(token))+`","password":"correct horse battery staple","totp_secret":"JBSWY3DPEHPK3PXP"}`))
	setup.Header.Set("Content-Type", "application/json")
	setupResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(setupResponse, setup)
	if setupResponse.Code != http.StatusCreated {
		t.Fatalf("setup failed: %d %s", setupResponse.Code, setupResponse.Body.String())
	}
	login := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewBufferString(`{"password":"correct horse battery staple","recovery_code":"`+decodeRecoveryCode(t, setupResponse.Body.Bytes())+`"}`))
	login.Header.Set("Content-Type", "application/json")
	login.RemoteAddr = "127.0.0.1:12345"
	loginResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", loginResponse.Code, loginResponse.Body.String())
	}
	cookie := loginResponse.Result().Cookies()[0]
	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	sessionRequest.AddCookie(cookie)
	sessionResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK || decodeCSRF(t, sessionResponse.Body.Bytes()) != decodeCSRF(t, loginResponse.Body.Bytes()) {
		t.Fatalf("session must return the existing CSRF token: %d %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	guarded := httptest.NewRequest(http.MethodPost, "/api/nodes/"+node.ID+"/enrollment-token", nil)
	guarded.AddCookie(cookie)
	guarded.Header.Set("X-CSRF-Token", decodeCSRF(t, loginResponse.Body.Bytes()))
	guardedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(guardedResponse, guarded)
	if guardedResponse.Code != http.StatusConflict {
		t.Fatalf("expected EdgeBinaryURL guard, got %d %s", guardedResponse.Code, guardedResponse.Body.String())
	}
	server.EdgeBinaryURL = "https://downloads.example.test/edge"
	server.EdgeBinarySHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ready := httptest.NewRequest(http.MethodPost, "/api/nodes/"+node.ID+"/enrollment-token", nil)
	ready.AddCookie(cookie)
	ready.Header.Set("X-CSRF-Token", decodeCSRF(t, loginResponse.Body.Bytes()))
	readyResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(readyResponse, ready)
	if readyResponse.Code != http.StatusCreated || !bytes.Contains(readyResponse.Body.Bytes(), []byte("--binary-sha256")) || !bytes.Contains(readyResponse.Body.Bytes(), []byte("--enrollment-token")) || !bytes.Contains(readyResponse.Body.Bytes(), []byte("sudo bash -s")) || !bytes.Contains(readyResponse.Body.Bytes(), []byte("https://edge-control.example.test:8443")) {
		t.Fatalf("expected checksum-bound command, got %d %s", readyResponse.Code, readyResponse.Body.String())
	}
	if err := database.SetNodeCertificate(node.ID, "sha256:existing-edge"); err != nil {
		t.Fatal(err)
	}
	upgrade := httptest.NewRequest(http.MethodPost, "/api/nodes/"+node.ID+"/enrollment-token", nil)
	upgrade.AddCookie(cookie)
	upgrade.Header.Set("X-CSRF-Token", decodeCSRF(t, loginResponse.Body.Bytes()))
	upgradeResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(upgradeResponse, upgrade)
	if upgradeResponse.Code != http.StatusCreated || bytes.Contains(upgradeResponse.Body.Bytes(), []byte("--enrollment-token")) || !bytes.Contains(upgradeResponse.Body.Bytes(), []byte(`"enrollment_required":false`)) {
		t.Fatalf("expected identity-preserving upgrade command, got %d %s", upgradeResponse.Code, upgradeResponse.Body.String())
	}
	server.EdgeBinarySHA256 = strings.Repeat("z", 64)
	invalid := httptest.NewRequest(http.MethodPost, "/api/nodes/"+node.ID+"/enrollment-token", nil)
	invalid.AddCookie(cookie)
	invalid.Header.Set("X-CSRF-Token", decodeCSRF(t, loginResponse.Body.Bytes()))
	invalidResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusConflict {
		t.Fatalf("expected malformed checksum guard, got %d %s", invalidResponse.Code, invalidResponse.Body.String())
	}
	server.EdgeBinarySHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	server.EdgeBinaryURL = "http://downloads.example.test/edge"
	insecure := httptest.NewRequest(http.MethodPost, "/api/nodes/"+node.ID+"/enrollment-token", nil)
	insecure.AddCookie(cookie)
	insecure.Header.Set("X-CSRF-Token", decodeCSRF(t, loginResponse.Body.Bytes()))
	insecureResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(insecureResponse, insecure)
	if insecureResponse.Code != http.StatusConflict {
		t.Fatalf("expected HTTP binary URL guard, got %d %s", insecureResponse.Code, insecureResponse.Body.String())
	}
}

func TestRequestIPHonorsTrustedProxyOnly(t *testing.T) {
	_, trustedProxy, err := net.ParseCIDR("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{TrustedProxyCIDRs: []*net.IPNet{trustedProxy}}
	proxied := httptest.NewRequest(http.MethodGet, "/", nil)
	proxied.RemoteAddr = "127.0.0.1:49152"
	proxied.Header.Set("X-Real-IP", "203.0.113.10")
	if got := server.requestIP(proxied); got != "203.0.113.10" {
		t.Fatalf("trusted proxy address = %q", got)
	}
	direct := httptest.NewRequest(http.MethodGet, "/", nil)
	direct.RemoteAddr = "198.51.100.11:49152"
	direct.Header.Set("X-Real-IP", "203.0.113.10")
	if got := server.requestIP(direct); got != "198.51.100.11" {
		t.Fatalf("direct client address = %q", got)
	}
}

func TestEdgeBinaryRequiresConfiguredRegularFile(t *testing.T) {
	server := &Server{}
	missing := httptest.NewRecorder()
	server.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/downloads/cdn-edge-agent-linux-amd64", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing edge binary = %d", missing.Code)
	}
	path := filepath.Join(t.TempDir(), "cdn-edge-agent-linux-amd64")
	if err := os.WriteFile(path, []byte("edge-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	server.EdgeBinaryPath = path
	served := httptest.NewRecorder()
	server.Handler().ServeHTTP(served, httptest.NewRequest(http.MethodGet, "/downloads/cdn-edge-agent-linux-amd64", nil))
	if served.Code != http.StatusOK || served.Body.String() != "edge-binary" {
		t.Fatalf("edge binary response = %d %q", served.Code, served.Body.String())
	}
	if got := served.Header().Get("Content-Disposition"); !strings.Contains(got, "cdn-edge-agent-linux-amd64") {
		t.Fatalf("content disposition = %q", got)
	}
}

func TestPublishRejectsOnlyRevokedNodes(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeRevoked); err != nil {
		t.Fatal(err)
	}
	_, err = database.CreateSite(domain.Site{Name: "site", Domains: []string{"cdn.example.test"}, Nodes: []string{node.ID}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}}, "zone")
	if err == nil {
		t.Fatal("expected revoked-node validation error")
	}
}

func TestSiteDomainIsExclusive(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	first := domain.Site{Name: "first", Domains: []string{"cdn.example.test"}, Nodes: []string{node.ID}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}}
	if _, err := database.CreateSite(first, "zone"); err != nil {
		t.Fatal(err)
	}
	second := domain.Site{Name: "second", Domains: []string{"cdn.example.test"}, Nodes: []string{node.ID}, PrimaryOrigin: domain.Origin{URL: "https://other-origin.example.test", Enabled: true}}
	if _, err := database.CreateSite(second, "zone"); err == nil {
		t.Fatal("expected duplicate-domain error")
	}
}

func TestInternalCARenewsOnlyTheMatchingNodeCertificate(t *testing.T) {
	ca, err := LoadOrCreateInternalCA(filepath.Join(t.TempDir(), "pki"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "cdn-edge"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	issued, err := ca.SignCSR(csr, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	issuedBlock, _ := pem.Decode(issued)
	if issuedBlock == nil {
		t.Fatal("issued certificate is invalid")
	}
	if _, err := ca.SignRenewal(issuedBlock.Bytes, csr, "node-1"); err != nil {
		t.Fatalf("expected renewal to succeed: %v", err)
	}
	if _, err := ca.SignRenewal(issuedBlock.Bytes, csr, "node-2"); err == nil {
		t.Fatal("expected renewal with another node ID to fail")
	}
}

func TestStoreCertificateRejectsMismatchedPrivateKey(t *testing.T) {
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
	certificate, _, notAfter := testCertificate(t, "cdn.example.test")
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherKeyDER, err := x509.MarshalPKCS8PrivateKey(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	if err := publisher.StoreCertificate("site", certificate, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherKeyDER}), notAfter); err == nil {
		t.Fatal("expected mismatched private key to be rejected")
	}
}

func decodeCSRF(t *testing.T, body []byte) string {
	t.Helper()
	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result["csrf_token"]
}

func decodeRecoveryCode(t *testing.T, body []byte) string {
	t.Helper()
	var result struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.RecoveryCodes) == 0 {
		t.Fatal("setup response did not include recovery codes")
	}
	return result.RecoveryCodes[0]
}
