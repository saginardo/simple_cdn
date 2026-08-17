package control

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/integrations"
	"simple_cdn/internal/nginx"
	"simple_cdn/internal/store"
)

type siteNodeDrainFixture struct {
	database *store.Store
	publish  Publisher
	oldNode  domain.Node
	newNode  domain.Node
	siteID   string
	oldPub   store.SitePublication
}

type publicationChangingDNS struct {
	*MemoryDNS
	onReconcile func() error
}

func (d *publicationChangingDNS) Reconcile(ctx context.Context, zoneID, owner string, desired []integrations.DNSRecord) error {
	if err := d.MemoryDNS.Reconcile(ctx, zoneID, owner, desired); err != nil {
		return err
	}
	if d.onReconcile == nil {
		return nil
	}
	onReconcile := d.onReconcile
	d.onReconcile = nil
	return onReconcile()
}

func newSiteNodeDrainFixture(t *testing.T, oldTTL, newTTL int) siteNodeDrainFixture {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewEncryptionKey()
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	oldNode, err := database.CreateNode("drain-old", "203.0.113.201")
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	newNode, err := database.CreateNode("drain-new", "203.0.113.202")
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	for _, node := range []domain.Node{oldNode, newNode} {
		if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	oldTTLValue := oldTTL
	site, err := database.CreateSite(domain.Site{
		Name: "drain-site", Domains: []string{"drain.example.test"}, Nodes: []string{oldNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		DNSTTLSeconds: &oldTTLValue, Enabled: true,
	}, "zone-drain")
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	certificate, privateKey, notAfter := testCertificate(t, site.Domains...)
	if err := publisher.StoreCertificate(site.ID, certificate, privateKey, notAfter); err != nil {
		database.Close()
		t.Fatal(err)
	}
	initialTask, err := publisher.PublishSite(site.ID)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	initialState, _, err := database.NodeState(oldNode.ID)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Heartbeat(oldNode.ID, initialState.Version, "", &domain.ApplyReport{Version: initialState.Version, Status: domain.ApplySucceeded}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.ReconcilePublishTasks(); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if initialTask.ID == "" {
		database.Close()
		t.Fatal("initial publish task has no ID")
	}
	oldPub, err := database.SitePublication(site.ID)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	draft, zoneID, err := database.GetSite(site.ID)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	newTTLValue := newTTL
	draft.Nodes = []string{newNode.ID}
	draft.DNSTTLSeconds = &newTTLValue
	if _, err := database.UpdateSite(draft, zoneID); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(site.ID); err != nil {
		database.Close()
		t.Fatal(err)
	}
	newState, _, err := database.NodeState(newNode.ID)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Heartbeat(newNode.ID, newState.Version, "", &domain.ApplyReport{Version: newState.Version, Status: domain.ApplySucceeded}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.ReconcilePublishTasks(); err != nil {
		database.Close()
		t.Fatal(err)
	}
	return siteNodeDrainFixture{database: database, publish: publisher, oldNode: oldNode, newNode: newNode, siteID: site.ID, oldPub: oldPub}
}

func markDrainNodeHealthy(t *testing.T, database *store.Store, siteID, nodeID string) {
	t.Helper()
	for range 5 {
		if _, err := database.RecordNodeHealth(nodeID, true, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := database.RecordSiteNodeHealth(siteID, nodeID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSiteNodeDrainSkipsStalePublicationSnapshot(t *testing.T) {
	fixture := newSiteNodeDrainFixture(t, 60, 60)
	defer fixture.database.Close()

	markDrainNodeHealthy(t, fixture.database, fixture.siteID, fixture.newNode.ID)
	nodes, err := fixture.database.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	dns := &MemoryDNS{}
	manager := HealthManager{Server: &Server{Store: fixture.database, DNS: dns, Publisher: fixture.publish}}
	if _, err := manager.reconcileSiteDNSOutcomeForPublication(context.Background(), fixture.oldPub, nodes); err != nil {
		t.Fatal(err)
	}
	if records := dns.Zones[fixture.oldPub.Site.ZoneID]; len(records) != 0 {
		t.Fatalf("stale publication changed DNS records: %#v", records)
	}
	drains, err := fixture.database.ListSiteNodeDrainsForSite(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drains) != 1 || drains[0].DNSReadyAt != nil || drains[0].RemoveAfter != nil {
		t.Fatalf("stale publication started a drain timer: %#v", drains)
	}
}

func TestSiteNodeDrainDoesNotClearDNSForStalePublicationSnapshot(t *testing.T) {
	fixture := newSiteNodeDrainFixture(t, 60, 60)
	defer fixture.database.Close()

	record := integrations.DNSRecord{
		Type:    "A",
		Name:    fixture.oldPub.Site.Domains[0],
		Content: fixture.newNode.PublicIPv4,
	}
	dns := &MemoryDNS{Zones: map[string][]integrations.DNSRecord{
		fixture.oldPub.Site.ZoneID: {record},
	}}
	manager := HealthManager{Server: &Server{Store: fixture.database, DNS: dns, Publisher: fixture.publish}}
	if err := manager.clearSiteDNS(context.Background(), fixture.oldPub.Site, &fixture.oldPub); err != nil {
		t.Fatal(err)
	}
	if records := dns.Zones[fixture.oldPub.Site.ZoneID]; len(records) != 1 || records[0] != record {
		t.Fatalf("stale publication cleared current DNS records: %#v", records)
	}
	drains, err := fixture.database.ListSiteNodeDrainsForSite(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drains) != 1 || drains[0].DNSReadyAt != nil || drains[0].RemoveAfter != nil {
		t.Fatalf("stale publication started a drain timer: %#v", drains)
	}
}

func TestSiteNodeDrainDoesNotMarkNewDrainWhenPublicationChangesDuringDNS(t *testing.T) {
	fixture := newSiteNodeDrainFixture(t, 60, 60)
	defer fixture.database.Close()

	thirdNode, err := fixture.database.CreateNode("drain-third", "203.0.113.203")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.SetNodeStatus(thirdNode.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	markDrainNodeHealthy(t, fixture.database, fixture.siteID, fixture.newNode.ID)
	nodes, err := fixture.database.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	publication, err := fixture.database.SitePublication(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	dns := &publicationChangingDNS{MemoryDNS: &MemoryDNS{}}
	dns.onReconcile = func() error {
		draft, zoneID, err := fixture.database.GetSite(fixture.siteID)
		if err != nil {
			return err
		}
		draft.Nodes = []string{thirdNode.ID}
		if _, err := fixture.database.UpdateSite(draft, zoneID); err != nil {
			return err
		}
		_, err = fixture.publish.PublishSite(fixture.siteID)
		return err
	}
	manager := HealthManager{Server: &Server{Store: fixture.database, DNS: dns, Publisher: fixture.publish}}
	if _, err := manager.reconcileSiteDNSOutcomeForPublication(context.Background(), publication, nodes); err != nil {
		t.Fatal(err)
	}
	if records := dns.Zones[publication.Site.ZoneID]; len(records) != 1 || records[0].Content != fixture.newNode.PublicIPv4 {
		t.Fatalf("DNS provider did not receive the original snapshot: %#v", records)
	}
	drains, err := fixture.database.ListSiteNodeDrainsForSite(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drains) != 2 {
		t.Fatalf("publication change did not retain both drain snapshots: %#v", drains)
	}
	for _, drain := range drains {
		if drain.DNSReadyAt != nil || drain.RemoveAfter != nil {
			t.Fatalf("stale DNS call marked drain %s ready: %#v", drain.ID, drain)
		}
	}
	current, err := fixture.database.SitePublication(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Site.Nodes) != 1 || current.Site.Nodes[0] != thirdNode.ID {
		t.Fatalf("concurrent publication did not become current: %#v", current.Site.Nodes)
	}
}

func TestSiteNodeDrainStartsAfterDNSCutoverAndUsesConservativeTTL(t *testing.T) {
	fixture := newSiteNodeDrainFixture(t, 300, 60)
	defer fixture.database.Close()
	dns := &MemoryDNS{}
	manager := HealthManager{Server: &Server{Store: fixture.database, DNS: dns, Publisher: fixture.publish}}
	markDrainNodeHealthy(t, fixture.database, fixture.siteID, fixture.newNode.ID)
	nodes, err := fixture.database.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	publication, err := fixture.database.SitePublication(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	if err := manager.reconcileSiteDNS(context.Background(), publication.Site, nodes); err != nil {
		t.Fatal(err)
	}
	drains, err := fixture.database.ListSiteNodeDrainsForSite(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drains) != 1 || drains[0].DNSReadyAt == nil || drains[0].RemoveAfter == nil {
		t.Fatalf("DNS reconciliation did not start the drain clock: %#v", drains)
	}
	if drains[0].DNSReadyAt.Before(started) {
		t.Fatalf("drain clock started before DNS reconciliation: %s < %s", drains[0].DNSReadyAt, started)
	}
	if got, want := drains[0].RemoveAfter.Sub(*drains[0].DNSReadyAt), 10*time.Minute; got != want {
		t.Fatalf("drain window = %s, want %s (old TTL + 5 minute grace)", got, want)
	}
	if records := dns.Zones[publication.Site.ZoneID]; len(records) != 1 || records[0].Content != fixture.newNode.PublicIPv4 {
		t.Fatalf("DNS did not switch to the new node: %#v", records)
	}
}

func TestSiteNodeDrainDoesNotStartWhenDNSCutoverFails(t *testing.T) {
	fixture := newSiteNodeDrainFixture(t, 60, 60)
	defer fixture.database.Close()
	manager := HealthManager{Server: &Server{Store: fixture.database, DNS: &failingHealthDNS{}, Publisher: fixture.publish}}
	markDrainNodeHealthy(t, fixture.database, fixture.siteID, fixture.newNode.ID)
	nodes, err := fixture.database.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	publication, err := fixture.database.SitePublication(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSiteDNS(context.Background(), publication.Site, nodes); err == nil {
		t.Fatal("expected the DNS cutover to fail")
	}
	drains, err := fixture.database.ListSiteNodeDrainsForSite(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drains) != 1 || drains[0].DNSReadyAt != nil || drains[0].RemoveAfter != nil {
		t.Fatalf("failed DNS cutover started the drain clock: %#v", drains)
	}
}

func TestSiteNodeDrainUsesHistoricalGlobalTTL(t *testing.T) {
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
	if err := database.SaveDNSDefaultTTL(300); err != nil {
		t.Fatal(err)
	}
	oldNode, err := database.CreateNode("historical-ttl-old", "203.0.113.211")
	if err != nil {
		t.Fatal(err)
	}
	newNode, err := database.CreateNode("historical-ttl-new", "203.0.113.212")
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []domain.Node{oldNode, newNode} {
		if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
			t.Fatal(err)
		}
	}
	site, err := database.CreateSite(domain.Site{
		Name: "historical-global-ttl", Domains: []string{"historical-ttl.example.test"}, Nodes: []string{oldNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-historical-ttl")
	if err != nil {
		t.Fatal(err)
	}
	certificate, privateKey, notAfter := testCertificate(t, site.Domains...)
	if err := publisher.StoreCertificate(site.ID, certificate, privateKey, notAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(site.ID); err != nil {
		t.Fatal(err)
	}
	oldState, _, err := database.NodeState(oldNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(oldNode.ID, oldState.Version, "", &domain.ApplyReport{Version: oldState.Version, Status: domain.ApplySucceeded}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkSiteNodeDrainsDNSReadyWithTTLs(site.ID, time.Now().UTC(), 300, 300, siteNodeDrainGrace); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveDNSDefaultTTL(60); err != nil {
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
	newState, _, err := database.NodeState(newNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(newNode.ID, newState.Version, "", &domain.ApplyReport{Version: newState.Version, Status: domain.ApplySucceeded}); err != nil {
		t.Fatal(err)
	}
	readyAt := time.Now().UTC()
	if _, err := database.MarkSiteNodeDrainsDNSReadyWithTTLs(site.ID, readyAt, 60, 300, siteNodeDrainGrace); err != nil {
		t.Fatal(err)
	}
	publication, err := database.SitePublication(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if publication.DNSTTLSeconds != 60 {
		t.Fatalf("recorded current provider TTL = %d, want 60", publication.DNSTTLSeconds)
	}
	drains, err := database.ListSiteNodeDrainsForSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drains) != 1 || drains[0].DNSTTLSeconds != 300 || drains[0].DNSReadyAt == nil || drains[0].RemoveAfter == nil {
		t.Fatalf("historical global TTL was not retained: %#v", drains)
	}
	if got, want := drains[0].RemoveAfter.Sub(*drains[0].DNSReadyAt), 10*time.Minute; got != want {
		t.Fatalf("historical global TTL drain window = %s, want %s", got, want)
	}
}

func TestExpiredSiteNodeDrainRequiresEdgeConfirmationAndRetries(t *testing.T) {
	fixture := newSiteNodeDrainFixture(t, 60, 60)
	defer fixture.database.Close()
	readyAt := time.Now().UTC().Add(-10 * time.Minute)
	if _, err := fixture.database.MarkSiteNodeDrainsDNSReady(fixture.siteID, readyAt, 60, siteNodeDrainGrace); err != nil {
		t.Fatal(err)
	}
	oldBefore, _, err := fixture.database.NodeState(fixture.oldNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.publish.ReconcileDrains(); err != nil {
		t.Fatal(err)
	}
	oldAfter, _, err := fixture.database.NodeState(fixture.oldNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if oldAfter.Version <= oldBefore.Version || nginx.HasSiteHealth(oldAfter.NginxConfig, fixture.siteID) {
		t.Fatalf("expired drain was not removed from desired state: before=%d after=%d", oldBefore.Version, oldAfter.Version)
	}
	drains, err := fixture.database.ListSiteNodeDrainsForSite(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drains) != 1 || drains[0].CleanupTaskID == "" {
		t.Fatalf("drain row was not held for edge confirmation: %#v", drains)
	}
	cleanupTask, err := fixture.database.GetTask(drains[0].CleanupTaskID)
	if err != nil || cleanupTask.Status != domain.TaskApplying {
		t.Fatalf("cleanup task = %#v, err=%v", cleanupTask, err)
	}
	if err := fixture.publish.PublishAll(); err != nil {
		t.Fatal(err)
	}
	oldDuringCleanup, _, err := fixture.database.NodeState(fixture.oldNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if nginx.HasSiteHealth(oldDuringCleanup.NginxConfig, fixture.siteID) {
		t.Fatal("a concurrent full rebuild re-added an actively cleaning drain")
	}
	if err := fixture.database.Heartbeat(fixture.oldNode.ID, oldBefore.Version, "", &domain.ApplyReport{Version: oldAfter.Version, Status: domain.ApplyFailed, Code: "nginx_config_invalid"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.ReconcilePublishTasks(); err != nil {
		t.Fatal(err)
	}
	drains, err = fixture.database.ListSiteNodeDrainsForSite(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drains) != 1 || drains[0].CleanupTaskID != "" {
		t.Fatalf("failed edge cleanup discarded the drain snapshot: %#v", drains)
	}
	if err := fixture.publish.ReconcileDrains(); err != nil {
		t.Fatal(err)
	}
	drains, err = fixture.database.ListSiteNodeDrainsForSite(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drains) != 1 || drains[0].CleanupTaskID == "" {
		t.Fatalf("drain cleanup was not retried: %#v", drains)
	}
	if err := fixture.database.Heartbeat(fixture.oldNode.ID, oldAfter.Version, "", &domain.ApplyReport{Version: oldAfter.Version, Status: domain.ApplySucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.ReconcilePublishTasks(); err != nil {
		t.Fatal(err)
	}
	drains, err = fixture.database.ListSiteNodeDrainsForSite(fixture.siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drains) != 0 {
		t.Fatalf("drain snapshot remained after confirmed cleanup: %#v", drains)
	}
}

func TestConcurrentSiteNodeDrainsOnSameEdgeStayRemovedAcrossCleanupTasks(t *testing.T) {
	fixture := newSiteNodeDrainFixture(t, 60, 60)
	defer fixture.database.Close()

	ttl := 60
	secondSite, err := fixture.database.CreateSite(domain.Site{
		Name: "second-drain-site", Domains: []string{"second-drain.example.test"}, Nodes: []string{fixture.oldNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		DNSTTLSeconds: &ttl, Enabled: true,
	}, "zone-second-drain")
	if err != nil {
		t.Fatal(err)
	}
	certificate, privateKey, notAfter := testCertificate(t, secondSite.Domains...)
	if err := fixture.publish.StoreCertificate(secondSite.ID, certificate, privateKey, notAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.publish.PublishSite(secondSite.ID); err != nil {
		t.Fatal(err)
	}
	oldState, _, err := fixture.database.NodeState(fixture.oldNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.Heartbeat(fixture.oldNode.ID, oldState.Version, "", &domain.ApplyReport{Version: oldState.Version, Status: domain.ApplySucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.ReconcilePublishTasks(); err != nil {
		t.Fatal(err)
	}

	secondDraft, zoneID, err := fixture.database.GetSite(secondSite.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondDraft.Nodes = []string{fixture.newNode.ID}
	if _, err := fixture.database.UpdateSite(secondDraft, zoneID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.publish.PublishSite(secondSite.ID); err != nil {
		t.Fatal(err)
	}
	newState, _, err := fixture.database.NodeState(fixture.newNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.Heartbeat(fixture.newNode.ID, newState.Version, "", &domain.ApplyReport{Version: newState.Version, Status: domain.ApplySucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.ReconcilePublishTasks(); err != nil {
		t.Fatal(err)
	}

	readyAt := time.Now().UTC().Add(-10 * time.Minute)
	for _, siteID := range []string{fixture.siteID, secondSite.ID} {
		if _, err := fixture.database.MarkSiteNodeDrainsDNSReady(siteID, readyAt, ttl, siteNodeDrainGrace); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.publish.ReconcileDrains(); err != nil {
		t.Fatal(err)
	}
	cleanedState, _, err := fixture.database.NodeState(fixture.oldNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, siteID := range []string{fixture.siteID, secondSite.ID} {
		if nginx.HasSiteHealth(cleanedState.NginxConfig, siteID) {
			t.Fatalf("cleanup of another site re-added %s to the old edge", siteID)
		}
		drains, err := fixture.database.ListSiteNodeDrainsForSite(siteID)
		if err != nil {
			t.Fatal(err)
		}
		if len(drains) != 1 || drains[0].CleanupTaskID == "" {
			t.Fatalf("site %s drain was not waiting for edge confirmation: %#v", siteID, drains)
		}
	}

	if err := fixture.database.Heartbeat(fixture.oldNode.ID, cleanedState.Version, "", &domain.ApplyReport{Version: cleanedState.Version, Status: domain.ApplySucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.ReconcilePublishTasks(); err != nil {
		t.Fatal(err)
	}
	for _, siteID := range []string{fixture.siteID, secondSite.ID} {
		drains, err := fixture.database.ListSiteNodeDrainsForSite(siteID)
		if err != nil {
			t.Fatal(err)
		}
		if len(drains) != 0 {
			t.Fatalf("site %s drain remained after the shared edge confirmed cleanup: %#v", siteID, drains)
		}
	}
}
