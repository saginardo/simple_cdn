package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"simple_cdn/internal/domain"
)

func TestStaticAssetAndBindingLifecycle(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("static-edge", "203.0.113.90")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "static-site", Domains: []string{"static.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-static")
	if err != nil {
		t.Fatal(err)
	}
	asset, err := database.CreateStaticAsset(domain.StaticAsset{
		Name: "Logo", OriginalName: "logo.svg", SHA256: strings.Repeat("a", 64),
		SizeBytes: 128, ContentType: "image/svg+xml; charset=utf-8",
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := database.ListStaticAssets()
	if err != nil || len(listed) != 1 || listed[0].Bindings == nil || len(listed[0].Bindings) != 0 {
		t.Fatalf("unbound assets = %#v, err = %v", listed, err)
	}
	binding, err := database.CreateStaticAssetBinding(domain.StaticAssetBinding{
		AssetID: asset.ID, SiteID: site.ID, URLPath: "/brand/logo.svg",
		CacheControl: domain.StaticAssetCacheImmutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := database.StaticAsset(asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ContentType != "image/svg+xml" || len(loaded.Bindings) != 1 || loaded.Bindings[0].ID != binding.ID {
		t.Fatalf("loaded asset = %#v", loaded)
	}
	references, err := database.ListStaticAssetReferences()
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].SHA256 != asset.SHA256 || references[0].SiteID != site.ID || references[0].URLPath != "/brand/logo.svg" {
		t.Fatalf("references = %#v", references)
	}
	updated, err := database.UpdateStaticAssetBinding(asset.ID, binding.ID, domain.StaticAssetBinding{
		SiteID: site.ID, URLPath: "/logo.svg", CacheControl: domain.StaticAssetCacheNoCache,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.URLPath != "/logo.svg" || updated.CacheControl != domain.StaticAssetCacheNoCache {
		t.Fatalf("updated binding = %#v", updated)
	}
	renamed, err := database.UpdateStaticAssetName(asset.ID, "Primary logo")
	if err != nil || renamed.Name != "Primary logo" {
		t.Fatalf("renamed asset = %#v, err = %v", renamed, err)
	}
	deleted, err := database.DeleteStaticAsset(asset.ID)
	if err != nil || len(deleted.Bindings) != 1 {
		t.Fatalf("deleted asset = %#v, err = %v", deleted, err)
	}
	if _, err := database.StaticAsset(asset.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted asset lookup error = %v", err)
	}
	references, err = database.ListStaticAssetReferences()
	if err != nil || len(references) != 0 {
		t.Fatalf("references after cascade = %#v, err = %v", references, err)
	}
}

func TestStaticAssetBindingRejectsDuplicateSitePath(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, _ := database.CreateNode("static-edge", "203.0.113.91")
	site, err := database.CreateSite(domain.Site{
		Name: "static-site", Domains: []string{"static-duplicate.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-static")
	if err != nil {
		t.Fatal(err)
	}
	for index, digest := range []string{strings.Repeat("b", 64), strings.Repeat("c", 64)} {
		asset, createErr := database.CreateStaticAsset(domain.StaticAsset{
			Name: "Asset", OriginalName: "asset.txt", SHA256: digest, SizeBytes: int64(index + 1), ContentType: "text/plain",
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		_, createErr = database.CreateStaticAssetBinding(domain.StaticAssetBinding{
			AssetID: asset.ID, SiteID: site.ID, URLPath: "/same.txt", CacheControl: domain.StaticAssetCacheHour,
		})
		if index == 0 && createErr != nil {
			t.Fatal(createErr)
		}
		if index == 1 && createErr == nil {
			t.Fatal("accepted duplicate site URL path")
		}
	}
}
