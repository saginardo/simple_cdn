package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const ManagedRecordPrefix = "cdn-platform:"

type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type,omitempty"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Comment string `json:"comment"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type DNSProvider interface {
	Reconcile(ctx context.Context, zoneID, owner string, desired []DNSRecord) error
	RemoveNode(ctx context.Context, zoneID, nodeID string) error
	RemoveSiteNode(ctx context.Context, zoneID, siteID, nodeID string) error
	RemoveSiteNodes(ctx context.Context, zoneID, siteID string, nodeIDs []string) error
}

type ZoneResolver interface {
	ResolveZoneID(ctx context.Context, domains []string) (string, error)
}

var (
	ErrZoneNotFound = errors.New("no accessible Cloudflare zone matches the site domains")
	ErrZoneMismatch = errors.New("site domains belong to different Cloudflare zones")
)

type CloudflareDNS struct {
	BaseURL string
	Token   func() (string, error)
	Client  *http.Client
}

type cloudflareZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c CloudflareDNS) ResolveZoneID(ctx context.Context, domains []string) (string, error) {
	if len(domains) == 0 {
		return "", ErrZoneNotFound
	}
	if c.Token == nil {
		return "", errors.New("Cloudflare API token is not configured")
	}
	token, err := c.Token()
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("Cloudflare API token is empty")
	}
	zones, err := c.listZones(ctx, token)
	if err != nil {
		return "", err
	}

	resolvedID := ""
	resolvedName := ""
	for _, domainName := range domains {
		domainName = canonicalRecordName(domainName)
		best := cloudflareZone{}
		for _, zone := range zones {
			zoneName := canonicalRecordName(zone.Name)
			if zoneName == "" || (domainName != zoneName && !strings.HasSuffix(domainName, "."+zoneName)) {
				continue
			}
			if len(zoneName) > len(best.Name) {
				best = cloudflareZone{ID: strings.TrimSpace(zone.ID), Name: zoneName}
			}
		}
		if best.ID == "" {
			return "", fmt.Errorf("%w: %s", ErrZoneNotFound, domainName)
		}
		if resolvedID != "" && best.ID != resolvedID {
			return "", fmt.Errorf("%w: %s matches %s, not %s", ErrZoneMismatch, domainName, best.Name, resolvedName)
		}
		resolvedID = best.ID
		resolvedName = best.Name
	}
	return resolvedID, nil
}

func (c CloudflareDNS) Reconcile(ctx context.Context, zoneID, owner string, desired []DNSRecord) error {
	if strings.TrimSpace(zoneID) == "" {
		return fmt.Errorf("Cloudflare zone ID is required")
	}
	if strings.TrimSpace(owner) == "" {
		return fmt.Errorf("DNS record owner is required")
	}
	token, err := c.Token()
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("Cloudflare API token is empty")
	}
	existing, err := c.listRecords(ctx, zoneID, token, "")
	if err != nil {
		return err
	}
	wanted := make(map[string]DNSRecord, len(desired))
	for _, record := range desired {
		if record.Name == "" || record.Content == "" || !strings.HasPrefix(record.Comment, ManagedRecordPrefix) {
			return fmt.Errorf("invalid managed DNS record")
		}
		record.Type = canonicalRecordType(record)
		parsed := net.ParseIP(strings.TrimSpace(record.Content))
		if parsed == nil || (record.Type == "A" && parsed.To4() == nil) || (record.Type == "AAAA" && parsed.To4() != nil) {
			return fmt.Errorf("invalid managed %s record content %q", record.Type, record.Content)
		}
		if record.Type != "A" && record.Type != "AAAA" {
			return fmt.Errorf("unsupported managed DNS record type %q", record.Type)
		}
		record.Content = parsed.String()
		if record.TTL == 0 {
			record.TTL = 60
		}
		record.Proxied = false // This project uses Cloudflare as authoritative DNS only.
		wanted[recordKey(record)] = record
	}
	managed := make(map[string]DNSRecord)
	ownerPrefix := ManagedRecordPrefix + owner + ";"
	for _, record := range existing {
		if recordIsAddress(record) && strings.HasPrefix(record.Comment, ownerPrefix) {
			managed[recordKey(record)] = record
		}
	}
	for key, desiredRecord := range wanted {
		for _, existingRecord := range existing {
			if canonicalRecordName(existingRecord.Name) == canonicalRecordName(desiredRecord.Name) && (!recordIsAddress(existingRecord) || !strings.HasPrefix(existingRecord.Comment, ownerPrefix)) {
				return fmt.Errorf("refusing to manage DNS name %s because record %s (%s) is not owned by %s", desiredRecord.Name, existingRecord.ID, recordTypeLabel(existingRecord), owner)
			}
		}
		if existingRecord, found := managed[key]; found {
			if existingRecord.TTL != desiredRecord.TTL || existingRecord.Proxied != desiredRecord.Proxied {
				if err := c.updateRecord(ctx, zoneID, token, existingRecord.ID, desiredRecord); err != nil {
					return err
				}
			}
			continue
		}
		if err := c.createRecord(ctx, zoneID, token, desiredRecord); err != nil {
			return err
		}
	}
	for key, existingRecord := range managed {
		if _, found := wanted[key]; found {
			continue
		}
		if err := c.deleteRecord(ctx, zoneID, token, existingRecord.ID); err != nil {
			return err
		}
	}
	return nil
}

