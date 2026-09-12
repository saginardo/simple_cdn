package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

type rolloutTransport func(*http.Request) (*http.Response, error)

func (f rolloutTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpgradeRolloutHealthGateFailureResumeAndRestart(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	unhealthy := false
	probes := 0
	newServer := func() *Server {
		s := &Server{Store: database, EdgeControlURL: "https://control.test:8443", EdgeBinaryURL: "https://control.test/edge", EdgeBinarySHA256: strings.Repeat("2", 64), NginxBundleURL: "https://control.test/nginx", NginxBundleSHA256: strings.Repeat("4", 64), NginxVersion: "1.30.4"}
		s.HealthManager = &HealthManager{Server: s, Client: &http.Client{Transport: rolloutTransport(func(r *http.Request) (*http.Response, error) {
			probes++
			code, body := 200, "ok"
			if unhealthy {
				code, body = 503, "unavailable"
			}
			return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		return s
	}
	server := newServer()
	var nodes []domain.Node
	for i, ip := range []string{"203.0.113.120", "203.0.113.121", "203.0.113.122"} {
		node, e := database.CreateNode([]string{"canary", "next", "last"}[i], ip)
		if e != nil {
			t.Fatal(e)
		}
		nodes = append(nodes, node)
		if e = database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilityOnlineUpgrade, domain.EdgeCapabilityNginxBundle}); e != nil {
			t.Fatal(e)
		}
		if e = database.HeartbeatWithArtifacts(node.ID, 0, "", nil, "v0", strings.Repeat("1", 64), "1.30.3", strings.Repeat("3", 64), ""); e != nil {
			t.Fatal(e)
		}
	}
	response := httptest.NewRecorder()
	server.startAllNodeUpgrades(response, httptest.NewRequest(http.MethodPost, "/api/nodes/upgrade-all", strings.NewReader(`{"max_parallel":2,"health_window_seconds":30,"canary_node_id":"`+nodes[0].ID+`"}`)))
	if response.Code != http.StatusAccepted {
		t.Fatal(response.Code, response.Body.String())
	}
	var result nodeUpgradeAllResponse
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	rollout := *result.Rollout
	if rollout.Members[0].NodeID != nodes[0].ID || rollout.Members[0].State != "upgrading" {
		t.Fatalf("canary: %#v", rollout)
	}
	tick := func() {
		t.Helper()
		if e := server.reconcileUpgradeRollout(context.Background(), time.Now().UTC()); e != nil {
			t.Fatal(e)
		}
		rollout, err = database.LatestUpgradeRollout()
		if err != nil {
			t.Fatal(err)
		}
	}
	completeInstall := func(member domain.NodeUpgradeRolloutMember) {
		t.Helper()
		_, e := database.RecordNodeUpgradeReport(member.NodeID, domain.NodeUpgradeReport{TaskID: member.TaskID, Status: domain.NodeUpgradeSucceeded, InstalledSHA256: server.EdgeBinarySHA256, InstalledNginxSHA256: server.NginxBundleSHA256})
		if e != nil {
			t.Fatal(e)
		}
	}
	heartbeat := func(member domain.NodeUpgradeRolloutMember) {
		t.Helper()
		if e := database.HeartbeatWithArtifacts(member.NodeID, 0, "", nil, "v1", server.EdgeBinarySHA256, "1.30.4", server.NginxBundleSHA256, ""); e != nil {
			t.Fatal(e)
		}
	}
	completeInstall(rollout.Members[0])
	tick()
	if rollout.Members[0].HealthySince != nil || probes != 0 {
		t.Fatal("accepted heartbeat from before install")
	}
	heartbeat(rollout.Members[0])
	task, e := database.NodeUpgradeTask(rollout.Members[0].TaskID)
	if e != nil {
		t.Fatal(e)
	}
	if ready, detail, e := server.upgradeRolloutNodeReady(context.Background(), nodes[0].ID, task, time.Now().UTC().Add(45*time.Second)); e != nil || !ready {
		t.Fatalf("normal heartbeat interval rejected: %s %v", detail, e)
	}
	if ready, _, e := server.upgradeRolloutNodeReady(context.Background(), nodes[0].ID, task, time.Now().UTC().Add(91*time.Second)); e != nil || ready {
		t.Fatalf("stale heartbeat accepted: %v", e)
	}
	unhealthy = true
	tick()
	if rollout.State != "paused" || rollout.Members[0].State != "failed" {
		t.Fatalf("probe failure did not pause: %#v", rollout)
	}
	for _, member := range rollout.Members[1:] {
		if member.TaskID != "" {
			t.Fatal("canary failure dispatched followers")
		}
	}
	oldTask := rollout.Members[0].TaskID
	if _, err = database.ChangeUpgradeRollout(rollout.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	unhealthy = false
	tick()
	tick()
	if rollout.Members[0].State != "verifying" || rollout.Members[0].TaskID != oldTask || rollout.Members[0].HealthySince == nil {
		t.Fatalf("health retry reinstalled: %#v", rollout)
	}
	// Persist an interrupted window. Restart must not count unobserved downtime.
	past := time.Now().UTC().Add(-time.Minute)
	rollout.Members[0].HealthySince = &past
	rollout.Members[0].LastObservedAt = &past
	if err = database.SaveUpgradeRollout(&rollout); err != nil {
		t.Fatal(err)
	}
	server = newServer()
	tick()
	if rollout.Members[0].State != "verifying" || rollout.Members[0].HealthySince.Before(past.Add(30*time.Second)) {
		t.Fatal("restart counted a stale observation window")
	}
	// A current probe after a completed continuous window releases the next batch.
	old := time.Now().UTC().Add(-31 * time.Second)
	rollout.Members[0].HealthySince = &old
	if err = database.SaveUpgradeRollout(&rollout); err != nil {
		t.Fatal(err)
	}
	tick()
	if rollout.Members[0].State != "succeeded" || rollout.Members[1].State != "upgrading" || rollout.Members[2].State != "upgrading" {
		t.Fatalf("next batch: %#v", rollout)
	}
	for i := 1; i < 3; i++ {
		completeInstall(rollout.Members[i])
		heartbeat(rollout.Members[i])
	}
	tick()
	for i := 1; i < 3; i++ {
		rollout.Members[i].HealthySince = &old
	}
	if err = database.SaveUpgradeRollout(&rollout); err != nil {
		t.Fatal(err)
	}
	tick()
	if rollout.State != "succeeded" {
		t.Fatalf("not complete: %#v", rollout)
	}
}

func TestUpgradeRolloutRejectsInvalidOptions(t *testing.T) {
	for _, body := range []string{`{"max_parallel":0}`, `{"max_parallel":11}`, `{"health_window_seconds":1}`, `{"canary":"unknown"}`} {
		server := &Server{}
		response := httptest.NewRecorder()
		server.startAllNodeUpgrades(response, httptest.NewRequest(http.MethodPost, "/api/nodes/upgrade-all", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d", body, response.Code)
		}
	}
}
