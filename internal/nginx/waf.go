package nginx

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"simple_cdn/internal/domain"
)

const (
	powVerifyPath = "/_cdn/pow/verify"

	// These markers let the controller distinguish runtime capability state
	// from the legacy security renderer when an agent reports a downgrade.
	WAFRuntimeMarker = "# CDN WAF runtime: enabled"
	POWRuntimeMarker = "# CDN proof-of-work runtime: enabled"
)

type renderedWAFLog struct {
	Index  int
	Format string
}

func jsonLogLiteral(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func enabledPOWPolicies(policies []domain.POWPolicyRuntime) []domain.POWPolicyRuntime {
	result := make([]domain.POWPolicyRuntime, 0, len(policies))
	for _, policy := range policies {
		if policy.Policy.Enabled {
			result = append(result, policy)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Policy.Priority != result[j].Policy.Priority {
			return result[i].Policy.Priority < result[j].Policy.Priority
		}
		return result[i].Policy.ID < result[j].Policy.ID
	})
	return result
}

func renderWAFConfig(policies []domain.SecurityPolicy, powPolicies []domain.POWPolicyRuntime, httpEnabled, powCapable bool) (string, []renderedWAFLog, error) {
	var result strings.Builder
	result.WriteString(WAFRuntimeMarker)
	result.WriteByte('\n')
	if powCapable {
		result.WriteString(POWRuntimeMarker)
		result.WriteByte('\n')
	}
	result.WriteString(SecurityRevisionMarker(policies))
	result.WriteByte('\n')
	publicPOWPolicies := make([]domain.POWPolicy, 0, len(powPolicies))
	for _, policy := range powPolicies {
		publicPOWPolicies = append(publicPOWPolicies, policy.Policy)
	}
	result.WriteString(POWRevisionMarker(publicPOWPolicies))
	result.WriteByte('\n')

	waf, err := normalizeEnabledWAFPolicies(policies)
	if err != nil {
		return "", nil, err
	}
	pow, err := normalizeEnabledPOWPolicies(powPolicies)
	if err != nil {
		return "", nil, err
	}
	logs := make([]renderedWAFLog, 0, len(waf))
	for index, policy := range waf {
		if policy.Action == domain.SecurityActionAllow {
			continue
		}
		logs = append(logs, renderedWAFLog{Index: index + 1, Format: fmt.Sprintf("cdn_security_json_%d", index+1)})
	}
	if !httpEnabled || len(waf) == 0 && len(pow) == 0 {
		return result.String(), logs, nil
	}
	for _, log := range logs {
		fmt.Fprintf(&result, "map $request_id $cdn_security_match_%d { default \"\"; }\n", log.Index)
	}
	result.WriteString(`map $request_id $cdn_security_policy_id { default ""; }
map $request_id $cdn_security_action { default ""; }
map $request_id $cdn_security_ban_seconds { default 0; }
map $request_id $cdn_security_matched_field { default ""; }
log_format cdn_security_json escape=json '{"timestamp":"$time_iso8601","policy_id":"$cdn_security_policy_id","action":"$cdn_security_action","ban_seconds":$cdn_security_ban_seconds,"site_id":"$cdn_site_id","client_ip":"$remote_addr","host":"$host","method":"$request_method","path":"$uri","raw_uri":"$request_uri","query":"$args","user_agent":"$http_user_agent","matched_field":"$cdn_security_matched_field"}';
`)
	for index, policy := range waf {
		if policy.Action == domain.SecurityActionAllow {
			continue
		}
		field := ""
		if len(policy.Conditions) > 0 {
			field = string(policy.Conditions[0].Field)
		}
		fmt.Fprintf(&result, "log_format cdn_security_json_%d escape=json '{\"timestamp\":\"$time_iso8601\",\"policy_id\":%s,\"action\":%s,\"ban_seconds\":%d,\"site_id\":\"$cdn_site_id\",\"client_ip\":\"$remote_addr\",\"host\":\"$host\",\"method\":\"$request_method\",\"path\":\"$uri\",\"raw_uri\":\"$request_uri\",\"query\":\"$args\",\"user_agent\":\"$http_user_agent\",\"matched_field\":%s}';\n",
			index+1, jsonLogLiteral(policy.ID), jsonLogLiteral(string(policy.Action)), policy.BanDurationSeconds, jsonLogLiteral(field))
	}
	result.WriteString("init_worker_by_lua_block {\n    local decode = ngx.decode_base64\n    local waf_policies = {\n")
	for _, policy := range waf {
		writeWAFPolicy(&result, policy)
	}
	result.WriteString("    }\n    local pow_policies = {\n")
	for _, policy := range pow {
		writePOWPolicy(&result, policy)
	}
	result.WriteString(wafRuntimeLua)
	return result.String(), logs, nil
}

func normalizeEnabledWAFPolicies(policies []domain.SecurityPolicy) ([]domain.SecurityPolicy, error) {
	result := make([]domain.SecurityPolicy, 0, len(policies))
	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}
		normalized, err := domain.NormalizeSecurityPolicy(policy)
		if err != nil {
			return nil, fmt.Errorf("security policy %q: %w", policy.Name, err)
		}
		normalized.ID = policy.ID
		result = append(result, normalized)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority < result[j].Priority
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func normalizeEnabledPOWPolicies(policies []domain.POWPolicyRuntime) ([]domain.POWPolicyRuntime, error) {
	result := make([]domain.POWPolicyRuntime, 0, len(policies))
	for _, runtimePolicy := range policies {
		if !runtimePolicy.Policy.Enabled {
			continue
		}
		policy, err := domain.NormalizePOWPolicy(runtimePolicy.Policy)
		if err != nil {
			return nil, fmt.Errorf("proof-of-work policy %q: %w", runtimePolicy.Policy.Name, err)
		}
		policy.ID = runtimePolicy.Policy.ID
		secret, err := base64.RawStdEncoding.DecodeString(runtimePolicy.Secret)
		if err != nil || len(secret) != 32 {
			return nil, fmt.Errorf("proof-of-work policy %q has an invalid runtime secret", policy.Name)
		}
		result = append(result, domain.POWPolicyRuntime{Policy: policy, Secret: runtimePolicy.Secret})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Policy.Priority != result[j].Policy.Priority {
			return result[i].Policy.Priority < result[j].Policy.Priority
		}
		return result[i].Policy.ID < result[j].Policy.ID
	})
	return result, nil
}

