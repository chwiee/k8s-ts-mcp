package mcptools

import (
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// ToolAccess maps a tool name to an AD-group-name substring a caller must
// have at least one recognized group matching (case-insensitive) to even
// see that tool registered for their MCP session — separate from
// policy.Engine's tier gating, which decides whether an already-visible
// tool's ACTION is allowed to run once called. A tool missing from this map,
// or mapped to "", is visible to any caller with at least one recognized
// group. Deliberately just a substring match, not a real RBAC engine — this
// is the mechanism behind "se o grupo não for de SRE (conter SRE no nome)
// não deverá conseguir executar / ter ciência da existência da tool".
//
// Enforced at Register (see Register below), called once per new MCP
// session with that session's own Server copy — a caller whose groups don't
// satisfy a tool's requirement never sees it in tools/list and can't call
// it either, since it was never registered on their session's *mcp.Server
// in the first place. There's no separate runtime check to bypass by
// calling the tool name directly.
type ToolAccess map[string]string

// Allows reports whether any of groups satisfies this ToolAccess's
// requirement for toolName. No entry (or an empty requirement) means
// unrestricted. A nil ToolAccess (no --tool-access-config configured) is
// unrestricted for every tool — same permissive default as every other
// optional config in this package.
func (access ToolAccess) Allows(toolName string, groups []string) bool {
	required := access[toolName]
	if required == "" {
		return true
	}
	for _, g := range groups {
		if strings.Contains(strings.ToUpper(g), strings.ToUpper(required)) {
			return true
		}
	}
	return false
}

// toolAccessConfig is the top-level shape of --tool-access-config's YAML.
type toolAccessConfig struct {
	Tools []struct {
		Name              string `yaml:"name"`
		RequiresGroupName string `yaml:"requires_group_name"`
	} `yaml:"tools"`
}

// LoadToolAccess reads a "tools: [{name, requires_group_name}]" YAML
// document.
func LoadToolAccess(r io.Reader) (ToolAccess, error) {
	var cfg toolAccessConfig
	if err := yaml.NewDecoder(r).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decoding tool access config: %w", err)
	}
	access := make(ToolAccess, len(cfg.Tools))
	for _, t := range cfg.Tools {
		if t.Name == "" {
			return nil, fmt.Errorf("tool access config entry missing name: %+v", t)
		}
		access[t.Name] = t.RequiresGroupName
	}
	return access, nil
}
