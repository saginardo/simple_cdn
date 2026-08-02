package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"simple_cdn/internal/domain"
)

func (a *Agent) syncStaticAssets(references []domain.StaticAssetReference) error {
	desired, err := normalizeDesiredStaticAssets(references)
	if err != nil {
		return err
	}
	if len(desired) == 0 {
		return nil
	}
	if err := os.MkdirAll(a.Config.StaticAssetDirectory, 0o755); err != nil {
		return fmt.Errorf("create static resource directory: %w", err)
	}
	for digest, reference := range desired {
		destination := filepath.Join(a.Config.StaticAssetDirectory, digest)
		matches, err := staticAssetFileMatches(destination, reference)
		if err != nil {
			return fmt.Errorf("inspect static resource %s: %w", digest, err)
		}
		if matches {
			continue
		}
		if err := a.downloadStaticAsset(reference, destination); err != nil {
			return err
		}
	}
	return nil
}

func normalizeDesiredStaticAssets(references []domain.StaticAssetReference) (map[string]domain.StaticAssetReference, error) {
	desired := make(map[string]domain.StaticAssetReference, len(references))
	paths := make(map[string]struct{}, len(references))
	for _, reference := range references {
		normalized, err := domain.NormalizeStaticAssetReference(reference)
		if err != nil {
			return nil, fmt.Errorf("invalid static resource reference: %w", err)
		}
		pathKey := normalized.SiteID + "\x00" + normalized.URLPath
		if _, found := paths[pathKey]; found {
			return nil, fmt.Errorf("duplicate static resource URL %s for site %s", normalized.URLPath, normalized.SiteID)
		}
		paths[pathKey] = struct{}{}
		if existing, found := desired[normalized.SHA256]; found && existing.SizeBytes != normalized.SizeBytes {
			return nil, fmt.Errorf("static resource %s has inconsistent sizes", normalized.SHA256)
		}
		desired[normalized.SHA256] = normalized
	}
	return desired, nil
}

func staticAssetFileMatches(path string, reference domain.StaticAssetReference) (bool, error) {
	pathInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !pathInfo.Mode().IsRegular() {
		return false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(pathInfo, info) || info.Size() != reference.SizeBytes {
		return false, nil
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false, err
	}
	return hex.EncodeToString(hash.Sum(nil)) == reference.SHA256, nil
}

func (a *Agent) downloadStaticAsset(reference domain.StaticAssetReference, destination string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.Config.ControlURL+"/api/edge/v1/static-assets/"+reference.SHA256, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := a.client().Do(request)
	if err != nil {
		return fmt.Errorf("download static resource %s: %w", reference.SHA256, err)
	}
	defer drainAndClose(response.Body)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("download static resource %s: %s: %s", reference.SHA256,
			response.Status, strings.TrimSpace(string(body)))
	}
	if response.ContentLength >= 0 && response.ContentLength != reference.SizeBytes {
		return fmt.Errorf("download static resource %s: content length %d, expected %d",
			reference.SHA256, response.ContentLength, reference.SizeBytes)
	}
	temporary, err := os.CreateTemp(a.Config.StaticAssetDirectory, ".static-resource-*")
	if err != nil {
		return fmt.Errorf("create static resource temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		return err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, reference.SizeBytes+1))
	if err != nil {
		return fmt.Errorf("write static resource %s: %w", reference.SHA256, err)
	}
	if written != reference.SizeBytes {
		return fmt.Errorf("download static resource %s: received %d bytes, expected %d",
			reference.SHA256, written, reference.SizeBytes)
	}
	if digest := hex.EncodeToString(hash.Sum(nil)); digest != reference.SHA256 {
		return fmt.Errorf("download static resource %s: SHA-256 mismatch (%s)", reference.SHA256, digest)
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("install static resource %s: %w", reference.SHA256, err)
	}
	if err := syncDirectory(a.Config.StaticAssetDirectory); err != nil {
		return fmt.Errorf("sync static resource directory: %w", err)
	}
	committed = true
	return nil
}

func (a *Agent) cleanupStaticAssets(references []domain.StaticAssetReference) error {
	desired, err := normalizeDesiredStaticAssets(references)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(a.Config.StaticAssetDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		name := entry.Name()
		if !domain.ValidStaticAssetSHA256(name) {
			continue
		}
		if _, found := desired[name]; found {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			failures = append(failures, infoErr)
			continue
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if removeErr := os.Remove(filepath.Join(a.Config.StaticAssetDirectory, name)); removeErr != nil {
			failures = append(failures, removeErr)
		}
	}
	if len(failures) == 0 {
		return syncDirectory(a.Config.StaticAssetDirectory)
	}
	return errors.Join(failures...)
}
