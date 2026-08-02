package domain

import (
	"slices"
	"testing"
)

func TestDefaultSecurityPolicyPatterns(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		matches  []string
		excludes []string
	}{
		{
			name: "sensitive files", pattern: DefaultSecurityPolicyPattern,
			matches: []string{
				"/.env", "/app/.env.production", "/app/.env.local.php", "/.git/config",
				"/nested/repo/.git/HEAD", "/.gitconfig", "/.git-credentials.bak",
				"/home/.AWS/credentials", "/nested/.docker/config.json", "/home/.ssh/id_rsa",
				"/nested/.htpasswd.old", "/assets/.DS_Store", "/home/.npmrc",
				"/home/.bash_history", "/keys/id_ed25519", "/state/terraform.tfstate.backup",
				"/blog/wp-config.php.orig",
			},
			excludes: []string{
				"/", "/assets/app.js", "/environment", "/.environment", "/.envato/logo",
				"/.github/workflows/build.yml", "/.gitignore", "/.dockerignore", "/api/.awsome",
				"/assets/.DS_Store.css", "/users/id_rsa_public", "/wp-config.php.css",
			},
		},
		{
			name: "PHP probes", pattern: DefaultPHPSecurityPolicyPattern,
			matches: []string{
				"/phpinfo.php", "/PHP-INFO.PHP", "/admin/shell.php", "/nested/webshell.phtml",
				"/cmd.php.bak", "/wso.php.jpg", "/queryVersion.php", "/leftDao.phar",
				"/backdoor.php7/path",
			},
			excludes: []string{
				"/index.php", "/api.php", "/admin.php", "/config.php", "/status.php",
				"/mail.php", "/test.php", "/debug.php", "/probe.php", "/i.php",
				"/assets/shell.php.js", "/shell.js", "/cmd.php.css",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			matcher, err := CompileSecurityPattern(test.pattern)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range test.matches {
				if !matcher.MatchString(path) {
					t.Errorf("default policy did not match %q", path)
				}
			}
			for _, path := range test.excludes {
				if matcher.MatchString(path) {
					t.Errorf("default policy unexpectedly matched %q", path)
				}
			}
		})
	}
}

func TestDefaultSecurityPolicyIDsAndDurations(t *testing.T) {
	for _, id := range []string{
		DefaultSecurityPolicyID,
		DefaultPHPSecurityPolicyID,
		DefaultPathTraversalPolicyID,
		DefaultSQLInjectionPolicyID,
		DefaultXSSPolicyID,
		DefaultScannerUAPolicyID,
	} {
		if !IsBuiltinSecurityPolicyID(id) {
			t.Errorf("default policy ID %q was not recognized", id)
		}
	}
	if IsBuiltinSecurityPolicyID("11111111-1111-4111-8111-111111111111") {
		t.Fatal("custom policy ID was recognized as built-in")
	}
	for _, seconds := range []int{3600, 21600, 43200, 86400, 259200, 604800} {
		if !ValidSecurityBanDuration(seconds) {
			t.Errorf("duration %d is not accepted", seconds)
		}
	}
	if ValidSecurityBanDuration(7200) {
		t.Fatal("unsupported duration was accepted")
	}
}

