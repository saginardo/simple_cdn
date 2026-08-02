package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestSecurityPolicyAndBanLifecycle(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	policies, err := database.ListSecurityPolicies()
	if err != nil || len(policies) != 6 {
		t.Fatalf("seeded policies = %#v, err=%v", policies, err)
	}
	for _, seeded := range policies {
		if !seeded.Builtin {
			t.Fatalf("seeded policy is not built-in: %#v", seeded)
		}
	}
	policy, err := database.SecurityPolicy(domain.DefaultSecurityPolicyID)
	if err != nil {
		t.Fatal(err)
	}
	phpPolicy, err := database.SecurityPolicy(domain.DefaultPHPSecurityPolicyID)
	if err != nil || phpPolicy.Action != domain.SecurityActionBlock || phpPolicy.BanDurationSeconds != 0 || phpPolicy.Priority != 200 {
		t.Fatalf("seeded PHP policy = %#v, err=%v", phpPolicy, err)
	}
	policy.BanDurationSeconds = 3600
	updated, err := database.UpdateSecurityPolicy(policy.ID, policy)
	if err != nil || updated.BanDurationSeconds != 3600 || !updated.Builtin {
		t.Fatalf("updated policy = %#v, err=%v", updated, err)
	}
	if err := database.DeleteSecurityPolicy(policy.ID); err == nil {
		t.Fatal("built-in policy was deleted")
	}
	if err := database.DeleteSecurityPolicy(phpPolicy.ID); err == nil {
		t.Fatal("built-in PHP policy was deleted")
	}
	node, err := database.CreateNode("security-edge", "203.0.113.77")
	if err != nil {
		t.Fatal(err)
	}
	event := domain.SecurityEvent{
		ID: "11111111-1111-4111-8111-111111111111", PolicyID: policy.ID, ClientIP: "8.8.8.8", Host: "cdn.example.test", Path: "/.env",
		Method: "GET", Action: domain.SecurityActionBan, BanDurationSeconds: 3600, ObservedAt: time.Now().UTC(),
	}
	accepted, err := database.RecordSecurityEvents(node.ID, []domain.SecurityEvent{event})
	if err != nil || accepted != 1 {
		t.Fatalf("record event accepted=%d, err=%v", accepted, err)
	}
	bans, err := database.ListActiveSecurityBans()
	if err != nil || len(bans) != 1 || bans[0].IP != "8.8.8.8" || bans[0].TriggerNodeID != node.ID {
		t.Fatalf("active bans = %#v, err=%v", bans, err)
	}
	if remaining := time.Until(bans[0].ExpiresAt); remaining < 59*time.Minute || remaining > 61*time.Minute {
		t.Fatalf("unexpected ban duration %s", remaining)
	}
	events, err := database.ListRecentSecurityEvents(10)
	if err != nil || len(events) != 1 || events[0].PolicyName != policy.Name {
		t.Fatalf("recent events = %#v, err=%v", events, err)
	}
	if err := database.DeleteSecurityBan("8.8.8.8"); err != nil {
		t.Fatal(err)
	}
	if bans, err := database.ListActiveSecurityBans(); err != nil || len(bans) != 0 {
		t.Fatalf("bans after delete = %#v, err=%v", bans, err)
	}
	bad := event
	bad.ID = "22222222-2222-4222-8222-222222222222"
	bad.Path = "/assets/app.js"
	if _, err := database.RecordSecurityEvents(node.ID, []domain.SecurityEvent{bad}); err == nil {
		t.Fatal("event whose path did not match the policy was accepted")
	} else {
		var inputError *SecurityEventInputError
		if !errors.As(err, &inputError) || inputError.Index != 0 {
			t.Fatalf("invalid event error = %#v", err)
		}
	}
	stale := event
	stale.ID = "55555555-5555-4555-8555-555555555555"
	stale.ClientIP = "9.9.9.9"
	stale.ObservedAt = time.Now().UTC().Add(-2 * time.Hour)
	if accepted, err := database.RecordSecurityEvents(node.ID, []domain.SecurityEvent{stale}); err != nil || accepted != 1 {
		t.Fatalf("stale event accepted=%d, err=%v", accepted, err)
	}
	if bans, err := database.ListActiveSecurityBans(); err != nil || len(bans) != 0 {
		t.Fatalf("stale event created a fresh ban: %#v, err=%v", bans, err)
	}
	phpEvent := domain.SecurityEvent{
		ID: "66666666-6666-4666-8666-666666666666", PolicyID: phpPolicy.ID,
		ClientIP: "8.8.4.4", Host: "cdn.example.test", Path: "/nested/shell.php",
		Method: "GET", Action: domain.SecurityActionBlock, ObservedAt: time.Now().UTC(),
	}
	if accepted, err := database.RecordSecurityEvents(node.ID, []domain.SecurityEvent{phpEvent}); err != nil || accepted != 1 {
		t.Fatalf("PHP block event accepted=%d, err=%v", accepted, err)
	}
	if bans, err := database.ListActiveSecurityBans(); err != nil || len(bans) != 0 {
		t.Fatalf("PHP block event created a ban: %#v, err=%v", bans, err)
	}
}

