package domain

import (
	"errors"
	"mime"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	EdgeCapabilityStaticAssets       = "static_assets_v1"
	MaxStaticAssetBytes        int64 = 32 << 20
	StaticAssetCacheHour             = "public, max-age=3600"
	StaticAssetCacheDay              = "public, max-age=86400"
	StaticAssetCacheImmutable        = "public, max-age=31536000, immutable"
	StaticAssetCacheNoCache          = "no-cache"
)

var staticAssetDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var staticAssetContentTypePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#&^_.+-]*/[A-Za-z0-9][A-Za-z0-9!#&^_.+-]*$`)

type StaticAsset struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	OriginalName string               `json:"original_name"`
	SHA256       string               `json:"sha256"`
	SizeBytes    int64                `json:"size_bytes"`
	ContentType  string               `json:"content_type"`
	Bindings     []StaticAssetBinding `json:"bindings"`
	CreatedAt    time.Time            `json:"created_at"`
	UpdatedAt    time.Time            `json:"updated_at"`
}

type StaticAssetBinding struct {
	ID           string    `json:"id"`
	AssetID      string    `json:"asset_id"`
	SiteID       string    `json:"site_id"`
	URLPath      string    `json:"url_path"`
	CacheControl string    `json:"cache_control"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type StaticAssetReference struct {
	AssetID      string `json:"asset_id"`
	BindingID    string `json:"binding_id"`
	SiteID       string `json:"site_id"`
	URLPath      string `json:"url_path"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"size_bytes"`
	ContentType  string `json:"content_type"`
	CacheControl string `json:"cache_control"`
}

func NormalizeStaticAsset(asset StaticAsset) (StaticAsset, error) {
	asset.Name = strings.TrimSpace(asset.Name)
	asset.OriginalName = strings.TrimSpace(asset.OriginalName)
	asset.SHA256 = strings.ToLower(strings.TrimSpace(asset.SHA256))
	asset.ContentType = strings.TrimSpace(asset.ContentType)
	if asset.Name == "" || len(asset.Name) > 120 || strings.ContainsAny(asset.Name, "\x00\r\n") {
		return StaticAsset{}, errors.New("static asset name must be 1-120 characters")
	}
	if asset.OriginalName == "" || len(asset.OriginalName) > 255 || path.Base(asset.OriginalName) != asset.OriginalName ||
		strings.ContainsAny(asset.OriginalName, "\x00\r\n/\\") {
		return StaticAsset{}, errors.New("static asset original filename is invalid")
	}
	if !ValidStaticAssetSHA256(asset.SHA256) {
		return StaticAsset{}, errors.New("static asset SHA-256 is invalid")
	}
	if asset.SizeBytes < 0 || asset.SizeBytes > MaxStaticAssetBytes {
		return StaticAsset{}, errors.New("static asset size is out of range")
	}
	mediaType, err := normalizeStaticAssetContentType(asset.ContentType)
	if err != nil {
		return StaticAsset{}, errors.New("static asset content type is invalid")
	}
	asset.ContentType = mediaType
	return asset, nil
}

func NormalizeStaticAssetBinding(binding StaticAssetBinding) (StaticAssetBinding, error) {
	binding.AssetID = strings.TrimSpace(binding.AssetID)
	binding.SiteID = strings.TrimSpace(binding.SiteID)
	binding.URLPath = strings.TrimSpace(binding.URLPath)
	binding.CacheControl = strings.TrimSpace(binding.CacheControl)
	if binding.AssetID == "" || binding.SiteID == "" {
		return StaticAssetBinding{}, errors.New("static asset and site are required")
	}
	if !ValidStaticAssetURLPath(binding.URLPath) {
		return StaticAssetBinding{}, errors.New("static asset URL must be a clean absolute path")
	}
	if binding.CacheControl == "" {
		binding.CacheControl = StaticAssetCacheHour
	}
	if !ValidStaticAssetCacheControl(binding.CacheControl) {
		return StaticAssetBinding{}, errors.New("static asset cache policy is not supported")
	}
	return binding, nil
}

func ValidStaticAssetSHA256(value string) bool {
	return staticAssetDigestPattern.MatchString(strings.ToLower(strings.TrimSpace(value)))
}

func NormalizeStaticAssetReference(reference StaticAssetReference) (StaticAssetReference, error) {
	reference.AssetID = strings.TrimSpace(reference.AssetID)
	reference.BindingID = strings.TrimSpace(reference.BindingID)
	reference.SiteID = strings.TrimSpace(reference.SiteID)
	reference.URLPath = strings.TrimSpace(reference.URLPath)
	reference.SHA256 = strings.ToLower(strings.TrimSpace(reference.SHA256))
	reference.CacheControl = strings.TrimSpace(reference.CacheControl)
	if reference.AssetID == "" || reference.BindingID == "" || reference.SiteID == "" {
		return StaticAssetReference{}, errors.New("static asset reference IDs are required")
	}
	if !ValidStaticAssetURLPath(reference.URLPath) || !ValidStaticAssetSHA256(reference.SHA256) {
		return StaticAssetReference{}, errors.New("static asset reference path or SHA-256 is invalid")
	}
	if reference.SizeBytes < 0 || reference.SizeBytes > MaxStaticAssetBytes {
		return StaticAssetReference{}, errors.New("static asset reference size is out of range")
	}
	contentType, err := normalizeStaticAssetContentType(reference.ContentType)
	if err != nil {
		return StaticAssetReference{}, errors.New("static asset reference content type is invalid")
	}
	reference.ContentType = contentType
	if !ValidStaticAssetCacheControl(reference.CacheControl) {
		return StaticAssetReference{}, errors.New("static asset reference cache policy is not supported")
	}
	return reference, nil
}

func ValidStaticAssetURLPath(value string) bool {
	if value == "" || len(value) > 1024 || value == "/" || !strings.HasPrefix(value, "/") ||
		strings.ContainsAny(value, "\x00\r\n\t \"{};$\\") || path.Clean(value) != value ||
		value == "/__cdn_health" || strings.HasPrefix(value, "/_cdn/") {
		return false
	}
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Path == value
}

func normalizeStaticAssetContentType(value string) (string, error) {
	value = strings.TrimSpace(value)
	mediaType, _, err := mime.ParseMediaType(value)
	mediaType = strings.ToLower(mediaType)
	if err != nil || len(value) > 200 || !staticAssetContentTypePattern.MatchString(mediaType) {
		return "", errors.New("invalid content type")
	}
	return mediaType, nil
}

func ValidStaticAssetCacheControl(value string) bool {
	switch value {
	case StaticAssetCacheHour, StaticAssetCacheDay, StaticAssetCacheImmutable, StaticAssetCacheNoCache:
		return true
	default:
		return false
	}
}
