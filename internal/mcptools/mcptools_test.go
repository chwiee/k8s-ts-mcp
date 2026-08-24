package mcptools

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/chwiee/k8s-ts-mcp/internal/agentauth"
	"github.com/chwiee/k8s-ts-mcp/internal/audit"
	"github.com/chwiee/k8s-ts-mcp/internal/inventory"
	"github.com/chwiee/k8s-ts-mcp/internal/policy"
	"github.com/chwiee/k8s-ts-mcp/internal/runbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/transport"
	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

// fakeHandler simulates a cluster-agent for a single playbook: proposes one
// "safe" action and, when executed, always resolves on the first attempt.
type fakeHandler struct{}

func (fakeHandler) Diagnose(_ context.Context, sig *pb.Signal) (string, string, []*pb.ProposedAction, map[string]string, bool, error) {
	return "core/crashloopbackoff", "pod " + sig.GetName() + " em CrashLoopBackOff",
		[]*pb.ProposedAction{{Name: "restart pod", Description: "kubectl delete pod " + sig.GetName(), Risk: "safe"}}, nil, false, nil
}

func (fakeHandler) Execute(_ context.Context, _ string, sig *pb.Signal) (bool, []*pb.Attempt, error) {
	return true, []*pb.Attempt{{Action: "restart pod", Output: "pod " + sig.GetName() + " deleted, recreated", Validated: true}}, nil
}

func (fakeHandler) Scan(_ context.Context, ns string) ([]*pb.PodIssue, error) {
	return []*pb.PodIssue{{Namespace: ns, Name: "nginx-abc", Kind: "PodCrashLoopBackOff", Detail: "back-off restarting failed container"}}, nil
}

func (fakeHandler) ApproveAction(context.Context, string, *pb.Signal, string, map[string]string) (*pb.Attempt, error) {
	return nil, fmt.Errorf("approve_action should never be called: fakeHandler's ladder has no high-risk/skipped action")
}

func (fakeHandler) GetLogs(_ context.Context, ns, name, deployment string, _ int64) (string, error) {
	if deployment != "" {
		return "log do deployment " + deployment, nil
	}
	return "log de " + ns + "/" + name, nil
}

func (fakeHandler) ListNodes(_ context.Context) ([]*pb.NodeInfo, error) {
	return []*pb.NodeInfo{{Name: "spoke-1-control-plane", Zone: "us-east-1a", Region: "us-east-1", InstanceType: "t3.medium", Architecture: "amd64", Ready: true}}, nil
}

// mixedRiskHandler simulates a playbook shaped like core/oomkilled: one safe
// action that doesn't resolve the incident, followed by a high-risk one —
// mirroring exactly what internal/execengine.Run itself would produce (the
// safe action really attempted, the high-risk one recorded as skipped).
type mixedRiskHandler struct{}

func (mixedRiskHandler) Diagnose(_ context.Context, sig *pb.Signal) (string, string, []*pb.ProposedAction, map[string]string, bool, error) {
	return "core/oomkilled", "pod " + sig.GetName() + " foi morto por OOM", []*pb.ProposedAction{
		{Name: "restart pod", Description: "kubectl delete pod " + sig.GetName(), Risk: "safe"},
		{Name: "increase memory limit", Description: "kubectl set resources ...", Risk: "high"},
	}, map[string]string{"deployment": "oomer"}, false, nil
}

func (mixedRiskHandler) Execute(_ context.Context, _ string, sig *pb.Signal) (bool, []*pb.Attempt, error) {
	return false, []*pb.Attempt{
		{Action: "restart pod", Output: "pod " + sig.GetName() + " deleted, recreated", Validated: false},
		{Action: "increase memory limit", Description: "kubectl set resources ...", Skipped: true, Error: "ação de alto risco — não executada automaticamente, requer aprovação manual"},
	}, nil
}

func (mixedRiskHandler) Scan(_ context.Context, ns string) ([]*pb.PodIssue, error) {
	return []*pb.PodIssue{{Namespace: ns, Name: "oomer-abc", Kind: "PodOOMKilled"}}, nil
}

