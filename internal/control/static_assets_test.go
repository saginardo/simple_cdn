package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func TestStaticAssetUploadStoresContentAddressedObjectAndDeduplicates(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	directory := filepath.Join(t.TempDir(), "objects")
	server := &Server{Store: database, StaticAssetDirectory: directory}
	contents := []byte("console.log('managed static resource');\n")

	upload := func() (*httptest.ResponseRecorder, domain.StaticAsset) {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		if err := writer.WriteField("name", "Application bundle"); err != nil {
			t.Fatal(err)
		}
		part, err := writer.CreateFormFile("file", "app.js")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(contents); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/static-assets", body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request = request.WithContext(context.WithValue(request.Context(), adminContextKey{}, "admin"))
		response := httptest.NewRecorder()
		server.uploadStaticAsset(response, request)
		var asset domain.StaticAsset
		if err := json.Unmarshal(response.Body.Bytes(), &asset); err != nil {
			t.Fatalf("decode upload response %d %q: %v", response.Code, response.Body.String(), err)
		}
		return response, asset
	}

	firstResponse, first := upload()
	if firstResponse.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d: %s", firstResponse.Code, firstResponse.Body.String())
	}
	wantedDigest := fmt.Sprintf("%x", sha256.Sum256(contents))
	if first.ID == "" || first.SHA256 != wantedDigest || first.SizeBytes != int64(len(contents)) || first.Name != "Application bundle" {
		t.Fatalf("uploaded asset = %#v", first)
	}
	stored, err := os.ReadFile(filepath.Join(directory, wantedDigest))
	if err != nil || !bytes.Equal(stored, contents) {
		t.Fatalf("stored object = %q, err = %v", stored, err)
	}
	secondResponse, second := upload()
	if secondResponse.Code != http.StatusOK || second.ID != first.ID {
		t.Fatalf("deduplicated upload = status %d, asset %#v", secondResponse.Code, second)
	}
	assets, err := database.ListStaticAssets()
	if err != nil || len(assets) != 1 {
		t.Fatalf("stored assets = %#v, err = %v", assets, err)
	}
}

