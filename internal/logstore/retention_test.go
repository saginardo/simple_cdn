package logstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEnsureSchemaUsesSevenDayRetentionForAllHistory(t *testing.T) {
	var tables []string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		if strings.HasPrefix(query, "CREATE TABLE") && strings.Contains(query, "TTL ") {
			tables = append(tables, query)
		}
	}))
	defer server.Close()
	if err := (ClickHouse{Endpoint: server.URL}).EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tables) != 7 {
		t.Fatalf("history tables = %d, want 7", len(tables))
	}
	for _, query := range tables {
		if !strings.Contains(query, "+ INTERVAL 7 DAY DELETE") || strings.Contains(query, "INTERVAL 30 DAY") {
			t.Errorf("unexpected history retention: %s", query)
		}
	}
}

func TestRetentionMigratesExistingTablesAndSkipsUnchangedTables(t *testing.T) {
	var altered []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		if strings.HasPrefix(query, "SELECT name, engine_full") {
			if request.URL.Query().Get("param_database") != "restored_database" {
				t.Error("retention did not inspect the configured database")
			}
			_, _ = io.WriteString(response, `{"name":"cdn_access_logs","engine_full":"MergeTree TTL timestamp + toIntervalDay(7)"}`+"\n"+
				`{"name":"cdn_site_minute","engine_full":"SummingMergeTree TTL minute + toIntervalDay(30)"}`+"\n"+
				`{"name":"cdn_cache_minute","engine_full":"SummingMergeTree"}`+"\n"+
				`{"name":"cdn_tcp_monitoring_history","engine_full":"ReplacingMergeTree TTL checked_at + toIntervalDay(7)"}`+"\n")
			return
		}
		altered = append(altered, query)
	}))
	defer server.Close()
	if err := (ClickHouse{Endpoint: server.URL, Database: "restored_database"}).ensureRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ALTER TABLE `restored_database`.`cdn_site_minute` MODIFY TTL minute + INTERVAL 7 DAY DELETE",
		"ALTER TABLE `restored_database`.`cdn_cache_minute` MODIFY TTL minute + INTERVAL 7 DAY DELETE",
	}
	if strings.Join(altered, "\n") != strings.Join(want, "\n") {
		t.Fatalf("retention updates = %#v, want %#v", altered, want)
	}
}

func TestRetentionPropagatesMigrationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Query().Get("query"), "SELECT") {
			_, _ = io.WriteString(response, `{"name":"cdn_status_minute","engine_full":"SummingMergeTree TTL minute + toIntervalDay(30)"}`)
			return
		}
		http.Error(response, "retention migration failed", http.StatusInternalServerError)
	}))
	defer server.Close()
	err := (ClickHouse{Endpoint: server.URL}).ensureRetention(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cdn_status_minute") {
		t.Fatalf("migration error = %v", err)
	}
}

func TestClickHouseSevenDayRetentionMigrationIntegration(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("CLICKHOUSE_TEST_URL"))
	if endpoint == "" {
		t.Skip("CLICKHOUSE_TEST_URL is not configured")
	}
	database := fmt.Sprintf("cdn_retention_test_%d", time.Now().UnixNano())
	c := ClickHouse{Endpoint: endpoint, Database: database}
	t.Cleanup(func() {
		_ = c.queryInDatabase(context.Background(), "default", "DROP DATABASE IF EXISTS "+identifier(database)+" SYNC", nil)
	})
	if err := c.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"ALTER TABLE " + identifier(database) + ".cdn_origin_minute MODIFY TTL minute + INTERVAL 30 DAY DELETE",
		"INSERT INTO " + identifier(database) + ".cdn_origin_minute (minute, site_id, node_id, connect_samples) VALUES (now() - INTERVAL 8 DAY, 'expired', 'node-1', 1), (now() - INTERVAL 1 DAY, 'kept', 'node-1', 1)",
	} {
		if err := c.query(context.Background(), statement, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.ensureRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.ensureRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.query(context.Background(), "OPTIMIZE TABLE "+identifier(database)+".cdn_origin_minute FINAL", nil); err != nil {
		t.Fatal(err)
	}
	response, err := c.request(context.Background(), database, "SELECT site_id FROM "+identifier(database)+".cdn_origin_minute ORDER BY site_id FORMAT JSONEachRow", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var row struct {
		SiteID string `json:"site_id"`
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&row); err != nil || row.SiteID != "kept" {
		t.Fatalf("retained row = %#v, error = %v", row, err)
	}
	if err := decoder.Decode(&row); err != io.EOF {
		t.Fatalf("expired row survived the seven-day migration: %#v, error = %v", row, err)
	}
}