// ApproveAction simulates agentcore.Handler.ApproveAction actually running
// the one high-risk action Execute above reported as skipped — proving
// approve_action's plumbing reaches an action that Execute alone never
// would.
func (mixedRiskHandler) ApproveAction(_ context.Context, playbookID string, sig *pb.Signal, actionName string, meta map[string]string) (*pb.Attempt, error) {
	if playbookID != "core/oomkilled" {
		return nil, fmt.Errorf("playbook mismatch: mixedRiskHandler only knows core/oomkilled, got %q", playbookID)
	}
	if meta["deployment"] != "oomer" {
		return nil, fmt.Errorf("meta não chegou como esperado: %v", meta)
	}
	if actionName != "increase memory limit" {
		return nil, fmt.Errorf("action %q não está na ladder atual do playbook %q", actionName, playbookID)
	}
	return &pb.Attempt{Action: actionName, Output: "memory limit increased for " + sig.GetName(), Validated: true}, nil
}

func (mixedRiskHandler) GetLogs(_ context.Context, ns, name, deployment string, _ int64) (string, error) {
	if deployment != "" {
		return "log do deployment " + deployment, nil
	}
	return "log de " + ns + "/" + name, nil
}

func (mixedRiskHandler) ListNodes(_ context.Context) ([]*pb.NodeInfo, error) {
	return []*pb.NodeInfo{{Name: "spoke-1-control-plane", Zone: "us-east-1a", Region: "us-east-1", InstanceType: "t3.medium", Architecture: "amd64", Ready: true}}, nil
}

// noPlaybookHandler simulates a signal no compiled playbook covers — the
// runbook fallback path. Logs, when set, is what GetLogs returns — used to
// test runbookFallback's log_signatures matching without a real cluster.
type noPlaybookHandler struct {
	Logs string
}

func (noPlaybookHandler) Diagnose(context.Context, *pb.Signal) (string, string, []*pb.ProposedAction, map[string]string, bool, error) {
	return "", "", nil, nil, true, nil
}

func (noPlaybookHandler) Execute(context.Context, string, *pb.Signal) (bool, []*pb.Attempt, error) {
	return false, nil, fmt.Errorf("execute should never be called when Diagnose reported no_playbook")
}

func (noPlaybookHandler) Scan(context.Context, string) ([]*pb.PodIssue, error) {
	return nil, nil
}

func (noPlaybookHandler) ApproveAction(context.Context, string, *pb.Signal, string, map[string]string) (*pb.Attempt, error) {
	return nil, fmt.Errorf("approve_action should never be called: noPlaybookHandler has no playbook at all")
}

func (h noPlaybookHandler) GetLogs(context.Context, string, string, string, int64) (string, error) {
	return h.Logs, nil
}

func (noPlaybookHandler) ListNodes(context.Context) ([]*pb.NodeInfo, error) {
	return nil, nil
}

// newTestServer wires a real bufconn hub<->agent connection (cluster
// "spoke-1") plus a real policy engine and a temp-file audit store, exactly
// like production wiring minus TLS and the real Kubernetes API.
func newTestServer(t *testing.T, groups policy.GroupMapping) *Server {
	t.Helper()
	return newTestServerWithHandler(t, groups, fakeHandler{})
}

