package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func TestCacheOperationsAPITracksNodeWarmupResultAndRetriesWithoutInvalidatingAgain(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-cache-results", "203.0.113.97")
	if err != nil {
		t.Fatal(err)
	}
	capabilities := []string{domain.EdgeCapabilityCacheControl, domain.EdgeCapabilityCacheWarmupResults}
	if err := database.SetNodeCapabilities(node.ID, capabilities); err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(node.ID, 0, "", nil); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "cache-console", Domains: []string{"cdn.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin:        domain.Origin{URL: "https://origin.example.test", Enabled: true},
		RequestBodyBuffering: true, OriginResponseBuffering: true, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	certificate, privateKey, notAfter := testCertificate(t, "cdn.example.test")
	if err := publisher.StoreCertificate(site.ID, certificate, privateKey, notAfter); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, Publisher: publisher}

	createdResponse := requestSiteResponse(t, server, http.MethodPost, "/api/cache/operations", map[string]any{
		"site_id": site.ID, "scope": "url", "value": "/assets/app.js", "prewarm": true,
	})
	if createdResponse.Code != http.StatusAccepted {
		t.Fatalf("create cache operation = %d %s", createdResponse.Code, createdResponse.Body.String())
	}
	var operation domain.CacheOperation
	if err := json.NewDecoder(createdResponse.Body).Decode(&operation); err != nil {
		t.Fatal(err)
	}
	if operation.Kind != domain.CacheOperationInvalidate || operation.Status != domain.CacheOperationApplying ||
		len(operation.PrewarmPaths) != 1 || operation.PrewarmPaths[0] != "/assets/app.js" || len(operation.Nodes) != 1 ||
		operation.Nodes[0].WarmupStatus != domain.CacheWarmupPending {
		t.Fatalf("created cache operation = %#v", operation)
	}
	state, _, err := database.NodeState(node.ID)
	if err != nil || len(state.CacheWarmups) != 1 || state.CacheWarmups[0].ID != operation.ID {
		t.Fatalf("published cache warmup state = %#v, %v", state.CacheWarmups, err)
	}

	completedAt := time.Now().UTC().Add(-time.Second)
	payload, err := json.Marshal(heartbeatRequest{
		AppliedVersion: state.Version,
		ApplyReport:    &domain.ApplyReport{Version: state.Version, Status: domain.ApplySucceeded, Detail: "Nginx is listening"},
		Capabilities:   capabilities,
		CacheWarmupResults: []domain.CacheWarmupResult{{
			WarmupID: operation.ID, SiteID: site.ID, Status: domain.CacheWarmupSucceeded,
			AttemptedURLs: 1, SucceededURLs: 1, CompletedAt: completedAt,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := httptest.NewRequest(http.MethodPost, "/api/edge/v1/heartbeat", bytes.NewReader(payload))
	heartbeat = heartbeat.WithContext(context.WithValue(heartbeat.Context(), edgeContextKey{}, node.ID))
	heartbeatResponse := httptest.NewRecorder()
	server.heartbeat(heartbeatResponse, heartbeat)
	if heartbeatResponse.Code != http.StatusOK {
		t.Fatalf("cache result heartbeat = %d %s", heartbeatResponse.Code, heartbeatResponse.Body.String())
	}

	overviewResponse := requestSiteResponse(t, server, http.MethodGet, "/api/cache/overview", map[string]any{})
	if overviewResponse.Code != http.StatusOK {
		t.Fatalf("cache overview = %d %s", overviewResponse.Code, overviewResponse.Body.String())
	}
	var overview cacheOperationsOverviewResponse
	if err := json.NewDecoder(overviewResponse.Body).Decode(&overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Operations) != 1 || overview.Operations[0].Status != domain.CacheOperationSucceeded ||
		len(overview.Operations[0].Nodes) != 1 || overview.Operations[0].Nodes[0].WarmupStatus != domain.CacheWarmupSucceeded ||
		overview.Operations[0].Nodes[0].SucceededURLs != 1 || len(overview.Sites) != 1 || overview.Sites[0].ReportingNodeCount != 1 {
		t.Fatalf("cache overview after result = %#v", overview)
	}
	generation := overview.Operations[0].CacheGeneration

	retryResponse := requestSiteResponse(t, server, http.MethodPost, "/api/cache/operations/"+operation.ID+"/retry", map[string]any{})
	if retryResponse.Code != http.StatusAccepted {
		t.Fatalf("retry cache prewarm = %d %s", retryResponse.Code, retryResponse.Body.String())
	}
	var retry domain.CacheOperation
	if err := json.NewDecoder(retryResponse.Body).Decode(&retry); err != nil {
		t.Fatal(err)
	}
	if retry.Kind != domain.CacheOperationPrewarmRetry || retry.RetryOfID != operation.ID || retry.CacheGeneration != generation || retry.ID == operation.ID {
		t.Fatalf("cache prewarm retry = %#v", retry)
	}
	updated, _, err := database.GetSite(site.ID)
	if err != nil || updated.CacheGeneration != generation || updated.CacheWarmups[len(updated.CacheWarmups)-1].ID != retry.ID {
		t.Fatalf("site after prewarm retry = %#v, %v", updated, err)
	}

	retryState, _, err := database.NodeState(node.ID)
	if err != nil || retryState.Version <= state.Version {
		t.Fatalf("retry desired state = %#v, %v", retryState, err)
	}
	retryPayload, err := json.Marshal(heartbeatRequest{
		AppliedVersion: retryState.Version,
		ApplyReport:    &domain.ApplyReport{Version: retryState.Version, Status: domain.ApplySucceeded, Detail: "Nginx is listening"},
		Capabilities:   capabilities,
		CacheWarmupResults: []domain.CacheWarmupResult{{
			WarmupID: retry.ID, SiteID: site.ID, Status: domain.CacheWarmupFailed,
			AttemptedURLs: 1,
			Failures: []domain.CacheWarmupFailure{{
				Path: "/assets/app.js", Detail: "origin returned 503 Service Unavailable",
			}},
			CompletedAt: time.Now().UTC(),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	retryHeartbeat := httptest.NewRequest(http.MethodPost, "/api/edge/v1/heartbeat", bytes.NewReader(retryPayload))
	retryHeartbeat = retryHeartbeat.WithContext(context.WithValue(retryHeartbeat.Context(), edgeContextKey{}, node.ID))
	retryHeartbeatResponse := httptest.NewRecorder()
	server.heartbeat(retryHeartbeatResponse, retryHeartbeat)
	if retryHeartbeatResponse.Code != http.StatusOK {
		t.Fatalf("retry result heartbeat = %d %s", retryHeartbeatResponse.Code, retryHeartbeatResponse.Body.String())
	}

	detailResponse := requestSiteResponse(t, server, http.MethodGet, "/api/cache/operations/"+retry.ID, map[string]any{})
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("retry operation detail = %d %s", detailResponse.Code, detailResponse.Body.String())
	}
	var detail domain.CacheOperation
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detail.Status != domain.CacheOperationPartial || len(detail.Nodes) != 1 ||
		detail.Nodes[0].WarmupStatus != domain.CacheWarmupFailed || detail.Nodes[0].AttemptedURLs != 1 ||
		len(detail.Nodes[0].Failures) != 1 || detail.Nodes[0].Failures[0].Path != "/assets/app.js" ||
		detail.Nodes[0].Failures[0].Detail != "origin returned 503 Service Unavailable" {
		t.Fatalf("retry operation after failed node prewarm = %#v", detail)
	}
}
