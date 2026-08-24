// Package agentauth scopes a calling agent's bearer token to the AWS
// accounts it may touch — e.g. the MARS team's calling agent may only ever
// reach clusters in MARS's own AWS account(s), never another team's,
// regardless of what cluster_id it asks for. This is deliberately a small,
// stable config (one entry per team/agent) — see internal/inventory and
// internal/rolecluster for why adding a *cluster* never needs to touch this:
// only onboarding a brand new team/agent does.
package agentauth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scope is what one calling agent's token authorizes.
type Scope struct {
	AgentName       string
	AllowedAccounts map[string]bool
}

// AllowsAccount reports whether accountID is in scope.
//
// A nil Scope means no --agent-scopes-config was configured at all — the
// same permissive default this codebase uses for other optional config
// (see mcptools.Server.Inventory), so local/dev testing never requires
// standing up token config first. That is a different case from a hub that
// *does* require a token and the caller presented none, or an unrecognized
// one — that case must use DenyAllScope, never nil, so it fails closed
// instead of silently becoming unrestricted.
func (s *Scope) AllowsAccount(accountID string) bool {
	if s == nil {
		return true
	}
	return s.AllowedAccounts[accountID]
}

// DenyAllScope authorizes no AWS account at all. Use this — never nil, see
// AllowsAccount's doc comment — when a hub with --agent-scopes-config
// configured receives a request with a missing or unrecognized token.
var DenyAllScope = &Scope{AgentName: "", AllowedAccounts: map[string]bool{}}

// AgentConfig is one entry in --agent-scopes-config's YAML.
type AgentConfig struct {
	Name            string   `yaml:"name"`
	Token           string   `yaml:"token"`
	AllowedAccounts []string `yaml:"allowed_accounts"`
}

// Config is the top-level shape of --agent-scopes-config's YAML file.
type Config struct {
	Agents []AgentConfig `yaml:"agents"`
}

// LoadConfig reads an "agents: [...]" YAML document.
func LoadConfig(r io.Reader) (Config, error) {
	var cfg Config
	if err := yaml.NewDecoder(r).Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decoding agent scopes config: %w", err)
	}
	for i, a := range cfg.Agents {
		if a.Name == "" || a.Token == "" || len(a.AllowedAccounts) == 0 {
			return Config{}, fmt.Errorf("agent scopes config entry %d is missing a required field (name/token/allowed_accounts): %+v", i, AgentConfig{Name: a.Name, AllowedAccounts: a.AllowedAccounts})
		}
	}
	return cfg, nil
}

// Registry resolves a bearer token to its Scope. Tokens are only ever held
// and compared as a SHA-256 hash — never in plaintext, never logged — same
// discipline internal/redact applies to everything else that crosses a
// trust boundary.
type Registry struct {
	byTokenHash map[string]*Scope
}

// NewRegistry builds a Registry from a list of agent configs — typically
// Config.Agents loaded once at hub-server startup.
func NewRegistry(agents []AgentConfig) *Registry {
	byTokenHash := make(map[string]*Scope, len(agents))
	for _, a := range agents {
		allowed := make(map[string]bool, len(a.AllowedAccounts))
		for _, acct := range a.AllowedAccounts {
			allowed[acct] = true
		}
		byTokenHash[hashToken(a.Token)] = &Scope{AgentName: a.Name, AllowedAccounts: allowed}
	}
	return &Registry{byTokenHash: byTokenHash}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Resolve returns the Scope for a presented bearer token. ok is false when
// the token is empty or doesn't match any configured agent — callers must
// treat that as "deny" (see DenyAllScope), not fall back to Scope's nil
// "unrestricted" default, which is reserved for "no Registry configured at
// all."
func (r *Registry) Resolve(token string) (*Scope, bool) {
	if r == nil || token == "" {
		return nil, false
	}
	s, ok := r.byTokenHash[hashToken(token)]
	return s, ok
}

// TokenFromHeader extracts a bearer token from an HTTP Authorization
// header, e.g. "Bearer abc123" -> "abc123". Returns "" if the header is
// missing or not in Bearer form.
func TokenFromHeader(h http.Header) string {
	auth := h.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimPrefix(auth, prefix)
}
