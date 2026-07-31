package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/integrations"
	"simple_cdn/internal/store"
)

func TestPrepareNodeUninstallWithdrawsDNSAndReportsSiteBlockers(t *testing.T) {
	server, database, cookie := newNodeUninstallTestServer(t)
	node, err := database.CreateNode("edge-1", "203.0.113.40")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name:          "customer-site",
		Domains:       []string{"cdn.example.test"},
		Nodes:         []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		Enabled:       true,
	}, "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeDraining); err != nil {
		t.Fatal(err)
	}
	dns := server.DNS.(*MemoryDNS)
	dns.Zones["zone-1"] = []integrations.DNSRecord{
		{Name: "cdn.example.test", Type: "A", Content: node.PublicIPv4, Comment: integrations.ManagedRecordPrefix + "site=" + site.ID + ";node=" + node.ID},
		{Name: "other.example.test", Type: "A", Content: "203.0.113.41", Comment: integrations.ManagedRecordPrefix + "site=" + site.ID + ";node=" + node.ID + "0"},
	}

	response := serveNodeUninstallAdmin(t, server, cookie, http.MethodPost, "/api/nodes/"+node.ID+"/uninstall", nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("prepare response = %d %s", response.Code, response.Body.String())
	}
	var status nodeUninstallStatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Job == nil || status.Job.Status != store.NodeUninstallPreparing || status.ReadyInSeconds <= 0 {
		t.Fatalf("prepare status = %#v", status)
	}
	if len(status.Blockers) != 1 || status.Blockers[0].Code != "still_assigned" || status.Blockers[0].SiteID != site.ID {
		t.Fatalf("prepare blockers = %#v", status.Blockers)
	}
	if records := dns.Zones["zone-1"]; len(records) != 1 || records[0].Comment != integrations.ManagedRecordPrefix+"site="+site.ID+";node="+node.ID+"0" {
		t.Fatalf("DNS records after preparation = %#v", records)
	}

	blocked := serveNodeUninstallAdmin(t, server, cookie, http.MethodPost, "/api/nodes/"+node.ID+"/uninstall/command", nil)
	if blocked.Code != http.StatusConflict || !strings.Contains(blocked.Body.String(), "still_assigned") {
		t.Fatalf("blocked command = %d %s", blocked.Code, blocked.Body.String())
	}
}

func TestCanceledNodeUninstallIsIdleAndCanBePreparedAgain(t *testing.T) {
	server, database, cookie := newNodeUninstallTestServer(t)
	node, err := database.CreateNode("edge-canceled", "203.0.113.42")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name:          "retained-site",
		Domains:       []string{"retained.example.test"},
		Nodes:         []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		Enabled:       true,
	}, "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeDraining); err != nil {
		t.Fatal(err)
	}

	prepared := serveNodeUninstallAdmin(t, server, cookie, http.MethodPost, "/api/nodes/"+node.ID+"/uninstall", nil)
	if prepared.Code != http.StatusCreated {
		t.Fatalf("prepare response = %d %s", prepared.Code, prepared.Body.String())
	}
	canceled := serveNodeUninstallAdmin(t, server, cookie, http.MethodDelete, "/api/nodes/"+node.ID+"/uninstall", nil)
	if canceled.Code != http.StatusOK {
		t.Fatalf("cancel response = %d %s", canceled.Code, canceled.Body.String())
	}
	var canceledStatus nodeUninstallStatusResponse
	if err := json.Unmarshal(canceled.Body.Bytes(), &canceledStatus); err != nil {
		t.Fatal(err)
	}
	if canceledStatus.Job == nil || canceledStatus.Job.Status != store.NodeUninstallCanceled {
		t.Fatalf("canceled job = %#v", canceledStatus.Job)
	}
	if len(canceledStatus.Blockers) != 0 || canceledStatus.CanGenerateCommand || canceledStatus.ReadyInSeconds != 0 {
		t.Fatalf("canceled workflow is still active: %#v", canceledStatus)
	}

	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	retained := serveNodeUninstallAdmin(t, server, cookie, http.MethodGet, "/api/nodes/"+node.ID+"/uninstall", nil)
	var retainedStatus nodeUninstallStatusResponse
	if err := json.Unmarshal(retained.Body.Bytes(), &retainedStatus); err != nil {
		t.Fatal(err)
	}
	if retainedStatus.Node.Status != domain.NodeActive || retainedStatus.Job == nil || retainedStatus.Job.Status != store.NodeUninstallCanceled || len(retainedStatus.Blockers) != 0 {
		t.Fatalf("retained node status = %#v", retainedStatus)
	}

	if err := database.SetNodeStatus(node.ID, domain.NodeDraining); err != nil {
		t.Fatal(err)
	}
	restarted := serveNodeUninstallAdmin(t, server, cookie, http.MethodPost, "/api/nodes/"+node.ID+"/uninstall", nil)
	if restarted.Code != http.StatusCreated {
		t.Fatalf("restart response = %d %s", restarted.Code, restarted.Body.String())
	}
	var restartedStatus nodeUninstallStatusResponse
	if err := json.Unmarshal(restarted.Body.Bytes(), &restartedStatus); err != nil {
		t.Fatal(err)
	}
	if restartedStatus.Job == nil || restartedStatus.Job.Status != store.NodeUninstallPreparing || len(restartedStatus.Blockers) != 1 || restartedStatus.Blockers[0].SiteID != site.ID {
		t.Fatalf("restarted workflow = %#v", restartedStatus)
	}
}

