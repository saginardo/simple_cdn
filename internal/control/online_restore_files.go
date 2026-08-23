package control

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"simple_cdn/internal/project"
	"simple_cdn/internal/store"
)

type onlineRestoreArtifacts struct {
	SnapshotRoot         string
	DatabasePath         string
	SecretsArchive       string
	TLSArchive           string
	StaticAssetDirectory string
	ClickHouseBackup     string
	ClickHouseDatabase   string
	DatabaseSHA256       string
	SecretsSHA256        string
	TLSSHA256            string
	StaticAssetsSHA256   string
	CAFingerprint        string
	SchemaVersion        int
}

func validateOnlineRestoreSnapshot(jobRoot string, cipher *Cipher, tlsDomain string) (onlineRestoreArtifacts, error) {
	snapshotRoot := filepath.Join(jobRoot, "snapshot")
	artifacts := onlineRestoreArtifacts{
		SnapshotRoot:         snapshotRoot,
		DatabasePath:         filepath.Join(snapshotRoot, "backup", "staging", "control", "control.db"),
		SecretsArchive:       filepath.Join(snapshotRoot, "backup", "staging", "control", "control-secrets.tar.gz"),
		TLSArchive:           filepath.Join(snapshotRoot, "backup", "staging", "control", "control-tls.tar.gz"),
		StaticAssetDirectory: filepath.Join(snapshotRoot, "backup", "staging", "control", "static-assets", "objects"),
	}
	backupPath, backupDatabase, err := resolveClickHouseBackup(snapshotRoot)
	if err != nil {
		return onlineRestoreArtifacts{}, err
	}
	artifacts.ClickHouseBackup = backupPath
	artifacts.ClickHouseDatabase = backupDatabase
	for _, path := range []string{artifacts.DatabasePath, artifacts.SecretsArchive, artifacts.TLSArchive} {
		if info, err := os.Stat(path); err != nil {
			return onlineRestoreArtifacts{}, fmt.Errorf("snapshot artifact %s: %w", filepath.Base(path), err)
		} else if !info.Mode().IsRegular() {
			return onlineRestoreArtifacts{}, fmt.Errorf("snapshot artifact %s is not a regular file", filepath.Base(path))
		}
	}
	if info, err := os.Stat(artifacts.ClickHouseBackup); err != nil {
		return onlineRestoreArtifacts{}, fmt.Errorf("snapshot ClickHouse backup: %w", err)
	} else if !info.IsDir() {
		return onlineRestoreArtifacts{}, errors.New("snapshot ClickHouse backup is not a directory")
	}

	database, err := store.OpenImmutable(artifacts.DatabasePath)
	if err != nil {
		return onlineRestoreArtifacts{}, fmt.Errorf("open restored SQLite database: %w", err)
	}
	defer database.Close()
	if err := database.IntegrityCheck(); err != nil {
		return onlineRestoreArtifacts{}, err
	}
	artifacts.SchemaVersion, err = database.SchemaVersion()
	if err != nil {
		return onlineRestoreArtifacts{}, fmt.Errorf("read restored schema version: %w", err)
	}
	if artifacts.SchemaVersion < 1 || artifacts.SchemaVersion > store.LatestSchemaVersion() {
		return onlineRestoreArtifacts{}, fmt.Errorf("restored schema version %d is outside the supported range 1-%d", artifacts.SchemaVersion, store.LatestSchemaVersion())
	}
	if cipher == nil {
		return onlineRestoreArtifacts{}, errors.New("restore cipher is required")
	}
	for _, name := range []string{store.SecretCloudflareAPIToken, store.SecretSMTPPassword, store.SecretBackupAccessKey, store.SecretBackupPassword} {
		ciphertext, secretErr := database.Secret(name)
		if errors.Is(secretErr, store.ErrNotFound) {
			continue
		}
		if secretErr != nil {
			return onlineRestoreArtifacts{}, fmt.Errorf("read restored secret %s: %w", name, secretErr)
		}
		if _, err := cipher.Decrypt(ciphertext); err != nil {
			return onlineRestoreArtifacts{}, fmt.Errorf("restored secret %s cannot be decrypted with CONTROL_ENCRYPTION_KEY: %w", name, err)
		}
	}

	if artifacts.DatabaseSHA256, err = fileSHA256(artifacts.DatabasePath); err != nil {
		return onlineRestoreArtifacts{}, err
	}
	if artifacts.SecretsSHA256, err = fileSHA256(artifacts.SecretsArchive); err != nil {
		return onlineRestoreArtifacts{}, err
	}
	if artifacts.TLSSHA256, err = fileSHA256(artifacts.TLSArchive); err != nil {
		return onlineRestoreArtifacts{}, err
	}
	if artifacts.StaticAssetsSHA256, err = VerifyStaticAssetBackup(artifacts.DatabasePath, artifacts.StaticAssetDirectory); err != nil {
		return onlineRestoreArtifacts{}, fmt.Errorf("validate managed static asset backup: %w", err)
	}

	validationRoot := filepath.Join(jobRoot, "validation")
	if err := os.RemoveAll(validationRoot); err != nil {
		return onlineRestoreArtifacts{}, err
	}
	defer os.RemoveAll(validationRoot)
	controlRoot := filepath.Join(validationRoot, "control")
	tlsRoot := filepath.Join(validationRoot, "tls")
	if err := extractRestoreArchive(artifacts.SecretsArchive, controlRoot); err != nil {
		return onlineRestoreArtifacts{}, fmt.Errorf("validate control secrets archive: %w", err)
	}
	for _, name := range []string{"edge-ca.crt", "edge-ca.key"} {
		if info, err := os.Stat(filepath.Join(controlRoot, "pki", name)); err != nil || !info.Mode().IsRegular() {
			return onlineRestoreArtifacts{}, fmt.Errorf("restored internal CA is missing %s", name)
		}
	}
	ca, err := LoadOrCreateInternalCA(filepath.Join(controlRoot, "pki"))
	if err != nil {
		return onlineRestoreArtifacts{}, fmt.Errorf("validate restored internal CA: %w", err)
	}
	artifacts.CAFingerprint = CertificateFingerprintDER(ca.Certificate.Raw)
	if err := extractRestoreArchive(artifacts.TLSArchive, tlsRoot); err != nil {
		return onlineRestoreArtifacts{}, fmt.Errorf("validate control TLS archive: %w", err)
	}
	if tlsDomain = strings.TrimSpace(tlsDomain); tlsDomain != "" {
		certificatePath := filepath.Join(tlsRoot, "live", tlsDomain, "fullchain.pem")
		keyPath := filepath.Join(tlsRoot, "live", tlsDomain, "privkey.pem")
		if _, err := tls.LoadX509KeyPair(certificatePath, keyPath); err != nil {
			return onlineRestoreArtifacts{}, fmt.Errorf("validate restored control TLS key pair: %w", err)
		}
	}
	return artifacts, nil
}

