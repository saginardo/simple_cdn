package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestSiteJSONDefaultsDynamicCompressionForLegacySnapshots(t *testing.T) {
	var site Site
	if err := json.Unmarshal([]byte(`{"id":"site-1"}`), &site); err != nil {
		t.Fatal(err)
	}
	if !site.DynamicCompressionEnabled || !slices.Equal(site.CompressionExcludedMIMETypes, []string{"text/event-stream"}) {
		t.Fatalf("legacy compression defaults = enabled %v, exclusions %#v", site.DynamicCompressionEnabled, site.CompressionExcludedMIMETypes)
	}
	if err := json.Unmarshal([]byte(`{"dynamic_compression_enabled":false,"compression_excluded_mime_types":[]}`), &site); err != nil {
		t.Fatal(err)
	}
	if site.DynamicCompressionEnabled || len(site.CompressionExcludedMIMETypes) != 0 {
		t.Fatalf("explicit compression settings were not preserved: %#v", site)
	}
}

func TestCompressionMIMEExclusionsNormalizeAndDriveStaticEligibility(t *testing.T) {
	exclusions, err := NormalizeCompressionExcludedMIMETypes([]string{" Text/Event-Stream ", "application/json", "application/json"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(exclusions, []string{"application/json", "text/event-stream"}) {
		t.Fatalf("normalized exclusions = %#v", exclusions)
	}
	types := DynamicCompressionMIMETypes(exclusions)
	if slices.Contains(types, "application/json") || slices.Contains(types, "text/event-stream") || !slices.Contains(types, "text/css") {
		t.Fatalf("dynamic compression types = %#v", types)
	}
	if _, err := NormalizeCompressionExcludedMIMETypes([]string{"text/html"}); err == nil {
		t.Fatal("accepted text/html, which Nginx compression filters cannot exclude")
	}
	if !PrecompressibleStaticAsset("application/javascript; charset=utf-8", 1024) ||
		PrecompressibleStaticAsset("text/event-stream", 1024) ||
		PrecompressibleStaticAsset("application/javascript", MinPrecompressedStaticAssetSize-1) {
		t.Fatal("static precompression eligibility is incorrect")
	}
}

func TestCacheInvalidationTargetsAndWarmupLimits(t *testing.T) {
	for _, test := range []struct {
		scope CacheInvalidationScope
		value string
	}{
		{CacheInvalidationURL, "/assets/app.js?v=2"},
		{CacheInvalidationPrefix, "/assets/v2/"},
	} {
		if got, err := NormalizeCacheInvalidationTarget(test.scope, test.value); err != nil || got != test.value {
			t.Fatalf("normalize %s %q = %q, %v", test.scope, test.value, got, err)
		}
	}
	for _, value := range []string{"/", "/assets/../secret", "/assets/?v=1", "/__cdn_health"} {
		if _, err := NormalizeCacheInvalidationTarget(CacheInvalidationPrefix, value); err == nil {
			t.Fatalf("accepted unsafe cache prefix %q", value)
		}
	}
	paths := make([]string, 0, MaxCacheWarmupPaths*2)
	for index := 0; index < MaxCacheWarmupPaths; index++ {
		path := fmt.Sprintf("/asset-%d.js", index)
		paths = append(paths, path, path)
	}
	if normalized, err := NormalizeCacheWarmupPaths(paths); err != nil || len(normalized) != MaxCacheWarmupPaths {
		t.Fatalf("deduplicated warmup paths = %d, %v", len(normalized), err)
	}

	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	jobs := make([]CacheWarmup, MaxCacheWarmups+1)
	for index := range jobs {
		jobs[index] = CacheWarmup{ID: fmt.Sprintf("job-%d", index), SiteID: "site", Host: "cdn.example.test", Paths: []string{"/app.js"}, CreatedAt: now}
	}
	if _, err := NormalizeCacheWarmups(jobs); err == nil {
		t.Fatal("site-level warmup limit was not enforced")
	}
	if normalized, err := NormalizeDesiredCacheWarmups(jobs); err != nil || len(normalized) != len(jobs) {
		t.Fatalf("multi-site desired warmups = %d, %v", len(normalized), err)
	}
}
