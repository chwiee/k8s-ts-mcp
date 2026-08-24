// Package redact is the last line of defense against secrets and tokens
// leaving a cluster-agent: it scrubs free-text command output and strips
// sensitive fields from structured Kubernetes objects. It is applied in the
// agent, before anything is sent to the hub, written to the audit trail, or
// handed to an LLM to write a post-mortem — never as a filter applied later.
package redact

import (
	"regexp"
	"strings"
)

const Placeholder = "[REDACTED]"

// textPatterns catches secret-shaped substrings in otherwise free-form
// command output (stdout/stderr), independent of whether the playbook author
// remembered to avoid fetching the field in the first place.
var textPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                                       // AWS access key ID
	regexp.MustCompile(`(?i)\baws_secret_access_key\s*[:=]\s*\S+`),                                   // AWS secret access key assignment
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),                      // JWT
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._-]+\b`),                                           // Bearer token
	regexp.MustCompile(`(?i)\b(x-api-key|api[_-]?key)\s*[:=]\s*\S+`),                                 // API key assignment
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), // PEM private key
}

// Text replaces every secret-shaped substring in s with Placeholder.
func Text(s string) string {
	for _, p := range textPatterns {
		s = p.ReplaceAllString(s, Placeholder)
	}
	return s
}

// sensitiveKeys are object field names that are always stripped regardless
// of their value, when walking a structured object with Object.
var sensitiveKeys = map[string]bool{
	"data":       true,
	"stringdata": true,
	"password":   true,
	"secret":     true,
	"token":      true,
	"apikey":     true,
	"api_key":    true,
	"privatekey": true,
}

// Object walks a decoded JSON/YAML value (map[string]any / []any / scalars,
// as produced by encoding/json or yaml.v3 unmarshaling into `any`) and:
//   - drops any map key from sensitiveKeys entirely (not just masks it — a
//     Kubernetes Secret's "data"/"stringData" never needs to leave the agent
//     for a diagnostic playbook to report the Secret's name and age), and
//   - additionally scrubs Text() over every remaining string value, in case
//     a token was embedded somewhere not covered by a known field name.
//
// It returns a new value; the input is not mutated.
func Object(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if sensitiveKeys[strings.ToLower(k)] {
				continue
			}
			out[k] = Object(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Object(val)
		}
		return out
	case string:
		return Text(t)
	default:
		return v
	}
}
