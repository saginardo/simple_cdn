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

func TestNginxArtifactCatalogReferencesInFlightUpgradeDigests(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	current := testNginxArtifact("1.30.4", strings.Repeat("1", 64))
	if _, _, err := database.SaveNginxArtifactCandidate(current); err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.PromoteNginxArtifact(current.SHA256); err != nil {
		t.Fatal(err)
	}
	retired := testNginxArtifact("1.30.5", strings.Repeat("2", 64))
	if _, _, err := database.SaveNginxArtifactCandidate(retired); err != nil {
		t.Fatal(err)
	}
	candidate := testNginxArtifact("1.30.6", strings.Repeat("3", 64))
	if _, _, err := database.SaveNginxArtifactCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	list, err := database.ListNginxArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("artifact catalog length = %d, want 3", len(list))
	}
	states := map[string]domain.NginxArtifactState{}
	for _, artifact := range list {
		states[artifact.SHA256] = artifact.State
	}
	if states[current.SHA256] != domain.NginxArtifactCurrent || states[retired.SHA256] != domain.NginxArtifactRetired || states[candidate.SHA256] != domain.NginxArtifactCandidate {
		t.Fatalf("artifact states = %#v", states)
	}

	referenced, err := database.ReferencedUpgradeNginxSHA256s()
	if err != nil {
		t.Fatal(err)
	}
	if len(referenced) != 0 {
		t.Fatalf("referenced digests before upgrade = %#v", referenced)
	}

	node, err := database.CreateNode("edge-gc-query", "203.0.113.189")
	if err != nil {
		t.Fatal(err)
	}
	sourceNginx := strings.Repeat("e", 64)
	targetNginx := strings.Repeat("f", 64)
	if err := database.HeartbeatWithArtifacts(node.ID, 0, "", nil, "v1", strings.Repeat("9", 64), "1.30.5", sourceNginx, ""); err != nil {
		t.Fatal(err)
	}
	instruction := testUpgradeInstruction(strings.Repeat("8", 64))
	instruction.NginxBundle = &domain.UpgradeArtifact{URL: "https://control.example.test/nginx", SHA256: targetNginx}
	task, created, err := database.CreateOrGetNodeUpgrade(node.ID, instruction, time.Now().Add(time.Hour))
	if err != nil || !created {
		t.Fatalf("create upgrade task: created=%v, err=%v", created, err)
	}
	referenced, err = database.ReferencedUpgradeNginxSHA256s()
	if err != nil {
		t.Fatal(err)
	}
	if _, hasTarget := referenced[targetNginx]; !hasTarget {
		t.Fatalf("in-flight target digest is not referenced: %#v", referenced)
	}
	if _, hasSource := referenced[sourceNginx]; !hasSource {
		t.Fatalf("in-flight source digest is not referenced: %#v", referenced)
	}
	if _, hasCurrent := referenced[candidate.SHA256]; hasCurrent {
		t.Fatalf("unrelated digest is referenced: %#v", referenced)
	}

	if _, err := database.RecordNodeUpgradeReport(node.ID, domain.NodeUpgradeReport{
		TaskID: task.ID, Status: domain.NodeUpgradeSucceeded, InstalledSHA256: strings.Repeat("8", 64), InstalledNginxSHA256: targetNginx,
	}); err != nil {
		t.Fatal(err)
	}
	referenced, err = database.ReferencedUpgradeNginxSHA256s()
	if err != nil {
		t.Fatal(err)
	}
	if len(referenced) != 0 {
		t.Fatalf("completed task still references digests: %#v", referenced)
	}
}