func verifyOnlineRestoreArtifactHashes(jobRoot string, job OnlineRestoreJob) (onlineRestoreArtifacts, error) {
	artifacts := onlineRestoreArtifacts{
		SnapshotRoot:         filepath.Join(jobRoot, "snapshot"),
		DatabasePath:         filepath.Join(jobRoot, "snapshot", "backup", "staging", "control", "control.db"),
		SecretsArchive:       filepath.Join(jobRoot, "snapshot", "backup", "staging", "control", "control-secrets.tar.gz"),
		TLSArchive:           filepath.Join(jobRoot, "snapshot", "backup", "staging", "control", "control-tls.tar.gz"),
		StaticAssetDirectory: filepath.Join(jobRoot, "snapshot", "backup", "staging", "control", "static-assets", "objects"),
	}
	var err error
	artifacts.ClickHouseBackup, artifacts.ClickHouseDatabase, err = resolveClickHouseBackup(artifacts.SnapshotRoot)
	if err != nil {
		return onlineRestoreArtifacts{}, err
	}
	checks := []struct {
		path string
		want string
	}{
		{artifacts.DatabasePath, job.DatabaseSHA256},
		{artifacts.SecretsArchive, job.SecretsSHA256},
		{artifacts.TLSArchive, job.TLSSHA256},
	}
	for _, check := range checks {
		got, err := fileSHA256(check.path)
		if err != nil {
			return onlineRestoreArtifacts{}, err
		}
		if got != check.want {
			return onlineRestoreArtifacts{}, fmt.Errorf("staged restore artifact %s changed after validation", filepath.Base(check.path))
		}
	}
	artifacts.StaticAssetsSHA256, err = VerifyStaticAssetBackup(artifacts.DatabasePath, artifacts.StaticAssetDirectory)
	if err != nil {
		return onlineRestoreArtifacts{}, fmt.Errorf("verify managed static asset backup: %w", err)
	}
	if artifacts.StaticAssetsSHA256 != job.StaticAssetsSHA256 {
		return onlineRestoreArtifacts{}, errors.New("staged managed static asset objects changed after validation")
	}
	return artifacts, nil
}

