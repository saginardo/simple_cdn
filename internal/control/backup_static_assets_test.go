package control

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func TestStageAndVerifyStaticAssetBackup(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "control.db")
	database, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string][]byte{
		"app.js":     []byte("console.log('restored');\n"),
		"robots.txt": []byte("User-agent: *\nDisallow:\n"),
	}
	digests := make(map[string]string, len(objects))
	for name, contents := range objects {
		digest := sha256.Sum256(contents)
		digests[name] = hex.EncodeToString(digest[:])
		if _, err := database.CreateStaticAsset(domain.StaticAsset{
			Name: name, OriginalName: name, SHA256: digests[name], SizeBytes: int64(len(contents)), ContentType: "text/plain",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(root, "live", "static-assets", "objects")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, contents := range objects {
		if err := os.WriteFile(filepath.Join(source, digests[name]), contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "unreferenced"), []byte("not in SQLite"), 0o644); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(root, "staging", "static-assets", "objects")
	if err := StageStaticAssetBackup(databasePath, source, destination); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(objects) {
		t.Fatalf("staged objects = %v", entries)
	}
	for name, contents := range objects {
		staged, err := os.ReadFile(filepath.Join(destination, digests[name]))
		if err != nil || string(staged) != string(contents) {
			t.Fatalf("staged object %s = %q, err = %v", name, staged, err)
		}
	}
	digest, err := VerifyStaticAssetBackup(databasePath, destination)
	if err != nil || len(digest) != sha256.Size*2 {
		t.Fatalf("backup digest = %q, err = %v", digest, err)
	}
	digestAgain, err := VerifyStaticAssetBackup(databasePath, destination)
	if err != nil || digestAgain != digest {
		t.Fatalf("repeated backup digest = %q, err = %v", digestAgain, err)
	}

	corruptPath := filepath.Join(destination, digests["app.js"])
	if err := os.WriteFile(corruptPath, []byte(strings.Repeat("x", len(objects["app.js"]))), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyStaticAssetBackup(databasePath, destination); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("corrupt object verification error = %v", err)
	}
}

func TestStaticAssetBackupRequiresEveryReferencedObjectAndRejectsExtras(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "control.db")
	database, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	contents := []byte("required object")
	digestBytes := sha256.Sum256(contents)
	digest := hex.EncodeToString(digestBytes[:])
	if _, err := database.CreateStaticAsset(domain.StaticAsset{
		Name: "required", OriginalName: "required.txt", SHA256: digest,
		SizeBytes: int64(len(contents)), ContentType: "text/plain",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	missingSource := filepath.Join(root, "missing")
	if err := os.MkdirAll(missingSource, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := StageStaticAssetBackup(databasePath, missingSource, filepath.Join(root, "stage-missing", "objects")); err == nil || !strings.Contains(err.Error(), digest) {
		t.Fatalf("missing object staging error = %v", err)
	}
	if _, err := VerifyStaticAssetBackup(databasePath, missingSource); err == nil || !strings.Contains(err.Error(), digest) {
		t.Fatalf("missing object verification error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(missingSource, digest), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(missingSource, "unexpected"), []byte("extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyStaticAssetBackup(databasePath, missingSource); err == nil || !strings.Contains(err.Error(), "unexpected object") {
		t.Fatalf("extra object verification error = %v", err)
	}
}

func TestStaticAssetBackupAllowsEmptyObjectSetWithoutLiveDirectory(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "control.db")
	database, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(databasePath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}

	destination := filepath.Join(root, "staging", "static-assets", "objects")
	if err := StageStaticAssetBackup(databasePath, filepath.Join(root, "does-not-exist"), destination); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty staged directory = %v, err = %v", entries, err)
	}
	withDirectory, err := VerifyStaticAssetBackup(databasePath, destination)
	if err != nil {
		t.Fatal(err)
	}
	withoutDirectory, err := VerifyStaticAssetBackup(databasePath, filepath.Join(root, "absent"))
	if err != nil || withoutDirectory != withDirectory {
		t.Fatalf("empty backup digests = %q and %q, err = %v", withDirectory, withoutDirectory, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(databasePath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("static asset snapshot validation created %s: %v", filepath.Base(databasePath+suffix), err)
		}
	}
}
