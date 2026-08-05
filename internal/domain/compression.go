package domain

import (
	"errors"
	"mime"
	"slices"
	"sort"
	"strings"
)

const (
	EdgeCapabilityCompression       = "compression_v1"
	MinPrecompressedStaticAssetSize = int64(256)
	MaxCompressionMIMEExclusions    = 32
)

var compressionMIMETypes = []string{
	"application/atom+xml",
	"application/javascript",
	"application/json",
	"application/ld+json",
	"application/manifest+json",
	"application/rss+xml",
	"application/wasm",
	"application/xhtml+xml",
	"application/xml",
	"application/x-javascript",
	"application/vnd.ms-fontobject",
	"font/eot",
	"font/otf",
	"font/ttf",
	"image/svg+xml",
	"text/css",
	"text/event-stream",
	"text/javascript",
	"text/plain",
	"text/xml",
}

func DefaultCompressionExcludedMIMETypes() []string {
	return []string{"text/event-stream"}
}

func NormalizeCompressionExcludedMIMETypes(values []string) ([]string, error) {
	if len(values) > MaxCompressionMIMEExclusions {
		return nil, errors.New("too many compression MIME exclusions")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		mediaType, parameters, err := mime.ParseMediaType(value)
		mediaType = strings.ToLower(mediaType)
		if err != nil || len(parameters) != 0 || len(value) > 200 || !staticAssetContentTypePattern.MatchString(mediaType) {
			return nil, errors.New("compression exclusions must be MIME types without parameters")
		}
		if mediaType == "text/html" {
			return nil, errors.New("text/html cannot be excluded by the Nginx compression filters")
		}
		if _, found := seen[mediaType]; found {
			continue
		}
		seen[mediaType] = struct{}{}
		result = append(result, mediaType)
	}
	sort.Strings(result)
	return result, nil
}

func DynamicCompressionMIMETypes(excluded []string) []string {
	excludedSet := make(map[string]struct{}, len(excluded))
	for _, value := range excluded {
		excludedSet[strings.ToLower(strings.TrimSpace(value))] = struct{}{}
	}
	result := make([]string, 0, len(compressionMIMETypes))
	for _, value := range compressionMIMETypes {
		if _, found := excludedSet[value]; !found {
			result = append(result, value)
		}
	}
	return result
}

func PrecompressibleStaticAsset(contentType string, size int64) bool {
	contentType, _, _ = strings.Cut(strings.ToLower(strings.TrimSpace(contentType)), ";")
	return size >= MinPrecompressedStaticAssetSize &&
		slices.Contains(compressionMIMETypes, contentType) &&
		!slices.Contains(DefaultCompressionExcludedMIMETypes(), contentType)
}
