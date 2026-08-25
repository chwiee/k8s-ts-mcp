package agentauth

import (
	"net/http"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	doc := `
agents:
  - name: mars-agent
    token: mars-secret-token
    allowed_accounts: ["123456789012"]
  - name: floci-agent
    token: floci-secret-token
    allowed_accounts: ["000000000000"]
`
	cfg, err := LoadConfig(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Agents) != 2 || cfg.Agents[0].Name != "mars-agent" {
		t.Errorf("Agents = %+v", cfg.Agents)
	}
}

func TestLoadConfig_MissingField(t *testing.T) {
	doc := `
agents:
  - name: mars-agent
    token: mars-secret-token
`
	_, err := LoadConfig(strings.NewReader(doc))
	if err == nil {
		t.Fatal("LoadConfig accepted an entry with no allowed_accounts, want an error")
	}
}

func TestRegistry_Resolve(t *testing.T) {
	reg := NewRegistry([]AgentConfig{
		{Name: "mars-agent", Token: "mars-secret-token", AllowedAccounts: []string{"123456789012"}},
	})

	scope, ok := reg.Resolve("mars-secret-token")
	if !ok {
		t.Fatal("Resolve(mars-secret-token) = not found, want found")
	}
	if scope.AgentName != "mars-agent" || !scope.AllowsAccount("123456789012") {
		t.Errorf("scope = %+v", scope)
	}
	if scope.AllowsAccount("999999999999") {
		t.Error("scope allows an account it was never granted")
	}
}

func TestRegistry_Resolve_UnknownToken(t *testing.T) {
	reg := NewRegistry([]AgentConfig{
		{Name: "mars-agent", Token: "mars-secret-token", AllowedAccounts: []string{"123456789012"}},
	})
	if _, ok := reg.Resolve("wrong-token"); ok {
		t.Error("Resolve(wrong-token) = found, want not found")
	}
	if _, ok := reg.Resolve(""); ok {
		t.Error("Resolve(\"\") = found, want not found")
	}
}

func TestScope_AllowsAccount_NilIsUnrestricted(t *testing.T) {
	var s *Scope
	if !s.AllowsAccount("any-account-at-all") {
		t.Error("nil *Scope must be unrestricted (no --agent-scopes-config configured)")
	}
}

func TestLoadConfig_ADGroups(t *testing.T) {
	doc := `
agents:
  - name: sre-agent
    token: sre-secret-token
    allowed_accounts: ["123456789012"]
    ad_groups: ["infra-sre-prod"]
  - name: readonly-agent
    token: readonly-secret-token
    allowed_accounts: ["123456789012"]
`
	cfg, err := LoadConfig(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Agents[0].ADGroups) != 1 || cfg.Agents[0].ADGroups[0] != "infra-sre-prod" {
		t.Errorf("Agents[0].ADGroups = %v", cfg.Agents[0].ADGroups)
	}
	if cfg.Agents[1].ADGroups != nil {
		t.Errorf("Agents[1].ADGroups = %v, want nil — ad_groups is optional", cfg.Agents[1].ADGroups)
	}
}

func TestRegistry_Resolve_CarriesADGroups(t *testing.T) {
	reg := NewRegistry([]AgentConfig{
		{Name: "sre-agent", Token: "sre-secret-token", AllowedAccounts: []string{"123456789012"}, ADGroups: []string{"infra-sre-prod"}},
	})
	scope, ok := reg.Resolve("sre-secret-token")
	if !ok {
		t.Fatal("Resolve(sre-secret-token) = not found, want found")
	}
	if len(scope.ADGroups) != 1 || scope.ADGroups[0] != "infra-sre-prod" {
		t.Errorf("scope.ADGroups = %v, want [infra-sre-prod]", scope.ADGroups)
	}
}

func TestDenyAllScope(t *testing.T) {
	if DenyAllScope.AllowsAccount("123456789012") {
		t.Error("DenyAllScope allowed an account — it must allow none")
	}
	if len(DenyAllScope.ADGroups) != 0 {
		t.Errorf("DenyAllScope.ADGroups = %v, want empty — a denied caller has no simulated AD identity either", DenyAllScope.ADGroups)
	}
}

func TestTokenFromHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer abc123")
	if got := TokenFromHeader(h); got != "abc123" {
		t.Errorf("TokenFromHeader = %q, want abc123", got)
	}

	h2 := http.Header{}
	if got := TokenFromHeader(h2); got != "" {
		t.Errorf("TokenFromHeader with no header = %q, want empty", got)
	}

	h3 := http.Header{}
	h3.Set("Authorization", "Basic dXNlcjpwYXNz")
	if got := TokenFromHeader(h3); got != "" {
		t.Errorf("TokenFromHeader with non-Bearer auth = %q, want empty", got)
	}
}