func writeWAFPolicy(result *strings.Builder, policy domain.SecurityPolicy) {
	result.WriteString("        { id = ")
	result.WriteString(luaDecodedString(policy.ID))
	result.WriteString(", action = ")
	result.WriteString(luaDecodedString(string(policy.Action)))
	result.WriteString(", ban_seconds = ")
	result.WriteString(strconv.Itoa(policy.BanDurationSeconds))
	result.WriteString(", status = ")
	result.WriteString(strconv.Itoa(policy.ResponseStatus))
	writeLuaSites(result, policy.SiteIDs)
	result.WriteString(", matched_field = ")
	result.WriteString(luaDecodedString(string(policy.Conditions[0].Field)))
	result.WriteString(", conditions = {\n")
	for _, condition := range policy.Conditions {
		result.WriteString("            { field = ")
		result.WriteString(luaDecodedString(string(condition.Field)))
		result.WriteString(", operator = ")
		result.WriteString(luaDecodedString(string(condition.Operator)))
		result.WriteString(", value = ")
		result.WriteString(luaDecodedString(condition.Value))
		if condition.HeaderName != "" {
			result.WriteString(", header_name = ")
			result.WriteString(luaDecodedString(condition.HeaderName))
		}
		if condition.Negate {
			result.WriteString(", negate = true")
		}
		if condition.CaseSensitive {
			result.WriteString(", case_sensitive = true")
		}
		if condition.Operator == domain.SecurityOperatorCIDR {
			result.WriteString(", cidrs = {")
			for _, value := range strings.Split(condition.Value, ",") {
				prefix, _ := netip.ParsePrefix(value)
				result.WriteString("{ network = ")
				result.WriteString(luaDecodedBytes(prefix.Masked().Addr().AsSlice()))
				result.WriteString(", bits = ")
				result.WriteString(strconv.Itoa(prefix.Bits()))
				result.WriteString(" },")
			}
			result.WriteString(" }")
		}
		result.WriteString(" },\n")
	}
	result.WriteString("        } },\n")
}