func newTestServerWithHandler(t *testing.T, groups policy.GroupMapping, handler transport.Handler) *Server {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	hub := transport.NewServer()
	pb.RegisterAgentServiceServer(grpcServer, hub)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := transport.NewClient("passthrough:///bufnet", "spoke-1", "test", insecure.NewCredentials(), handler,
		grpc.WithContextDialer(dialer))
	go func() { _ = client.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, id := range hub.LocalClusters() {
			if id == "spoke-1" {
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	engine, err := policy.NewEngine(context.Background(), groups)
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	store, err := audit.NewFileStore(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("audit.NewFileStore: %v", err)
	}

	return &Server{Hub: hub, Policy: engine, Audit: store, ExecutionFlag: true}
}

func TestTroubleshoot_ReadonlyStaysDryRun(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"time-x-readonly": policy.TierReadOnly})
	s.CallerGroups = []string{"time-x-readonly"}

	_, out, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "PodCrashLoopBackOff", Namespace: "default", Name: "nginx-abc",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}
	if !out.Allowed || !out.DryRun {
		t.Fatalf("out = %+v, want Allowed=true DryRun=true", out)
	}
	if len(out.ProposedCommands) != 1 || !strings.Contains(out.ProposedCommands[0], "kubectl delete pod nginx-abc") {
		t.Errorf("ProposedCommands = %v, want the kubectl command", out.ProposedCommands)
	}
	if out.Resolved || len(out.Attempts) != 0 {
		t.Errorf("readonly caller must never get execution attempts, got %+v", out)
	}
	if out.IncidentID == "" {
		t.Error("IncidentID not set")
	}
}

func TestTroubleshoot_AdminExecutes(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.CallerGroups = []string{"infra-prod-admins"}

	_, out, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "PodCrashLoopBackOff", Namespace: "default", Name: "nginx-abc",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}
	if !out.Allowed || out.DryRun {
		t.Fatalf("out = %+v, want Allowed=true DryRun=false", out)
	}
	if !out.Resolved {
		t.Errorf("Resolved = false, want true")
	}
	if len(out.Attempts) != 1 || out.Attempts[0].Action != "restart pod" {
		t.Errorf("Attempts = %v, want one 'restart pod' attempt", out.Attempts)
	}

	// The postmortem tool should now be able to render this same incident.
	_, pm, err := s.postmortemHandler()(context.Background(), nil, GetPostmortemIn{IncidentID: out.IncidentID})
	if err != nil {
		t.Fatalf("get_postmortem: %v", err)
	}
	if !strings.Contains(pm.Text, "Resolvido") || !strings.Contains(pm.Text, "restart pod") {
		t.Errorf("postmortem text missing expected content:\n%s", pm.Text)
	}
}

// TestTroubleshoot_SafeActionRunsForRealDespiteHighRiskLaterInLadder guards
// the maxRisk fix: a ladder with a safe action followed by a high-risk one
// (like core/oomkilled) must still let the safe action execute for real —
// it must not fall back to dry-run just because something later in the
// ladder is high-risk.
func TestTroubleshoot_SafeActionRunsForRealDespiteHighRiskLaterInLadder(t *testing.T) {
	s := newTestServerWithHandler(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin}, mixedRiskHandler{})
	s.CallerGroups = []string{"infra-prod-admins"}

	_, out, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "PodOOMKilled", Namespace: "default", Name: "oomer-abc",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}
	if !out.Allowed || out.DryRun {
		t.Fatalf("out = %+v, want Allowed=true DryRun=false — a high-risk rung later in the ladder must not force the whole request dry-run", out)
	}
	if out.Resolved {
		t.Errorf("Resolved = true, want false — neither the failed safe attempt nor the skipped high-risk one fixed it")
	}
	if len(out.Attempts) != 2 {
		t.Fatalf("Attempts = %+v, want 2 (the real safe attempt plus the skip record)", out.Attempts)
	}
	if out.Attempts[0].Skipped {
		t.Errorf("Attempts[0] (safe) should not be skipped: %+v", out.Attempts[0])
	}
	if !out.Attempts[1].Skipped {
		t.Errorf("Attempts[1] (high risk) should be skipped: %+v", out.Attempts[1])
	}
}