func (c CloudflareDNS) ValidateToken(ctx context.Context, token string, zoneIDs []string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("Cloudflare API token is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/user/tokens/verify", nil)
	if err != nil {
		return err
	}
	response, err := c.doWithRetry(ctx, request, token)
	if err != nil {
		return fmt.Errorf("verify Cloudflare API token: %w", err)
	}
	var payload cloudflareResponse[struct {
		Status string `json:"status"`
	}]
	decodeErr := json.NewDecoder(response.Body).Decode(&payload)
	response.Body.Close()
	if decodeErr != nil {
		return decodeErr
	}
	if !payload.Success || payload.Result.Status != "active" {
		return fmt.Errorf("Cloudflare API token is not active: %s", payload.message())
	}
	seen := make(map[string]struct{}, len(zoneIDs))
	for _, zoneID := range zoneIDs {
		zoneID = strings.TrimSpace(zoneID)
		if zoneID == "" {
			continue
		}
		if _, found := seen[zoneID]; found {
			continue
		}
		seen[zoneID] = struct{}{}
		if _, err := c.listRecords(ctx, zoneID, token, ""); err != nil {
			return fmt.Errorf("read Cloudflare zone %s: %w", zoneID, err)
		}
	}
	return nil
}

func (c CloudflareDNS) RemoveNode(ctx context.Context, zoneID, nodeID string) error {
	if strings.TrimSpace(nodeID) == "" {
		return fmt.Errorf("node ID is required")
	}
	return c.removeManagedRecords(ctx, zoneID, func(comment string) bool {
		return ManagedRecordMatchesNode(comment, nodeID)
	})
}

func (c CloudflareDNS) RemoveSiteNode(ctx context.Context, zoneID, siteID, nodeID string) error {
	return c.RemoveSiteNodes(ctx, zoneID, siteID, []string{nodeID})
}

func (c CloudflareDNS) RemoveSiteNodes(ctx context.Context, zoneID, siteID string, nodeIDs []string) error {
	if strings.TrimSpace(siteID) == "" {
		return fmt.Errorf("site ID is required")
	}
	nodes := make(map[string]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		nodeID = strings.TrimSpace(nodeID)
		if nodeID == "" {
			continue
		}
		nodes[nodeID] = struct{}{}
	}
	if len(nodes) == 0 {
		return fmt.Errorf("node IDs are required")
	}
	return c.removeManagedRecords(ctx, zoneID, func(comment string) bool {
		for nodeID := range nodes {
			if ManagedRecordMatchesSiteNode(comment, siteID, nodeID) {
				return true
			}
		}
		return false
	})
}

func (c CloudflareDNS) removeManagedRecords(ctx context.Context, zoneID string, matches func(string) bool) error {
	if strings.TrimSpace(zoneID) == "" {
		return fmt.Errorf("Cloudflare zone ID is required")
	}
	token, err := c.Token()
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("Cloudflare API token is empty")
	}
	records, err := c.listRecords(ctx, zoneID, token, "")
	if err != nil {
		return err
	}
	for _, record := range records {
		if !recordIsAddress(record) || !matches(record.Comment) {
			continue
		}
		if err := c.deleteRecord(ctx, zoneID, token, record.ID); err != nil {
			return err
		}
	}
	return nil
}

func ManagedRecordMatchesNode(comment, nodeID string) bool {
	return managedRecordMatchesFields(comment, map[string]string{"node": nodeID})
}

func ManagedRecordMatchesSiteNode(comment, siteID, nodeID string) bool {
	return managedRecordMatchesFields(comment, map[string]string{"site": siteID, "node": nodeID})
}

func managedRecordMatchesFields(comment string, required map[string]string) bool {
	if !strings.HasPrefix(comment, ManagedRecordPrefix) {
		return false
	}
	foundFields := make(map[string]string, len(required))
	for _, field := range strings.Split(strings.TrimPrefix(comment, ManagedRecordPrefix), ";") {
		key, value, found := strings.Cut(field, "=")
		if found {
			if _, requiredField := required[key]; requiredField {
				foundFields[key] = value
			}
		}
	}
	for key, value := range required {
		if foundFields[key] != value {
			return false
		}
	}
	return true
}

func recordKey(record DNSRecord) string {
	content := strings.TrimSpace(record.Content)
	if parsed := net.ParseIP(content); parsed != nil {
		content = parsed.String()
	}
	return canonicalRecordType(record) + "\x00" + canonicalRecordName(record.Name) + "\x00" + content + "\x00" + record.Comment
}

func canonicalRecordName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

func canonicalRecordType(record DNSRecord) string {
	if strings.TrimSpace(record.Type) == "" {
		return "A"
	}
	return strings.ToUpper(strings.TrimSpace(record.Type))
}