func TestNodeUninstallTreatsPublishedAssignmentAsActiveWhileDraftIsPending(t *testing.T) {
	server, database, cookie := newNodeUninstallTestServer(t)
	oldNode, err := database.CreateNode("edge-old", "203.0.113.43")
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := database.CreateNode("edge-replacement", "203.0.113.44")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "pending-move", Domains: []string{"pending-move.example.test"}, Nodes: []string{oldNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-1")
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
	draft.Nodes = []string{replacement.ID}
	if _, err := database.UpdateSite(draft, zoneID); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(oldNode.ID, domain.NodeDraining); err != nil {
		t.Fatal(err)
	}

	response := serveNodeUninstallAdmin(t, server, cookie, http.MethodPost, "/api/nodes/"+oldNode.ID+"/uninstall", nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("prepare response = %d %s", response.Code, response.Body.String())
	}
	var status nodeUninstallStatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Job == nil || len(status.Blockers) != 1 || status.Blockers[0].Code != "site_not_published" || status.Blockers[0].SiteID != site.ID {
		t.Fatalf("published assignment was not retained as a blocker: %#v", status)
	}
	assigned, err := server.assignedSites(oldNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(assigned) != 1 || assigned[0].ID != site.ID {
		t.Fatalf("published assignment missing from node record guard: %#v", assigned)
	}
}

func TestNodeUninstallCommandCallbacksAndRecordDeletion(t *testing.T) {
	server, database, cookie := newNodeUninstallTestServer(t)
	node, err := database.CreateNode("edge-remove", "203.0.113.50")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCertificate(node.ID, "sha256:edge-remove"); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeDraining); err != nil {
		t.Fatal(err)
	}
	if _, err := database.PrepareNodeUninstall(node.ID, nil, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	commandResponse := serveNodeUninstallAdmin(t, server, cookie, http.MethodPost, "/api/nodes/"+node.ID+"/uninstall/command", nil)
	if commandResponse.Code != http.StatusCreated {
		t.Fatalf("command response = %d %s", commandResponse.Code, commandResponse.Body.String())
	}
	var commandStatus nodeUninstallStatusResponse
	if err := json.Unmarshal(commandResponse.Body.Bytes(), &commandStatus); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commandStatus.UninstallCommand, "https://control.example.test/uninstall-edge.sh") || !strings.Contains(commandStatus.UninstallCommand, "sudo bash -s") {
		t.Fatalf("uninstall command = %q", commandStatus.UninstallCommand)
	}
	for _, expected := range []string{
		`--node-id "` + node.ID + `"`,
		`--node-name 'edge-remove'`,
		`--node-ipv4 "203.0.113.50"`,
	} {
		if !strings.Contains(commandStatus.UninstallCommand, expected) {
			t.Fatalf("uninstall command does not contain %q: %q", expected, commandStatus.UninstallCommand)
		}
	}
	match := regexp.MustCompile(`--token "([^"]+)"`).FindStringSubmatch(commandStatus.UninstallCommand)
	if len(match) != 2 {
		t.Fatalf("token missing from command %q", commandStatus.UninstallCommand)
	}
	token := match[1]

	unauthorized := serveNodeUninstallCallback(server, http.MethodPost, "/api/edge/v1/uninstall/start", "invalid", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("invalid callback = %d %s", unauthorized.Code, unauthorized.Body.String())
	}
	started := serveNodeUninstallCallback(server, http.MethodPost, "/api/edge/v1/uninstall/start", token, nil)
	if started.Code != http.StatusOK {
		t.Fatalf("start callback = %d %s", started.Code, started.Body.String())
	}
	startedNode, err := database.GetNode(node.ID)
	if err != nil || startedNode.Status != domain.NodeUninstalling {
		t.Fatalf("started node = %#v, %v", startedNode, err)
	}
	completed := serveNodeUninstallCallback(server, http.MethodPost, "/api/edge/v1/uninstall/complete", token, nil)
	if completed.Code != http.StatusOK {
		t.Fatalf("complete callback = %d %s", completed.Code, completed.Body.String())
	}
	repeated := serveNodeUninstallCallback(server, http.MethodPost, "/api/edge/v1/uninstall/complete", token, nil)
	if repeated.Code != http.StatusOK {
		t.Fatalf("repeated complete callback = %d %s", repeated.Code, repeated.Body.String())
	}
	statusResponse := serveNodeUninstallAdmin(t, server, cookie, http.MethodGet, "/api/nodes/"+node.ID+"/uninstall", nil)
	var completedStatus nodeUninstallStatusResponse
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &completedStatus); err != nil {
		t.Fatal(err)
	}
	if completedStatus.Node.Status != domain.NodeUninstalled || completedStatus.Job == nil || completedStatus.Job.Status != store.NodeUninstallSucceeded {
		t.Fatalf("completed status = %#v", completedStatus)
	}

	wrongDelete := serveNodeUninstallAdmin(t, server, cookie, http.MethodDelete, "/api/nodes/"+node.ID, map[string]string{"confirmation": "wrong"})
	if wrongDelete.Code != http.StatusBadRequest {
		t.Fatalf("wrong deletion confirmation = %d %s", wrongDelete.Code, wrongDelete.Body.String())
	}
	deleted := serveNodeUninstallAdmin(t, server, cookie, http.MethodDelete, "/api/nodes/"+node.ID, map[string]string{"confirmation": node.Name})
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete response = %d %s", deleted.Code, deleted.Body.String())
	}
}