func TestApproveAction_RunsHighRiskActionAfterApproval(t *testing.T) {
	s := newTestServerWithHandler(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin}, mixedRiskHandler{})
	s.CallerGroups = []string{"infra-prod-admins"}

	_, ts, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "PodOOMKilled", Namespace: "default", Name: "oomer-abc",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}

	_, out, err := s.approveActionHandler()(context.Background(), nil, ApproveActionIn{
		IncidentID: ts.IncidentID, ActionName: "increase memory limit",
	})
	if err != nil {
		t.Fatalf("approve_action: %v", err)
	}
	if out.OriginalIncidentID != ts.IncidentID {
		t.Errorf("OriginalIncidentID = %q, want %q", out.OriginalIncidentID, ts.IncidentID)
	}
	if out.IncidentID == "" || out.IncidentID == ts.IncidentID {
		t.Errorf("IncidentID = %q, want a new, non-empty incident id distinct from the original", out.IncidentID)
	}
	if !out.Attempt.Validated {
		t.Errorf("Attempt.Validated = false, want true: %+v", out.Attempt)
	}
	if out.Attempt.Skipped {
		t.Errorf("Attempt.Skipped = true, want false — approval must actually run the action, not skip it again")
	}

	// The approval itself should also be a retrievable postmortem.
	_, pm, err := s.postmortemHandler()(context.Background(), nil, GetPostmortemIn{IncidentID: out.IncidentID})
	if err != nil {
		t.Fatalf("get_postmortem for the approval incident: %v", err)
	}
	if !strings.Contains(pm.Text, "increase memory limit") {
		t.Errorf("postmortem for the approval incident missing the approved action:\n%s", pm.Text)
	}
}

func TestApproveAction_DeniesNonProdAdminTier(t *testing.T) {
	s := newTestServerWithHandler(t, policy.GroupMapping{
		"infra-prod-admins":    policy.TierProdAdmin,
		"infra-nonprod-admins": policy.TierNonProdAdmin,
	}, mixedRiskHandler{})
	s.CallerGroups = []string{"infra-prod-admins"}

	_, ts, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "PodOOMKilled", Namespace: "default", Name: "oomer-abc",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}

	// Same incident, but the caller approving it only has nonprod-admin now.
	s.CallerGroups = []string{"infra-nonprod-admins"}
	_, _, err = s.approveActionHandler()(context.Background(), nil, ApproveActionIn{
		IncidentID: ts.IncidentID, ActionName: "increase memory limit",
	})
	if err == nil {
		t.Fatal("approve_action succeeded for a nonprod-admin caller, want an error")
	}
}

func TestApproveAction_RejectsUnknownActionName(t *testing.T) {
	s := newTestServerWithHandler(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin}, mixedRiskHandler{})
	s.CallerGroups = []string{"infra-prod-admins"}

	_, ts, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "PodOOMKilled", Namespace: "default", Name: "oomer-abc",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}

	_, _, err = s.approveActionHandler()(context.Background(), nil, ApproveActionIn{
		IncidentID: ts.IncidentID, ActionName: "delete the whole cluster",
	})
	if err == nil {
		t.Fatal("approve_action succeeded for an action never proposed on this incident, want an error")
	}
}

func TestApproveAction_DeniedWhenGlobalExecutionDisabled(t *testing.T) {
	s := newTestServerWithHandler(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin}, mixedRiskHandler{})
	s.CallerGroups = []string{"infra-prod-admins"}

	_, ts, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "PodOOMKilled", Namespace: "default", Name: "oomer-abc",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}

	// Simulates a hub-server started without --execution-enabled — the
	// operator's global kill switch. Explicit per-action approval must not
	// be a way around it.
	s.ExecutionFlag = false
	_, _, err = s.approveActionHandler()(context.Background(), nil, ApproveActionIn{
		IncidentID: ts.IncidentID, ActionName: "increase memory limit",
	})
	if err == nil {
		t.Fatal("approve_action succeeded with ExecutionFlag=false, want an error")
	}
}

func TestApproveAction_RejectsNonHighRiskAction(t *testing.T) {
	s := newTestServerWithHandler(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin}, mixedRiskHandler{})
	s.CallerGroups = []string{"infra-prod-admins"}

	_, ts, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "PodOOMKilled", Namespace: "default", Name: "oomer-abc",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}

	_, _, err = s.approveActionHandler()(context.Background(), nil, ApproveActionIn{
		IncidentID: ts.IncidentID, ActionName: "restart pod", // proposed, but risk=safe, not high
	})
	if err == nil {
		t.Fatal("approve_action succeeded for a non-high-risk action, want an error — that path doesn't need explicit approval")
	}
}

