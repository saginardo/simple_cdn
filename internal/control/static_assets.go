package control

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

const staticAssetMultipartOverhead int64 = 1 << 20

type staticAssetOverview struct {
	Assets       []domain.StaticAsset `json:"assets"`
	Sites        []domain.Site        `json:"sites"`
	MaxFileBytes int64                `json:"max_file_bytes"`
	CachePresets []string             `json:"cache_presets"`
}

func (s *Server) listStaticAssets(response http.ResponseWriter, _ *http.Request) {
	assets, err := s.Store.ListStaticAssets()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	sites, err := s.Store.ListSites()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	httpSites := sites[:0]
	for _, site := range sites {
		if !site.TCPOnly && !site.Deleting {
			httpSites = append(httpSites, site)
		}
	}
	writeJSON(response, http.StatusOK, staticAssetOverview{
		Assets: assets, Sites: httpSites, MaxFileBytes: domain.MaxStaticAssetBytes,
		CachePresets: []string{domain.StaticAssetCacheHour, domain.StaticAssetCacheDay,
			domain.StaticAssetCacheImmutable, domain.StaticAssetCacheNoCache},
	})
}

func (s *Server) uploadStaticAsset(response http.ResponseWriter, request *http.Request) {
	s.staticAssetMu.Lock()
	defer s.staticAssetMu.Unlock()
	directory, err := s.staticAssetObjectDirectory()
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, err)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, domain.MaxStaticAssetBytes+staticAssetMultipartOverhead)
	reader, err := request.MultipartReader()
	if err != nil {
		writeError(response, http.StatusBadRequest, errors.New("multipart file upload is required"))
		return
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	temporary, err := os.CreateTemp(directory, ".upload-*")
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	temporaryPath := temporary.Name()
	installed := false
	defer func() {
		_ = temporary.Close()
		if !installed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	var name, originalName, contentType string
	var size int64
	var digest string
	fileSeen := false
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			writeError(response, http.StatusBadRequest, nextErr)
			return
		}
		switch part.FormName() {
		case "name":
			value, readErr := io.ReadAll(io.LimitReader(part, 4097))
			_ = part.Close()
			if readErr != nil || len(value) > 4096 {
				writeError(response, http.StatusBadRequest, errors.New("static resource name is too long"))
				return
			}
			name = string(value)
		case "file":
			if fileSeen || part.FileName() == "" {
				_ = part.Close()
				writeError(response, http.StatusBadRequest, errors.New("exactly one file is required"))
				return
			}
			fileSeen = true
			originalName = filepath.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
			prefix, readErr := io.ReadAll(io.LimitReader(part, 512))
			if readErr != nil {
				_ = part.Close()
				writeError(response, http.StatusBadRequest, readErr)
				return
			}
			hash := sha256.New()
			firstWritten, writeErr := io.MultiWriter(temporary, hash).Write(prefix)
			if writeErr != nil || firstWritten != len(prefix) {
				_ = part.Close()
				writeError(response, http.StatusInternalServerError, errors.New("write uploaded static resource"))
				return
			}
			rest, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(part, domain.MaxStaticAssetBytes-int64(len(prefix))+1))
			_ = part.Close()
			if copyErr != nil {
				writeError(response, http.StatusBadRequest, copyErr)
				return
			}
			size = int64(len(prefix)) + rest
			if size > domain.MaxStaticAssetBytes {
				writeError(response, http.StatusRequestEntityTooLarge, fmt.Errorf("static resources are limited to %d bytes", domain.MaxStaticAssetBytes))
				return
			}
			digest = hex.EncodeToString(hash.Sum(nil))
			contentType = mime.TypeByExtension(strings.ToLower(filepath.Ext(originalName)))
			if contentType == "" || strings.EqualFold(contentType, "application/octet-stream") {
				contentType = http.DetectContentType(prefix)
			}
		default:
			_ = part.Close()
		}
	}
	if !fileSeen {
		writeError(response, http.StatusBadRequest, errors.New("file is required"))
		return
	}
	if strings.TrimSpace(name) == "" {
		name = originalName
	}
	asset, err := domain.NormalizeStaticAsset(domain.StaticAsset{
		Name: name, OriginalName: originalName, SHA256: digest, SizeBytes: size, ContentType: contentType,
	})
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if err := temporary.Sync(); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if err := temporary.Close(); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	destination := filepath.Join(directory, asset.SHA256)
	if info, statErr := os.Lstat(destination); statErr == nil && info.IsDir() {
		writeError(response, http.StatusInternalServerError, errors.New("static resource object path is a directory"))
		return
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		writeError(response, http.StatusInternalServerError, statErr)
		return
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	installed = true
	if err := syncStaticAssetDirectory(directory); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if existing, lookupErr := s.Store.StaticAssetBySHA256(asset.SHA256); lookupErr == nil {
		writeJSON(response, http.StatusOK, existing)
		return
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		writeError(response, http.StatusInternalServerError, lookupErr)
		return
	}
	created, err := s.Store.CreateStaticAsset(asset)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "upload", "static_asset", created.ID, created.Name)
	writeJSON(response, http.StatusCreated, created)
}

