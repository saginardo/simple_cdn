package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestNginxArtifactCatalogPromotesOneImmutableTarget(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	first := testNginxArtifact("1.30.5", strings.Repeat("1", 64))
	saved, created, err := database.SaveNginxArtifactCandidate(first)
	if err != nil || !created || saved.State != domain.NginxArtifactCandidate {
		t.Fatalf("save first candidate = %#v, created=%v, err=%v", saved, created, err)
	}
	promoted, changed, err := database.PromoteNginxArtifact(first.SHA256)
	if err != nil || !changed || promoted.State != domain.NginxArtifactCurrent || promoted.PromotedAt == nil {
		t.Fatalf("promote first = %#v, changed=%v, err=%v", promoted, changed, err)
	}

	second := testNginxArtifact("1.30.6", strings.Repeat("2", 64))
	if _, created, err := database.SaveNginxArtifactCandidate(second); err != nil || !created {
		t.Fatalf("save second candidate: created=%v, err=%v", created, err)
	}
	current, err := database.CurrentNginxArtifact()
	if err != nil || current.SHA256 != first.SHA256 {
		t.Fatalf("current before approval = %#v, err=%v", current, err)
	}
	candidate, err := database.CandidateNginxArtifact()
	if err != nil || candidate.SHA256 != second.SHA256 {
		t.Fatalf("candidate = %#v, err=%v", candidate, err)
	}
	if _, changed, err := database.PromoteNginxArtifact(second.SHA256); err != nil || !changed {
		t.Fatalf("promote second: changed=%v, err=%v", changed, err)
	}
	retired, err := database.NginxArtifactBySHA(first.SHA256)
	if err != nil || retired.State != domain.NginxArtifactRetired {
		t.Fatalf("retired first = %#v, err=%v", retired, err)
	}

	rebuilt := first
	rebuilt.SHA256 = strings.Repeat("3", 64)
	if _, _, err := database.SaveNginxArtifactCandidate(rebuilt); err == nil {
		t.Fatal("same Nginx version with a different digest was accepted")
	}
	if _, err := database.CandidateNginxArtifact(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("candidate remains after promotion: %v", err)
	}
}

func testNginxArtifact(version, digest string) domain.NginxArtifact {
	return domain.NginxArtifact{
		SHA256: digest, Version: version, ReleaseTag: "nginx-v" + version,
		SourceURL:         "https://github.com/example/project/releases/download/nginx-v" + version + "/cdn-nginx-linux-amd64.tar.gz",
		OfficialSourceURL: "https://nginx.org/download/nginx-" + version + ".tar.gz",
		SourceSHA256:      strings.Repeat("a", 64), BuildCommit: strings.Repeat("b", 40),
		SizeBytes: 1024, DownloadedAt: time.Now().UTC(),
	}
}