func TestTroubleshoot_UnknownGroupDenied(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.CallerGroups = []string{"some-unmapped-group"}

	_, out, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "PodCrashLoopBackOff", Namespace: "default", Name: "nginx-abc",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}
	if out.Allowed {
		t.Errorf("out.Allowed = true, want false for an unmapped group")
	}
	if len(out.ProposedCommands) != 0 || out.Resolved {
		t.Errorf("a denied caller should get no commands and no execution, got %+v", out)
	}
}

func TestTroubleshoot_UnknownCluster(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.CallerGroups = []string{"infra-prod-admins"}

	_, _, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "does-not-exist", Kind: "PodCrashLoopBackOff",
	})
	if err == nil {
		t.Fatal("expected an error for a cluster with no connected agent")
	}
}

func TestScanCluster(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.CallerGroups = []string{"infra-prod-admins"}

	_, out, err := s.scanClusterHandler()(context.Background(), nil, ScanClusterIn{ClusterID: "spoke-1", Namespace: "default"})
	if err != nil {
		t.Fatalf("scan_cluster: %v", err)
	}
	if len(out.Issues) != 1 || out.Issues[0].Kind != "PodCrashLoopBackOff" || out.Issues[0].Name != "nginx-abc" {
		t.Errorf("Issues = %+v, want one PodCrashLoopBackOff/nginx-abc entry", out.Issues)
	}
}

func TestListNodes_NoInventory(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.CallerGroups = []string{"infra-prod-admins"}

	_, out, err := s.listNodesHandler()(context.Background(), nil, ListNodesIn{ClusterID: "spoke-1"})
	if err != nil {
		t.Fatalf("list_nodes: %v", err)
	}
	if out.InventoryKnown {
		t.Errorf("InventoryKnown = true, want false — Server.Inventory is nil in this test")
	}
	if out.AWSAccountID != "" || out.Region != "" {
		t.Errorf("out = %+v, want empty account/region when the cluster isn't in the inventory", out)
	}
	if len(out.Nodes) != 1 || out.Nodes[0].Name != "spoke-1-control-plane" {
		t.Errorf("Nodes = %+v, want one node named spoke-1-control-plane", out.Nodes)
	}
}

