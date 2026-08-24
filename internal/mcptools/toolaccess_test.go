package mcptools

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chwiee/k8s-ts-mcp/internal/policy"
)

func TestLoadToolAccess(t *testing.T) {
	doc := `
tools:
  - name: approve_action
    requires_group_name: SRE
  - name: get_postmortem
    requires_group_name: ""
`
	access, err := LoadToolAccess(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("LoadToolAccess: %v", err)
	}
	if access["approve_action"] != "SRE" {
		t.Errorf("access[approve_action] = %q, want SRE", access["approve_action"])
	}
}

func TestLoadToolAccess_MissingName(t *testing.T) {
	doc := `
tools:
  - requires_group_name: SRE
`
	if _, err := LoadToolAccess(strings.NewReader(doc)); err == nil {
		t.Fatal("LoadToolAccess accepted an entry with no name, want an error")
	}
}

func TestToolAccess_Allows(t *testing.T) {
	access := ToolAccess{"approve_action": "SRE"}

	if !access.Allows("approve_action", []string{"infra-sre-prod"}) {
		t.Error("Allows(approve_action, [infra-sre-prod]) = false, want true (case-insensitive substring match)")
	}
	if access.Allows("approve_action", []string{"infra-prod-admins"}) {
		t.Error("Allows(approve_action, [infra-prod-admins]) = true, want false — no group contains SRE")
	}
	if !access.Allows("scan_cluster", []string{"infra-prod-admins"}) {
		t.Error("Allows(scan_cluster, ...) = false, want true — scan_cluster has no entry, unrestricted")
	}
}

func TestToolAccess_Allows_NilUnrestricted(t *testing.T) {
	var access ToolAccess
	if !access.Allows("approve_action", nil) {
		t.Error("nil ToolAccess must be unrestricted (no --tool-access-config configured)")
	}
}

// listRegisteredTools connects a real in-memory MCP client to a server
// built by Register(srv, s), and returns the names tools/list reports —
// proving the restriction happens at registration (never visible, not just
// never callable), the same way a real caller would observe it.
func listRegisteredTools(t *testing.T, s *Server) []string {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	Register(srv, s)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := srv.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("Server.Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("Client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	result, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestRegister_ToolHiddenWithoutRequiredGroup(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.CallerGroups = []string{"infra-prod-admins"} // no "SRE" substring anywhere
	s.ToolAccess = ToolAccess{"approve_action": "SRE"}

	names := listRegisteredTools(t, s)
	if containsName(names, "approve_action") {
		t.Error("approve_action appeared in tools/list for a caller with no SRE group, want hidden entirely")
	}
	if !containsName(names, "scan_cluster") {
		t.Error("scan_cluster missing from tools/list — it has no ToolAccess entry, must stay unrestricted")
	}
}

func TestRegister_ToolVisibleWithRequiredGroup(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-sre-prod": policy.TierProdAdmin})
	s.CallerGroups = []string{"infra-sre-prod"}
	s.ToolAccess = ToolAccess{"approve_action": "SRE"}

	names := listRegisteredTools(t, s)
	if !containsName(names, "approve_action") {
		t.Error("approve_action missing from tools/list for a caller whose group contains SRE, want visible")
	}
}