func writePOWPolicy(result *strings.Builder, runtimePolicy domain.POWPolicyRuntime) {
	policy := runtimePolicy.Policy
	secret, _ := base64.RawStdEncoding.DecodeString(runtimePolicy.Secret)
	result.WriteString("        { id = ")
	result.WriteString(luaDecodedString(policy.ID))
	result.WriteString(", path_pattern = ")
	result.WriteString(luaDecodedString(policy.PathPattern))
	result.WriteString(", difficulty = ")
	result.WriteString(strconv.Itoa(policy.DifficultyBits))
	result.WriteString(", challenge_ttl = ")
	result.WriteString(strconv.Itoa(policy.ChallengeTTLSeconds))
	result.WriteString(", pass_ttl = ")
	result.WriteString(strconv.Itoa(policy.PassTTLSeconds))
	result.WriteString(", secret = ")
	result.WriteString(luaDecodedBytes(secret))
	writeLuaSites(result, policy.SiteIDs)
	result.WriteString(" },\n")
}

func writeLuaSites(result *strings.Builder, siteIDs []string) {
	if len(siteIDs) == 0 {
		return
	}
	result.WriteString(", sites = {")
	for _, siteID := range siteIDs {
		result.WriteString("[")
		result.WriteString(luaDecodedString(siteID))
		result.WriteString("] = true,")
	}
	result.WriteString(" }")
}

func luaDecodedString(value string) string { return luaDecodedBytes([]byte(value)) }

func luaDecodedBytes(value []byte) string {
	return `decode("` + base64.StdEncoding.EncodeToString(value) + `")`
}