func recordIsAddress(record DNSRecord) bool {
	recordType := canonicalRecordType(record)
	return recordType == "A" || recordType == "AAAA"
}

func recordTypeLabel(record DNSRecord) string {
	return canonicalRecordType(record)
}

func (c CloudflareDNS) listRecords(ctx context.Context, zoneID, token, recordType string) ([]DNSRecord, error) {
	var all []DNSRecord
	for page := 1; ; page++ {
		values := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if recordType != "" {
			values.Set("type", recordType)
		}
		endpoint := c.baseURL() + "/zones/" + url.PathEscape(zoneID) + "/dns_records?" + values.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		response, err := c.doWithRetry(ctx, request, token)
		if err != nil {
			return nil, err
		}
		var payload cloudflareResponse[[]DNSRecord]
		decodeErr := json.NewDecoder(response.Body).Decode(&payload)
		response.Body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if !payload.Success {
			return nil, fmt.Errorf("Cloudflare list DNS records: %s", payload.message())
		}
		all = append(all, payload.Result...)
		if payload.ResultInfo == nil || payload.ResultInfo.TotalPages <= page || len(payload.Result) == 0 {
			return all, nil
		}
	}
}

func (c CloudflareDNS) listZones(ctx context.Context, token string) ([]cloudflareZone, error) {
	var all []cloudflareZone
	for page := 1; ; page++ {
		values := url.Values{"per_page": {"50"}, "page": {strconv.Itoa(page)}}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/zones?"+values.Encode(), nil)
		if err != nil {
			return nil, err
		}
		response, err := c.doWithRetry(ctx, request, token)
		if err != nil {
			return nil, err
		}
		var payload cloudflareResponse[[]cloudflareZone]
		decodeErr := json.NewDecoder(response.Body).Decode(&payload)
		response.Body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if !payload.Success {
			return nil, fmt.Errorf("Cloudflare list zones: %s", payload.message())
		}
		all = append(all, payload.Result...)
		if payload.ResultInfo == nil || payload.ResultInfo.TotalPages <= page || len(payload.Result) == 0 {
			return all, nil
		}
	}
}

func (c CloudflareDNS) createRecord(ctx context.Context, zoneID, token string, record DNSRecord) error {
	record.Type = canonicalRecordType(record)
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/zones/"+url.PathEscape(zoneID)+"/dns_records", bytes.NewReader(body))
	if err != nil {
		return err
	}
	response, err := c.doWithRetry(ctx, request, token)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var payload cloudflareResponse[json.RawMessage]
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return err
	}
	if !payload.Success {
		return fmt.Errorf("Cloudflare create DNS record: %s", payload.message())
	}
	return nil
}

func (c CloudflareDNS) updateRecord(ctx context.Context, zoneID, token, recordID string, record DNSRecord) error {
	record.Type = canonicalRecordType(record)
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL()+"/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(recordID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	response, err := c.doWithRetry(ctx, request, token)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var payload cloudflareResponse[json.RawMessage]
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return err
	}
	if !payload.Success {
		return fmt.Errorf("Cloudflare update DNS record: %s", payload.message())
	}
	return nil
}

func (c CloudflareDNS) deleteRecord(ctx context.Context, zoneID, token, recordID string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL()+"/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(recordID), nil)
	if err != nil {
		return err
	}
	response, err := c.doWithRetry(ctx, request, token)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var payload cloudflareResponse[json.RawMessage]
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return err
	}
	if !payload.Success {
		return fmt.Errorf("Cloudflare delete DNS record: %s", payload.message())
	}
	return nil
}

func (c CloudflareDNS) do(request *http.Request, token string) (*http.Response, error) {
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		response.Body.Close()
		return nil, fmt.Errorf("Cloudflare API %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return response, nil
}

func (c CloudflareDNS) doWithRetry(ctx context.Context, request *http.Request, token string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		clone := request.Clone(ctx)
		if request.GetBody != nil {
			body, err := request.GetBody()
			if err != nil {
				return nil, err
			}
			clone.Body = body
		}
		response, err := c.do(clone, token)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !isRetryableCloudflareError(err) || attempt == 3 {
			break
		}
		backoff := time.Duration(1<<attempt) * 250 * time.Millisecond
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

func isRetryableCloudflareError(err error) bool {
	message := err.Error()
	return strings.Contains(message, " 429 ") || strings.Contains(message, " 500 ") || strings.Contains(message, " 502 ") || strings.Contains(message, " 503 ") || strings.Contains(message, " 504 ")
}

func (c CloudflareDNS) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://api.cloudflare.com/client/v4"
}

type cloudflareResponse[T any] struct {
	Success    bool `json:"success"`
	Result     T    `json:"result"`
	ResultInfo *struct {
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (r cloudflareResponse[T]) message() string {
	parts := make([]string, 0, len(r.Errors))
	for _, error := range r.Errors {
		if error.Message != "" {
			parts = append(parts, error.Message)
		}
	}
	if len(parts) == 0 {
		return "unknown Cloudflare error"
	}
	return strings.Join(parts, "; ")
}
