package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func TestPublishOverviewSeparatesIPv6EligibilityFromConfigurationResult(t *testing.T) {
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
	ipv6Node, err := database.CreateNodeWithAddresses("edge-dual-stack", "203.0.113.20", "2001:db8::20")
	if err != nil {
		t.Fatal(err)
	}
	ipv4Node, err := database.CreateNode("edge-ipv4", "203.0.113.21")
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []domain.Node{ipv6Node, ipv4Node} {
		if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
			t.Fatal(err)
		}
	}
	site, err := database.CreateSite(domain.Site{
		Name: "dual stack", Domains: []string{"cdn.example.test"}, Nodes: []string{ipv6Node.ID, ipv4Node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, IPv6Enabled: true, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	task, created, err := database.CreateOrGetActivePublishTask(site.ID, time.Now().Add(time.Minute))
	if err != nil || !created {
		t.Fatalf("create publish task: %#v %t %v", task, created, err)
	}
	if err := database.UpdateTask(task.ID, domain.TaskApplying, "waiting for edge"); err != nil {
		t.Fatal(err)
	}
	updates := []store.NodeStateUpdate{
		{NodeID: ipv6Node.ID, State: domain.DesiredState{Version: 1, NginxConfig: "# dual stack"}},
		{NodeID: ipv4Node.ID, State: domain.DesiredState{Version: 1, NginxConfig: "# IPv4 only"}},
	}
	targets := []store.PublishTaskNode{
		{NodeID: ipv6Node.ID, TargetVersion: 1},
		{NodeID: ipv4Node.ID, TargetVersion: 1},
	}
	if _, err := database.CommitSitePublication(site.ID, site.ConfigVersion, task.ID, updates, targets); err != nil {
		t.Fatal(err)
	}
	report := &domain.ApplyReport{Version: 1, Status: domain.ApplySucceeded, Detail: "configuration applied"}
	for _, node := range []domain.Node{ipv6Node, ipv4Node} {
		if err := database.HeartbeatWithArtifacts(node.ID, 1, "", report, "0.1.53", strings.Repeat("a", 64), "1.27.5", strings.Repeat("b", 64), ""); err != nil {
			t.Fatal(err)
		}
		for range 5 {
			if _, err := database.RecordNodeHealth(node.ID, true, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := database.RecordSiteNodeHealth(site.ID, node.ID, true, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	for range 5 {
		if _, err := database.RecordSiteNodeIPv6Health(site.ID, ipv6Node.ID, true, ""); err != nil {
			t.Fatal(err)
		}
	}

	server := &Server{Store: database}
	request := httptest.NewRequest(http.MethodGet, "/api/publish", nil)
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/publish = %d %s", response.Code, response.Body.String())
	}
	var overview publishOverviewResponse
	if err := json.NewDecoder(response.Body).Decode(&overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Sites) != 1 || overview.Sites[0].Task == nil || overview.Sites[0].Task.Status != domain.TaskSucceeded {
		t.Fatalf("unexpected publish overview: %#v", overview)
	}
	if len(overview.Sites[0].Nodes) != 2 || len(overview.History) != 1 {
		t.Fatalf("unexpected overview counts: %#v", overview)
	}
	byID := make(map[string]publishNodeOverview, 2)
	for _, node := range overview.Sites[0].Nodes {
		byID[node.NodeID] = node
	}
	if !byID[ipv6Node.ID].IPv4DNSEligible || !byID[ipv6Node.ID].IPv6DNSEligible {
		t.Fatalf("dual-stack node eligibility = %#v", byID[ipv6Node.ID])
	}
	if !byID[ipv4Node.ID].IPv4DNSEligible || byID[ipv4Node.ID].IPv6DNSEligible || byID[ipv4Node.ID].ConfigurationStatus != "succeeded" {
		t.Fatalf("IPv4-only node was treated as a publish failure: %#v", byID[ipv4Node.ID])
	}
	if byID[ipv6Node.ID].AgentVersion != "0.1.53" || byID[ipv6Node.ID].NginxVersion != "1.27.5" ||
		byID[ipv6Node.ID].AgentSHA256 == "" || byID[ipv6Node.ID].NginxSHA256 == "" || byID[ipv6Node.ID].DriftReason != "" {
		t.Fatalf("runtime identity or drift = %#v", byID[ipv6Node.ID])
	}
}

func TestPublishNodeDriftReason(t *testing.T) {
	tests := []struct {
		name       string
		node       domain.Node
		desired    int64
		publishing bool
		want       string
	}{
		{name: "inactive", node: domain.Node{Status: domain.NodeDraining}, desired: 2, want: "node_inactive"},
		{name: "missing desired state", node: domain.Node{Status: domain.NodeActive}, want: "desired_state_missing"},
		{name: "version behind", node: domain.Node{Status: domain.NodeActive, AppliedVersion: 1}, desired: 2, want: "version_behind"},
		{name: "publication active", node: domain.Node{Status: domain.NodeActive, AppliedVersion: 2}, desired: 2, publishing: true, want: "publication_active"},
		{name: "aligned", node: domain.Node{Status: domain.NodeActive, AppliedVersion: 2}, desired: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := publishNodeDriftReason(test.node, test.desired, test.publishing); got != test.want {
				t.Fatalf("publishNodeDriftReason() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRetryFailedPublishTargetsOnlyCurrentFailedNodeVersion(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-retry", "203.0.113.22")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "retry", Domains: []string{"retry.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	failedTask, created, err := database.CreateOrGetActivePublishTask(site.ID, time.Now().Add(-time.Second))
	if err != nil || !created {
		t.Fatalf("create failed task: %#v %t %v", failedTask, created, err)
	}
	if err := database.UpdateTask(failedTask.ID, domain.TaskApplying, "waiting for edge"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CommitSitePublication(site.ID, site.ConfigVersion, failedTask.ID,
		[]store.NodeStateUpdate{{NodeID: node.ID, State: domain.DesiredState{Version: 2, NginxConfig: "# retry"}}},
		[]store.PublishTaskNode{{NodeID: node.ID, TargetVersion: 2}}); err != nil {
		t.Fatal(err)
	}
	failedStatus, err := database.PublishStatus(site.ID)
	if err != nil || failedStatus.Task == nil || failedStatus.Task.Status != domain.TaskFailed {
		t.Fatalf("failed status = %#v, %v", failedStatus, err)
	}

	retry, err := (Publisher{Store: database}).RetryFailedPublish(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID == failedTask.ID || retry.Status != domain.TaskApplying {
		t.Fatalf("retry task = %#v", retry)
	}
	retryStatus, err := database.PublishStatus(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retryStatus.Task == nil || retryStatus.Task.ID != retry.ID || len(retryStatus.Nodes) != 1 ||
		retryStatus.Nodes[0].NodeID != node.ID || retryStatus.Nodes[0].TargetVersion != 2 || retryStatus.Nodes[0].Status != domain.PublishNodePending {
		t.Fatalf("retry status = %#v", retryStatus)
	}
}
