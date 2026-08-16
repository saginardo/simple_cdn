package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/integrations"
	"simple_cdn/internal/logstore"
	"simple_cdn/internal/store"
)

type cachePrewarmLogStore struct {
	logstore.Noop
	events []domain.AccessLogEvent
}

func (s cachePrewarmLogStore) Recent(context.Context, string, int) ([]domain.AccessLogEvent, error) {
	return append([]domain.AccessLogEvent(nil), s.events...), nil
}

type recordingZoneResolver struct {
	zoneID  string
	err     error
	domains []string
	calls   int
}

func (r *recordingZoneResolver) ResolveZoneID(_ context.Context, domains []string) (string, error) {
	r.calls++
	r.domains = append([]string(nil), domains...)
	return r.zoneID, r.err
}

func TestSiteAPIAutomaticallyResolvesCloudflareZone(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-zone", "203.0.113.90")
	if err != nil {
		t.Fatal(err)
	}
	resolver := &recordingZoneResolver{zoneID: "zone-auto"}
	server := &Server{Store: database, ZoneResolver: resolver}
	created := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "automatic zone", "domains": []string{"CDN.Example.Test."}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	})
	if created.ZoneID != "zone-auto" || resolver.calls != 1 || len(resolver.domains) != 1 || resolver.domains[0] != "cdn.example.test" {
		t.Fatalf("automatic zone result = %#v, resolver = %#v", created, resolver)
	}

	updated := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, map[string]any{
		"name": created.Name, "domains": created.Domains, "node_ids": created.Nodes,
		"primary_origin": created.PrimaryOrigin, "enabled": created.Enabled,
	})
	if updated.ZoneID != "zone-auto" || resolver.calls != 1 {
		t.Fatalf("omitted update zone = %q, resolver calls = %d", updated.ZoneID, resolver.calls)
	}
}

func TestSiteAPIStoresBackupNodesAndPreservesThemWhenOmitted(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	primary, err := database.CreateNode("primary-edge", "203.0.113.92")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := database.CreateNode("backup-edge", "203.0.113.93")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	created := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "backup nodes", "zone_id": "zone", "domains": []string{"backup.example.test"},
		"node_ids": []string{primary.ID}, "backup_node_ids": []string{backup.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	})
	if len(created.BackupNodes) != 1 || created.BackupNodes[0] != backup.ID {
		t.Fatalf("created backup nodes = %#v", created.BackupNodes)
	}

	updated := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, map[string]any{
		"name": created.Name, "domains": created.Domains, "node_ids": created.Nodes,
		"primary_origin": created.PrimaryOrigin, "enabled": created.Enabled,
	})
	if len(updated.BackupNodes) != 1 || updated.BackupNodes[0] != backup.ID {
		t.Fatalf("omitted backup nodes changed assignment = %#v", updated.BackupNodes)
	}

	cleared := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, map[string]any{
		"name": created.Name, "domains": created.Domains, "node_ids": created.Nodes,
		"backup_node_ids": []string{}, "primary_origin": created.PrimaryOrigin, "enabled": created.Enabled,
	})
	if len(cleared.BackupNodes) != 0 {
		t.Fatalf("explicitly cleared backup nodes = %#v", cleared.BackupNodes)
	}
}

