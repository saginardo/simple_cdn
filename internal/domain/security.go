package domain

import (
	"errors"
	"net/netip"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"time"
)

const (
	EdgeCapabilitySecurity                 = "edge_security_v1"
	EdgeCapabilityRateLimit                = "edge_rate_limit_v1"
	EdgeCapabilityWAFChain                 = "waf_chain_v1"
	EdgeCapabilityPOWChallenge             = "pow_challenge_v1"
	RateLimitKeyClientIP                   = "client_ip"
	MinRateLimitRPS                        = 1
	MaxRateLimitRPS                        = 100000
	DefaultRateLimitBanAfterConsecutive429 = 3
	MinRateLimitBanAfterConsecutive429     = 1
	MaxRateLimitBanAfterConsecutive429     = 100
	DefaultRateLimitBanDurationSeconds     = 3600

	DefaultSecurityPolicyID      = "00000000-0000-4000-8000-000000000001"
	DefaultSecurityPolicyPattern = `(?i)^/+(?:[^/]+/)*(?:\.env(?:[._~-][A-Za-z0-9][A-Za-z0-9._~-]*)?|\.git(?:config|-credentials)?(?:[._~-](?:old|bak|backup|save|txt|new|swp|orig|copy|disabled|zip|gz|tgz|tar|7z|rar|[0-9]+))?|\.(?:aws|azure|docker|svn|hg|ssh|kube|gnupg|terraform)|\.ht(?:access|passwd)(?:[._~-](?:old|bak|backup|save|txt|new|swp|orig|copy|disabled|zip|gz|tgz|tar|7z|rar|[0-9]+))?|\.DS_Store|\.(?:npmrc|pypirc|netrc)|\.(?:bash|zsh|mysql|psql|rediscli|python)_history|id_(?:rsa|dsa|ecdsa|ed25519)(?:[._~-](?:old|bak|backup|save|txt|new|swp|orig|copy|disabled|zip|gz|tgz|tar|7z|rar|[0-9]+))?|terraform\.tfstate(?:\.backup)?|wp-config\.php(?:[._~-](?:old|bak|backup|save|txt|new|swp|orig|copy|disabled|zip|gz|tgz|tar|7z|rar|[0-9]+))?)(?:/|$)`

	DefaultPHPSecurityPolicyID      = "00000000-0000-4000-8000-000000000002"
	DefaultPHPSecurityPolicyPattern = `(?i)^/+(?:[^/]+/)*(?:php[-_]?info|phpversion|phptest|pinfo|webshell|shell|cmd|c99|r57|wso|b374k|alfa|xleet|backdoor|leftdao|queryversion)\.(?:php(?:[0-9]+)?|phtml|phar)(?:[._~-](?:old|bak|backup|save|txt|new|swp|jpg|jpeg|png|gif|zip|gz|tgz|tar|7z|rar))?(?:/|$)`

	DefaultPathTraversalPolicyID      = "00000000-0000-4000-8000-000000000003"
	DefaultPathTraversalPolicyPattern = `(?i)(?:^|[/\\])(?:\.\.|%2e%2e|%252e%252e)(?:[/\\]|%2f|%5c|%252f|%255c|$)`
	DefaultSQLInjectionPolicyID       = "00000000-0000-4000-8000-000000000004"
	DefaultSQLInjectionPolicyPattern  = `(?i)(?:\bunion(?:\s|%20|\+)+select\b|(?:'|%27)(?:\s|%20|\+)*(?:or|and)(?:\s|%20|\+)+(?:'[^']*'|[0-9]+)(?:\s|%20|\+)*(?:=|%3d)|\b(?:sleep|benchmark)\s*\(|\binformation_schema\b)`
	DefaultXSSPolicyID                = "00000000-0000-4000-8000-000000000005"
	DefaultXSSPolicyPattern           = `(?i)(?:<|%3c)(?:script|iframe|svg)\b|(?:javascript|data)(?::|%3a)|\bon(?:error|load|click)(?:\s|%20)*(?:=|%3d)`
	DefaultScannerUAPolicyID          = "00000000-0000-4000-8000-000000000006"
	DefaultScannerUAPolicyPattern     = `(?i)(?:sqlmap|nikto|nuclei|masscan|zgrab|gobuster|dirbuster|wpscan|acunetix|nessus)`

	MaxSecurityConditions      = 8
	DefaultSecurityBlockStatus = 403
	DefaultPOWDifficultyBits   = 18
	MinPOWDifficultyBits       = 16
	MaxPOWDifficultyBits       = 24
	DefaultPOWChallengeTTL     = 120
	MinPOWChallengeTTL         = 30
	MaxPOWChallengeTTL         = 600
	DefaultPOWPassTTL          = 1800
	MinPOWPassTTL              = 300
	MaxPOWPassTTL              = 86400
)

