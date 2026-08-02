package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/nginx"
	"simple_cdn/internal/store"
)

func TestSecurityPoliciesRenderOnlyForCapableNodes(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	capable, err := database.CreateNode("security-capable", "203.0.113.81")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := database.CreateNode("security-legacy", "203.0.113.82")
	if err != nil {
		t.Fatal(err)
	}
	accessOnly, err := database.CreateNode("security-access-only", "203.0.113.80")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(capable.ID, []string{domain.EdgeCapabilitySecurity, domain.EdgeCapabilityRateLimit}); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(accessOnly.ID, []string{domain.EdgeCapabilitySecurity}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateRateLimitPolicy(domain.RateLimitPolicy{
		Name: "all requests", Enabled: true, RequestsPerSecond: 20,
	}); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "disabled-security-site", Domains: []string{"disabled.example.test"},
		Nodes: []string{capable.ID, accessOnly.ID, legacy.ID}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		Enabled: false,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	if _, err := publisher.PublishSite(site.ID); err != nil {
		t.Fatal(err)
	}
	capableState, _, err := database.NodeState(capable.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacyState, _, err := database.NodeState(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capableState.NginxConfig, "cdn_security_policy_id") || !strings.Contains(capableState.NginxConfig, "return 444") {
		t.Fatalf("capable node lacks security configuration:\n%s", capableState.NginxConfig)
	}
	if !strings.Contains(capableState.NginxConfig, "lua_shared_dict cdn_rate_limit") {
		t.Fatalf("capable node lacks rate limit configuration:\n%s", capableState.NginxConfig)
	}
	accessOnlyState, _, err := database.NodeState(accessOnly.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(accessOnlyState.NginxConfig, "cdn_security_policy_id") || strings.Contains(accessOnlyState.NginxConfig, "cdn_rate_limit") {
		t.Fatalf("access-only node received the wrong security configuration:\n%s", accessOnlyState.NginxConfig)
	}
	if strings.Contains(legacyState.NginxConfig, "cdn_security_policy_id") || strings.Contains(legacyState.NginxConfig, "cdn_rate_limit") {
		t.Fatalf("legacy node received unsupported security configuration:\n%s", legacyState.NginxConfig)
	}
	overview, err := (&Server{Store: database}).securityOverview(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range overview.Nodes {
		if node.ID == capable.ID && (!node.Configured || !node.RateLimitConfigured || !node.RateLimitCapable) {
			t.Fatalf("capable node was not marked configured: %#v", node)
		}
		if node.ID == accessOnly.ID && (!node.Configured || node.RateLimitConfigured || node.RateLimitCapable) {
			t.Fatalf("access-only node coverage is incorrect: %#v", node)
		}
		if node.ID == legacy.ID && (node.Configured || node.RateLimitConfigured) {
			t.Fatalf("legacy node was marked configured: %#v", node)
		}
	}
}

func TestSecurityOverviewTracksWAFAndPOWConfiguration(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("modern-security", "203.0.113.89")
	if err != nil {
		t.Fatal(err)
	}
	capabilities := []string{domain.EdgeCapabilityWAFChain, domain.EdgeCapabilityPOWChallenge}
	if err := database.SetNodeCapabilities(node.ID, capabilities); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "pow-site", Domains: []string{"pow.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	powPolicy, err := database.CreatePOWPolicy(domain.POWPolicy{
		Name: "browser check", Enabled: true, SiteIDs: []string{site.ID}, PathPattern: `^/private`,
		DifficultyBits: 18, ChallengeTTLSeconds: 120, PassTTLSeconds: 1800, Priority: 100,
	}, []byte("encrypted-secret"))
	if err != nil {
		t.Fatal(err)
	}
	policies, err := database.ListSecurityPolicies()
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := nginx.RenderWithRuntimeOptions([]domain.Site{site}, policies, nil, nginx.RenderRuntimeOptions{
		DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB,
		WAFChainCapable:    true,
		POWCapable:         true,
		POWPolicies: []domain.POWPolicyRuntime{{
			Policy: powPolicy, Secret: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32)),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 1, NginxConfig: configuration}, nil); err != nil {
		t.Fatal(err)
	}
	overview, err := (&Server{Store: database}).securityOverview(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Nodes) != 1 || !overview.Nodes[0].Configured || !overview.Nodes[0].POWConfigured {
		t.Fatalf("modern security coverage = %#v", overview.Nodes)
	}
	configuration = strings.Replace(configuration, nginx.POWRuntimeMarker, "# proof-of-work runtime removed", 1)
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 2, NginxConfig: configuration}, nil); err != nil {
		t.Fatal(err)
	}
	overview, err = (&Server{Store: database}).securityOverview(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !overview.Nodes[0].Configured || overview.Nodes[0].POWConfigured {
		t.Fatalf("stale proof-of-work coverage = %#v", overview.Nodes[0])
	}
}

func TestEdgeSecurityEventsReportsRejectedEventIndex(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("security-edge", "203.0.113.84")
	if err != nil {
		t.Fatal(err)
	}
	batch := domain.EdgeSecurityEventBatch{Events: []domain.SecurityEvent{{
		ID: "11111111-1111-4111-8111-111111111111", PolicyID: domain.DefaultSecurityPolicyID,
		ClientIP: "8.8.8.8", Path: "/not-sensitive", Action: domain.SecurityActionBan, ObservedAt: time.Now().UTC(),
	}}}
	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/edge/v1/security-events", bytes.NewReader(payload))
	request = request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, node.ID))
	response := httptest.NewRecorder()
	(&Server{Store: database}).edgeSecurityEvents(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	var result struct {
		InvalidEventIndex *int `json:"invalid_event_index"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.InvalidEventIndex == nil || *result.InvalidEventIndex != 0 {
		t.Fatalf("rejection body = %s, err=%v", response.Body.String(), err)
	}
}

func TestEdgeRateLimitBanEventIsAccepted(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("rate-limit-edge", "203.0.113.92")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := database.CreateRateLimitPolicy(domain.RateLimitPolicy{
		Name: "error burst", Enabled: true, RequestsPerSecond: 5,
		ResponseConditionEnabled: true, ResponseStatusClasses: []int{4, 5},
		BanEnabled: true, BanAfterConsecutive429: 3, BanDurationSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	batch := domain.EdgeSecurityEventBatch{Events: []domain.SecurityEvent{{
		ID: "99999999-9999-4999-8999-999999999999", PolicyID: policy.ID,
		ClientIP: "9.9.9.9", Host: "cdn.example.test", Path: "/api/failures", Method: "GET",
		Action: domain.SecurityActionBan, BanDurationSeconds: 3600, ObservedAt: time.Now().UTC(),
	}}}
	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/edge/v1/security-events", bytes.NewReader(payload))
	request = request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, node.ID))
	response := httptest.NewRecorder()
	(&Server{Store: database}).edgeSecurityEvents(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	bans, err := database.ListActiveSecurityBans()
	if err != nil || len(bans) != 1 || bans[0].IP != "9.9.9.9" || bans[0].PolicyID != policy.ID {
		t.Fatalf("rate limit bans = %#v, err=%v", bans, err)
	}
}

func TestSecurityOverviewReportsCoverage(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("security-edge", "203.0.113.83")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilitySecurity}); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	site, err := database.CreateSite(domain.Site{
		Name: "security-site", Domains: []string{"security.example.test"},
		Nodes: []string{node.ID}, PrimaryOrigin: domain.Origin{URL: "http://origin.example.test", Enabled: true}, Enabled: false,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	overview, err := server.securityOverview(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Policies) != 6 || len(overview.Nodes) != 1 || !overview.Nodes[0].Capable || overview.Nodes[0].Configured ||
		len(overview.Sites) != 1 || overview.Sites[0].ID != site.ID || overview.Sites[0].Domains[0] != "security.example.test" {
		t.Fatalf("security overview = %#v", overview)
	}
}

func TestMoveSecurityPolicyAPI(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	created, err := database.CreateSecurityPolicy(domain.SecurityPolicy{
		Name: "first custom", Enabled: true,
		Conditions: []domain.SecurityCondition{{
			Field: domain.SecurityFieldPath, Operator: domain.SecurityOperatorPrefix, Value: "/private",
		}},
		Action: domain.SecurityActionBlock, Priority: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, Publisher: Publisher{Store: database}}
	request := httptest.NewRequest(http.MethodPost, "/api/security/policies/"+created.ID+"/move", strings.NewReader(`{"direction":"down"}`))
	request.SetPathValue("id", created.ID)
	response := httptest.NewRecorder()
	server.moveSecurityPolicy(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("move status = %d, body=%s", response.Code, response.Body.String())
	}
	var overview securityOverviewResponse
	if err := json.Unmarshal(response.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Policies) != 7 || overview.Policies[1].ID != created.ID {
		t.Fatalf("move response policies = %#v", overview.Policies)
	}
}

func TestRateLimitPolicyAPI(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	server := &Server{Store: database, Publisher: Publisher{Store: database}}

	request := httptest.NewRequest(http.MethodPost, "/api/security/rate-limit-policies", strings.NewReader(`{
			"name":"API errors","enabled":true,"requests_per_second":8,
			"response_condition_enabled":true,"response_status_classes":[5,4],
			"ban_enabled":true,"ban_after_consecutive_429":3,"ban_duration_seconds":3600
		}`))
	response := httptest.NewRecorder()
	server.createRateLimitPolicy(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", response.Code, response.Body.String())
	}
	var overview securityOverviewResponse
	if err := json.Unmarshal(response.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.RateLimitPolicies) != 1 || overview.RateLimitPolicies[0].Key != domain.RateLimitKeyClientIP ||
		overview.RateLimitPolicies[0].ResponseStatusClasses[0] != 4 || !overview.RateLimitPolicies[0].BanEnabled ||
		overview.RateLimitPolicies[0].BanAfterConsecutive429 != 3 || overview.RateLimitPolicies[0].BanDurationSeconds != 3600 {
		t.Fatalf("create response = %#v", overview.RateLimitPolicies)
	}
	policyID := overview.RateLimitPolicies[0].ID

	request = httptest.NewRequest(http.MethodPut, "/api/security/rate-limit-policies/"+policyID, strings.NewReader(`{
			"name":"All traffic","enabled":true,"requests_per_second":30,
			"response_condition_enabled":false,"response_status_classes":[4,5],
			"ban_enabled":false,"ban_after_consecutive_429":3,"ban_duration_seconds":3600
	}`))
	request.SetPathValue("id", policyID)
	response = httptest.NewRecorder()
	server.updateRateLimitPolicy(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", response.Code, response.Body.String())
	}
	updated, err := database.RateLimitPolicy(policyID)
	if err != nil || updated.RequestsPerSecond != 30 || updated.ResponseStatusClasses != nil {
		t.Fatalf("updated policy = %#v, err=%v", updated, err)
	}

	request = httptest.NewRequest(http.MethodPut, "/api/security/rate-limit-policies/"+policyID, strings.NewReader(`{
			"name":"Invalid","enabled":true,"requests_per_second":30,
			"response_condition_enabled":true,"response_status_classes":[],
			"ban_enabled":true,"ban_after_consecutive_429":3,"ban_duration_seconds":3600
	}`))
	request.SetPathValue("id", policyID)
	response = httptest.NewRecorder()
	server.updateRateLimitPolicy(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status = %d, body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/security/rate-limit-policies/"+policyID, nil)
	request.SetPathValue("id", policyID)
	response = httptest.NewRecorder()
	server.deleteRateLimitPolicy(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", response.Code, response.Body.String())
	}
	if _, err := database.RateLimitPolicy(policyID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted policy lookup = %v", err)
	}
}

func TestPOWPolicyAPIAndNodeSecretScoping(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("pow-edge", "203.0.113.220")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "pow-site", Domains: []string{"pow.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "http://origin.example.test", Enabled: true}, Enabled: false,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	publisher := Publisher{Store: database, Cipher: cipher}
	server := &Server{Store: database, Cipher: cipher, Publisher: publisher}
	payload := fmt.Sprintf(`{
		"name":"browser check","enabled":true,"site_ids":[%q],"path_pattern":"^/private",
		"difficulty_bits":18,"challenge_ttl_seconds":120,"pass_ttl_seconds":1800,"priority":100
	}`, site.ID)
	request := httptest.NewRequest(http.MethodPost, "/api/security/pow-policies", strings.NewReader(payload))
	response := httptest.NewRecorder()
	server.createPOWPolicy(response, request)
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("create status = %d, body=%s", response.Code, response.Body.String())
	}
	var overview securityOverviewResponse
	if err := json.Unmarshal(response.Body.Bytes(), &overview); err != nil || len(overview.POWPolicies) != 1 {
		t.Fatalf("create response = %s, err=%v", response.Body.String(), err)
	}
	policy := overview.POWPolicies[0]
	before, err := database.ListEnabledPOWPolicyMaterials()
	if err != nil || len(before) != 1 {
		t.Fatalf("stored proof-of-work material = %#v, err=%v", before, err)
	}
	runtimes, err := publisher.powPoliciesForNode(before, []domain.Site{{ID: site.ID, Enabled: true}}, []string{
		domain.EdgeCapabilityWAFChain, domain.EdgeCapabilityPOWChallenge,
	})
	if err != nil || len(runtimes) != 1 {
		t.Fatalf("target node runtimes = %#v, err=%v", runtimes, err)
	}
	secret, err := base64.RawStdEncoding.DecodeString(runtimes[0].Secret)
	if err != nil || len(secret) != 32 {
		t.Fatalf("runtime secret length = %d, err=%v", len(secret), err)
	}
	runtimes, err = publisher.powPoliciesForNode(before, nil, []string{
		domain.EdgeCapabilityWAFChain, domain.EdgeCapabilityPOWChallenge,
	})
	if err != nil || len(runtimes) != 0 {
		t.Fatalf("unrelated node received runtimes = %#v, err=%v", runtimes, err)
	}

	payload = fmt.Sprintf(`{
		"name":"browser check","enabled":true,"site_ids":[%q],"path_pattern":"^/private",
		"difficulty_bits":20,"challenge_ttl_seconds":120,"pass_ttl_seconds":1800,"priority":100
	}`, site.ID)
	request = httptest.NewRequest(http.MethodPut, "/api/security/pow-policies/"+policy.ID, strings.NewReader(payload))
	request.SetPathValue("id", policy.ID)
	response = httptest.NewRecorder()
	server.updatePOWPolicy(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", response.Code, response.Body.String())
	}
	after, err := database.ListEnabledPOWPolicyMaterials()
	if err != nil || len(after) != 1 || !bytes.Equal(before[0].SecretCiphertext, after[0].SecretCiphertext) {
		t.Fatalf("proof-of-work secret changed during update: before=%#v after=%#v err=%v", before, after, err)
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/security/pow-policies/"+policy.ID, nil)
	request.SetPathValue("id", policy.ID)
	response = httptest.NewRecorder()
	server.deletePOWPolicy(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestRateLimitBanPoliciesRequireRateAndSecurityCapabilities(t *testing.T) {
	policies := []domain.RateLimitPolicy{{
		ID: "99999999-9999-4999-8999-999999999999", Name: "errors", Enabled: true,
		RequestsPerSecond: 5, ResponseConditionEnabled: true, ResponseStatusClasses: []int{4, 5},
		BanEnabled: true, BanAfterConsecutive429: 3, BanDurationSeconds: 3600,
	}}
	if got := rateLimitPoliciesForCapabilities(policies, nil); got != nil {
		t.Fatalf("node without rate limit capability received policies: %#v", got)
	}
	rateOnly := rateLimitPoliciesForCapabilities(policies, []string{domain.EdgeCapabilityRateLimit})
	if len(rateOnly) != 1 || rateOnly[0].BanEnabled || !policies[0].BanEnabled {
		t.Fatalf("rate-only policy downgrade = %#v, original=%#v", rateOnly, policies)
	}
	fullyCapable := rateLimitPoliciesForCapabilities(policies, []string{
		domain.EdgeCapabilityRateLimit, domain.EdgeCapabilitySecurity,
	})
	if len(fullyCapable) != 1 || !fullyCapable[0].BanEnabled {
		t.Fatalf("fully capable policies = %#v", fullyCapable)
	}
}