func TestNormalizeSecurityPolicy(t *testing.T) {
	policy, err := NormalizeSecurityPolicy(SecurityPolicy{
		Name: " sensitive files ", Enabled: true, Pattern: DefaultSecurityPolicyPattern,
		Action: SecurityActionBan, BanDurationSeconds: 21600, Priority: 100,
	})
	if err != nil || policy.Name != "sensitive files" {
		t.Fatalf("normalized policy = %#v, err=%v", policy, err)
	}
	if policy.ResponseStatus != 403 || len(policy.Conditions) != 1 || policy.Pattern == "" {
		t.Fatalf("legacy policy was not upgraded: %#v", policy)
	}
	if _, err := NormalizeSecurityPolicy(SecurityPolicy{
		Name: "PHP probes", Enabled: true, Pattern: DefaultPHPSecurityPolicyPattern,
		Action: SecurityActionBlock, Priority: 200,
	}); err != nil {
		t.Fatalf("default PHP policy was rejected: %v", err)
	}
	if _, err := NormalizeSecurityPolicy(SecurityPolicy{Name: "bad", Pattern: `(?=lookahead)`, Action: SecurityActionBlock, Priority: 1}); err == nil {
		t.Fatal("unsupported regular expression was accepted")
	}
	if _, err := NormalizeSecurityPolicy(SecurityPolicy{Name: "variable", Pattern: `^/$request_uri`, Action: SecurityActionBlock, Priority: 1}); err == nil {
		t.Fatal("Nginx variable reference was accepted")
	}
	for _, pattern := range []string{`^/(foo)$1`, `^/price\$`} {
		if _, err := NormalizeSecurityPolicy(SecurityPolicy{Name: "dollar", Pattern: pattern, Action: SecurityActionBlock, Priority: 1}); err == nil {
			t.Fatalf("unsafe dollar pattern %q was accepted", pattern)
		}
	}
	if _, err := NormalizeSecurityPolicy(SecurityPolicy{Name: "backtracking", Pattern: `^/(a+)+$`, Action: SecurityActionBlock, Priority: 1}); err == nil {
		t.Fatal("potentially unsafe repeated group was accepted")
	}
	if _, err := NormalizeSecurityPolicy(SecurityPolicy{Name: "safe", Pattern: `(?i)^/+wp-admin(?:/.*|$)`, Action: SecurityActionBlock, Priority: 1}); err != nil {
		t.Fatalf("safe custom pattern was rejected: %v", err)
	}
}