type SecurityPolicyAction string

const (
	SecurityActionAllow     SecurityPolicyAction = "allow"
	SecurityActionLog       SecurityPolicyAction = "log"
	SecurityActionBlock     SecurityPolicyAction = "block"
	SecurityActionBan       SecurityPolicyAction = "ban"
	SecurityActionChallenge SecurityPolicyAction = "challenge"
)

type SecurityMatchField string

const (
	SecurityFieldPath      SecurityMatchField = "path"
	SecurityFieldRawURI    SecurityMatchField = "raw_uri"
	SecurityFieldQuery     SecurityMatchField = "query"
	SecurityFieldMethod    SecurityMatchField = "method"
	SecurityFieldHost      SecurityMatchField = "host"
	SecurityFieldUserAgent SecurityMatchField = "user_agent"
	SecurityFieldClientIP  SecurityMatchField = "client_ip"
	SecurityFieldHeader    SecurityMatchField = "header"
	SecurityFieldBody      SecurityMatchField = "body"
)

type SecurityMatchOperator string

const (
	SecurityOperatorRegex    SecurityMatchOperator = "regex"
	SecurityOperatorEquals   SecurityMatchOperator = "equals"
	SecurityOperatorContains SecurityMatchOperator = "contains"
	SecurityOperatorPrefix   SecurityMatchOperator = "prefix"
	SecurityOperatorSuffix   SecurityMatchOperator = "suffix"
	SecurityOperatorCIDR     SecurityMatchOperator = "cidr"
)

type SecurityCondition struct {
	Field         SecurityMatchField    `json:"field"`
	Operator      SecurityMatchOperator `json:"operator"`
	Value         string                `json:"value"`
	HeaderName    string                `json:"header_name,omitempty"`
	Negate        bool                  `json:"negate,omitempty"`
	CaseSensitive bool                  `json:"case_sensitive,omitempty"`
}

var SecurityBanDurations = []int{3600, 21600, 43200, 86400, 259200, 604800}

type SecurityPolicy struct {
	ID                 string               `json:"id"`
	Builtin            bool                 `json:"builtin"`
	Name               string               `json:"name"`
	Enabled            bool                 `json:"enabled"`
	SiteIDs            []string             `json:"site_ids,omitempty"`
	Conditions         []SecurityCondition  `json:"conditions"`
	Pattern            string               `json:"pattern"`
	Action             SecurityPolicyAction `json:"action"`
	BanDurationSeconds int                  `json:"ban_duration_seconds,omitempty"`
	ResponseStatus     int                  `json:"response_status,omitempty"`
	Priority           int                  `json:"priority"`
	CreatedAt          time.Time            `json:"created_at"`
	UpdatedAt          time.Time            `json:"updated_at"`
}

type POWPolicy struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Enabled             bool      `json:"enabled"`
	SiteIDs             []string  `json:"site_ids"`
	PathPattern         string    `json:"path_pattern"`
	DifficultyBits      int       `json:"difficulty_bits"`
	ChallengeTTLSeconds int       `json:"challenge_ttl_seconds"`
	PassTTLSeconds      int       `json:"pass_ttl_seconds"`
	Priority            int       `json:"priority"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type POWPolicyRuntime struct {
	Policy POWPolicy `json:"policy"`
	Secret string    `json:"secret"`
}

type RateLimitPolicy struct {
	ID                       string    `json:"id"`
	Name                     string    `json:"name"`
	Enabled                  bool      `json:"enabled"`
	Key                      string    `json:"key"`
	RequestsPerSecond        int       `json:"requests_per_second"`
	ResponseConditionEnabled bool      `json:"response_condition_enabled"`
	ResponseStatusClasses    []int     `json:"response_status_classes,omitempty"`
	BanEnabled               bool      `json:"ban_enabled"`
	BanAfterConsecutive429   int       `json:"ban_after_consecutive_429"`
	BanDurationSeconds       int       `json:"ban_duration_seconds"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