func TestListNodes_InventoryMatch(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.CallerGroups = []string{"infra-prod-admins"}
	s.Inventory = inventory.New([]inventory.ClusterInfo{
		{ClusterID: "spoke-1", AWSAccountID: "000000000000", Region: "us-east-1", EKSClusterName: "probe-cluster"},
	})

	_, out, err := s.listNodesHandler()(context.Background(), nil, ListNodesIn{ClusterID: "spoke-1", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("list_nodes: %v", err)
	}
	if !out.InventoryKnown || out.AWSAccountID != "000000000000" || out.Region != "us-east-1" || out.EKSClusterName != "probe-cluster" {
		t.Errorf("out = %+v, want the inventory's account/region/eks_cluster_name", out)
	}
	if len(out.Nodes) != 1 {
		t.Errorf("Nodes = %+v, want one node", out.Nodes)
	}
}

// scopeAllowing builds an *agentauth.Scope authorizing exactly the given
// AWS account IDs, going through agentauth.Registry.Resolve like a real
// token lookup would — not constructed by hand — so these tests exercise
// the same path production wiring does.
func scopeAllowing(t *testing.T, accounts ...string) *agentauth.Scope {
	t.Helper()
	reg := agentauth.NewRegistry([]agentauth.AgentConfig{
		{Name: "mars-agent", Token: "t", AllowedAccounts: accounts},
	})
	scope, ok := reg.Resolve("t")
	if !ok {
		t.Fatal("Resolve(t) = not found, want found")
	}
	return scope
}

func TestCheckClusterScope_NilAgentScopeUnrestricted(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.Inventory = inventory.New([]inventory.ClusterInfo{
		{ClusterID: "spoke-1", AWSAccountID: "999999999999", Region: "us-east-1"},
	})
	// s.AgentScope is nil (no --agent-scopes-config) — must never refuse.
	if _, _, err := s.checkClusterScope(context.Background(), "spoke-1"); err != nil {
		t.Fatalf("checkClusterScope with nil AgentScope: %v", err)
	}
}

func TestCheckClusterScope_AllowedAccount(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.Inventory = inventory.New([]inventory.ClusterInfo{
		{ClusterID: "spoke-1", AWSAccountID: "123456789012", Region: "us-east-1"},
	})
	s.AgentScope = scopeAllowing(t, "123456789012")

	if _, _, err := s.checkClusterScope(context.Background(), "spoke-1"); err != nil {
		t.Fatalf("checkClusterScope for an in-scope account: %v", err)
	}
}

func TestCheckClusterScope_DeniedAccount(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.Inventory = inventory.New([]inventory.ClusterInfo{
		{ClusterID: "spoke-1", AWSAccountID: "999999999999", Region: "us-east-1"},
	})
	s.AgentScope = scopeAllowing(t, "123456789012")

	if _, _, err := s.checkClusterScope(context.Background(), "spoke-1"); err == nil {
		t.Fatal("checkClusterScope allowed a cluster in an account outside the agent's scope, want an error")
	}
}

func TestCheckClusterScope_UnknownClusterDeniedWhenScoped(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	// No Inventory entry for spoke-1 at all.
	s.AgentScope = scopeAllowing(t, "123456789012")

	if _, _, err := s.checkClusterScope(context.Background(), "spoke-1"); err == nil {
		t.Fatal("checkClusterScope allowed a cluster the inventory can't confirm, want an error — can't prove it's in scope")
	}
}

func TestScanCluster_DeniedOutsideAgentScope(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.CallerGroups = []string{"infra-prod-admins"}
	s.Inventory = inventory.New([]inventory.ClusterInfo{
		{ClusterID: "spoke-1", AWSAccountID: "999999999999", Region: "us-east-1"},
	})
	s.AgentScope = scopeAllowing(t, "123456789012")

	_, _, err := s.scanClusterHandler()(context.Background(), nil, ScanClusterIn{ClusterID: "spoke-1"})
	if err == nil {
		t.Fatal("scan_cluster ran against a cluster outside the agent's account scope, want an error")
	}
}

func TestListNodes_RegionMismatchRefused(t *testing.T) {
	s := newTestServer(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin})
	s.CallerGroups = []string{"infra-prod-admins"}
	s.Inventory = inventory.New([]inventory.ClusterInfo{
		{ClusterID: "spoke-1", AWSAccountID: "000000000000", Region: "us-east-1"},
	})

	_, _, err := s.listNodesHandler()(context.Background(), nil, ListNodesIn{ClusterID: "spoke-1", Region: "us-west-2"})
	if err == nil {
		t.Fatal("list_nodes accepted a region that doesn't match the inventory, want an error instead of a wrong-cluster node list")
	}
}

const testRunbookDoc = `# Catálogo de teste

## Nó preso em NotReady
kind: NodeNotReady
keywords: notready, kubelet down

**Causa comum**: kubelet parou de reportar.

**Solução**: reiniciar o kubelet do nó.

---

## Falha de execução por arquitetura incompatível
kind: PodExecFormatError
keywords: exec format error, arquitetura, arm, amd64
log_signatures: exec format error, exec user process caused
log_source: self

**Causa comum**: imagem buildada para arquitetura de CPU diferente do nó.

**Solução**: rebuild pra arquitetura certa ou nodeAffinity se houver nó compatível.
`

func newTestRunbookStore(t *testing.T) *runbooks.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runbooks.md")
	if err := os.WriteFile(path, []byte(testRunbookDoc), 0o644); err != nil {
		t.Fatalf("writing test runbooks doc: %v", err)
	}
	store, err := runbooks.NewStore(path)
	if err != nil {
		t.Fatalf("runbooks.NewStore: %v", err)
	}
	return store
}

