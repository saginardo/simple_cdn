package store

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"simple_cdn/internal/domain"
)

func TestControlSettingsDefaultsAndOverrides(t *testing.T) {
	const logo = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	path := filepath.Join(t.TempDir(), "control.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	settings, err := database.ControlSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.DNSDefaultTTLSeconds != domain.DefaultDNSTTLSeconds || settings.CacheDefaultSizeGB != domain.DefaultCacheMaxSizeGB || settings.SMTP.Override {
		t.Fatalf("unexpected defaults: %#v", settings)
	}
	if settings.BackupOverride || settings.Backup.Region != domain.DefaultBackupRegion || settings.Backup.BackupTime != domain.DefaultBackupTime {
		t.Fatalf("unexpected backup defaults: %#v", settings.Backup)
	}
	if settings.Branding != domain.DefaultBrandingSettings() {
		t.Fatalf("unexpected branding defaults: %#v", settings.Branding)
	}
	branding := domain.BrandingSettings{Name: "DustK CDN", Subtitle: "运营面板", LogoDataURL: logo}
	if err := database.SaveBrandingSettings(branding); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveDNSDefaultTTL(120); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveDNSDefaultTTL(301); err == nil {
		t.Fatal("accepted DNS TTL above maximum")
	}
	if err := database.SaveCacheDefaultSizeGB(8); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveCacheDefaultSizeGB(0); err == nil {
		t.Fatal("accepted cache default below minimum")
	}
	smtp := SMTPSettings{Enabled: true, Host: "smtp.example.test", Port: 465, Username: "mailer", FromAddress: "cdn@example.test", Recipients: []string{"ops@example.test"}, NotificationCategories: []string{"monitoring", "backup"}, Security: "tls"}
	if err := database.SaveSMTPSettings(smtp, []byte("encrypted-password"), true); err != nil {
		t.Fatal(err)
	}
	settings, err = database.ControlSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.DNSDefaultTTLSeconds != 120 || settings.CacheDefaultSizeGB != 8 || settings.Branding != branding || !settings.SMTP.Override || settings.SMTP.Host != smtp.Host || len(settings.SMTP.Recipients) != 1 || len(settings.SMTP.NotificationCategories) != 2 || settings.SMTP.NotificationCategories[0] != "monitoring" {
		t.Fatalf("unexpected saved settings: %#v", settings)
	}
	stored, err := database.Secret(SecretSMTPPassword)
	if err != nil || !bytes.Equal(stored, []byte("encrypted-password")) {
		t.Fatalf("stored SMTP secret = %q, %v", stored, err)
	}
	if err := database.ClearSMTPSettings(); err != nil {
		t.Fatal(err)
	}
	settings, err = database.ControlSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.SMTP.Override {
		t.Fatalf("SMTP override not cleared: %#v", settings.SMTP)
	}
	if _, err := database.Secret(SecretSMTPPassword); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SMTP password remains after reset: %v", err)
	}
	backup := domain.BackupSettings{Repository: "s3:https://account.r2.cloudflarestorage.com/backup", AccessKeyID: "access", Region: "auto", BackupTime: "04:10", RandomDelaySeconds: 600}
	if err := database.SaveBackupSettings(backup, []byte("encrypted-access-key"), true, []byte("encrypted-password"), true); err != nil {
		t.Fatal(err)
	}
	settings, err = database.ControlSettings()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.BackupOverride || settings.Backup != backup {
		t.Fatalf("saved backup settings = %#v", settings.Backup)
	}
	if secret, err := database.Secret(SecretBackupAccessKey); err != nil || !bytes.Equal(secret, []byte("encrypted-access-key")) {
		t.Fatalf("stored backup access key = %q, %v", secret, err)
	}
	snapshot, err := database.BackupSettingsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Override || snapshot.Settings != backup || !bytes.Equal(snapshot.AccessKeyCiphertext, []byte("encrypted-access-key")) || !bytes.Equal(snapshot.PasswordCiphertext, []byte("encrypted-password")) {
		t.Fatalf("backup settings snapshot = %#v", snapshot)
	}
	if err := database.ClearBackupSettings(); err != nil {
		t.Fatal(err)
	}
	settings, err = database.ControlSettings()
	if err != nil || settings.BackupOverride {
		t.Fatalf("cleared backup settings = %#v, %v", settings.Backup, err)
	}
	for _, name := range []string{SecretBackupAccessKey, SecretBackupPassword} {
		if _, err := database.Secret(name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("backup secret %s remains after reset: %v", name, err)
		}
	}
}

