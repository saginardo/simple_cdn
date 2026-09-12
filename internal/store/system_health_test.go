package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestSystemHealthInputsUsePublishedStateAndNeverReconcile(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node, err := db.CreateNode("edge", "203.0.113.2")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Heartbeat(node.ID, 4, "", nil); err != nil {
		t.Fatal(err)
	}
	site, err := db.CreateSite(domain.Site{Name: "draft", Domains: []string{"cdn.example.test"}, Nodes: []string{node.ID}, Enabled: true, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := db.db.Exec(query, args...); e != nil {
			t.Fatal(e)
		}
	}
	published := site
	published.Name = "serving"
	published.IPv6Enabled = true
	body, _ := json.Marshal(published)
	exec(`INSERT INTO site_publications(site_id,site_json,certificate_not_after,published_at,private_key_ciphertext) VALUES(?,?,?,?,?)`, site.ID, string(body), stamp(at.Add(24*time.Hour)), stamp(at), []byte("PRIVATE-KEY-SENTINEL"))
	exec(`INSERT INTO certificates(site_id,certificate_ciphertext,private_key_ciphertext,not_after,updated_at) VALUES(?,?,?,?,?)`, site.ID, []byte("cert"), []byte("PRIVATE-KEY-SENTINEL"), stamp(at.Add(90*24*time.Hour)), stamp(at))
	exec(`INSERT INTO node_states(node_id,version,nginx_config,updated_at) VALUES(?,?,?,?)`, node.ID, 5, "events {}", stamp(at))
	exec(`INSERT OR REPLACE INTO node_health(node_id,dns_eligible,last_checked_at) VALUES(?,?,?)`, node.ID, true, stamp(at))
	exec(`INSERT OR REPLACE INTO site_node_health(site_id,node_id,dns_eligible,last_checked_at,ipv6_dns_eligible,ipv6_last_checked_at,ipv6_last_error) VALUES(?,?,?,?,?,?,?)`, site.ID, node.ID, true, stamp(at), false, stamp(at), "IPv6 unavailable")
	for i, kind := range []string{"publish_site", "publish_site", "issue_certificate", "renew_certificate"} {
		exec(`INSERT INTO deployment_tasks(id,kind,site_id,status,deadline_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, string(rune('a'+i)), kind, site.ID, "failed", stamp(at.Add(-time.Hour)), stamp(at), stamp(at))
	}
	exec(`UPDATE deployment_tasks SET status='applying' WHERE id='b'`)
	upgrade, _, err := db.CreateOrGetNodeUpgrade(node.ID, testUpgradeInstruction(strings.Repeat("2", 64)), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE node_upgrade_tasks SET deadline_at=? WHERE id=?`, stamp(at.Add(-time.Hour)), upgrade.ID)
	inputs, err := db.SystemHealthInputs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.Nodes) != 1 || inputs.Nodes[0].AppliedVersion != 4 || inputs.DesiredVersions[node.ID] != 5 {
		t.Fatalf("nodes: %#v", inputs)
	}
	if len(inputs.Sites) != 1 || inputs.Sites[0].Draft.Name != "draft" || inputs.Sites[0].Published.Name != "serving" || !inputs.Sites[0].Published.IPv6Enabled {
		t.Fatalf("sites: %#v", inputs.Sites)
	}
	if !inputs.Sites[0].ServingNotAfter.Equal(at.Add(24*time.Hour)) || !inputs.Sites[0].Certificate.NotAfter.Equal(at.Add(90*24*time.Hour)) {
		t.Fatal("serving and issued certificate expiry were conflated")
	}
	if !inputs.NodeProbes[node.ID].Eligible || len(inputs.SiteProbes) != 2 || !inputs.SiteProbes[0].Eligible || inputs.SiteProbes[1].Eligible || inputs.SiteProbes[1].Error != "IPv6 unavailable" {
		t.Fatalf("probes: %#v", inputs)
	}
	if len(inputs.Tasks) != 2 {
		t.Fatalf("latest tasks: %#v", inputs.Tasks)
	}
	ids := map[string]bool{}
	for _, task := range inputs.Tasks {
		ids[task.ID] = true
		if task.ID == "b" && task.Status != domain.TaskApplying {
			t.Fatal("read reconciled expired task")
		}
	}
	if !ids["b"] || !ids["d"] {
		t.Fatalf("task tie order: %#v", ids)
	}
	if len(inputs.Upgrades) != 1 || inputs.Upgrades[0].Status != domain.NodeUpgradeQueued {
		t.Fatal("read reconciled expired upgrade")
	}
	encoded, _ := json.Marshal(inputs)
	if strings.Contains(string(encoded), "PRIVATE-KEY-SENTINEL") {
		t.Fatal("private key exposed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = db.SystemHealthInputs(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}

func TestSystemHealthHistorySurvivesRestartAndDistinguishesRetirement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	at := time.Now().UTC()
	start := at
	save := func(states ...domain.HealthState) domain.SystemHealthSnapshot {
		t.Helper()
		snapshot := domain.SystemHealthSnapshot{CollectedAt: &at}
		for i, state := range states {
			snapshot.Checks = append(snapshot.Checks, domain.HealthCheck{ID: string(rune('a' + i)), Title: "heartbeat", State: state, NodeIDs: []string{"edge"}, SiteIDs: []string{"site"}, ActionURL: "/nodes/edge"})
		}
		if err := db.SaveSystemHealthSnapshot(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
		result, err := db.SystemHealthSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	save(domain.HealthWarning, domain.HealthUnknown, domain.HealthCritical)
	at = at.Add(time.Minute)
	changed := save(domain.HealthCritical, domain.HealthWarning, domain.HealthCritical)
	if !changed.Checks[0].Since.Equal(start) || len(changed.Recoveries) != 0 {
		t.Fatal("severity transition reset the episode")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at = at.Add(time.Minute)
	result := save(domain.HealthHealthy, domain.HealthDisabled)
	if len(result.Recoveries) != 3 {
		t.Fatalf("history: %#v", result.Recoveries)
	}
	resolved, retired := 0, 0
	for _, event := range result.Recoveries {
		if event.Outcome == "resolved" {
			resolved++
		} else if event.Outcome == "retired" {
			retired++
		}
		if !event.StartedAt.Equal(start) || event.NodeIDs[0] != "edge" {
			t.Fatal("episode evidence lost")
		}
	}
	if resolved != 1 || retired != 2 {
		t.Fatal("removal or disable counted as recovery")
	}
	if err = db.SaveSystemHealthSnapshot(ctx, result); err == nil {
		t.Fatal("accepted non-advancing snapshot")
	}
	at = at.Add(time.Minute)
	invalid := domain.SystemHealthSnapshot{CollectedAt: &at, Checks: []domain.HealthCheck{{ID: "duplicate"}, {ID: "duplicate"}}}
	if err = db.SaveSystemHealthSnapshot(ctx, invalid); err == nil {
		t.Fatal("accepted duplicate checks")
	}
	unchanged, _ := db.SystemHealthSnapshot(ctx)
	if !unchanged.CollectedAt.Equal(*result.CollectedAt) {
		t.Fatal("failed save damaged snapshot")
	}
	for i := 0; i < 105; i++ {
		at = at.Add(time.Minute)
		save(domain.HealthWarning)
		at = at.Add(time.Minute)
		result = save(domain.HealthHealthy)
	}
	if len(result.Recoveries) != 100 {
		t.Fatalf("unbounded history: %d", len(result.Recoveries))
	}
	at = at.AddDate(0, 0, 31)
	result = save(domain.HealthHealthy)
	if len(result.Recoveries) != 0 {
		t.Fatal("expired history retained")
	}
}
