package logstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// CREATE TABLE IF NOT EXISTS cannot update retention on an existing deployment
// or a restored backup. Read the current engines so unchanged tables do not
// schedule another TTL materialization each time schema setup runs.
func (c ClickHouse) ensureRetention(ctx context.Context) error {
	columns := map[string]string{
		"cdn_access_logs":            "timestamp",
		"cdn_site_minute":            "minute",
		"cdn_status_minute":          "minute",
		"cdn_cache_minute":           "minute",
		"cdn_origin_minute":          "minute",
		"cdn_compression_minute":     "minute",
		"cdn_tcp_monitoring_history": "checked_at",
	}
	query := `SELECT name, engine_full FROM system.tables
WHERE database = {database:String} AND name IN (
 'cdn_access_logs', 'cdn_site_minute', 'cdn_status_minute', 'cdn_cache_minute',
 'cdn_origin_minute', 'cdn_compression_minute', 'cdn_tcp_monitoring_history'
) ORDER BY name FORMAT JSONEachRow`
	response, err := c.request(ctx, "system", query, nil, url.Values{"param_database": {c.database()}})
	if err != nil {
		return fmt.Errorf("read ClickHouse retention: %w", err)
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	for {
		var table struct {
			Name       string `json:"name"`
			EngineFull string `json:"engine_full"`
		}
		if err := decoder.Decode(&table); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode ClickHouse retention: %w", err)
		}
		column, ok := columns[table.Name]
		if !ok {
			return fmt.Errorf("unexpected ClickHouse retention table %q", table.Name)
		}
		if strings.Contains(table.EngineFull, "TTL "+column+" + toIntervalDay(7)") {
			continue
		}
		statement := "ALTER TABLE " + identifier(c.database()) + "." + identifier(table.Name) +
			" MODIFY TTL " + column + " + INTERVAL 7 DAY DELETE"
		if err := c.query(ctx, statement, nil); err != nil {
			return fmt.Errorf("update ClickHouse retention for %s: %w", table.Name, err)
		}
	}
}
