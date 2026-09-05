package control

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func TestListSitesIncludesActivePublishTask(t *testing.T) {
	_, server, site := newSitesListTestEnv(t)

	publishResponse := requestSiteResponse(t, server, http.MethodPost, "/api/sites/"+site.ID+"/publish", nil)
	if publishResponse.Code != http.StatusAccepted {
		t.Fatalf("publish = %d %s", publishResponse.Code, publishResponse.Body.String())
	}
	item := findSiteListItem(t, listSiteItems(t, server), site.ID)
	// The publication is committed synchronously, so the site is published
	// while the task still waits for edge confirmations — the list must carry
	// the task so it can show "publishing" instead of a bare "published".
	if !item.Published {
		t.Fatal("site not marked published after publish")
	}
	if item.LatestTask == nil || item.LatestTask.Status != domain.TaskApplying {
		t.Fatalf("latest task = %#v, want applying", item.LatestTask)
	}
}

func TestListSitesDropsPublishTaskStaleToSiteChanges(t *testing.T) {
	database, server, site := newSitesListTestEnv(t)

	if _, _, err := database.CreateOrGetActivePublishTask(site.ID, time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	item := findSiteListItem(t, listSiteItems(t, server), site.ID)
	if item.LatestTask == nil {
		t.Fatal("latest task omitted before the site changed")
	}

	requestSite(t, server, http.MethodPut, "/api/sites/"+site.ID, map[string]any{
		"name": site.Name, "domains": site.Domains, "node_ids": site.Nodes,
		"primary_origin": site.PrimaryOrigin, "enabled": site.Enabled,
	})
	item = findSiteListItem(t, listSiteItems(t, server), site.ID)
	if item.LatestTask != nil {
		t.Fatalf("latest task after site change = %#v, want omitted", item.LatestTask)
	}
}

func newSitesListTestEnv(t *testing.T) (*store.Store, *Server, domain.Site) {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-list", "203.0.113.94")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilityTCPStream}); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
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
	server := &Server{Store: database, Publisher: Publisher{Store: database, Cipher: cipher}}
	site := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "publish list", "zone_id": "zone", "domains": []string{"list.example.test"},
		"node_ids": []string{node.ID}, "tcp_only": true,
		"tcp_forwards": []map[string]any{{
			"name": "TCP", "listen_port": 8080, "upstream_host": "origin.example.test", "upstream_port": 80,
		}},
		"enabled": true,
	})
	return database, server, site
}

func listSiteItems(t *testing.T, server *Server) []siteListItem {
	t.Helper()
	response := requestSiteResponse(t, server, http.MethodGet, "/api/sites", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/sites = %d %s", response.Code, response.Body.String())
	}
	// siteListItem inherits Site's custom UnmarshalJSON, which would discard
	// latest_task, so decode the site and the task separately.
	var raws []map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&raws); err != nil {
		t.Fatal(err)
	}
	items := make([]siteListItem, 0, len(raws))
	for _, raw := range raws {
		var item siteListItem
		if taskJSON, found := raw["latest_task"]; found && string(taskJSON) != "null" {
			task := new(domain.DeploymentTask)
			if err := json.Unmarshal(taskJSON, task); err != nil {
				t.Fatal(err)
			}
			item.LatestTask = task
		}
		delete(raw, "latest_task")
		siteJSON, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(siteJSON, &item.Site); err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	return items
}

func findSiteListItem(t *testing.T, items []siteListItem, siteID string) siteListItem {
	t.Helper()
	for _, item := range items {
		if item.ID == siteID {
			return item
		}
	}
	t.Fatalf("site %s missing from list", siteID)
	return siteListItem{}
}
