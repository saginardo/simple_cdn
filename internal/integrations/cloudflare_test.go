package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCloudflareDNSResolvesMostSpecificZoneAcrossPages(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/zones" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		pages++
		if request.URL.Query().Get("page") == "1" {
			_ = json.NewEncoder(response).Encode(map[string]any{
				"success":     true,
				"result":      []map[string]string{{"id": "zone-parent", "name": "dustk.com"}},
				"result_info": map[string]any{"total_pages": 2},
			})
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"success":     true,
			"result":      []map[string]string{{"id": "zone-api", "name": "api.dustk.com"}},
			"result_info": map[string]any{"total_pages": 2},
		})
	}))
	defer server.Close()

	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}
	zoneID, err := dns.ResolveZoneID(context.Background(), []string{"API.DUSTK.COM.", "cdn.api.dustk.com"})
	if err != nil {
		t.Fatal(err)
	}
	if zoneID != "zone-api" || pages != 2 {
		t.Fatalf("zone ID = %q, pages = %d", zoneID, pages)
	}
}

func TestCloudflareDNSRejectsMissingAndMismatchedZones(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(map[string]any{
			"success": true,
			"result": []map[string]string{
				{"id": "zone-example", "name": "example.com"},
				{"id": "zone-other", "name": "other.test"},
			},
			"result_info": map[string]any{"total_pages": 1},
		})
	}))
	defer server.Close()
	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}

	if _, err := dns.ResolveZoneID(context.Background(), []string{"cdn.example.com", "api.other.test"}); !errors.Is(err, ErrZoneMismatch) {
		t.Fatalf("mismatched zones error = %v", err)
	}
	if _, err := dns.ResolveZoneID(context.Background(), []string{"cdn.unknown.test"}); !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("missing zone error = %v", err)
	}
}

func TestCloudflareDNSRejectsUnmanagedNameCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected %s request", request.Method)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"success":     true,
			"result":      []DNSRecord{{ID: "manual", Name: "cdn.example.test", Content: "203.0.113.9", TTL: 60}},
			"result_info": map[string]any{"total_pages": 1},
		})
	}))
	defer server.Close()
	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}
	err := dns.Reconcile(context.Background(), "zone", "site=site-1", []DNSRecord{{Name: "cdn.example.test", Content: "203.0.113.10", Comment: "cdn-platform:site=site-1;node=node-1", TTL: 60}})
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected unmanaged collision error, got %v", err)
	}
}

func TestCloudflareDNSReconcilesOnlyCurrentOwnerAndPaginates(t *testing.T) {
	var created, deleted string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			page := request.URL.Query().Get("page")
			if page == "1" {
				_ = json.NewEncoder(response).Encode(map[string]any{
					"success":     true,
					"result":      []DNSRecord{{ID: "old", Name: "cdn.example.test", Content: "203.0.113.8", Comment: "cdn-platform:site=site-1;node=old", TTL: 60}},
					"result_info": map[string]any{"total_pages": 2},
				})
				return
			}
			_ = json.NewEncoder(response).Encode(map[string]any{
				"success":     true,
				"result":      []DNSRecord{{ID: "other", Name: "other.example.test", Content: "203.0.113.20", Comment: "cdn-platform:site=other;node=other", TTL: 60}},
				"result_info": map[string]any{"total_pages": 2},
			})
		case http.MethodPost:
			var record DNSRecord
			if err := json.NewDecoder(request.Body).Decode(&record); err != nil {
				t.Fatal(err)
			}
			created = record.Content
			_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "result": map[string]any{}})
		case http.MethodDelete:
			deleted = request.URL.Path
			_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "result": map[string]any{}})
		default:
			t.Fatalf("unexpected method %s", request.Method)
		}
	}))
	defer server.Close()
	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}
	err := dns.Reconcile(context.Background(), "zone", "site=site-1", []DNSRecord{{Name: "cdn.example.test", Content: "203.0.113.10", Comment: "cdn-platform:site=site-1;node=node-1", TTL: 60}})
	if err != nil {
		t.Fatal(err)
	}
	if created != "203.0.113.10" || !strings.HasSuffix(deleted, "/old") {
		t.Fatalf("unexpected reconciliation: created=%q deleted=%q", created, deleted)
	}
}

func TestCloudflareDNSRejectsCollisionEvenWhenCurrentRecordAlreadyMatches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected %s request", request.Method)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"success": true,
			"result": []DNSRecord{
				{ID: "managed", Name: "cdn.example.test", Content: "203.0.113.10", Comment: "cdn-platform:site=site-1;node=node-1", TTL: 60},
				{ID: "manual", Name: "cdn.example.test", Content: "203.0.113.20", TTL: 60},
			},
			"result_info": map[string]any{"total_pages": 1},
		})
	}))
	defer server.Close()
	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}
	err := dns.Reconcile(context.Background(), "zone", "site=site-1", []DNSRecord{{Name: "cdn.example.test", Content: "203.0.113.10", Comment: "cdn-platform:site=site-1;node=node-1", TTL: 60}})
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected conflict with manual record, got %v", err)
	}
}

func TestCloudflareDNSRejectsCNAMECollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected %s request", request.Method)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"success":     true,
			"result":      []DNSRecord{{ID: "alias", Type: "CNAME", Name: "cdn.example.test", Content: "elsewhere.example.test", TTL: 60}},
			"result_info": map[string]any{"total_pages": 1},
		})
	}))
	defer server.Close()
	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}
	err := dns.Reconcile(context.Background(), "zone", "site=site-1", []DNSRecord{{Name: "cdn.example.test", Content: "203.0.113.10", Comment: "cdn-platform:site=site-1;node=node-1", TTL: 60}})
	if err == nil || !strings.Contains(err.Error(), "CNAME") {
		t.Fatalf("expected CNAME collision error, got %v", err)
	}
}

func TestCloudflareDNSReconcilesAAndAAAARecords(t *testing.T) {
	created := make([]DNSRecord, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(response).Encode(map[string]any{
				"success": true, "result": []DNSRecord{}, "result_info": map[string]any{"total_pages": 1},
			})
		case http.MethodPost:
			var record DNSRecord
			if err := json.NewDecoder(request.Body).Decode(&record); err != nil {
				t.Fatal(err)
			}
			created = append(created, record)
			_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "result": map[string]any{}})
		default:
			t.Fatalf("unexpected method %s", request.Method)
		}
	}))
	defer server.Close()
	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}
	comment := "cdn-platform:site=site-1;node=node-1"
	if err := dns.Reconcile(context.Background(), "zone", "site=site-1", []DNSRecord{
		{Name: "cdn.example.test", Content: "203.0.113.10", Comment: comment, TTL: 60},
		{Type: "aaaa", Name: "cdn.example.test", Content: "2001:0db8::10", Comment: comment, TTL: 60},
	}); err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 {
		t.Fatalf("created records = %#v", created)
	}
	byType := make(map[string]DNSRecord, len(created))
	for _, record := range created {
		byType[record.Type] = record
	}
	if byType["A"].Content != "203.0.113.10" || byType["AAAA"].Content != "2001:db8::10" {
		t.Fatalf("created records by type = %#v", byType)
	}
}

func TestCloudflareDNSRemovesStaleAAAAWhenSiteDisablesIPv6(t *testing.T) {
	deleted := ""
	comment := "cdn-platform:site=site-1;node=node-1"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(response).Encode(map[string]any{
				"success": true,
				"result": []DNSRecord{
					{ID: "ipv4", Type: "A", Name: "cdn.example.test", Content: "203.0.113.10", Comment: comment, TTL: 60},
					{ID: "ipv6", Type: "AAAA", Name: "cdn.example.test", Content: "2001:db8::10", Comment: comment, TTL: 60},
				},
				"result_info": map[string]any{"total_pages": 1},
			})
		case http.MethodDelete:
			deleted = request.URL.Path
			_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "result": map[string]any{}})
		default:
			t.Fatalf("unexpected method %s", request.Method)
		}
	}))
	defer server.Close()
	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}
	if err := dns.Reconcile(context.Background(), "zone", "site=site-1", []DNSRecord{{
		Type: "A", Name: "cdn.example.test", Content: "203.0.113.10", Comment: comment, TTL: 60,
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(deleted, "/ipv6") {
		t.Fatalf("deleted record = %q", deleted)
	}
}

func TestCloudflareDNSRemoveNodeDeletesOnlyExactManagedRecords(t *testing.T) {
	deleted := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			if got := request.URL.Query().Get("type"); got != "" {
				t.Fatalf("record type filter = %q", got)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{
				"success": true,
				"result": []DNSRecord{
					{ID: "target-1", Type: "A", Name: "a.example.test", Comment: "cdn-platform:site=one;node=node-1"},
					{ID: "target-2", Type: "A", Name: "b.example.test", Comment: "cdn-platform:node=node-1;site=two"},
					{ID: "target-v6", Type: "AAAA", Name: "b.example.test", Comment: "cdn-platform:node=node-1;site=two"},
					{ID: "similar", Type: "A", Name: "c.example.test", Comment: "cdn-platform:site=one;node=node-10"},
					{ID: "manual", Type: "A", Name: "d.example.test", Comment: "node=node-1"},
					{ID: "cname", Type: "CNAME", Name: "e.example.test", Comment: "cdn-platform:site=one;node=node-1"},
				},
				"result_info": map[string]any{"total_pages": 1},
			})
		case http.MethodDelete:
			deleted = append(deleted, request.URL.Path)
			_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "result": map[string]any{}})
		default:
			t.Fatalf("unexpected method %s", request.Method)
		}
	}))
	defer server.Close()

	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}
	if err := dns.RemoveNode(context.Background(), "zone", "node-1"); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 3 || !strings.HasSuffix(deleted[0], "/target-1") || !strings.HasSuffix(deleted[1], "/target-2") || !strings.HasSuffix(deleted[2], "/target-v6") {
		t.Fatalf("deleted records = %#v", deleted)
	}
}