func TestTroubleshoot_FallsBackToRunbook_WhenNoPlaybookMatches(t *testing.T) {
	s := newTestServerWithHandler(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin}, noPlaybookHandler{})
	s.CallerGroups = []string{"infra-prod-admins"}
	s.Runbooks = newTestRunbookStore(t)
	s.ExecutionFlag = true // must not matter — runbook fallback is always dry-run regardless

	_, out, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "NodeNotReady",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}
	if !out.Allowed || !out.DryRun {
		t.Fatalf("out = %+v, want Allowed=true DryRun=true — a runbook is always proposal-only", out)
	}
	if out.PlaybookID != "runbook" {
		t.Errorf("PlaybookID = %q, want \"runbook\"", out.PlaybookID)
	}
	if len(out.ProposedCommands) != 1 || !strings.Contains(out.ProposedCommands[0], "reiniciar o kubelet") {
		t.Errorf("ProposedCommands = %v, want the runbook's Body", out.ProposedCommands)
	}
	if out.IncidentID == "" {
		t.Error("IncidentID not set")
	}
}

func TestTroubleshoot_RunbookFallback_NoMatchingEntry(t *testing.T) {
	s := newTestServerWithHandler(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin}, noPlaybookHandler{})
	s.CallerGroups = []string{"infra-prod-admins"}
	s.Runbooks = newTestRunbookStore(t)

	_, out, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "SomethingNobodyHasEverSeen",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}
	if len(out.ProposedCommands) != 0 {
		t.Errorf("ProposedCommands = %v, want none — no runbook entry matches this kind", out.ProposedCommands)
	}
	if !out.DryRun {
		t.Error("DryRun = false, want true")
	}
}

func TestTroubleshoot_RunbookFallback_ConfirmedByLogs(t *testing.T) {
	handler := noPlaybookHandler{Logs: "starting up...\nexec format error\n"}
	s := newTestServerWithHandler(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin}, handler)
	s.CallerGroups = []string{"infra-prod-admins"}
	s.Runbooks = newTestRunbookStore(t)

	_, out, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "PodExecFormatError", Namespace: "default", Name: "wrongarch-abc",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}
	if !strings.Contains(out.Summary, "confirmado pelo log") {
		t.Errorf("Summary = %q, want it to say the entry was confirmed by log", out.Summary)
	}
	if len(out.ProposedCommands) != 2 || !strings.Contains(out.ProposedCommands[1], "exec format error") {
		t.Errorf("ProposedCommands = %v, want the runbook body plus the log evidence", out.ProposedCommands)
	}
}

func TestTroubleshoot_RunbookFallback_LogDoesNotConfirm(t *testing.T) {
	handler := noPlaybookHandler{Logs: "everything is fine here, no errors at all\n"}
	s := newTestServerWithHandler(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin}, handler)
	s.CallerGroups = []string{"infra-prod-admins"}
	s.Runbooks = newTestRunbookStore(t)

	_, out, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "PodExecFormatError", Namespace: "default", Name: "wrongarch-abc",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}
	if strings.Contains(out.Summary, "confirmado pelo log") {
		t.Errorf("Summary = %q, should not claim log confirmation when the signature isn't there", out.Summary)
	}
	if len(out.ProposedCommands) != 1 {
		t.Errorf("ProposedCommands = %v, want just the runbook body (no log evidence) when unconfirmed", out.ProposedCommands)
	}
}

func TestTroubleshoot_RunbookFallback_NoStoreConfigured(t *testing.T) {
	s := newTestServerWithHandler(t, policy.GroupMapping{"infra-prod-admins": policy.TierProdAdmin}, noPlaybookHandler{})
	s.CallerGroups = []string{"infra-prod-admins"}
	// s.Runbooks left nil on purpose.

	_, out, err := s.troubleshootHandler()(context.Background(), nil, TroubleshootIn{
		ClusterID: "spoke-1", Kind: "NodeNotReady",
	})
	if err != nil {
		t.Fatalf("troubleshoot: %v", err)
	}
	if out.Summary == "" {
		t.Error("Summary empty, want a message explaining no runbook store is configured")
	}
}
