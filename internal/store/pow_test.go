package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"simple_cdn/internal/domain"
)

func TestPOWPolicyLifecycle(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("pow-edge", "203.0.113.210")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "pow-site", Domains: []string{"pow.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "http://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("encrypted-secret-material")
	created, err := database.CreatePOWPolicy(domain.POWPolicy{
		Name: "browser validation", Enabled: true, SiteIDs: []string{site.ID}, Priority: 100,
	}, secret)
	if err != nil || created.ID == "" || created.PathPattern != `^/` {
		t.Fatalf("created proof-of-work policy = %#v, err=%v", created, err)
	}
	materials, err := database.ListEnabledPOWPolicyMaterials()
	if err != nil || len(materials) != 1 || !bytes.Equal(materials[0].SecretCiphertext, secret) {
		t.Fatalf("proof-of-work materials = %#v, err=%v", materials, err)
	}
	created.DifficultyBits = 20
	created.Enabled = false
	updated, err := database.UpdatePOWPolicy(created.ID, created)
	if err != nil || updated.DifficultyBits != 20 || updated.Enabled {
		t.Fatalf("updated proof-of-work policy = %#v, err=%v", updated, err)
	}
	materials, err = database.ListEnabledPOWPolicyMaterials()
	if err != nil || len(materials) != 0 {
		t.Fatalf("disabled proof-of-work materials = %#v, err=%v", materials, err)
	}
	if err := database.DeletePOWPolicy(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.POWPolicy(created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted proof-of-work policy lookup = %v", err)
	}
}

func TestPOWPolicyRejectsUnknownSite(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.CreatePOWPolicy(domain.POWPolicy{
		Name: "unknown", SiteIDs: []string{"missing-site"}, Priority: 1,
	}, []byte("ciphertext"))
	if err == nil {
		t.Fatal("proof-of-work policy accepted an unknown site")
	}
}