func TestOpenImmutableReadsWALSnapshotWithoutCreatingSidecars(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "control.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	wantVersion, err := database.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}

	snapshot, err := OpenImmutable(path)
	if err != nil {
		t.Fatal(err)
	}
	gotVersion, err := snapshot.SchemaVersion()
	closeErr := snapshot.Close()
	if err != nil || closeErr != nil || gotVersion != wantVersion {
		t.Fatalf("immutable schema version = %d, query error = %v, close error = %v", gotVersion, err, closeErr)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("immutable open created %s: %v", filepath.Base(path+suffix), err)
		}
	}
}

func TestReadSecretUsesLiveReadOnlyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.SetSecret(SecretCloudflareAPIToken, []byte("ciphertext")); err != nil {
		t.Fatal(err)
	}
	var readOnlyPaths []string
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(candidate); err == nil {
			if err := os.Chmod(candidate, 0o444); err != nil {
				t.Fatal(err)
			}
			readOnlyPaths = append(readOnlyPaths, candidate)
		}
	}
	defer func() {
		for _, candidate := range readOnlyPaths {
			_ = os.Chmod(candidate, 0o644)
		}
	}()
	loaded, err := ReadSecret(path, SecretCloudflareAPIToken)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, []byte("ciphertext")) {
		t.Fatalf("read-only secret = %q", loaded)
	}
}

func TestSiteDNSTTLIsolatedByPublishedSnapshot(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge", "203.0.113.90")
	if err != nil {
		t.Fatal(err)
	}
	ttl := 180
	site, err := database.CreateSite(domain.Site{
		Name: "ttl-site", Domains: []string{"ttl.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true, DNSTTLSeconds: &ttl,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	if site.DNSTTLSeconds == nil || *site.DNSTTLSeconds != 180 {
		t.Fatalf("created TTL = %#v", site.DNSTTLSeconds)
	}
	if _, err := database.MarkSitePublished(site.ID); err != nil {
		t.Fatal(err)
	}
	draft, zoneID, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	updatedTTL := 300
	draft.DNSTTLSeconds = &updatedTTL
	if _, err := database.UpdateSite(draft, zoneID); err != nil {
		t.Fatal(err)
	}
	publication, err := database.SitePublication(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if publication.Site.DNSTTLSeconds == nil || *publication.Site.DNSTTLSeconds != 180 {
		t.Fatalf("draft TTL leaked into publication: %#v", publication.Site.DNSTTLSeconds)
	}
}

func TestTCPForwardsAreIsolatedByPublishedSnapshot(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("mail-edge", "203.0.113.91")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "mail", Domains: []string{"mail.example.test"}, Nodes: []string{node.ID}, TCPOnly: true, Enabled: true,
		TCPForwards: []domain.TCPForward{{Name: "SMTPS", ListenPort: 9465, ListenTLS: true, UpstreamHost: "mail.example.test", UpstreamPort: 465, UpstreamTLS: true}},
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
	draft.TCPForwards[0].ListenPort = 9993
	if _, err := database.UpdateSite(draft, zoneID); err != nil {
		t.Fatal(err)
	}
	publication, err := database.SitePublication(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !publication.Site.TCPOnly || len(publication.Site.TCPForwards) != 1 || publication.Site.TCPForwards[0].ListenPort != 9465 {
		t.Fatalf("TCP draft leaked into publication: %#v", publication.Site)
	}
}

func TestNodeCapabilitiesAreReplacedByEachHeartbeatAdvertisement(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge", "203.0.113.92")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilityTCPStream, domain.EdgeCapabilityTCPStream}); err != nil {
		t.Fatal(err)
	}
	node, err = database.GetNode(node.ID)
	if err != nil || len(node.Capabilities) != 1 || node.Capabilities[0] != domain.EdgeCapabilityTCPStream {
		t.Fatalf("stored capabilities = %#v, %v", node.Capabilities, err)
	}
	if err := database.SetNodeCapabilities(node.ID, nil); err != nil {
		t.Fatal(err)
	}
	node, err = database.GetNode(node.ID)
	if err != nil || len(node.Capabilities) != 0 {
		t.Fatalf("cleared capabilities = %#v, %v", node.Capabilities, err)
	}
}

func TestLegacyNodeStateMigrationPreservesHTTPPortFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE node_states (
		node_id TEXT PRIMARY KEY,
		version INTEGER NOT NULL,
		nginx_config TEXT NOT NULL,
		certificate_ciphertext BLOB,
		private_key_ciphertext BLOB,
		updated_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO node_states(node_id, version, nginx_config, updated_at) VALUES ('legacy-node', 7, 'legacy HTTP config', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	state, _, err := database.NodeState("legacy-node")
	if err != nil {
		t.Fatal(err)
	}
	if state.PublicPorts != nil {
		t.Fatalf("legacy public ports = %#v, want nil fallback", state.PublicPorts)
	}
}