func TestStaticAssetBindingPublishesAndRemovesExactLocation(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, _ := NewEncryptionKey()
	cipher, _ := NewCipher(key)
	publisher := Publisher{Store: database, Cipher: cipher}
	node, err := database.CreateNode("static-edge", "203.0.113.92")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilityStaticAssets}); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "static-site", Domains: []string{"static-api.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-static")
	if err != nil {
		t.Fatal(err)
	}
	certificate, privateKey, notAfter := testCertificate(t, site.Domains...)
	if err := publisher.StoreCertificate(site.ID, certificate, privateKey, notAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishSite(site.ID); err != nil {
		t.Fatal(err)
	}
	asset, err := database.CreateStaticAsset(domain.StaticAsset{
		Name: "robots", OriginalName: "robots.txt", SHA256: strings.Repeat("d", 64),
		SizeBytes: 12, ContentType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, Cipher: cipher, Publisher: publisher, StaticAssetDirectory: t.TempDir()}
	request := httptest.NewRequest(http.MethodPost, "/api/static-assets/"+asset.ID+"/bindings",
		strings.NewReader(fmt.Sprintf(`{"site_id":%q,"url_path":"/robots.txt","cache_control":%q}`,
			site.ID, domain.StaticAssetCacheHour)))
	request.SetPathValue("id", asset.ID)
	request = request.WithContext(context.WithValue(request.Context(), adminContextKey{}, "admin"))
	response := httptest.NewRecorder()
	server.createStaticAssetBinding(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create binding status = %d: %s", response.Code, response.Body.String())
	}
	var binding domain.StaticAssetBinding
	if err := json.Unmarshal(response.Body.Bytes(), &binding); err != nil {
		t.Fatal(err)
	}
	state, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.StaticAssets) != 1 || state.StaticAssets[0].BindingID != binding.ID ||
		!strings.Contains(state.NginxConfig, `location = "/robots.txt"`) {
		t.Fatalf("published static asset state = %#v\n%s", state.StaticAssets, state.NginxConfig)
	}
	if err := database.SetNodeCapabilities(node.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := server.reconcileEdgeRuntimeCapabilities(node.ID, nil); err != nil {
		t.Fatal(err)
	}
	state, _, err = database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.StaticAssets) != 0 || strings.Contains(state.NginxConfig, `location = "/robots.txt"`) {
		t.Fatalf("static asset survived capability removal = %#v\n%s", state.StaticAssets, state.NginxConfig)
	}
	capabilities := []string{domain.EdgeCapabilityStaticAssets}
	if err := database.SetNodeCapabilities(node.ID, capabilities); err != nil {
		t.Fatal(err)
	}
	if err := server.reconcileEdgeRuntimeCapabilities(node.ID, capabilities); err != nil {
		t.Fatal(err)
	}
	state, _, err = database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.StaticAssets) != 1 || !strings.Contains(state.NginxConfig, `location = "/robots.txt"`) {
		t.Fatalf("static asset was not restored after capability recovery = %#v\n%s", state.StaticAssets, state.NginxConfig)
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/static-assets/"+asset.ID+"/bindings/"+binding.ID, nil)
	deleteRequest.SetPathValue("id", asset.ID)
	deleteRequest.SetPathValue("bindingID", binding.ID)
	deleteRequest = deleteRequest.WithContext(context.WithValue(deleteRequest.Context(), adminContextKey{}, "admin"))
	deleteResponse := httptest.NewRecorder()
	server.deleteStaticAssetBinding(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete binding status = %d: %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	state, _, err = database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.StaticAssets) != 0 || strings.Contains(state.NginxConfig, `location = "/robots.txt"`) {
		t.Fatalf("removed static asset remains in state = %#v\n%s", state.StaticAssets, state.NginxConfig)
	}
}

func TestEdgeStaticAssetOnlyServesObjectsInNodeDesiredState(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("authorized-edge", "203.0.113.93")
	if err != nil {
		t.Fatal(err)
	}
	otherNode, err := database.CreateNode("other-edge", "203.0.113.94")
	if err != nil {
		t.Fatal(err)
	}
	contents := []byte("edge object")
	digest := fmt.Sprintf("%x", sha256.Sum256(contents))
	reference := domain.StaticAssetReference{
		AssetID: "asset", BindingID: "binding", SiteID: "site", URLPath: "/object.txt",
		SHA256: digest, SizeBytes: int64(len(contents)), ContentType: "text/plain",
		CacheControl: domain.StaticAssetCacheHour,
	}
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 1, StaticAssets: []domain.StaticAssetReference{reference}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveNodeState(otherNode.ID, domain.DesiredState{Version: 1}, nil); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, digest), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, StaticAssetDirectory: directory}

	request := httptest.NewRequest(http.MethodGet, "/api/edge/v1/static-assets/"+digest, nil)
	request.SetPathValue("sha256", digest)
	request = request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, node.ID))
	response := httptest.NewRecorder()
	server.edgeStaticAsset(response, request)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), contents) {
		t.Fatalf("authorized download = %d %q", response.Code, response.Body.Bytes())
	}
	linkedObject := filepath.Join(directory, "linked-object")
	if err := os.WriteFile(linkedObject, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, digest)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkedObject, filepath.Join(directory, digest)); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/edge/v1/static-assets/"+digest, nil)
	request.SetPathValue("sha256", digest)
	request = request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, node.ID))
	response = httptest.NewRecorder()
	server.edgeStaticAsset(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("symlinked object download status = %d, want %d", response.Code, http.StatusInternalServerError)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/edge/v1/static-assets/"+digest, nil)
	request.SetPathValue("sha256", digest)
	request = request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, otherNode.ID))
	response = httptest.NewRecorder()
	server.edgeStaticAsset(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unauthorized node download status = %d", response.Code)
	}
}