func POWRevisionMarker(policies []domain.POWPolicy) string {
	ordered := append([]domain.POWPolicy(nil), policies...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for index := range ordered {
		if normalized, err := domain.NormalizePOWPolicy(ordered[index]); err == nil {
			normalized.ID = ordered[index].ID
			ordered[index] = normalized
		}
		ordered[index].CreatedAt = ordered[index].CreatedAt.UTC()
		ordered[index].UpdatedAt = ordered[index].UpdatedAt.UTC()
	}
	encoded, _ := json.Marshal(ordered)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("# CDN proof-of-work revision: %x", digest[:])
}

func HasPOWRevision(configuration string, policies []domain.POWPolicy) bool {
	return strings.Contains(configuration, POWRevisionMarker(policies))
}

func DecodePOWRuntimeSecret(value string) ([]byte, error) {
	secret, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(secret) != 32 {
		return nil, errors.New("invalid proof-of-work runtime secret")
	}
	return secret, nil
}

func POWRuntimeSecretDigest(value string) string {
	secret, err := DecodePOWRuntimeSecret(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(secret)
	return hex.EncodeToString(digest[:])
}

const wafRuntimeLua = `    }
    local bit = require "bit"
    local band, bor, bxor = bit.band, bit.bor, bit.bxor
    local rshift, lshift, ror, tobit = bit.rshift, bit.lshift, bit.ror, bit.tobit
    local POW_VERIFY_PATH = "/_cdn/pow/verify"
    local MAX_BODY_BYTES = 65536

    local function applies_to_site(policy, site_id)
        return policy.sites == nil or policy.sites[site_id or ""] == true
    end

    local function body_value()
        if ngx.ctx.cdn_security_body_loaded then
            return ngx.ctx.cdn_security_request_body or ""
        end
        ngx.ctx.cdn_security_body_loaded = true
        ngx.req.read_body()
        local request_body = string.sub(ngx.req.get_body_data() or "", 1, MAX_BODY_BYTES)
        if request_body == "" then
            local path = ngx.req.get_body_file()
            if path ~= nil then
                local file = io.open(path, "rb")
                if file ~= nil then
                    request_body = file:read(MAX_BODY_BYTES) or ""
                    file:close()
                end
            end
        end
        ngx.ctx.cdn_security_request_body = request_body
        return request_body
    end

    local function header_value(name)
        local headers = ngx.req.get_headers(100)
        local value = headers[name] or headers[string.lower(name)] or ""
        if type(value) == "table" then
            return table.concat(value, ",")
        end
        return tostring(value)
    end

    local function condition_value(condition)
        if condition.field == "path" then return ngx.var.uri or "" end
        if condition.field == "raw_uri" then return ngx.var.request_uri or "" end
        if condition.field == "query" then return ngx.var.args or "" end
        if condition.field == "method" then return ngx.req.get_method() or "" end
        if condition.field == "host" then return ngx.var.host or "" end
        if condition.field == "user_agent" then return ngx.var.http_user_agent or "" end
        if condition.field == "client_ip" then return ngx.var.remote_addr or "" end
        if condition.field == "header" then return header_value(condition.header_name) end
        if condition.field == "body" then return body_value() end
        return ""
    end

    local function matches_cidr(condition)
        local address = ngx.var.binary_remote_addr
        if address == nil then return false end
        for _, cidr in ipairs(condition.cidrs or {}) do
            if #address == #cidr.network then
                local full = math.floor(cidr.bits / 8)
                local remaining = cidr.bits % 8
                local prefix_matches = full == 0 or address:sub(1, full) == cidr.network:sub(1, full)
                if prefix_matches and (remaining == 0 or
                    band(address:byte(full + 1), 256 - 2 ^ (8 - remaining)) ==
                    band(cidr.network:byte(full + 1), 256 - 2 ^ (8 - remaining))) then
                    return true
                end
            end
        end
        return false
    end

    local function condition_matches(condition)
        local matched
        if condition.operator == "cidr" then
            matched = matches_cidr(condition)
        else
            local actual = condition_value(condition)
            local expected = condition.value
            if condition.operator == "regex" then
                matched = ngx.re.find(actual, expected, condition.case_sensitive and "j" or "ij") ~= nil
            else
                if not condition.case_sensitive then
                    actual, expected = string.lower(actual), string.lower(expected)
                end
                if condition.operator == "equals" then matched = actual == expected
                elseif condition.operator == "contains" then matched = string.find(actual, expected, 1, true) ~= nil
                elseif condition.operator == "prefix" then matched = string.sub(actual, 1, #expected) == expected
                elseif condition.operator == "suffix" then matched = #actual >= #expected and string.sub(actual, -#expected) == expected
                else matched = false end
            end
        end
        if condition.negate then return not matched end
        return matched
    end

    local function policy_matches(policy)
        if not applies_to_site(policy, ngx.var.cdn_site_id) then return false end
        for _, condition in ipairs(policy.conditions) do
            if not condition_matches(condition) then return false end
        end
        return true
    end

    local function record_policy(policy, index)
        ngx.var["cdn_security_match_" .. tostring(index)] = "1"
        ngx.var.cdn_security_policy_id = policy.id
        ngx.var.cdn_security_action = policy.action
        ngx.var.cdn_security_ban_seconds = tostring(policy.ban_seconds or 0)
        ngx.var.cdn_security_matched_field = policy.matched_field or ""
    end

    local function run_waf()
        for index, policy in ipairs(waf_policies) do
            if policy_matches(policy) then
                if policy.action == "allow" then return false, true end
                record_policy(policy, index)
                if policy.action ~= "log" then
                    ngx.header["Cache-Control"] = "no-store"
                    ngx.status = policy.status
                    ngx.exit(policy.status)
                    return true, false
                end
            end
        end
        return false, false
    end

    local function unsigned32(value)
        if value < 0 then return value + 4294967296 end
        return value
    end

    local function add32(...)
        local values = {...}
        local sum = 0
        for index = 1, #values do sum = (sum + unsigned32(values[index])) % 4294967296 end
        return tobit(sum)
    end

    local sha256_constants = {
        0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
        0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
        0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
        0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
        0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
        0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
        0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
        0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2
    }

    local function word_bytes(value)
        value = unsigned32(value)
        return string.char(math.floor(value / 16777216) % 256, math.floor(value / 65536) % 256,
            math.floor(value / 256) % 256, value % 256)
    end

    local function sha256(message)
        local byte_length = #message
        local bit_length = byte_length * 8
        local padding = (56 - ((byte_length + 1) % 64)) % 64
        message = message .. string.char(128) .. string.rep(string.char(0), padding) ..
            word_bytes(math.floor(bit_length / 4294967296)) .. word_bytes(bit_length % 4294967296)
        local h0,h1,h2,h3 = 0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a
        local h4,h5,h6,h7 = 0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19
        for offset = 1, #message, 64 do
            local words = {}
            for index = 0, 15 do
                local position = offset + index * 4
                words[index] = tobit(message:byte(position) * 16777216 + message:byte(position + 1) * 65536 +
                    message:byte(position + 2) * 256 + message:byte(position + 3))
            end
            for index = 16, 63 do
                local x, y = words[index - 15], words[index - 2]
                local s0 = bxor(ror(x, 7), ror(x, 18), rshift(x, 3))
                local s1 = bxor(ror(y, 17), ror(y, 19), rshift(y, 10))
                words[index] = add32(words[index - 16], s0, words[index - 7], s1)
            end
            local a,b,c,d,e,f,g,h = h0,h1,h2,h3,h4,h5,h6,h7
            for index = 0, 63 do
                local s1 = bxor(ror(e, 6), ror(e, 11), ror(e, 25))
                local choice = bxor(band(e, f), band(bit.bnot(e), g))
                local temp1 = add32(h, s1, choice, sha256_constants[index + 1], words[index])
                local s0 = bxor(ror(a, 2), ror(a, 13), ror(a, 22))
                local majority = bxor(band(a, b), band(a, c), band(b, c))
                local temp2 = add32(s0, majority)
                h,g,f,e,d,c,b,a = g,f,e,add32(d, temp1),c,b,a,add32(temp1, temp2)
            end
            h0,h1,h2,h3 = add32(h0,a),add32(h1,b),add32(h2,c),add32(h3,d)
            h4,h5,h6,h7 = add32(h4,e),add32(h5,f),add32(h6,g),add32(h7,h)
        end
        return word_bytes(h0)..word_bytes(h1)..word_bytes(h2)..word_bytes(h3)..
            word_bytes(h4)..word_bytes(h5)..word_bytes(h6)..word_bytes(h7)
    end

    local function has_zero_prefix(digest, bits)
        local full, remaining = math.floor(bits / 8), bits % 8
        for index = 1, full do if digest:byte(index) ~= 0 then return false end end
        return remaining == 0 or digest:byte(full + 1) < 2 ^ (8 - remaining)
    end

    local function hex(value)
        return (value:gsub(".", function(character) return string.format("%02x", string.byte(character)) end))
    end

    local function constant_time_equal(left, right)
        if left == nil or right == nil or #left ~= #right then return false end
        local difference = 0
        for index = 1, #left do difference = bor(difference, bxor(left:byte(index), right:byte(index))) end
        return difference == 0
    end

    local function base64url(value)
        return (ngx.encode_base64(value):gsub("%+", "-"):gsub("/", "_"):gsub("=+$", ""))
    end

    local function decode64url(value)
        value = value:gsub("-", "+"):gsub("_", "/")
        value = value .. string.rep("=", (4 - #value % 4) % 4)
        return decode(value)
    end

    local function hmac_sha256(key, message)
        if #key > 64 then key = sha256(key) end
        key = key .. string.rep(string.char(0), 64 - #key)
        local inner, outer = {}, {}
        for index = 1, 64 do
            local value = key:byte(index)
            inner[index] = string.char(bxor(value, 0x36))
            outer[index] = string.char(bxor(value, 0x5c))
        end
        return sha256(table.concat(outer) .. sha256(table.concat(inner) .. message))
    end

    local function signed_token(policy, payload)
        return base64url(payload) .. "." .. hex(hmac_sha256(policy.secret, payload))
    end

    local function verify_token(policy, token)
        local encoded, signature = token:match("^([A-Za-z0-9_-]+)%.([0-9a-f]+)$")
        if encoded == nil then return nil end
        local payload = decode64url(encoded)
        if payload == nil or not constant_time_equal(signature, hex(hmac_sha256(policy.secret, payload))) then return nil end
        return payload
    end

    local function policy_by_id(id)
        for _, policy in ipairs(pow_policies) do if policy.id == id then return policy end end
        return nil
    end

    local function cookie_name(policy)
        return "__cdn_pow_" .. policy.id:gsub("-", ""):sub(1, 12)
    end

    local function cookie_value(name)
        local cookies = ngx.var.http_cookie or ""
        for item in cookies:gmatch("[^;]+") do
            local key, value = item:match("^%s*([^=]+)=(.*)$")
            if key == name then return value end
        end
        return nil
    end

    local function valid_pass(policy)
        local token = cookie_value(cookie_name(policy))
        if token == nil then return false end
        local payload = verify_token(policy, token)
        if payload == nil then return false end
        local kind, id, expires, host, client_ip = payload:match("^([^|]+)|([^|]+)|([^|]+)|([^|]+)|([^|]+)$")
        return kind == "p" and id == policy.id and tonumber(expires) ~= nil and tonumber(expires) >= ngx.time() and
            host == (ngx.var.host or "") and client_ip == (ngx.var.remote_addr or "")
    end

    local function verification_request()
        local args = ngx.req.get_uri_args(4)
        local token, nonce = args.token, args.nonce
        if type(token) ~= "string" or type(nonce) ~= "string" or not nonce:match("^[0-9]+$") or #nonce > 20 then
            return ngx.exit(400)
        end
        local encoded = token:match("^([A-Za-z0-9_-]+)%.")
        local payload = encoded and decode64url(encoded)
        local kind, id
        if payload ~= nil then kind, id = payload:match("^([^|]+)|([^|]+)|") end
        local policy = id and policy_by_id(id)
        if kind ~= "c" or policy == nil then return ngx.exit(403) end
        payload = verify_token(policy, token)
        if payload == nil then return ngx.exit(403) end
        local _, _, expires, salt, host, client_ip = payload:match("^([^|]+)|([^|]+)|([^|]+)|([^|]+)|([^|]+)|([^|]+)$")
        if salt == nil or tonumber(expires) == nil or tonumber(expires) < ngx.time() or
            host ~= (ngx.var.host or "") or client_ip ~= (ngx.var.remote_addr or "") or
            not has_zero_prefix(sha256(token .. ":" .. nonce), policy.difficulty) then
            return ngx.exit(403)
        end
        local pass_expires = ngx.time() + policy.pass_ttl
        local pass = signed_token(policy, table.concat({"p", policy.id, tostring(pass_expires), host, client_ip}, "|"))
        ngx.header["Set-Cookie"] = cookie_name(policy) .. "=" .. pass .. "; Path=/; Max-Age=" ..
            tostring(policy.pass_ttl) .. "; Secure; HttpOnly; SameSite=Lax"
        ngx.header["Cache-Control"] = "no-store"
        ngx.status = 204
        ngx.exit(204)
        return true
    end

    local function challenge_page(policy)
        local expires = ngx.time() + policy.challenge_ttl
        local salt = ngx.var.request_id or tostring(ngx.now())
        local payload = table.concat({"c", policy.id, tostring(expires), salt, ngx.var.host or "", ngx.var.remote_addr or ""}, "|")
        local token = signed_token(policy, payload)
        ngx.header["Content-Type"] = "text/html; charset=utf-8"
        ngx.header["Cache-Control"] = "no-store, no-cache, must-revalidate"
        ngx.header["Content-Security-Policy"] = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'"
        ngx.header["Referrer-Policy"] = "no-referrer"
        ngx.header["X-Content-Type-Options"] = "nosniff"
        ngx.header["X-Frame-Options"] = "DENY"
        ngx.print([[<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Browser check</title><style>html{color-scheme:light dark;font-family:system-ui,sans-serif}body{min-height:100vh;margin:0;display:grid;place-items:center;background:#f7f9f8;color:#17211d}.box{width:min(34rem,calc(100% - 3rem));border-top:3px solid #16866f;padding:2rem 0}h1{font-size:1.35rem;margin:0 0 .65rem}p{color:#52615b;margin:.3rem 0}.line{height:3px;background:#d9e5e0;margin-top:1.5rem;overflow:hidden}.line:after{content:"";display:block;width:35%;height:100%;background:#16866f;animation:m 1.1s infinite ease-in-out}@keyframes m{from{transform:translateX(-110%)}to{transform:translateX(300%)}}@media(prefers-color-scheme:dark){body{background:#101613;color:#ecf4f0}p{color:#9fb0a8}.line{background:#29352f}}</style></head><body><main class="box"><h1>Checking your browser</h1><p id="s">Completing a short security check...</p><div class="line"></div></main><script>(()=>{const token="]])
        ngx.print(token)
        ngx.print([[",bits=]])
        ngx.print(policy.difficulty)
        ngx.print([[,enc=new TextEncoder(),status=document.getElementById("s");function ok(buffer){const a=new Uint8Array(buffer),n=Math.floor(bits/8),r=bits%8;for(let i=0;i<n;i++)if(a[i]!==0)return false;return r===0||a[n]<(1<<(8-r))}async function solve(){let start=0;for(;;){const jobs=[];for(let n=start;n<start+256;n++)jobs.push(crypto.subtle.digest("SHA-256",enc.encode(token+":"+n)).then(hash=>ok(hash)?n:-1));const values=await Promise.all(jobs);const nonce=values.find(value=>value>=0);if(nonce!==undefined){const response=await fetch("]])
        ngx.print(POW_VERIFY_PATH)
        ngx.say([[?token="+encodeURIComponent(token)+"&nonce="+nonce,{method:"POST",credentials:"same-origin",cache:"no-store"});if(response.ok){location.reload();return}throw new Error("verification rejected")}start+=256;if(start%8192===0)await new Promise(requestAnimationFrame)}}solve().catch(()=>{status.textContent="Security check failed. Reload to retry."})})();</script></body></html>]])
        ngx.exit(200)
        return true
    end

    local function run_pow()
        if ngx.var.scheme ~= "https" then return false end
        if ngx.var.uri == POW_VERIFY_PATH then
            if ngx.req.get_method() ~= "POST" then ngx.exit(405); return true end
            return verification_request()
        end
        if ngx.var.uri == "/__cdn_health" then return false end
        for _, policy in ipairs(pow_policies) do
            if applies_to_site(policy, ngx.var.cdn_site_id) and
                ngx.re.find(ngx.var.uri or "", policy.path_pattern, "j") ~= nil and not valid_pass(policy) then
                if ngx.req.get_method() ~= "GET" or (ngx.var.http_upgrade or "") ~= "" or
                    (ngx.var.http_content_type or ""):find("application/grpc", 1, true) ~= nil then
                    ngx.header["Cache-Control"] = "no-store"
                    ngx.exit(403)
                    return true
                end
                return challenge_page(policy)
            end
        end
        return false
    end

    local function access()
        if ngx.is_subrequest or ngx.ctx.cdn_security_checked then return false end
        ngx.ctx.cdn_security_checked = true
        if ngx.var.uri == "/__cdn_health" then return false end
        local stopped, bypass_pow = run_waf()
        if stopped then return true end
        if bypass_pow then return false end
        return run_pow()
    end

    package.loaded.simple_cdn_security = { access = access }
}
`