type SecurityEvent struct {
	ID                 string               `json:"id,omitempty"`
	NodeID             string               `json:"node_id,omitempty"`
	PolicyID           string               `json:"policy_id"`
	PolicyName         string               `json:"policy_name,omitempty"`
	SiteID             string               `json:"site_id,omitempty"`
	ClientIP           string               `json:"client_ip"`
	Host               string               `json:"host,omitempty"`
	Path               string               `json:"path"`
	RawURI             string               `json:"raw_uri,omitempty"`
	Query              string               `json:"query,omitempty"`
	UserAgent          string               `json:"user_agent,omitempty"`
	MatchedField       SecurityMatchField   `json:"matched_field,omitempty"`
	Method             string               `json:"method,omitempty"`
	Action             SecurityPolicyAction `json:"action"`
	BanDurationSeconds int                  `json:"ban_duration_seconds,omitempty"`
	ObservedAt         time.Time            `json:"observed_at"`
	BanExpiresAt       *time.Time           `json:"ban_expires_at,omitempty"`
	CreatedAt          time.Time            `json:"created_at,omitempty"`
}

type SecurityBan struct {
	IP            string    `json:"ip"`
	PolicyID      string    `json:"policy_id,omitempty"`
	PolicyName    string    `json:"policy_name,omitempty"`
	TriggerNodeID string    `json:"trigger_node_id,omitempty"`
	Host          string    `json:"host,omitempty"`
	Path          string    `json:"path,omitempty"`
	Method        string    `json:"method,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type EdgeSecurityEventBatch struct {
	Events []SecurityEvent `json:"events"`
}

type EdgeSecurityBanState struct {
	Bans        []EdgeSecurityBan `json:"bans"`
	GeneratedAt time.Time         `json:"generated_at"`
}

type EdgeSecurityBan struct {
	IP        string    `json:"ip"`
	ExpiresAt time.Time `json:"expires_at"`
}

func ValidSecurityBanDuration(seconds int) bool {
	for _, allowed := range SecurityBanDurations {
		if seconds == allowed {
			return true
		}
	}
	return false
}

func IsBuiltinSecurityPolicyID(id string) bool {
	switch id {
	case DefaultSecurityPolicyID, DefaultPHPSecurityPolicyID, DefaultPathTraversalPolicyID,
		DefaultSQLInjectionPolicyID, DefaultXSSPolicyID, DefaultScannerUAPolicyID:
		return true
	default:
		return false
	}
}

func isBuiltinSecurityPolicyPattern(pattern string) bool {
	switch pattern {
	case DefaultSecurityPolicyPattern, DefaultPHPSecurityPolicyPattern, DefaultPathTraversalPolicyPattern,
		DefaultSQLInjectionPolicyPattern, DefaultXSSPolicyPattern, DefaultScannerUAPolicyPattern:
		return true
	default:
		return false
	}
}

func NormalizeSecurityPolicy(policy SecurityPolicy) (SecurityPolicy, error) {
	policy.Name = strings.TrimSpace(policy.Name)
	if policy.Name == "" || len(policy.Name) > 80 {
		return SecurityPolicy{}, errors.New("security policy name must be 1-80 characters")
	}
	policy.SiteIDs = normalizedStringSet(policy.SiteIDs)
	if len(policy.Conditions) == 0 && strings.TrimSpace(policy.Pattern) != "" {
		policy.Conditions = []SecurityCondition{{
			Field: SecurityFieldPath, Operator: SecurityOperatorRegex, Value: policy.Pattern,
		}}
	}
	if len(policy.Conditions) == 0 || len(policy.Conditions) > MaxSecurityConditions {
		return SecurityPolicy{}, errors.New("security policy must contain 1-8 conditions")
	}
	conditions := make([]SecurityCondition, 0, len(policy.Conditions))
	for _, condition := range policy.Conditions {
		normalized, err := normalizeSecurityCondition(condition)
		if err != nil {
			return SecurityPolicy{}, err
		}
		conditions = append(conditions, normalized)
	}
	policy.Conditions = conditions
	policy.Pattern = legacySecurityPattern(policy)
	if policy.Priority < 1 || policy.Priority > 10000 {
		return SecurityPolicy{}, errors.New("security policy priority must be between 1 and 10000")
	}
	switch policy.Action {
	case SecurityActionAllow, SecurityActionLog:
		policy.BanDurationSeconds = 0
		policy.ResponseStatus = 0
	case SecurityActionBlock:
		policy.BanDurationSeconds = 0
		if policy.ResponseStatus == 0 {
			policy.ResponseStatus = DefaultSecurityBlockStatus
		}
		if !validSecurityResponseStatus(policy.ResponseStatus) {
			return SecurityPolicy{}, errors.New("security policy response status is not supported")
		}
	case SecurityActionBan:
		if !ValidSecurityBanDuration(policy.BanDurationSeconds) {
			return SecurityPolicy{}, errors.New("security policy ban duration is not supported")
		}
		if policy.ResponseStatus == 0 {
			policy.ResponseStatus = DefaultSecurityBlockStatus
		}
		if !validSecurityResponseStatus(policy.ResponseStatus) {
			return SecurityPolicy{}, errors.New("security policy response status is not supported")
		}
	default:
		return SecurityPolicy{}, errors.New("security policy action is not supported")
	}
	return policy, nil
}

func NormalizePOWPolicy(policy POWPolicy) (POWPolicy, error) {
	policy.Name = strings.TrimSpace(policy.Name)
	policy.SiteIDs = normalizedStringSet(policy.SiteIDs)
	policy.PathPattern = strings.TrimSpace(policy.PathPattern)
	if policy.Name == "" || len(policy.Name) > 80 {
		return POWPolicy{}, errors.New("proof-of-work policy name must be 1-80 characters")
	}
	if len(policy.SiteIDs) == 0 || len(policy.SiteIDs) > 100 {
		return POWPolicy{}, errors.New("proof-of-work policy must target 1-100 sites")
	}
	if policy.PathPattern == "" {
		policy.PathPattern = `^/`
	}
	if err := validateSecurityRegex(policy.PathPattern); err != nil {
		return POWPolicy{}, errors.New("proof-of-work path pattern is not in the supported regular expression subset")
	}
	if policy.DifficultyBits == 0 {
		policy.DifficultyBits = DefaultPOWDifficultyBits
	}
	if policy.DifficultyBits < MinPOWDifficultyBits || policy.DifficultyBits > MaxPOWDifficultyBits {
		return POWPolicy{}, errors.New("proof-of-work difficulty is out of range")
	}
	if policy.ChallengeTTLSeconds == 0 {
		policy.ChallengeTTLSeconds = DefaultPOWChallengeTTL
	}
	if policy.ChallengeTTLSeconds < MinPOWChallengeTTL || policy.ChallengeTTLSeconds > MaxPOWChallengeTTL {
		return POWPolicy{}, errors.New("proof-of-work challenge lifetime is out of range")
	}
	if policy.PassTTLSeconds == 0 {
		policy.PassTTLSeconds = DefaultPOWPassTTL
	}
	if policy.PassTTLSeconds < MinPOWPassTTL || policy.PassTTLSeconds > MaxPOWPassTTL {
		return POWPolicy{}, errors.New("proof-of-work pass lifetime is out of range")
	}
	if policy.Priority < 1 || policy.Priority > 10000 {
		return POWPolicy{}, errors.New("proof-of-work policy priority must be between 1 and 10000")
	}
	return policy, nil
}

func LegacySecurityPolicy(policy SecurityPolicy) (SecurityPolicy, bool) {
	normalized, err := NormalizeSecurityPolicy(policy)
	if err != nil || normalized.Pattern == "" || len(normalized.SiteIDs) != 0 ||
		(normalized.Action != SecurityActionBlock && normalized.Action != SecurityActionBan) {
		return SecurityPolicy{}, false
	}
	return normalized, true
}

func normalizeSecurityCondition(condition SecurityCondition) (SecurityCondition, error) {
	condition.Value = strings.TrimSpace(condition.Value)
	condition.HeaderName = strings.TrimSpace(condition.HeaderName)
	if condition.Value == "" || len(condition.Value) > 2048 || strings.ContainsAny(condition.Value, "\x00\r\n") {
		return SecurityCondition{}, errors.New("security condition value must be a single line of at most 2048 characters")
	}
	switch condition.Field {
	case SecurityFieldPath, SecurityFieldRawURI, SecurityFieldQuery, SecurityFieldMethod,
		SecurityFieldHost, SecurityFieldUserAgent, SecurityFieldClientIP, SecurityFieldBody:
		condition.HeaderName = ""
	case SecurityFieldHeader:
		if !validHeaderName(condition.HeaderName) {
			return SecurityCondition{}, errors.New("security condition header name is invalid")
		}
	default:
		return SecurityCondition{}, errors.New("security condition field is not supported")
	}
	switch condition.Operator {
	case SecurityOperatorRegex:
		if err := validateSecurityRegex(condition.Value); err != nil {
			return SecurityCondition{}, err
		}
	case SecurityOperatorEquals, SecurityOperatorContains, SecurityOperatorPrefix, SecurityOperatorSuffix:
	case SecurityOperatorCIDR:
		if condition.Field != SecurityFieldClientIP {
			return SecurityCondition{}, errors.New("CIDR matching is only supported for client IP conditions")
		}
		prefixes, err := normalizeCIDRs(condition.Value)
		if err != nil {
			return SecurityCondition{}, err
		}
		condition.Value = strings.Join(prefixes, ",")
	default:
		return SecurityCondition{}, errors.New("security condition operator is not supported")
	}
	return condition, nil
}

func validateSecurityRegex(pattern string) error {
	if !validSecurityPatternDollars(pattern) {
		return errors.New("security condition dollar signs may only be unescaped end anchors")
	}
	if _, err := CompileSecurityPattern(pattern); err != nil {
		return errors.New("security condition regular expression is not in the supported subset")
	}
	if isBuiltinSecurityPolicyPattern(pattern) {
		return nil
	}
	parsed, err := syntax.Parse(strings.ReplaceAll(pattern, "(?:", "("), syntax.Perl)
	if err != nil || hasUnsafeSecurityRepetition(parsed) || securityBacktrackingChoices(parsed) > 16 {
		return errors.New("security condition regular expression exceeds the safe backtracking subset")
	}
	return nil
}

func legacySecurityPattern(policy SecurityPolicy) string {
	if len(policy.SiteIDs) != 0 || len(policy.Conditions) != 1 {
		return ""
	}
	condition := policy.Conditions[0]
	if condition.Field == SecurityFieldPath && condition.Operator == SecurityOperatorRegex &&
		!condition.Negate && !condition.CaseSensitive && condition.HeaderName == "" {
		return condition.Value
	}
	return ""
}

func validSecurityResponseStatus(status int) bool {
	return status == DefaultSecurityBlockStatus || status == 404 || status == 444
}

func normalizedStringSet(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, found := seen[value]; found {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func normalizeCIDRs(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	if len(parts) > 64 {
		return nil, errors.New("security client IP condition exceeds 64 CIDRs")
	}
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			address, addressErr := netip.ParseAddr(part)
			if addressErr != nil {
				return nil, errors.New("security client IP condition contains an invalid CIDR")
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		result = append(result, prefix.Masked().String())
	}
	return normalizedStringSet(result), nil
}

func validHeaderName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", character) {
			continue
		}
		return false
	}
	return true
}

func NormalizeRateLimitPolicy(policy RateLimitPolicy) (RateLimitPolicy, error) {
	policy.Name = strings.TrimSpace(policy.Name)
	policy.Key = RateLimitKeyClientIP
	if policy.BanAfterConsecutive429 == 0 {
		policy.BanAfterConsecutive429 = DefaultRateLimitBanAfterConsecutive429
	}
	if policy.BanDurationSeconds == 0 {
		policy.BanDurationSeconds = DefaultRateLimitBanDurationSeconds
	}
	if policy.Name == "" || len(policy.Name) > 80 {
		return RateLimitPolicy{}, errors.New("rate limit policy name must be 1-80 characters")
	}
	if policy.RequestsPerSecond < MinRateLimitRPS || policy.RequestsPerSecond > MaxRateLimitRPS {
		return RateLimitPolicy{}, errors.New("rate limit requests per second is out of range")
	}
	if policy.BanAfterConsecutive429 < MinRateLimitBanAfterConsecutive429 ||
		policy.BanAfterConsecutive429 > MaxRateLimitBanAfterConsecutive429 {
		return RateLimitPolicy{}, errors.New("rate limit consecutive 429 ban threshold is out of range")
	}
	if !ValidSecurityBanDuration(policy.BanDurationSeconds) {
		return RateLimitPolicy{}, errors.New("rate limit ban duration is not supported")
	}
	if !policy.ResponseConditionEnabled {
		policy.ResponseStatusClasses = nil
		if policy.BanEnabled {
			return RateLimitPolicy{}, errors.New("rate limit IP ban requires a response condition")
		}
		return policy, nil
	}
	if len(policy.ResponseStatusClasses) == 0 {
		return RateLimitPolicy{}, errors.New("rate limit response condition requires at least one status class")
	}
	classes := append([]int(nil), policy.ResponseStatusClasses...)
	sort.Ints(classes)
	normalized := classes[:0]
	for _, class := range classes {
		if class < 2 || class > 5 {
			return RateLimitPolicy{}, errors.New("rate limit response status class must be between 2xx and 5xx")
		}
		if len(normalized) == 0 || normalized[len(normalized)-1] != class {
			normalized = append(normalized, class)
		}
	}
	policy.ResponseStatusClasses = normalized
	if policy.BanEnabled {
		for _, class := range policy.ResponseStatusClasses {
			if class != 4 && class != 5 {
				return RateLimitPolicy{}, errors.New("rate limit IP ban response conditions are limited to 4xx and 5xx")
			}
		}
	}
	return policy, nil
}

func validSecurityPatternDollars(pattern string) bool {
	for index := 0; index < len(pattern); index++ {
		if pattern[index] != '$' {
			continue
		}
		backslashes := 0
		for previous := index - 1; previous >= 0 && pattern[previous] == '\\'; previous-- {
			backslashes++
		}
		if backslashes%2 != 0 {
			return false
		}
		if index+1 < len(pattern) && pattern[index+1] != '|' && pattern[index+1] != ')' {
			return false
		}
	}
	return true
}

func securityBacktrackingChoices(expression *syntax.Regexp) int {
	if expression == nil {
		return 0
	}
	choices := 0
	switch expression.Op {
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat:
		choices++
	case syntax.OpAlternate:
		choices += len(expression.Sub) - 1
	}
	for _, child := range expression.Sub {
		choices += securityBacktrackingChoices(child)
	}
	return choices
}

func CompileSecurityPattern(pattern string) (*regexp.Regexp, error) {
	// Nginx uses PCRE. Restrict user input to the RE2-compatible subset plus
	// non-capturing groups, which have identical matching semantics here.
	return regexp.Compile(strings.ReplaceAll(pattern, "(?:", "("))
}

func hasUnsafeSecurityRepetition(expression *syntax.Regexp) bool {
	if expression == nil {
		return false
	}
	if (expression.Op == syntax.OpStar || expression.Op == syntax.OpPlus || expression.Op == syntax.OpRepeat) &&
		!safeSecurityRepeatOperand(expression.Sub[0]) {
		return true
	}
	for _, child := range expression.Sub {
		if hasUnsafeSecurityRepetition(child) {
			return true
		}
	}
	return false
}

func safeSecurityRepeatOperand(expression *syntax.Regexp) bool {
	for expression.Op == syntax.OpCapture && len(expression.Sub) == 1 {
		expression = expression.Sub[0]
	}
	switch expression.Op {
	case syntax.OpLiteral, syntax.OpCharClass, syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return true
	default:
		return false
	}
}
