package edge

import (
	"compress/gzip"
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

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"simple_cdn/internal/domain"
)

var staticAssetCompressions = []struct {
	suffix string
	open   func(io.Writer) (io.WriteCloser, error)
}{
	{suffix: ".gz", open: func(destination io.Writer) (io.WriteCloser, error) {
		return gzip.NewWriterLevel(destination, gzip.BestCompression)
	}},
	{suffix: ".br", open: func(destination io.Writer) (io.WriteCloser, error) {
		return brotli.NewWriterLevel(destination, 9), nil
	}},
	{suffix: ".zst", open: func(destination io.Writer) (io.WriteCloser, error) {
		return zstd.NewWriter(destination, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
	}},
}

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
		if !matches {
			if err := a.downloadStaticAsset(reference, destination); err != nil {
				return err
			}
		}
		if domain.PrecompressibleStaticAsset(reference.ContentType, reference.SizeBytes) {
			if err := ensureStaticAssetCompressions(destination, reference); err != nil {
				return fmt.Errorf("precompress static resource %s: %w", digest, err)
			}
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
		if existing, found := desired[normalized.SHA256]; found {
			if existing.SizeBytes != normalized.SizeBytes {
				return nil, fmt.Errorf("static resource %s has inconsistent sizes", normalized.SHA256)
			}
			// A digest can be exposed through more than one binding. Only create
			// sidecars when every binding has a compressible content type.
			if !domain.PrecompressibleStaticAsset(normalized.ContentType, normalized.SizeBytes) &&
				domain.PrecompressibleStaticAsset(existing.ContentType, existing.SizeBytes) {
				desired[normalized.SHA256] = normalized
			}
			continue
		}
		desired[normalized.SHA256] = normalized
	}
	return desired, nil
}

func ensureStaticAssetCompressions(sourcePath string, reference domain.StaticAssetReference) error {
	for _, compression := range staticAssetCompressions {
		destination := sourcePath + compression.suffix
		matches, err := compressedStaticAssetMatches(destination, reference, compression.suffix)
		if err != nil {
			return err
		}
		if matches {
			continue
		}
		if err := writeCompressedStaticAsset(sourcePath, destination, reference.SizeBytes, compression.open); err != nil {
			return err
		}
	}
	return nil
}

func writeCompressedStaticAsset(sourcePath, destination string, sourceSize int64, openWriter func(io.Writer) (io.WriteCloser, error)) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".static-compression-*")
	if err != nil {
		return err
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
	compressed, err := openWriter(temporary)
	if err != nil {
		return err
	}
	if _, err := io.Copy(compressed, source); err != nil {
		_ = compressed.Close()
		return err
	}
	if err := compressed.Close(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	info, err := temporary.Stat()
	if err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if info.Size() <= 0 || info.Size() >= sourceSize {
		if err := removeRegularOrSymlink(destination); err != nil {
			return err
		}
		return nil
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	committed = true
	return nil
}

func compressedStaticAssetMatches(path string, reference domain.StaticAssetReference, suffix string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() >= reference.SizeBytes {
		return false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	reader, closeReader, err := openStaticAssetCompressionReader(file, suffix)
	if err != nil {
		return false, nil
	}
	defer closeReader()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(reader, reference.SizeBytes+1))
	if err != nil || written != reference.SizeBytes {
		return false, nil
	}
	return hex.EncodeToString(hash.Sum(nil)) == reference.SHA256, nil
}

func openStaticAssetCompressionReader(source io.Reader, suffix string) (io.Reader, func(), error) {
	switch suffix {
	case ".gz":
		reader, err := gzip.NewReader(source)
		if err != nil {
			return nil, func() {}, err
		}
		return reader, func() { _ = reader.Close() }, nil
	case ".br":
		return brotli.NewReader(source), func() {}, nil
	case ".zst":
		reader, err := zstd.NewReader(source)
		if err != nil {
			return nil, func() {}, err
		}
		return reader, reader.Close, nil
	default:
		return nil, func() {}, errors.New("unsupported static compression suffix")
	}
}

func removeRegularOrSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("managed static compression path is not a regular file")
	}
	return os.Remove(path)
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
		digest, suffix, managed := managedStaticAssetName(name)
		if !managed {
			continue
		}
		if reference, found := desired[digest]; found {
			if suffix == "" || domain.PrecompressibleStaticAsset(reference.ContentType, reference.SizeBytes) {
				continue
			}
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

func managedStaticAssetName(name string) (string, string, bool) {
	if domain.ValidStaticAssetSHA256(name) {
		return name, "", true
	}
	for _, compression := range staticAssetCompressions {
		if digest := strings.TrimSuffix(name, compression.suffix); digest != name && domain.ValidStaticAssetSHA256(digest) {
			return digest, compression.suffix, true
		}
	}
	return "", "", false
}