func TestNormalizeSecurityPolicyChain(t *testing.T) {
	policy, err := NormalizeSecurityPolicy(SecurityPolicy{
		Name:    "API scanners",
		Enabled: true,
		SiteIDs: []string{" site-b ", "site-a", "site-a"},
		Conditions: []SecurityCondition{
			{Field: SecurityFieldPath, Operator: SecurityOperatorPrefix, Value: "/v1/"},
			{Field: SecurityFieldHeader, Operator: SecurityOperatorContains, HeaderName: "X-Scanner", Value: "yes"},
			{Field: SecurityFieldClientIP, Operator: SecurityOperatorCIDR, Value: "192.0.2.9, 2001:db8::/32"},
		},
		Action: SecurityActionBlock, ResponseStatus: 404, Priority: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(policy.SiteIDs, []string{"site-a", "site-b"}) || policy.Pattern != "" ||
		policy.ResponseStatus != 404 || policy.Conditions[2].Value != "192.0.2.9/32,2001:db8::/32" {
		t.Fatalf("normalized chain = %#v", policy)
	}

	invalid := []SecurityPolicy{
		{Name: "missing", Action: SecurityActionBlock, Priority: 1},
		{Name: "header", Conditions: []SecurityCondition{{Field: SecurityFieldHeader, Operator: SecurityOperatorEquals, HeaderName: "bad header", Value: "x"}}, Action: SecurityActionBlock, Priority: 1},
		{Name: "cidr field", Conditions: []SecurityCondition{{Field: SecurityFieldPath, Operator: SecurityOperatorCIDR, Value: "192.0.2.0/24"}}, Action: SecurityActionBlock, Priority: 1},
		{Name: "cidr value", Conditions: []SecurityCondition{{Field: SecurityFieldClientIP, Operator: SecurityOperatorCIDR, Value: "invalid"}}, Action: SecurityActionBlock, Priority: 1},
		{Name: "status", Conditions: []SecurityCondition{{Field: SecurityFieldPath, Operator: SecurityOperatorEquals, Value: "/"}}, Action: SecurityActionBlock, ResponseStatus: 500, Priority: 1},
	}
	for _, candidate := range invalid {
		if _, err := NormalizeSecurityPolicy(candidate); err == nil {
			t.Fatalf("invalid policy was accepted: %#v", candidate)
		}
	}
}

func TestNormalizePOWPolicy(t *testing.T) {
	policy, err := NormalizePOWPolicy(POWPolicy{
		Name: " Browser check ", SiteIDs: []string{"site-b", "site-a", "site-a"}, Priority: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if policy.Name != "Browser check" || !slices.Equal(policy.SiteIDs, []string{"site-a", "site-b"}) ||
		policy.PathPattern != `^/` || policy.DifficultyBits != DefaultPOWDifficultyBits ||
		policy.ChallengeTTLSeconds != DefaultPOWChallengeTTL || policy.PassTTLSeconds != DefaultPOWPassTTL {
		t.Fatalf("normalized proof-of-work policy = %#v", policy)
	}
	for _, candidate := range []POWPolicy{
		{Name: "no sites", Priority: 1},
		{Name: "bad regex", SiteIDs: []string{"site"}, PathPattern: `(?=x)`, Priority: 1},
		{Name: "too hard", SiteIDs: []string{"site"}, DifficultyBits: MaxPOWDifficultyBits + 1, Priority: 1},
	} {
		if _, err := NormalizePOWPolicy(candidate); err == nil {
			t.Fatalf("invalid proof-of-work policy was accepted: %#v", candidate)
		}
	}
}

func TestNormalizeRateLimitPolicy(t *testing.T) {
	policy, err := NormalizeRateLimitPolicy(RateLimitPolicy{
		Name: " API failures ", Enabled: true, RequestsPerSecond: 25,
		ResponseConditionEnabled: true, ResponseStatusClasses: []int{5, 4, 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if policy.Name != "API failures" || policy.Key != RateLimitKeyClientIP ||
		!slices.Equal(policy.ResponseStatusClasses, []int{4, 5}) ||
		policy.BanAfterConsecutive429 != DefaultRateLimitBanAfterConsecutive429 ||
		policy.BanDurationSeconds != DefaultRateLimitBanDurationSeconds {
		t.Fatalf("normalized rate limit policy = %#v", policy)
	}
	policy, err = NormalizeRateLimitPolicy(RateLimitPolicy{
		Name: "ban error bursts", RequestsPerSecond: 5, ResponseConditionEnabled: true,
		ResponseStatusClasses: []int{5, 4}, BanEnabled: true,
		BanAfterConsecutive429: 4, BanDurationSeconds: 21600,
	})
	if err != nil || !policy.BanEnabled || policy.BanAfterConsecutive429 != 4 || policy.BanDurationSeconds != 21600 {
		t.Fatalf("normalized rate limit ban policy = %#v, err=%v", policy, err)
	}

	policy, err = NormalizeRateLimitPolicy(RateLimitPolicy{
		Name: "all requests", RequestsPerSecond: 1, ResponseStatusClasses: []int{4, 5},
	})
	if err != nil || policy.ResponseStatusClasses != nil {
		t.Fatalf("unconditional rate limit policy = %#v, err=%v", policy, err)
	}

	invalid := []RateLimitPolicy{
		{Name: "", RequestsPerSecond: 1},
		{Name: "too low", RequestsPerSecond: 0},
		{Name: "too high", RequestsPerSecond: MaxRateLimitRPS + 1},
		{Name: "missing class", RequestsPerSecond: 1, ResponseConditionEnabled: true},
		{Name: "bad class", RequestsPerSecond: 1, ResponseConditionEnabled: true, ResponseStatusClasses: []int{1}},
		{Name: "ban without response condition", RequestsPerSecond: 1, BanEnabled: true},
		{Name: "ban success responses", RequestsPerSecond: 1, ResponseConditionEnabled: true, ResponseStatusClasses: []int{2, 4}, BanEnabled: true},
		{Name: "ban threshold low", RequestsPerSecond: 1, ResponseConditionEnabled: true, ResponseStatusClasses: []int{4}, BanEnabled: true, BanAfterConsecutive429: -1},
		{Name: "ban threshold high", RequestsPerSecond: 1, ResponseConditionEnabled: true, ResponseStatusClasses: []int{5}, BanEnabled: true, BanAfterConsecutive429: MaxRateLimitBanAfterConsecutive429 + 1},
		{Name: "ban duration", RequestsPerSecond: 1, ResponseConditionEnabled: true, ResponseStatusClasses: []int{4}, BanEnabled: true, BanDurationSeconds: 7200},
	}
	for _, candidate := range invalid {
		if _, err := NormalizeRateLimitPolicy(candidate); err == nil {
			t.Fatalf("invalid rate limit policy was accepted: %#v", candidate)
		}
	}
}
