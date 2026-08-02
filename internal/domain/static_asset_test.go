package domain

import (
	"strings"
	"testing"
)

func TestNormalizeStaticAsset(t *testing.T) {
	asset, err := NormalizeStaticAsset(StaticAsset{
		Name: "  status page  ", OriginalName: "status.txt", SHA256: strings.Repeat("A", 64),
		SizeBytes: 12, ContentType: "text/plain; charset=utf-8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.Name != "status page" || asset.SHA256 != strings.Repeat("a", 64) || asset.ContentType != "text/plain" {
		t.Fatalf("normalized asset = %#v", asset)
	}
}

func TestStaticAssetURLPathValidation(t *testing.T) {
	for _, value := range []string{"/logo.svg", "/assets/v1/app.js", "/favicon.ico"} {
		if !ValidStaticAssetURLPath(value) {
			t.Errorf("valid path %q was rejected", value)
		}
	}
	for _, value := range []string{"", "/", "relative.js", "/a/../b", "/a//b", "/_cdn/pow/verify", "/__cdn_health", "/a?x=1", "/encoded%2Fpath"} {
		if ValidStaticAssetURLPath(value) {
			t.Errorf("invalid path %q was accepted", value)
		}
	}
}

func TestNormalizeStaticAssetBindingDefaultsCachePolicy(t *testing.T) {
	binding, err := NormalizeStaticAssetBinding(StaticAssetBinding{AssetID: "asset", SiteID: "site", URLPath: "/app.js"})
	if err != nil {
		t.Fatal(err)
	}
	if binding.CacheControl != StaticAssetCacheHour {
		t.Fatalf("cache policy = %q", binding.CacheControl)
	}
	binding.CacheControl = "public, max-age=123"
	if _, err := NormalizeStaticAssetBinding(binding); err == nil {
		t.Fatal("accepted arbitrary cache policy")
	}
}