func resolveClickHouseBackup(snapshotRoot string) (string, string, error) {
	root := filepath.Join(snapshotRoot, "backup", "staging", "clickhouse")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", "", fmt.Errorf("snapshot ClickHouse backup: %w", err)
	}
	var foundPath, foundDatabase string
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), "-current") {
			continue
		}
		database := strings.TrimSuffix(entry.Name(), "-current")
		if database == "cdn-platform" {
			database = project.LegacyClickHouseDatabase
		}
		if !validRestoreIdentifier(database) {
			return "", "", errors.New("snapshot ClickHouse database name is invalid")
		}
		if foundPath != "" {
			return "", "", errors.New("snapshot contains multiple ClickHouse backups")
		}
		foundPath = filepath.Join(root, entry.Name())
		foundDatabase = database
	}
	if foundPath == "" {
		return "", "", errors.New("snapshot ClickHouse backup is missing")
	}
	return foundPath, foundDatabase, nil
}

func prepareClickHouseBackupPermissions(root string, groupID int) error {
	regularFiles := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!entry.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("unsupported ClickHouse backup entry: %s", path)
		}
		mode := os.FileMode(0o640)
		if entry.IsDir() {
			mode = 0o2750
		} else {
			regularFiles++
		}
		if groupID >= 0 {
			if err := os.Chown(path, -1, groupID); err != nil {
				return fmt.Errorf("share ClickHouse backup with gid %d: %w", groupID, err)
			}
		}
		return os.Chmod(path, mode)
	})
	if err != nil {
		return err
	}
	if regularFiles == 0 {
		return errors.New("ClickHouse backup is empty")
	}
	return nil
}

func extractRestoreArchive(archivePath, destinationRoot string) error {
	if err := os.MkdirAll(destinationRoot, 0o700); err != nil {
		return err
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	const maxArchiveBytes = int64(1 << 30)
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		cleanName, destination, err := safeRestorePath(destinationRoot, header.Name)
		if err != nil {
			return err
		}
		if cleanName == "." {
			continue
		}
		total += header.Size
		if header.Size < 0 || header.Size > maxArchiveBytes || total > maxArchiveBytes {
			return errors.New("restore archive exceeds the 1 GiB safety limit")
		}
		mode := os.FileMode(header.Mode) & 0o755
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destination, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			if _, err := os.Lstat(destination); err == nil {
				return fmt.Errorf("duplicate restore archive member %s", header.Name)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(destination), filepath.FromSlash(header.Linkname)))
			if !pathWithin(destinationRoot, resolved) {
				return fmt.Errorf("restore archive symlink %s escapes its root", header.Name)
			}
			if err := os.Symlink(header.Linkname, destination); err != nil {
				return err
			}
		case tar.TypeLink:
			_, target, err := safeRestorePath(destinationRoot, header.Linkname)
			if err != nil {
				return err
			}
			if err := os.Link(target, destination); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported restore archive entry type %d for %s", header.Typeflag, header.Name)
		}
	}
}

func safeRestorePath(root, name string) (string, string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("restore archive member %q escapes its root", name)
	}
	destination := filepath.Join(root, clean)
	if !pathWithin(root, destination) {
		return "", "", fmt.Errorf("restore archive member %q escapes its root", name)
	}
	return clean, destination, nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