func TestSiteAPIReturnsBadRequestWhenCloudflareZoneCannotBeResolved(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-zone", "203.0.113.91")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, ZoneResolver: &recordingZoneResolver{err: integrations.ErrZoneNotFound}}
	response := requestSiteResponse(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "missing zone", "domains": []string{"cdn.unknown.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "no accessible Cloudflare zone") {
		t.Fatalf("missing zone response = %d %s", response.Code, response.Body.String())
	}
}

func TestSiteClientMaxBodySizeAPI(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	defaultSite := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "default", "zone_id": "zone", "domains": []string{"default.example.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	})
	if defaultSite.ClientMaxBodySizeMB != domain.DefaultClientMaxBodySizeMB {
		t.Fatalf("omitted client max body size = %d", defaultSite.ClientMaxBodySizeMB)
	}

	var largest domain.Site
	for _, value := range []int{128, 256, 512, 1024} {
		created := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
			"name": fmt.Sprintf("body-%d", value), "zone_id": "zone", "domains": []string{fmt.Sprintf("body-%d.example.test", value)}, "node_ids": []string{node.ID},
			"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "client_max_body_size_mb": value, "enabled": true,
		})
		if created.ClientMaxBodySizeMB != value {
			t.Fatalf("created client max body size = %d, want %d", created.ClientMaxBodySizeMB, value)
		}
		if value == 1024 {
			largest = created
		}
	}

	updated := requestSite(t, server, http.MethodPut, "/api/sites/"+largest.ID, map[string]any{
		"name": largest.Name, "zone_id": largest.ZoneID, "domains": largest.Domains, "node_ids": largest.Nodes,
		"primary_origin": largest.PrimaryOrigin, "enabled": largest.Enabled,
	})
	if updated.ClientMaxBodySizeMB != 1024 {
		t.Fatalf("omitted update did not preserve client max body size: %#v", updated)
	}

	for _, value := range []int{0, 127, 129, 1025} {
		response := requestSiteResponse(t, server, http.MethodPost, "/api/sites", map[string]any{
			"name": "invalid", "zone_id": "zone", "domains": []string{"invalid.example.test"}, "node_ids": []string{node.ID},
			"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "client_max_body_size_mb": value, "enabled": true,
		})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("client max body size %d = %d %s", value, response.Code, response.Body.String())
		}
	}

	before, _, err := database.GetSite(largest.ID)
	if err != nil {
		t.Fatal(err)
	}
	response := requestSiteResponse(t, server, http.MethodPut, "/api/sites/"+largest.ID, map[string]any{
		"name": largest.Name, "zone_id": largest.ZoneID, "domains": largest.Domains, "node_ids": largest.Nodes,
		"primary_origin": largest.PrimaryOrigin, "client_max_body_size_mb": 129, "enabled": largest.Enabled,
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid update = %d %s", response.Code, response.Body.String())
	}
	after, _, err := database.GetSite(largest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ClientMaxBodySizeMB != before.ClientMaxBodySizeMB || after.ConfigVersion != before.ConfigVersion {
		t.Fatalf("invalid update changed site: before=%#v after=%#v", before, after)
	}
}

func TestSiteHTTP3APIIsOptInAndPreservesOmission(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("http3-edge", "203.0.113.30")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	base := map[string]any{
		"name": "http3-opt-in", "zone_id": "zone", "domains": []string{"http3.example.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	}
	created := requestSite(t, server, http.MethodPost, "/api/sites", base)
	if created.HTTP3Enabled {
		t.Fatal("new site enabled HTTP/3 by default")
	}
	base["http3_enabled"] = true
	updated := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if !updated.HTTP3Enabled {
		t.Fatal("site HTTP/3 opt-in was not saved")
	}
	delete(base, "http3_enabled")
	preserved := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if !preserved.HTTP3Enabled {
		t.Fatal("omitted HTTP/3 update did not preserve the current value")
	}
	base["http3_enabled"] = true
	base["tcp_only"] = true
	base["tcp_forwards"] = []map[string]any{{
		"name": "HTTPS passthrough", "listen_port": 8443,
		"upstream_host": "origin.example.test", "upstream_port": 443,
	}}
	tcpOnly := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if tcpOnly.HTTP3Enabled {
		t.Fatal("TCP-only site retained an HTTP/3 setting")
	}
}

func TestSiteProxyBufferingAPIDefaultsAndPreservesOmission(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("buffering-edge", "203.0.113.31")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	base := map[string]any{
		"name": "buffering", "zone_id": "zone", "domains": []string{"buffering.example.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	}
	created := requestSite(t, server, http.MethodPost, "/api/sites", base)
	if !created.RequestBodyBuffering || !created.OriginResponseBuffering {
		t.Fatalf("new site buffering defaults = request:%t response:%t", created.RequestBodyBuffering, created.OriginResponseBuffering)
	}
	base["request_body_buffering"] = false
	base["origin_response_buffering"] = false
	updated := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if updated.RequestBodyBuffering || updated.OriginResponseBuffering {
		t.Fatalf("disabled site buffering = request:%t response:%t", updated.RequestBodyBuffering, updated.OriginResponseBuffering)
	}
	delete(base, "request_body_buffering")
	delete(base, "origin_response_buffering")
	preserved := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if preserved.RequestBodyBuffering || preserved.OriginResponseBuffering {
		t.Fatalf("omitted update changed buffering = request:%t response:%t", preserved.RequestBodyBuffering, preserved.OriginResponseBuffering)
	}
}

func TestSiteClientKeepaliveTimeoutAPI(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("keepalive-edge", "203.0.113.11")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	base := map[string]any{
		"name": "keepalive", "zone_id": "zone", "domains": []string{"keepalive.example.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	}
	created := requestSite(t, server, http.MethodPost, "/api/sites", base)
	if created.ClientKeepaliveTimeoutSeconds != domain.DefaultClientKeepaliveTimeoutSeconds {
		t.Fatalf("default client keepalive = %d", created.ClientKeepaliveTimeoutSeconds)
	}
	base["client_keepalive_timeout_seconds"] = 240
	updated := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if updated.ClientKeepaliveTimeoutSeconds != 240 {
		t.Fatalf("configured client keepalive = %d", updated.ClientKeepaliveTimeoutSeconds)
	}
	delete(base, "client_keepalive_timeout_seconds")
	preserved := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if preserved.ClientKeepaliveTimeoutSeconds != 240 {
		t.Fatalf("omitted update did not preserve client keepalive = %d", preserved.ClientKeepaliveTimeoutSeconds)
	}
	for _, value := range []int{14, 3601} {
		base["client_keepalive_timeout_seconds"] = value
		response := requestSiteResponse(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("client keepalive %d = %d %s", value, response.Code, response.Body.String())
		}
	}
}

func TestSiteDNSTTLAPIHandlesOverrideInheritanceAndOmission(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-ttl", "203.0.113.81")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	base := map[string]any{
		"name": "ttl", "zone_id": "zone", "domains": []string{"ttl.example.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	}
	created := requestSite(t, server, http.MethodPost, "/api/sites", base)
	if created.DNSTTLSeconds != nil {
		t.Fatalf("omitted TTL did not inherit: %#v", created.DNSTTLSeconds)
	}
	base["dns_ttl_seconds"] = 180
	updated := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if updated.DNSTTLSeconds == nil || *updated.DNSTTLSeconds != 180 {
		t.Fatalf("TTL override = %#v", updated.DNSTTLSeconds)
	}
	delete(base, "dns_ttl_seconds")
	preserved := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if preserved.DNSTTLSeconds == nil || *preserved.DNSTTLSeconds != 180 {
		t.Fatalf("omitted update did not preserve TTL: %#v", preserved.DNSTTLSeconds)
	}
	base["dns_ttl_seconds"] = nil
	inherited := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if inherited.DNSTTLSeconds != nil {
		t.Fatalf("explicit null did not restore inheritance: %#v", inherited.DNSTTLSeconds)
	}
	for _, value := range []int{59, 301} {
		base["dns_ttl_seconds"] = value
		response := requestSiteResponse(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("TTL %d = %d %s", value, response.Code, response.Body.String())
		}
	}
}

func TestSiteIPv6DNSAPIIsOptInAndPreservedWhenOmitted(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNodeWithAddresses("edge-ipv6", "203.0.113.84", "2001:db8::84")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	base := map[string]any{
		"name": "ipv6", "zone_id": "zone", "domains": []string{"ipv6.example.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	}
	created := requestSite(t, server, http.MethodPost, "/api/sites", base)
	if created.IPv6Enabled {
		t.Fatal("site enabled IPv6 DNS without an explicit opt-in")
	}
	base["ipv6_enabled"] = true
	updated := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if !updated.IPv6Enabled {
		t.Fatal("site did not persist IPv6 DNS opt-in")
	}
	delete(base, "ipv6_enabled")
	preserved := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if !preserved.IPv6Enabled {
		t.Fatal("omitted IPv6 DNS setting was not preserved")
	}
	base["ipv6_enabled"] = false
	disabled := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if disabled.IPv6Enabled {
		t.Fatal("site did not disable IPv6 DNS")
	}
}

func TestSiteTCPForwardAPIHandlesTCPOnlyDrafts(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("mail-edge", "203.0.113.82")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	base := map[string]any{
		"name": "mail", "zone_id": "zone", "domains": []string{"mail.example.test"}, "node_ids": []string{node.ID},
		"tcp_only": true, "enabled": true,
		"tcp_forwards": []map[string]any{{
			"name": "IMAPS", "listen_port": 9993, "listen_tls": true,
			"upstream_host": "us1.workspace.org", "upstream_port": 993, "upstream_tls": true,
		}},
	}
	created := requestSite(t, server, http.MethodPost, "/api/sites", base)
	if !created.TCPOnly || len(created.TCPForwards) != 1 || created.TCPForwards[0].UpstreamTLSServerName != "us1.workspace.org" {
		t.Fatalf("created TCP site = %#v", created)
	}
	delete(base, "tcp_only")
	delete(base, "tcp_forwards")
	preserved := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if !preserved.TCPOnly || len(preserved.TCPForwards) != 1 || preserved.TCPForwards[0].ListenPort != 9993 {
		t.Fatalf("omitted TCP fields were not preserved: %#v", preserved)
	}
	base["tcp_only"] = true
	base["tcp_forwards"] = []map[string]any{{"name": "invalid", "listen_port": 443, "upstream_host": "mail.example.test", "upstream_port": 443}}
	response := requestSiteResponse(t, server, http.MethodPut, "/api/sites/"+created.ID, base)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "reserved HTTP port") {
		t.Fatalf("reserved TCP port = %d %s", response.Code, response.Body.String())
	}
}

func TestOriginAllowlistIncludesPublishedAndDraftAssignments(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	oldNode, err := database.CreateNode("edge-old", "203.0.113.72")
	if err != nil {
		t.Fatal(err)
	}
	newNode, err := database.CreateNode("edge-new", "203.0.113.73")
	if err != nil {
		t.Fatal(err)
	}
	sharedNode, err := database.CreateNode("edge-shared", "203.0.113.74")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "allowlist", Domains: []string{"allowlist.example.test"}, Nodes: []string{oldNode.ID, sharedNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkSitePublished(site.ID); err != nil {
		t.Fatal(err)
	}
	draft, zoneID, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft.Nodes = []string{sharedNode.ID, newNode.ID}
	if _, err := database.UpdateSite(draft, zoneID); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/sites/"+site.ID+"/origin-allowlist", nil)
	request.SetPathValue("id", site.ID)
	response := httptest.NewRecorder()
	(&Server{Store: database}).originAllowlist(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("allowlist response = %d %s", response.Code, response.Body.String())
	}
	var payload originAllowlistResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.IPv4CIDRs) != 3 || payload.IPv4CIDRs[0] != sharedNode.PublicIPv4+"/32" ||
		payload.IPv4CIDRs[1] != newNode.PublicIPv4+"/32" || payload.IPv4CIDRs[2] != oldNode.PublicIPv4+"/32" {
		t.Fatalf("allowlist omitted a published or draft node: %#v", payload.IPv4CIDRs)
	}
	if len(payload.Nodes) != 3 || payload.Nodes[0].NodeName != sharedNode.Name || payload.Nodes[0].Assignment != "current_and_published" ||
		payload.Nodes[1].NodeName != newNode.Name || payload.Nodes[1].Assignment != "current" ||
		payload.Nodes[2].NodeName != oldNode.Name || payload.Nodes[2].Assignment != "published_only" {
		t.Fatalf("allowlist node assignments = %#v", payload.Nodes)
	}
	if !strings.Contains(payload.Note, "待移除") {
		t.Fatalf("allowlist note did not explain the published-only node: %q", payload.Note)
	}
}

func TestSiteReadWriteTimeoutAPI(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	defaultSite := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "default-timeout", "zone_id": "zone", "domains": []string{"default-timeout.example.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "stream_paths": []string{"/legacy"}, "enabled": true,
	})
	if defaultSite.ReadWriteTimeoutSeconds != domain.DefaultReadWriteTimeoutSeconds {
		t.Fatalf("omitted read/write timeout = %d", defaultSite.ReadWriteTimeoutSeconds)
	}
	if defaultSite.StreamPaths == nil || len(defaultSite.StreamPaths) != 0 {
		t.Fatalf("legacy stream paths were not retired: %#v", defaultSite.StreamPaths)
	}

	var longest domain.Site
	for _, value := range []int{360, 900, 1800, 3600} {
		created := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
			"name": fmt.Sprintf("timeout-%d", value), "zone_id": "zone", "domains": []string{fmt.Sprintf("timeout-%d.example.test", value)}, "node_ids": []string{node.ID},
			"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "read_write_timeout_seconds": value, "enabled": true,
		})
		if created.ReadWriteTimeoutSeconds != value {
			t.Fatalf("created read/write timeout = %d, want %d", created.ReadWriteTimeoutSeconds, value)
		}
		if value == 3600 {
			longest = created
		}
	}

	updated := requestSite(t, server, http.MethodPut, "/api/sites/"+longest.ID, map[string]any{
		"name": longest.Name, "zone_id": longest.ZoneID, "domains": longest.Domains, "node_ids": longest.Nodes,
		"primary_origin": longest.PrimaryOrigin, "stream_paths": []string{"/legacy"}, "enabled": longest.Enabled,
	})
	if updated.ReadWriteTimeoutSeconds != 3600 {
		t.Fatalf("omitted update did not preserve read/write timeout: %#v", updated)
	}
	if updated.StreamPaths == nil || len(updated.StreamPaths) != 0 {
		t.Fatalf("legacy stream paths were not ignored on update: %#v", updated.StreamPaths)
	}

	for _, value := range []int{0, 359, 361, 7200} {
		response := requestSiteResponse(t, server, http.MethodPost, "/api/sites", map[string]any{
			"name": "invalid-timeout", "zone_id": "zone", "domains": []string{"invalid-timeout.example.test"}, "node_ids": []string{node.ID},
			"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "read_write_timeout_seconds": value, "enabled": true,
		})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("read/write timeout %d = %d %s", value, response.Code, response.Body.String())
		}
	}

	before, _, err := database.GetSite(longest.ID)
	if err != nil {
		t.Fatal(err)
	}
	response := requestSiteResponse(t, server, http.MethodPut, "/api/sites/"+longest.ID, map[string]any{
		"name": longest.Name, "zone_id": longest.ZoneID, "domains": longest.Domains, "node_ids": longest.Nodes,
		"primary_origin": longest.PrimaryOrigin, "read_write_timeout_seconds": 901, "enabled": longest.Enabled,
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid timeout update = %d %s", response.Code, response.Body.String())
	}
	after, _, err := database.GetSite(longest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ReadWriteTimeoutSeconds != before.ReadWriteTimeoutSeconds || after.ConfigVersion != before.ConfigVersion {
		t.Fatalf("invalid timeout update changed site: before=%#v after=%#v", before, after)
	}
}

func TestSiteOriginTLSServerNameAPI(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}

	created := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "ip-origin", "zone_id": "zone", "domains": []string{"lax.dustvm.de"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://203.0.113.20:443", "host_header": "lax.dustvm.de", "tls_server_name": "LAX.DUSTVM.DE", "http_version": "http2", "health_check_method": "GET", "health_check_path": "/health", "enabled": true},
		"backup_origin":  map[string]any{"url": "https://203.0.113.21:443", "host_header": "backup.dustvm.de", "tls_server_name": "backup.dustvm.de", "http_version": "http2", "health_check_method": "HEAD", "health_check_path": "/ready", "enabled": true},
		"enabled":        true,
	})
	if created.PrimaryOrigin.TLSServerName != "lax.dustvm.de" || created.BackupOrigin == nil || created.BackupOrigin.TLSServerName != "backup.dustvm.de" {
		t.Fatalf("unexpected TLS server names: %#v", created)
	}
	if created.PrimaryOrigin.HTTPVersion != domain.OriginHTTPVersionHTTP2 || created.BackupOrigin.HTTPVersion != domain.OriginHTTPVersionHTTP2 {
		t.Fatalf("unexpected origin HTTP versions: %#v", created)
	}
	if created.PrimaryOrigin.HealthCheckMethod != domain.OriginHealthCheckMethodGET || created.PrimaryOrigin.HealthCheckPath != "/health" ||
		created.BackupOrigin.HealthCheckMethod != domain.OriginHealthCheckMethodHEAD || created.BackupOrigin.HealthCheckPath != "/ready" {
		t.Fatalf("unexpected origin health requests: %#v", created)
	}
	loaded, _, err := database.GetSite(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PrimaryOrigin.TLSServerName != "lax.dustvm.de" || loaded.BackupOrigin == nil || loaded.BackupOrigin.TLSServerName != "backup.dustvm.de" {
		t.Fatalf("stored TLS server names were not preserved: %#v", loaded)
	}
	if loaded.PrimaryOrigin.HealthCheckMethod != domain.OriginHealthCheckMethodGET || loaded.PrimaryOrigin.HealthCheckPath != "/health" ||
		loaded.BackupOrigin.HealthCheckMethod != domain.OriginHealthCheckMethodHEAD || loaded.BackupOrigin.HealthCheckPath != "/ready" {
		t.Fatalf("stored origin health requests were not preserved: %#v", loaded)
	}

	updated := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, map[string]any{
		"name": created.Name, "zone_id": created.ZoneID, "domains": created.Domains, "node_ids": created.Nodes,
		"primary_origin": map[string]any{"url": created.PrimaryOrigin.URL, "host_header": created.PrimaryOrigin.HostHeader, "enabled": true},
		"backup_origin":  map[string]any{"url": created.BackupOrigin.URL, "host_header": created.BackupOrigin.HostHeader, "enabled": true}, "enabled": created.Enabled,
	})
	if updated.PrimaryOrigin.TLSServerName != "lax.dustvm.de" || updated.BackupOrigin == nil || updated.BackupOrigin.TLSServerName != "backup.dustvm.de" {
		t.Fatalf("omitted TLS server names did not preserve the existing values: %#v", updated)
	}
	if updated.PrimaryOrigin.HTTPVersion != domain.OriginHTTPVersionHTTP2 || updated.BackupOrigin.HTTPVersion != domain.OriginHTTPVersionHTTP2 {
		t.Fatalf("omitted origin HTTP versions did not preserve existing values: %#v", updated)
	}
	if updated.PrimaryOrigin.HealthCheckMethod != domain.OriginHealthCheckMethodGET || updated.PrimaryOrigin.HealthCheckPath != "/health" ||
		updated.BackupOrigin.HealthCheckMethod != domain.OriginHealthCheckMethodHEAD || updated.BackupOrigin.HealthCheckPath != "/ready" {
		t.Fatalf("omitted origin health requests did not preserve existing values: %#v", updated)
	}
	cleared := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, map[string]any{
		"name": updated.Name, "zone_id": updated.ZoneID, "domains": updated.Domains, "node_ids": updated.Nodes,
		"primary_origin": map[string]any{"url": updated.PrimaryOrigin.URL, "host_header": updated.PrimaryOrigin.HostHeader, "tls_server_name": "", "enabled": true},
		"backup_origin":  updated.BackupOrigin, "enabled": updated.Enabled,
	})
	if cleared.PrimaryOrigin.TLSServerName != "" {
		t.Fatalf("explicitly empty TLS server name was not cleared: %#v", cleared.PrimaryOrigin)
	}
	movedBackup := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, map[string]any{
		"name": cleared.Name, "zone_id": cleared.ZoneID, "domains": cleared.Domains, "node_ids": cleared.Nodes,
		"primary_origin": cleared.PrimaryOrigin,
		"backup_origin":  map[string]any{"url": "https://203.0.113.22:443", "host_header": cleared.BackupOrigin.HostHeader, "enabled": true}, "enabled": cleared.Enabled,
	})
	if movedBackup.BackupOrigin == nil || movedBackup.BackupOrigin.TLSServerName != "" {
		t.Fatalf("omitted TLS server name was carried to a different backup URL: %#v", movedBackup.BackupOrigin)
	}
	if movedBackup.BackupOrigin.HealthCheckMethod != "" || movedBackup.BackupOrigin.HealthCheckPath != "" {
		t.Fatalf("omitted health request was carried to a different backup URL: %#v", movedBackup.BackupOrigin)
	}

	defaultResponse := requestSiteResponse(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "default-sni", "zone_id": "zone", "domains": []string{"default-sni.example.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	})
	if defaultResponse.Code != http.StatusCreated {
		t.Fatalf("default TLS server name create = %d %s", defaultResponse.Code, defaultResponse.Body.String())
	}
	if strings.Contains(defaultResponse.Body.String(), `"tls_server_name"`) {
		t.Fatalf("empty TLS server name should be omitted from the API response: %s", defaultResponse.Body.String())
	}

	for name, origin := range map[string]map[string]any{
		"plain HTTP": {"url": "http://203.0.113.20:80", "tls_server_name": "lax.dustvm.de", "enabled": true},
		"IP name":    {"url": "https://203.0.113.20:443", "tls_server_name": "203.0.113.20", "enabled": true},
		"wildcard":   {"url": "https://203.0.113.20:443", "tls_server_name": "*.dustvm.de", "enabled": true},
	} {
		t.Run(name, func(t *testing.T) {
			response := requestSiteResponse(t, server, http.MethodPost, "/api/sites", map[string]any{
				"name": "invalid-" + strings.ReplaceAll(name, " ", "-"), "zone_id": "zone", "domains": []string{"invalid-" + strings.ReplaceAll(name, " ", "-") + ".example.test"}, "node_ids": []string{node.ID},
				"primary_origin": origin, "enabled": true,
			})
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid TLS server name = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSitePassthroughAPICompatibilityAndCacheInvalidation(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	defaultSite := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "cached", "zone_id": "zone", "domains": []string{"cached.example.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	})
	if defaultSite.Passthrough {
		t.Fatalf("omitted passthrough should default to cached mode: %#v", defaultSite)
	}
	invalidBody := []byte(`{"name":"grpc","zone_id":"zone","domains":["grpc.example.test"],"node_ids":["` + node.ID + `"],"primary_origin":{"url":"grpcs://origin.example.test","enabled":true},"passthrough":true,"enabled":true}`)
	invalidRequest := httptest.NewRequest(http.MethodPost, "/api/sites", bytes.NewReader(invalidBody))
	invalidRequest.Header.Set("Content-Type", "application/json")
	invalidRequest.Header.Set("X-CSRF-Token", "csrf-token")
	invalidRequest.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
	invalidResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("gRPC passthrough = %d %s", invalidResponse.Code, invalidResponse.Body.String())
	}

	created := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "stream", "zone_id": "zone", "domains": []string{"stream.example.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "passthrough": true, "enabled": true,
	})
	if !created.Passthrough || created.CacheGeneration != 1 {
		t.Fatalf("unexpected created site: %#v", created)
	}

	updated := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, map[string]any{
		"name": created.Name, "zone_id": created.ZoneID, "domains": created.Domains, "node_ids": created.Nodes,
		"primary_origin": created.PrimaryOrigin, "enabled": created.Enabled,
	})
	if !updated.Passthrough || updated.CacheGeneration != created.CacheGeneration {
		t.Fatalf("omitted passthrough did not preserve value: %#v", updated)
	}

	disabled := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, map[string]any{
		"name": updated.Name, "zone_id": updated.ZoneID, "domains": updated.Domains, "node_ids": updated.Nodes,
		"primary_origin": updated.PrimaryOrigin, "passthrough": false, "enabled": updated.Enabled,
	})
	if disabled.Passthrough || disabled.CacheGeneration != updated.CacheGeneration+1 {
		t.Fatalf("explicitly disabling passthrough did not update cache generation: %#v", disabled)
	}

	passthrough := requestSite(t, server, http.MethodPut, "/api/sites/"+created.ID, map[string]any{
		"name": disabled.Name, "zone_id": disabled.ZoneID, "domains": disabled.Domains, "node_ids": disabled.Nodes,
		"primary_origin": disabled.PrimaryOrigin, "passthrough": true, "enabled": disabled.Enabled,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/sites/"+passthrough.ID+"/invalidate-cache", nil)
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
	request.Header.Set("X-CSRF-Token", "csrf-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("passthrough cache invalidation = %d %s", response.Code, response.Body.String())
	}
	afterInvalidation, _, err := database.GetSite(passthrough.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterInvalidation.CacheGeneration != passthrough.CacheGeneration {
		t.Fatalf("rejected cache invalidation changed generation: before=%d after=%d", passthrough.CacheGeneration, afterInvalidation.CacheGeneration)
	}
}

func TestScopedCacheInvalidationAPIPublishesPrefixRuleAndPrewarmPaths(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-prewarm", "203.0.113.83")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilityCacheControl}); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "prewarm", Domains: []string{"cdn.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin:        domain.Origin{URL: "https://origin.example.test", Enabled: true},
		RequestBodyBuffering: true, OriginResponseBuffering: true, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	certificate, privateKey, notAfter := testCertificate(t, "cdn.example.test")
	if err := publisher.StoreCertificate(site.ID, certificate, privateKey, notAfter); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Store: database, Publisher: publisher,
		Logs: cachePrewarmLogStore{events: []domain.AccessLogEvent{
			{Method: http.MethodGet, Path: "/assets/app.js", Status: 200},
			{Method: http.MethodHead, Path: "/assets/manual.js", Status: 304},
			{Method: http.MethodGet, Path: "/outside.js", Status: 200},
			{Method: http.MethodPost, Path: "/assets/upload", Status: 201},
		}},
	}
	response := requestSiteResponse(t, server, http.MethodPost, "/api/sites/"+site.ID+"/invalidate-cache", map[string]any{
		"scope": "prefix", "value": "/assets/", "prewarm": true,
		"prewarm_paths": []string{"/assets/manual.js"},
	})
	if response.Code != http.StatusAccepted {
		t.Fatalf("scoped invalidation = %d %s", response.Code, response.Body.String())
	}
	loaded, _, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.CacheInvalidations) != 1 || loaded.CacheInvalidations[0].Scope != domain.CacheInvalidationPrefix || loaded.CacheInvalidations[0].Value != "/assets/" || len(loaded.CacheWarmups) != 1 {
		t.Fatalf("persisted cache operation = %#v", loaded)
	}
	paths := loaded.CacheWarmups[0].Paths
	if len(paths) != 2 || paths[0] != "/assets/manual.js" || paths[1] != "/assets/app.js" {
		t.Fatalf("prewarm paths = %#v", paths)
	}
	state, _, err := database.NodeState(node.ID)
	if err != nil || len(state.CacheWarmups) != 1 || state.CacheWarmups[0].ID != loaded.CacheWarmups[0].ID {
		t.Fatalf("published node warmups = %#v, %v", state.CacheWarmups, err)
	}
}

func requestSite(t *testing.T, server *Server, method, path string, input map[string]any) domain.Site {
	t.Helper()
	response := requestSiteResponse(t, server, method, path, input)
	if response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("%s %s = %d %s", method, path, response.Code, response.Body.String())
	}
	var site domain.Site
	if err := json.NewDecoder(response.Body).Decode(&site); err != nil {
		t.Fatal(err)
	}
	return site
}

func requestSiteResponse(t *testing.T, server *Server, method, path string, input map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "csrf-token")
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}