func (s *Server) updateStaticAsset(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	asset, err := s.Store.UpdateStaticAssetName(request.PathValue("id"), input.Name)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "update", "static_asset", asset.ID, asset.Name)
	writeJSON(response, http.StatusOK, asset)
}

func (s *Server) deleteStaticAsset(response http.ResponseWriter, request *http.Request) {
	s.staticAssetMu.Lock()
	defer s.staticAssetMu.Unlock()
	asset, err := s.Store.StaticAsset(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	removed := make([]domain.StaticAssetBinding, 0, len(asset.Bindings))
	for _, binding := range asset.Bindings {
		if err := s.Store.DeleteStaticAssetBinding(asset.ID, binding.ID); err != nil {
			s.restoreStaticAssetBindings(removed)
			writeStoreError(response, err)
			return
		}
		removed = append(removed, binding)
	}
	if len(removed) != 0 {
		if err := s.Publisher.PublishAll(); err != nil {
			s.restoreStaticAssetBindings(removed)
			writeStoreError(response, err)
			return
		}
	}
	if _, err := s.Store.DeleteStaticAsset(asset.ID); err != nil {
		writeStoreError(response, err)
		return
	}
	if directory, pathErr := s.staticAssetObjectDirectory(); pathErr == nil {
		if removeErr := os.Remove(filepath.Join(directory, asset.SHA256)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && s.Logger != nil {
			s.Logger.Warn("remove static resource object", "sha256", asset.SHA256, "error", removeErr)
		}
	}
	s.audit(request, adminID(request.Context()), "delete", "static_asset", asset.ID, asset.Name)
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) createStaticAssetBinding(response http.ResponseWriter, request *http.Request) {
	s.staticAssetMu.Lock()
	defer s.staticAssetMu.Unlock()
	var input struct {
		SiteID       string `json:"site_id"`
		URLPath      string `json:"url_path"`
		CacheControl string `json:"cache_control"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	if err := s.ensureStaticAssetSiteCapable(input.SiteID); err != nil {
		writeStoreError(response, err)
		return
	}
	binding, err := s.Store.CreateStaticAssetBinding(domain.StaticAssetBinding{
		AssetID: request.PathValue("id"), SiteID: input.SiteID, URLPath: input.URLPath,
		CacheControl: input.CacheControl,
	})
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if err := s.Publisher.PublishAll(); err != nil {
		_ = s.Store.DeleteStaticAssetBinding(binding.AssetID, binding.ID)
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "create", "static_asset_binding", binding.ID, binding.URLPath)
	writeJSON(response, http.StatusCreated, binding)
}

func (s *Server) updateStaticAssetBinding(response http.ResponseWriter, request *http.Request) {
	s.staticAssetMu.Lock()
	defer s.staticAssetMu.Unlock()
	assetID, bindingID := request.PathValue("id"), request.PathValue("bindingID")
	previous, err := s.Store.StaticAssetBinding(assetID, bindingID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	var input struct {
		SiteID       string `json:"site_id"`
		URLPath      string `json:"url_path"`
		CacheControl string `json:"cache_control"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	if err := s.ensureStaticAssetSiteCapable(input.SiteID); err != nil {
		writeStoreError(response, err)
		return
	}
	updated, err := s.Store.UpdateStaticAssetBinding(assetID, bindingID, domain.StaticAssetBinding{
		SiteID: input.SiteID, URLPath: input.URLPath, CacheControl: input.CacheControl,
	})
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if err := s.Publisher.PublishAll(); err != nil {
		_, _ = s.Store.UpdateStaticAssetBinding(assetID, bindingID, previous)
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "update", "static_asset_binding", updated.ID, updated.URLPath)
	writeJSON(response, http.StatusOK, updated)
}

func (s *Server) deleteStaticAssetBinding(response http.ResponseWriter, request *http.Request) {
	s.staticAssetMu.Lock()
	defer s.staticAssetMu.Unlock()
	assetID, bindingID := request.PathValue("id"), request.PathValue("bindingID")
	binding, err := s.Store.StaticAssetBinding(assetID, bindingID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if err := s.Store.DeleteStaticAssetBinding(assetID, bindingID); err != nil {
		writeStoreError(response, err)
		return
	}
	if err := s.Publisher.PublishAll(); err != nil {
		_ = s.Store.RestoreStaticAssetBinding(binding)
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "delete", "static_asset_binding", binding.ID, binding.URLPath)
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) edgeStaticAsset(response http.ResponseWriter, request *http.Request) {
	digest := strings.ToLower(strings.TrimSpace(request.PathValue("sha256")))
	if !domain.ValidStaticAssetSHA256(digest) {
		writeError(response, http.StatusNotFound, store.ErrNotFound)
		return
	}
	state, _, err := s.Store.NodeState(edgeNodeID(request.Context()))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	expectedSize := int64(-1)
	for _, reference := range state.StaticAssets {
		if reference.SHA256 == digest {
			expectedSize = reference.SizeBytes
			break
		}
	}
	if expectedSize < 0 {
		writeError(response, http.StatusNotFound, store.ErrNotFound)
		return
	}
	directory, err := s.staticAssetObjectDirectory()
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, err)
		return
	}
	objectPath := filepath.Join(directory, digest)
	pathInfo, err := os.Lstat(objectPath)
	if errors.Is(err, os.ErrNotExist) {
		writeError(response, http.StatusNotFound, store.ErrNotFound)
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if !pathInfo.Mode().IsRegular() {
		writeError(response, http.StatusInternalServerError, errors.New("static resource object is not a regular file"))
		return
	}
	file, err := os.Open(objectPath)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(pathInfo, info) || info.Size() != expectedSize {
		writeError(response, http.StatusInternalServerError, errors.New("static resource object is unavailable or inconsistent"))
		return
	}
	response.Header().Set("Content-Type", "application/octet-stream")
	response.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	response.Header().Set("ETag", `"sha256-`+digest+`"`)
	response.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(response, request, digest, info.ModTime(), file)
}

func (s *Server) ensureStaticAssetSiteCapable(siteID string) error {
	site, _, err := s.Store.GetSite(strings.TrimSpace(siteID))
	if err != nil {
		return err
	}
	if site.TCPOnly || site.Deleting {
		return errors.New("static resources require an active HTTP site")
	}
	for _, nodeID := range site.AssignedNodeIDs() {
		node, err := s.Store.GetNode(nodeID)
		if err != nil {
			return err
		}
		if !slices.Contains(node.Capabilities, domain.EdgeCapabilityStaticAssets) {
			return fmt.Errorf("edge %s must be upgraded before assigning static resources", node.Name)
		}
	}
	return nil
}

func (s *Server) restoreStaticAssetBindings(bindings []domain.StaticAssetBinding) {
	for _, binding := range bindings {
		if err := s.Store.RestoreStaticAssetBinding(binding); err != nil && s.Logger != nil {
			s.Logger.Error("restore static resource binding", "binding_id", binding.ID, "error", err)
		}
	}
}

func (s *Server) staticAssetObjectDirectory() (string, error) {
	directory := strings.TrimSpace(s.StaticAssetDirectory)
	if directory == "" {
		return "", errors.New("static resource storage is not configured")
	}
	absolute, err := filepath.Abs(filepath.Clean(directory))
	if err != nil || absolute != filepath.Clean(directory) {
		return "", errors.New("static resource storage path must be absolute")
	}
	return absolute, nil
}

func syncStaticAssetDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