func TestCloudflareDNSRemoveSiteNodeDeletesOnlyExactSiteAndNode(t *testing.T) {
	deleted := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(response).Encode(map[string]any{
				"success": true,
				"result": []DNSRecord{
					{ID: "target", Type: "A", Name: "a.example.test", Comment: "cdn-platform:site=site-1;node=node-1"},
					{ID: "target-v6", Type: "AAAA", Name: "a.example.test", Comment: "cdn-platform:site=site-1;node=node-1"},
					{ID: "other-site", Type: "A", Name: "b.example.test", Comment: "cdn-platform:site=site-2;node=node-1"},
					{ID: "other-node", Type: "A", Name: "c.example.test", Comment: "cdn-platform:site=site-1;node=node-2"},
				},
				"result_info": map[string]any{"total_pages": 1},
			})
		case http.MethodDelete:
			deleted = append(deleted, request.URL.Path)
			_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "result": map[string]any{}})
		default:
			t.Fatalf("unexpected method %s", request.Method)
		}
	}))
	defer server.Close()

	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}
	if err := dns.RemoveSiteNode(context.Background(), "zone", "site-1", "node-1"); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 || !strings.HasSuffix(deleted[0], "/target") || !strings.HasSuffix(deleted[1], "/target-v6") {
		t.Fatalf("deleted records = %#v", deleted)
	}
}

func TestCloudflareDNSUpdatesTTLWithoutReplacingRecord(t *testing.T) {
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(response).Encode(map[string]any{
				"success":     true,
				"result":      []DNSRecord{{ID: "managed", Type: "A", Name: "cdn.example.test", Content: "203.0.113.10", Comment: "cdn-platform:site=site-1;node=node-1", TTL: 60}},
				"result_info": map[string]any{"total_pages": 1},
			})
		case http.MethodPatch:
			patches++
			if !strings.HasSuffix(request.URL.Path, "/managed") {
				t.Fatalf("PATCH path = %s", request.URL.Path)
			}
			var record DNSRecord
			if err := json.NewDecoder(request.Body).Decode(&record); err != nil {
				t.Fatal(err)
			}
			if record.TTL != 300 || record.Content != "203.0.113.10" || record.Comment == "" || record.Proxied {
				t.Fatalf("PATCH record = %#v", record)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "result": map[string]any{}})
		default:
			t.Fatalf("unexpected method %s", request.Method)
		}
	}))
	defer server.Close()
	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}
	desired := []DNSRecord{{Name: "cdn.example.test", Content: "203.0.113.10", Comment: "cdn-platform:site=site-1;node=node-1", TTL: 300}}
	if err := dns.Reconcile(context.Background(), "zone", "site=site-1", desired); err != nil {
		t.Fatal(err)
	}
	if patches != 1 {
		t.Fatalf("PATCH count = %d", patches)
	}
}

func TestCloudflareDNSDoesNotWriteUnchangedTTL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", request.Method)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"success":     true,
			"result":      []DNSRecord{{ID: "managed", Type: "A", Name: "cdn.example.test", Content: "203.0.113.10", Comment: "cdn-platform:site=site-1;node=node-1", TTL: 120}},
			"result_info": map[string]any{"total_pages": 1},
		})
	}))
	defer server.Close()
	dns := CloudflareDNS{BaseURL: server.URL, Token: func() (string, error) { return "token", nil }}
	if err := dns.Reconcile(context.Background(), "zone", "site=site-1", []DNSRecord{{Name: "cdn.example.test", Content: "203.0.113.10", Comment: "cdn-platform:site=site-1;node=node-1", TTL: 120}}); err != nil {
		t.Fatal(err)
	}
}

func TestCloudflareDNSValidatesTokenAndDistinctZones(t *testing.T) {
	zoneReads := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer candidate" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		if request.URL.Path == "/user/tokens/verify" {
			_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "result": map[string]any{"status": "active"}})
			return
		}
		if strings.HasSuffix(request.URL.Path, "/dns_records") {
			parts := strings.Split(request.URL.Path, "/")
			zoneReads[parts[2]]++
			_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "result": []DNSRecord{}, "result_info": map[string]any{"total_pages": 1}})
			return
		}
		t.Fatalf("unexpected path %s", request.URL.Path)
	}))
	defer server.Close()
	dns := CloudflareDNS{BaseURL: server.URL}
	if err := dns.ValidateToken(context.Background(), "candidate", []string{"zone-a", "zone-b", "zone-a", ""}); err != nil {
		t.Fatal(err)
	}
	if zoneReads["zone-a"] != 1 || zoneReads["zone-b"] != 1 || len(zoneReads) != 2 {
		t.Fatalf("zone reads = %#v", zoneReads)
	}
}