func TestRateLimitBanEventLifecycle(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("rate-limit-edge", "203.0.113.91")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := database.CreateRateLimitPolicy(domain.RateLimitPolicy{
		Name: "error bursts", Enabled: true, RequestsPerSecond: 5,
		ResponseConditionEnabled: true, ResponseStatusClasses: []int{4, 5},
		BanEnabled: true, BanAfterConsecutive429: 3, BanDurationSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	event := domain.SecurityEvent{
		ID: "77777777-7777-4777-8777-777777777777", PolicyID: policy.ID,
		ClientIP: "9.9.9.9", Host: "cdn.example.test", Path: "/api/failures",
		Method: "GET", Action: domain.SecurityActionBan, BanDurationSeconds: 3600,
		ObservedAt: time.Now().UTC(),
	}
	if accepted, err := database.RecordSecurityEvents(node.ID, []domain.SecurityEvent{event}); err != nil || accepted != 1 {
		t.Fatalf("record rate limit event accepted=%d, err=%v", accepted, err)
	}
	bans, err := database.ListActiveSecurityBans()
	if err != nil || len(bans) != 1 || bans[0].IP != event.ClientIP || bans[0].PolicyID != policy.ID || bans[0].PolicyName != policy.Name {
		t.Fatalf("rate limit bans = %#v, err=%v", bans, err)
	}
	events, err := database.ListRecentSecurityEvents(10)
	if err != nil || len(events) != 1 || events[0].PolicyID != policy.ID || events[0].PolicyName != policy.Name {
		t.Fatalf("rate limit events = %#v, err=%v", events, err)
	}

	policy.BanEnabled = false
	if _, err := database.UpdateRateLimitPolicy(policy.ID, policy); err != nil {
		t.Fatal(err)
	}
	event.ID = "88888888-8888-4888-8888-888888888888"
	event.ClientIP = "8.8.4.4"
	if _, err := database.RecordSecurityEvents(node.ID, []domain.SecurityEvent{event}); err == nil {
		t.Fatal("disabled rate limit ban policy accepted an event")
	}
}

func TestBuiltinSecurityPolicyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`UPDATE security_policies SET enabled = 0, pattern = ?, action = ?,
		ban_duration_seconds = 0, priority = 321 WHERE id = ?`, legacyDefaultSecurityPolicyPattern,
		domain.SecurityActionBlock, domain.DefaultSecurityPolicyID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM security_policies WHERE id = ?`, domain.DefaultPHPSecurityPolicyID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 3`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sensitive, err := database.SecurityPolicy(domain.DefaultSecurityPolicyID)
	if err != nil {
		t.Fatal(err)
	}
	if sensitive.Pattern != domain.DefaultSecurityPolicyPattern || sensitive.Enabled ||
		sensitive.Action != domain.SecurityActionBlock || sensitive.BanDurationSeconds != 0 || sensitive.Priority != 321 {
		t.Fatalf("migrated sensitive policy = %#v", sensitive)
	}
	phpPolicy, err := database.SecurityPolicy(domain.DefaultPHPSecurityPolicyID)
	if err != nil || !phpPolicy.Builtin || !phpPolicy.Enabled || phpPolicy.Action != domain.SecurityActionBlock {
		t.Fatalf("migrated PHP policy = %#v, err=%v", phpPolicy, err)
	}

	customPattern := `(?i)^/+private-config(?:/|$)`
	customConditions := `[{"field":"path","operator":"regex","value":"(?i)^/+private-config(?:/|$)"}]`
	if _, err := database.db.Exec(`UPDATE security_policies SET pattern = ?, conditions_json = ? WHERE id = ?`,
		customPattern, customConditions, domain.DefaultSecurityPolicyID); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sensitive, err = database.SecurityPolicy(domain.DefaultSecurityPolicyID)
	if err != nil || sensitive.Pattern != customPattern {
		t.Fatalf("customized sensitive policy was overwritten: %#v, err=%v", sensitive, err)
	}
}

func TestCustomSecurityPolicyCRUD(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	created, err := database.CreateSecurityPolicy(domain.SecurityPolicy{
		Name: "PHP probes", Enabled: true,
		Conditions: []domain.SecurityCondition{
			{Field: domain.SecurityFieldPath, Operator: domain.SecurityOperatorPrefix, Value: "/wp-admin"},
			{Field: domain.SecurityFieldUserAgent, Operator: domain.SecurityOperatorContains, Value: "scanner"},
		},
		Action: domain.SecurityActionBlock, ResponseStatus: 404, Priority: 200,
	})
	if err != nil || created.ID == "" || created.Builtin || len(created.Conditions) != 2 || created.ResponseStatus != 404 {
		t.Fatalf("created policy = %#v, err=%v", created, err)
	}
	if err := database.DeleteSecurityPolicy(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SecurityPolicy(created.ID); err != ErrNotFound {
		t.Fatalf("deleted policy lookup = %v", err)
	}
}

func TestMoveSecurityPolicyReordersAndRenumbersChain(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	created, err := database.CreateSecurityPolicy(domain.SecurityPolicy{
		Name: "custom first", Enabled: true,
		Conditions: []domain.SecurityCondition{{
			Field: domain.SecurityFieldPath, Operator: domain.SecurityOperatorPrefix, Value: "/private",
		}},
		Action: domain.SecurityActionBlock, Priority: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MoveSecurityPolicy(created.ID, "down"); err != nil {
		t.Fatal(err)
	}
	policies, err := database.ListSecurityPolicies()
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 7 || policies[1].ID != created.ID || policies[0].ID != domain.DefaultSecurityPolicyID {
		t.Fatalf("moved policies = %#v", policies)
	}
	for index, policy := range policies {
		if policy.Priority != (index+1)*100 {
			t.Fatalf("policy %d priority = %d", index, policy.Priority)
		}
	}
	if err := database.MoveSecurityPolicy(created.ID, "sideways"); err == nil {
		t.Fatal("invalid direction was accepted")
	}
	if err := database.MoveSecurityPolicy("missing", "up"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing policy error = %v", err)
	}
}

func TestWAFEventValidationUsesRichRequestFields(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("waf-event-edge", "203.0.113.230")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := database.CreateSecurityPolicy(domain.SecurityPolicy{
		Name: "API scanner log", Enabled: true, Conditions: []domain.SecurityCondition{
			{Field: domain.SecurityFieldPath, Operator: domain.SecurityOperatorPrefix, Value: "/api/"},
			{Field: domain.SecurityFieldUserAgent, Operator: domain.SecurityOperatorContains, Value: "scanner"},
		},
		Action: domain.SecurityActionLog, Priority: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	event := domain.SecurityEvent{
		ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", PolicyID: policy.ID,
		ClientIP: "2606:4700:4700::1111", Host: "api.example.test", Path: "/api/items",
		RawURI: "/api/items?id=1", Query: "id=1", UserAgent: "Example Scanner",
		MatchedField: domain.SecurityFieldPath, Method: "GET", Action: domain.SecurityActionLog,
		ObservedAt: time.Now().UTC(),
	}
	if accepted, err := database.RecordSecurityEvents(node.ID, []domain.SecurityEvent{event}); err != nil || accepted != 1 {
		t.Fatalf("rich WAF event accepted=%d, err=%v", accepted, err)
	}
	recent, err := database.ListRecentSecurityEvents(1)
	if err != nil || len(recent) != 1 || recent[0].UserAgent != event.UserAgent ||
		recent[0].RawURI != event.RawURI || recent[0].MatchedField != event.MatchedField {
		t.Fatalf("stored rich WAF event = %#v, err=%v", recent, err)
	}
	event.ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	event.UserAgent = "regular browser"
	if _, err := database.RecordSecurityEvents(node.ID, []domain.SecurityEvent{event}); err == nil {
		t.Fatal("WAF event with mismatched User-Agent was accepted")
	}
}

func TestSecurityBanListingLimitAndCount(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("security-edge", "203.0.113.88")
	if err != nil {
		t.Fatal(err)
	}
	policies, err := database.ListSecurityPolicies()
	if err != nil {
		t.Fatal(err)
	}
	events := []domain.SecurityEvent{
		{ID: "33333333-3333-4333-8333-333333333333", PolicyID: policies[0].ID, ClientIP: "8.8.8.8", Path: "/.env", Action: domain.SecurityActionBan, ObservedAt: time.Now().UTC()},
		{ID: "44444444-4444-4444-8444-444444444444", PolicyID: policies[0].ID, ClientIP: "1.1.1.1", Path: "/.git/config", Action: domain.SecurityActionBan, ObservedAt: time.Now().UTC()},
	}
	if accepted, err := database.RecordSecurityEvents(node.ID, events); err != nil || accepted != len(events) {
		t.Fatalf("record events accepted=%d, err=%v", accepted, err)
	}
	count, err := database.CountActiveSecurityBans()
	if err != nil || count != 2 {
		t.Fatalf("active ban count=%d, err=%v", count, err)
	}
	bans, err := database.ListActiveSecurityBansLimit(1)
	if err != nil || len(bans) != 1 {
		t.Fatalf("limited bans=%#v, err=%v", bans, err)
	}
	if accepted, err := database.RecordSecurityEvents(node.ID, events); err != nil || accepted != len(events) {
		t.Fatalf("replay accepted=%d, err=%v", accepted, err)
	}
	recent, err := database.ListRecentSecurityEvents(10)
	if err != nil || len(recent) != len(events) {
		t.Fatalf("events after idempotent replay=%#v, err=%v", recent, err)
	}
}