func TestForceCompleteNodeUninstallRequiresExactNameAndNoBlockers(t *testing.T) {
	server, database, cookie := newNodeUninstallTestServer(t)
	node, err := database.CreateNode("offline-edge", "203.0.113.60")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeRevoked); err != nil {
		t.Fatal(err)
	}
	if _, err := database.PrepareNodeUninstall(node.ID, nil, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	wrong := serveNodeUninstallAdmin(t, server, cookie, http.MethodPost, "/api/nodes/"+node.ID+"/uninstall/force-complete", map[string]string{"confirmation": "OFFLINE-EDGE"})
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("wrong force confirmation = %d %s", wrong.Code, wrong.Body.String())
	}
	forced := serveNodeUninstallAdmin(t, server, cookie, http.MethodPost, "/api/nodes/"+node.ID+"/uninstall/force-complete", map[string]string{"confirmation": node.Name})
	if forced.Code != http.StatusOK {
		t.Fatalf("force response = %d %s", forced.Code, forced.Body.String())
	}
	var status nodeUninstallStatusResponse
	if err := json.Unmarshal(forced.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Node.Status != domain.NodeUninstalled || status.Job == nil || status.Job.Status != store.NodeUninstallForced || !status.Job.Forced {
		t.Fatalf("forced status = %#v", status)
	}
	repeated := serveNodeUninstallAdmin(t, server, cookie, http.MethodPost, "/api/nodes/"+node.ID+"/uninstall/force-complete", map[string]string{"confirmation": node.Name})
	if repeated.Code != http.StatusConflict {
		t.Fatalf("repeated force response = %d %s", repeated.Code, repeated.Body.String())
	}
}

func TestUninstallEdgeScriptIsPublicAndNotCached(t *testing.T) {
	server := &Server{}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/uninstall-edge.sh", nil))
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Body.String(), "#!/usr/bin/env bash") {
		t.Fatalf("script response = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("script cache control = %q", response.Header().Get("Cache-Control"))
	}
}

func newNodeUninstallTestServer(t *testing.T) (*Server, *store.Store, *http.Cookie) {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "uninstall-session", "uninstall-csrf", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Store:      database,
		DNS:        &MemoryDNS{Zones: make(map[string][]integrations.DNSRecord)},
		ControlURL: "https://control.example.test",
	}
	return server, database, &http.Cookie{Name: "cdn_session", Value: "uninstall-session"}
}

func serveNodeUninstallAdmin(t *testing.T, server *Server, cookie *http.Cookie, method, path string, input any) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Reader
	if input == nil {
		body = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, body)
	request.AddCookie(cookie)
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", "uninstall-csrf")
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func serveNodeUninstallCallback(server *Server, method, path, token string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}
