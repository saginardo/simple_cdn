package control

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

const staticAssetSchemaVersion = 28
const staticAssetBackupDigestPrefix = "simple_cdn-static-assets-v1\n"

// StageStaticAssetBackup copies exactly the content objects referenced by a
// SQLite backup into a clean, deterministic Restic staging directory.
func StageStaticAssetBackup(databasePath, sourceDirectory, destinationDirectory string) error {
	assets, err := staticAssetBackupMetadata(databasePath)
	if err != nil {
		return err
	}
	sourceDirectory = filepath.Clean(strings.TrimSpace(sourceDirectory))
	destinationDirectory = filepath.Clean(strings.TrimSpace(destinationDirectory))
	if sourceDirectory == "." || destinationDirectory == "." {
		return errors.New("static asset backup source and destination are required")
	}
	if _, err := os.Lstat(destinationDirectory); err == nil {
		return errors.New("static asset backup destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(assets) != 0 {
		if err := requireStaticAssetDirectory(sourceDirectory); err != nil {
			return err
		}
	}
	parent := filepath.Dir(destinationDirectory)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("create static asset backup parent: %w", err)
	}
	temporaryDirectory, err := os.MkdirTemp(parent, ".objects-*")
	if err != nil {
		return fmt.Errorf("create static asset backup staging directory: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)
	if err := os.Chmod(temporaryDirectory, 0o750); err != nil {
		return err
	}
	for _, asset := range assets {
		if err := copyStaticAssetBackupObject(asset, filepath.Join(sourceDirectory, asset.SHA256), filepath.Join(temporaryDirectory, asset.SHA256)); err != nil {
			return err
		}
	}
	if err := syncStaticAssetDirectory(temporaryDirectory); err != nil {
		return fmt.Errorf("sync static asset backup staging directory: %w", err)
	}
	if err := os.Rename(temporaryDirectory, destinationDirectory); err != nil {
		return fmt.Errorf("install static asset backup staging directory: %w", err)
	}
	if err := syncStaticAssetDirectory(parent); err != nil {
		return fmt.Errorf("sync static asset backup parent: %w", err)
	}
	return nil
}

// VerifyStaticAssetBackup proves that a restored object directory exactly
// matches the managed static asset metadata in its SQLite backup.
func VerifyStaticAssetBackup(databasePath, objectDirectory string) (string, error) {
	assets, err := staticAssetBackupMetadata(databasePath)
	if err != nil {
		return "", err
	}
	objectDirectory = filepath.Clean(strings.TrimSpace(objectDirectory))
	if objectDirectory == "." {
		return "", errors.New("static asset backup directory is required")
	}
	digest := sha256.New()
	_, _ = io.WriteString(digest, staticAssetBackupDigestPrefix)

	info, statErr := lstatStaticAssetObjectDirectory(objectDirectory)
	if errors.Is(statErr, os.ErrNotExist) && len(assets) == 0 {
		return hex.EncodeToString(digest.Sum(nil)), nil
	}
	if statErr != nil {
		return "", fmt.Errorf("inspect static asset backup directory: %w", statErr)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("static asset backup path is not a directory")
	}
	entries, err := os.ReadDir(objectDirectory)
	if err != nil {
		return "", fmt.Errorf("read static asset backup directory: %w", err)
	}
	expected := make(map[string]domain.StaticAsset, len(assets))
	for _, asset := range assets {
		expected[asset.SHA256] = asset
	}
	for _, entry := range entries {
		if _, found := expected[entry.Name()]; !found {
			return "", fmt.Errorf("static asset backup contains unexpected object %q", entry.Name())
		}
	}
	for _, asset := range assets {
		if err := verifyStaticAssetBackupObject(asset, filepath.Join(objectDirectory, asset.SHA256)); err != nil {
			return "", err
		}
		_, _ = io.WriteString(digest, asset.SHA256)
		_, _ = io.WriteString(digest, "\t")
		_, _ = io.WriteString(digest, strconv.FormatInt(asset.SizeBytes, 10))
		_, _ = io.WriteString(digest, "\n")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func staticAssetBackupMetadata(databasePath string) ([]domain.StaticAsset, error) {
	databasePath = filepath.Clean(strings.TrimSpace(databasePath))
	if databasePath == "." {
		return nil, errors.New("static asset backup database is required")
	}
	database, err := store.OpenImmutable(databasePath)
	if err != nil {
		return nil, fmt.Errorf("open static asset backup database: %w", err)
	}
	defer database.Close()
	version, err := database.SchemaVersion()
	if err != nil {
		return nil, fmt.Errorf("read static asset backup schema version: %w", err)
	}
	if version < staticAssetSchemaVersion {
		return []domain.StaticAsset{}, nil
	}
	assets, err := database.ListStaticAssets()
	if err != nil {
		return nil, fmt.Errorf("read static asset backup metadata: %w", err)
	}
	seen := make(map[string]struct{}, len(assets))
	for index, asset := range assets {
		normalized, err := domain.NormalizeStaticAsset(asset)
		if err != nil {
			return nil, fmt.Errorf("invalid static asset backup metadata for %q: %w", asset.ID, err)
		}
		if _, found := seen[normalized.SHA256]; found {
			return nil, fmt.Errorf("duplicate static asset backup digest %s", normalized.SHA256)
		}
		seen[normalized.SHA256] = struct{}{}
		assets[index] = normalized
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].SHA256 < assets[j].SHA256 })
	return assets, nil
}

func requireStaticAssetDirectory(path string) error {
	info, err := lstatStaticAssetObjectDirectory(path)
	if err != nil {
		return fmt.Errorf("inspect managed static asset directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed static asset path is not a directory")
	}
	return nil
}

func lstatStaticAssetObjectDirectory(path string) (os.FileInfo, error) {
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return nil, err
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("static asset object directory parent is not a directory")
	}
	return os.Lstat(path)
}

func verifyStaticAssetBackupObject(asset domain.StaticAsset, path string) error {
	file, _, err := openStaticAssetBackupObject(asset, path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, asset.SizeBytes+1))
	if err != nil {
		return fmt.Errorf("read static asset backup object %s: %w", asset.SHA256, err)
	}
	if written != asset.SizeBytes {
		return fmt.Errorf("static asset backup object %s has size %d, expected %d", asset.SHA256, written, asset.SizeBytes)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != asset.SHA256 {
		return fmt.Errorf("static asset backup object %s has SHA-256 %s", asset.SHA256, actual)
	}
	return nil
}

func copyStaticAssetBackupObject(asset domain.StaticAsset, sourcePath, destinationPath string) error {
	source, _, err := openStaticAssetBackupObject(asset, sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create static asset backup object %s: %w", asset.SHA256, err)
	}
	committed := false
	defer func() {
		_ = destination.Close()
		if !committed {
			_ = os.Remove(destinationPath)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(source, asset.SizeBytes+1))
	if err != nil {
		return fmt.Errorf("copy static asset backup object %s: %w", asset.SHA256, err)
	}
	if written != asset.SizeBytes {
		return fmt.Errorf("managed static asset object %s has size %d, expected %d", asset.SHA256, written, asset.SizeBytes)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != asset.SHA256 {
		return fmt.Errorf("managed static asset object %s has SHA-256 %s", asset.SHA256, actual)
	}
	if err := destination.Sync(); err != nil {
		return err
	}
	if err := destination.Close(); err != nil {
		return err
	}
	committed = true
	return nil
}

func openStaticAssetBackupObject(asset domain.StaticAsset, path string) (*os.File, os.FileInfo, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect static asset object %s: %w", asset.SHA256, err)
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("static asset object %s is not a regular file", asset.SHA256)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open static asset object %s: %w", asset.SHA256, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(pathInfo, info) || info.Size() != asset.SizeBytes {
		file.Close()
		return nil, nil, fmt.Errorf("static asset object %s is unavailable or has size %d, expected %d", asset.SHA256, info.Size(), asset.SizeBytes)
	}
	return file, info, nil
}
