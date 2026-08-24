// Package mcptools registers k8s-ts-mcp's hub-server MCP tools: list which
// clusters are connected, diagnose/remediate a signal on one of them
// (gated by internal/policy), and render a postmortem for a past incident
// from internal/audit.
package mcptools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chwiee/k8s-ts-mcp/internal/audit"
	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/policy"
	"github.com/chwiee/k8s-ts-mcp/internal/postmortem"
	"github.com/chwiee/k8s-ts-mcp/internal/runbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/transport"
	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

// Server holds everything the hub-server's tool handlers need.
type Server struct {
	Hub    *transport.Server
	Policy *policy.Engine
	Audit  audit.Store

	// ExecutionFlag is the global dry-run/auto switch (requirement #3).
	// policy.Engine can still force dry-run regardless (e.g. high-risk
	// actions, or a caller tier that never executes) — this only raises
	// the ceiling, it never lowers it.
	ExecutionFlag bool
	// ProdClusters marks which cluster IDs are "prod" for policy purposes.
	// There's no cluster inventory service yet — this is loaded from a
	// flag at hub-server startup (see cmd/hub-server).
	ProdClusters map[string]bool
	// CallerGroups is TEMPORARY: every request is currently evaluated as
	// if it came from these AD groups, since the calling agent (wb-ce-agent)
	// doesn't yet propagate a real caller identity. Remove once it does.
	CallerGroups []string
	// Runbooks is the static, human-authored knowledge base (see
	// docs/runbooks/kubernetes-errors.md) troubleshoot falls back to when
	// no compiled playbook matches a signal at all. Nil is valid — that
	// case just means no runbook fallback text is available.
	Runbooks *runbooks.Store
}

func (s *Server) clusterEnv(clusterID string) policy.ClusterEnv {
	if s.ProdClusters[clusterID] {
		return policy.EnvProd
	}
	return policy.EnvNonProd
}

// riskRank orders proposed-action risk strings for maxRisk.
var riskRank = map[string]int{
	string(policy.RiskSafe):   0,
	string(policy.RiskMedium): 1,
	string(policy.RiskHigh):   2,
}

// maxRisk returns the highest risk level among a playbook's proposed
// actions that could actually run automatically — RiskHigh actions are
// excluded on purpose. execengine.Run refuses to auto-run those no matter
// what the caller is authorized to do (see its doc comment), so letting a
// high-risk rung further down the ladder force the *whole* request into
// dry-run would block even the safe/medium actions before it from ever
// running for real. If every action in the ladder is RiskHigh (a purely
// diagnostic playbook, e.g. core/execformaterror), there's nothing else to
// gate on, so this falls back to RiskHigh — correctly keeping the whole
// thing dry-run/proposal-only, since that's all there is.
func maxRisk(actions []*pb.ProposedAction) policy.RiskLevel {
	best, bestRank := policy.RiskSafe, -1
	sawOnlyHigh := true
	for _, a := range actions {
		if a.Risk == string(policy.RiskHigh) {
			continue
		}
		sawOnlyHigh = false
		if r, ok := riskRank[a.Risk]; ok && r > bestRank {
			bestRank, best = r, policy.RiskLevel(a.Risk)
		}
	}
	if sawOnlyHigh {
		return policy.RiskHigh
	}
	return best
}

// Register wires every tool onto server.
func Register(server *mcp.Server, s *Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_clusters",
		Description: "Lista os clusters atualmente conectados ao hub e disponíveis para troubleshooting.",
	}, s.listClustersHandler())

	mcp.AddTool(server, &mcp.Tool{
		Name: "scan_cluster",
		Description: "Lista os pods com problema num cluster — use isto ANTES de troubleshoot sempre que o usuário " +
			"não tiver informado o nome exato de um pod (ex: \"valide os pods do spoke-1\", \"tem algo quebrado no " +
			"spoke-2?\"). Cada resultado já vem com kind pronto para passar direto pra troubleshoot; kind " +
			"\"PodUnknownError\" significa que o agente notou o pod doente mas não reconheceu o problema — não existe " +
			"playbook automatizado para esse caso, apenas reporte ao usuário.",
	}, s.scanClusterHandler())

	mcp.AddTool(server, &mcp.Tool{
		Name: "troubleshoot",
		Description: "Diagnostica um sinal num cluster (ex: pod em CrashLoopBackOff, nó com Calico degradado, " +
			"ScaledObject do KEDA travado) e, se a política de acesso permitir, executa a correção. Sempre pergunte " +
			"ao usuário o cluster_id se ele não tiver sido informado explicitamente — nunca adivinhe. Se o usuário " +
			"não souber o nome do pod, use scan_cluster primeiro. Retorna se a ação foi executada de verdade " +
			"(allowed=true, dry_run=false) ou só os comandos propostos (dry_run=true) — nesse caso, explique ao " +
			"usuário que ele precisa rodar os comandos manualmente. Se não houver playbook automatizado para o " +
			"sinal, a resposta vem do catálogo de runbooks (texto de referência) — sempre dry_run=true nesse caso, " +
			"porque é só conhecimento, nunca uma ação real.",
	}, s.troubleshootHandler())

	mcp.AddTool(server, &mcp.Tool{
		Name: "approve_action",
		Description: "Aprova e executa exatamente UMA ação de alto risco que troubleshoot propôs mas não executou " +
			"automaticamente (risk=\"high\" nos attempts com skipped=true, ou risk=\"high\" nos proposed_commands " +
			"quando dry_run=true). Use o incident_id retornado por troubleshoot e o action_name exato da ação. Só " +
			"chamadores com tier prod-admin podem aprovar, e só funciona se este hub estiver com execução ligada — " +
			"não é um jeito de contornar um hub configurado como só-proposta. O agente do cluster reconfere o " +
			"estado atual antes de rodar — se o problema já foi resolvido por outro caminho ou o playbook mudou, a " +
			"aprovação falha em vez de rodar algo que não é mais o que foi proposto. SEMPRE confirme com o usuário " +
			"antes de chamar esta ferramenta — diferente de troubleshoot em dry-run, isso executa uma mudança real " +
			"e irreversível no cluster.",
	}, s.approveActionHandler())

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_postmortem",
		Description: "Retorna o post-mortem em texto simples de um incidente, pelo incident_id retornado por troubleshoot.",
	}, s.postmortemHandler())
}

type ListClustersOut struct {
	ClusterIDs []string `json:"cluster_ids"`
}

type ScanClusterIn struct {
	ClusterID string `json:"cluster_id" jsonschema:"identificador do cluster registrado no hub — use list_clusters se não tiver certeza do ID exato"`
	Namespace string `json:"namespace,omitempty" jsonschema:"restringe a busca a um namespace; vazio busca em todos"`
}

type PodIssueOut struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Detail    string `json:"detail,omitempty"`
}

type ScanClusterOut struct {
	Issues []PodIssueOut `json:"issues"`
}

func (s *Server) scanClusterHandler() mcp.ToolHandlerFor[ScanClusterIn, ScanClusterOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ScanClusterIn) (*mcp.CallToolResult, ScanClusterOut, error) {
		resp, err := s.Hub.Scan(ctx, in.ClusterID, in.Namespace)
		if err != nil {
			return nil, ScanClusterOut{}, err
		}
		if resp.Error != "" {
			return nil, ScanClusterOut{}, fmt.Errorf("agente do cluster %s: %s", in.ClusterID, resp.Error)
		}
		out := ScanClusterOut{Issues: make([]PodIssueOut, 0, len(resp.Issues))}
		for _, i := range resp.Issues {
			out.Issues = append(out.Issues, PodIssueOut{Namespace: i.Namespace, Name: i.Name, Kind: i.Kind, Detail: i.Detail})
		}
		return nil, out, nil
	}
}

func (s *Server) listClustersHandler() mcp.ToolHandlerFor[struct{}, ListClustersOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListClustersOut, error) {
		ids, err := s.Hub.ConnectedClusters(ctx)
		if err != nil {
			return nil, ListClustersOut{}, fmt.Errorf("listando clusters: %w", err)
		}
		return nil, ListClustersOut{ClusterIDs: ids}, nil
	}
}

type TroubleshootIn struct {
	ClusterID string `json:"cluster_id" jsonschema:"identificador do cluster registrado no hub onde investigar — use list_clusters se não tiver certeza do ID exato"`
	Kind      string `json:"kind" jsonschema:"tipo do sinal — PodCrashLoopBackOff, PodOOMKilled, PodImagePullBackOff, PodExecFormatError, CalicoNodeDegraded ou KEDAScaledObjectStuck"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace do recurso afetado"`
	Name      string `json:"name,omitempty" jsonschema:"nome do recurso afetado (ex: nome do pod ou do scaledobject)"`
	NodeName  string `json:"node_name,omitempty" jsonschema:"nome do nó — usado por sinais de nível de nó, como o do Calico"`
}

type AttemptOut struct {
	Action      string `json:"action"`
	Description string `json:"description,omitempty"`
	Risk        string `json:"risk"`
	Output      string `json:"output,omitempty"`
	Error       string `json:"error,omitempty"`
	Validated   bool   `json:"validated"`
	RolledBack  bool   `json:"rolled_back"`
	Skipped     bool   `json:"skipped,omitempty"`
}

type TroubleshootOut struct {
	IncidentID       string       `json:"incident_id"`
	PlaybookID       string       `json:"playbook_id"`
	Summary          string       `json:"summary"`
	Allowed          bool         `json:"allowed"`
	DryRun           bool         `json:"dry_run"`
	Reason           string       `json:"reason"`
	ProposedCommands []string     `json:"proposed_commands,omitempty"`
	Resolved         bool         `json:"resolved,omitempty"`
	Attempts         []AttemptOut `json:"attempts,omitempty"`
}

func (s *Server) troubleshootHandler() mcp.ToolHandlerFor[TroubleshootIn, TroubleshootOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in TroubleshootIn) (*mcp.CallToolResult, TroubleshootOut, error) {
		sig := &pb.Signal{Kind: in.Kind, Namespace: in.Namespace, Name: in.Name, NodeName: in.NodeName}

		diag, err := s.Hub.Diagnose(ctx, in.ClusterID, sig)
		if err != nil {
			return nil, TroubleshootOut{}, err
		}
		if diag.Error != "" {
			return nil, TroubleshootOut{}, fmt.Errorf("agente do cluster %s: %s", in.ClusterID, diag.Error)
		}
		if diag.NoPlaybook {
			return s.runbookFallback(ctx, in)
		}

		decision, err := s.Policy.Decide(ctx, policy.Request{
			Groups:        s.CallerGroups,
			ClusterEnv:    s.clusterEnv(in.ClusterID),
			ActionRisk:    maxRisk(diag.ProposedActions),
			ExecutionFlag: s.ExecutionFlag,
		})
		if err != nil {
			return nil, TroubleshootOut{}, fmt.Errorf("avaliando política: %w", err)
		}

		entry := audit.Entry{
			ID: audit.NewID(), Time: time.Now().UTC(), ClusterID: in.ClusterID,
			Groups: s.CallerGroups, PlaybookID: diag.PlaybookId, Summary: diag.Summary,
			Decision: decision,
			Signal:   audit.SignalRecord{Kind: in.Kind, Namespace: in.Namespace, Name: in.Name, NodeName: in.NodeName},
			Meta:     diag.Meta,
		}
		for _, a := range diag.ProposedActions {
			entry.ProposedActions = append(entry.ProposedActions, audit.ProposedActionRecord{Name: a.Name, Description: a.Description, Risk: a.Risk})
		}
		out := TroubleshootOut{
			PlaybookID: diag.PlaybookId, Summary: diag.Summary,
			Allowed: decision.Allow, DryRun: decision.DryRun, Reason: decision.Reason,
		}

		switch {
		case !decision.Allow:
			// nothing to run or propose — the caller isn't authorized at all.
		case decision.DryRun:
			for _, a := range diag.ProposedActions {
				entry.ProposedCommands = append(entry.ProposedCommands, fmt.Sprintf("[%s] %s — %s", a.Risk, a.Name, a.Description))
			}
			out.ProposedCommands = entry.ProposedCommands
		default:
			exec, err := s.Hub.Execute(ctx, in.ClusterID, diag.PlaybookId, sig)
			if err != nil {
				return nil, TroubleshootOut{}, fmt.Errorf("executando no cluster %s: %w", in.ClusterID, err)
			}
			if exec.Error != "" {
				return nil, TroubleshootOut{}, fmt.Errorf("agente do cluster %s: %s", in.ClusterID, exec.Error)
			}
			result := &execengine.Result{PlaybookID: diag.PlaybookId, Resolved: exec.Resolved}
			for _, a := range exec.Attempts {
				result.Attempts = append(result.Attempts, execengine.AttemptResult{
					Action: a.Action, Description: a.Description, Risk: policy.RiskLevel(a.Risk),
					Output: a.Output, Err: a.Error, Validated: a.Validated, RolledBack: a.RolledBack,
					Skipped: a.Skipped,
				})
				out.Attempts = append(out.Attempts, AttemptOut{
					Action: a.Action, Description: a.Description, Risk: a.Risk, Output: a.Output,
					Error: a.Error, Validated: a.Validated, RolledBack: a.RolledBack, Skipped: a.Skipped,
				})
			}
			entry.Result = result
			out.Resolved = exec.Resolved
		}

		if err := s.Audit.Append(ctx, entry); err != nil {
			return nil, TroubleshootOut{}, fmt.Errorf("gravando auditoria: %w", err)
		}
		out.IncidentID = entry.ID
		return nil, out, nil
	}
}

// runbookFallback handles the case where no compiled playbook matched the
// signal at all (diag.NoPlaybook) — it never calls Diagnose/Execute again
// and never touches policy, because there's nothing to authorize: a
// runbook is text, always dry_run, no matter who's asking or whether
// execution is enabled.
func (s *Server) runbookFallback(ctx context.Context, in TroubleshootIn) (*mcp.CallToolResult, TroubleshootOut, error) {
	entry := audit.Entry{
		ID: audit.NewID(), Time: time.Now().UTC(), ClusterID: in.ClusterID,
		Groups: s.CallerGroups, PlaybookID: "runbook",
		Decision: policy.Decision{Allow: true, DryRun: true, Reason: "sem playbook automatizado para esse sinal — resposta vem do catálogo de runbooks, sempre só como proposta"},
	}
	out := TroubleshootOut{PlaybookID: "runbook", Allowed: true, DryRun: true, Reason: entry.Decision.Reason}

	var rb runbooks.Entry
	var ok bool
	if s.Runbooks != nil {
		rb, ok = s.Runbooks.Find(in.Kind, in.Kind)
	}
	switch {
	case ok && len(rb.LogSignatures) > 0:
		out.Summary, out.ProposedCommands = s.confirmViaLogs(ctx, in, rb)
	case ok:
		out.Summary = rb.Title
		out.ProposedCommands = []string{rb.Body}
	case s.Runbooks != nil:
		out.Summary = fmt.Sprintf("nenhum playbook automatizado nem entrada no catálogo de runbooks para o sinal %q — sem sugestão automática pra esse caso ainda.", in.Kind)
	default:
		out.Summary = fmt.Sprintf("nenhum playbook automatizado conhece o sinal %q, e não há catálogo de runbooks configurado neste hub.", in.Kind)
	}
	entry.Summary = out.Summary
	entry.ProposedCommands = out.ProposedCommands

	if err := s.Audit.Append(ctx, entry); err != nil {
		return nil, TroubleshootOut{}, fmt.Errorf("gravando auditoria: %w", err)
	}
	out.IncidentID = entry.ID
	return nil, out, nil
}

// confirmViaLogs fetches rb's declared log source — the signal's own pod by
// default (LogSource empty or "self"), or a fixed shared component like a
// KEDA operator (LogSource "namespace/deployment") — and checks it against
// rb.LogSignatures. This is the mechanism behind runbooks' log_signatures/
// log_source fields: an SRE teaches an entry to confirm itself against real
// log text just by editing the markdown, no Go code change needed (see
// internal/runbooks.Entry).
//
// A confirmed match returns the runbook's text flagged as log-confirmed,
// not just "this kind/keyword looked similar." A source that can't be
// reached, or one with no matching signature, silently falls back to the
// plain kind/keyword-matched text — the entry is still probably the right
// one, it just couldn't be confirmed by log this time (e.g. the pod's log
// already rotated past the failure).
func (s *Server) confirmViaLogs(ctx context.Context, in TroubleshootIn, rb runbooks.Entry) (summary string, proposedCommands []string) {
	ns, name, dep := in.Namespace, in.Name, ""
	if rb.LogSource != "" && rb.LogSource != "self" {
		if before, after, found := strings.Cut(rb.LogSource, "/"); found {
			ns, dep, name = before, after, ""
		}
	}

	resp, err := s.Hub.GetLogs(ctx, in.ClusterID, ns, name, dep, 0)
	if err != nil || resp.Error != "" || !rb.MatchesLogs(resp.Logs) {
		return rb.Title, []string{rb.Body}
	}
	return rb.Title + " — confirmado pelo log (assinatura conhecida encontrada)", []string{rb.Body, "Evidência no log:\n" + resp.Logs}
}

type ApproveActionIn struct {
	IncidentID string `json:"incident_id" jsonschema:"ID do incidente original retornado por troubleshoot, cuja ladder tinha uma ação de alto risco pendente"`
	ActionName string `json:"action_name" jsonschema:"nome exato da ação a aprovar, como apareceu em troubleshoot (ex: \"increase memory limit\")"`
}

type ApproveActionOut struct {
	IncidentID         string     `json:"incident_id"`
	OriginalIncidentID string     `json:"original_incident_id"`
	Attempt            AttemptOut `json:"attempt"`
}

// approveActionHandler implements the approve_action tool: it re-validates
// that action_name was really proposed (and really risk=high — a lower-risk
// action never needed this path in the first place) on a real past
// incident, gates on the caller's tier, then runs exactly that one action
// via internal/transport's ApproveAction, which ends up calling
// execengine.RunApproved on the agent side — the only place a RiskHigh
// action is ever actually allowed to execute.
func (s *Server) approveActionHandler() mcp.ToolHandlerFor[ApproveActionIn, ApproveActionOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ApproveActionIn) (*mcp.CallToolResult, ApproveActionOut, error) {
		if !s.ExecutionFlag {
			return nil, ApproveActionOut{}, fmt.Errorf("execução está desligada globalmente neste hub (--execution-enabled=false) — aprovação explícita não é uma exceção a isso")
		}

		entry, ok, err := s.Audit.Get(ctx, in.IncidentID)
		if err != nil {
			return nil, ApproveActionOut{}, fmt.Errorf("buscando incidente %s: %w", in.IncidentID, err)
		}
		if !ok {
			return nil, ApproveActionOut{}, fmt.Errorf("incidente %q não encontrado", in.IncidentID)
		}

		var target *audit.ProposedActionRecord
		for i := range entry.ProposedActions {
			if entry.ProposedActions[i].Name == in.ActionName {
				target = &entry.ProposedActions[i]
				break
			}
		}
		if target == nil {
			return nil, ApproveActionOut{}, fmt.Errorf("ação %q não foi proposta no incidente %s", in.ActionName, in.IncidentID)
		}
		if target.Risk != string(policy.RiskHigh) {
			return nil, ApproveActionOut{}, fmt.Errorf(
				"ação %q é risco %q, não \"high\" — não precisa de aprovação explícita, chame troubleshoot de novo",
				in.ActionName, target.Risk)
		}

		tier, ok := s.Policy.CallerTier(s.CallerGroups)
		if !ok || tier != policy.TierProdAdmin {
			return nil, ApproveActionOut{}, fmt.Errorf("aprovar uma ação de alto risco requer tier %q", policy.TierProdAdmin)
		}

		sig := &pb.Signal{Kind: entry.Signal.Kind, Namespace: entry.Signal.Namespace, Name: entry.Signal.Name, NodeName: entry.Signal.NodeName}
		resp, err := s.Hub.ApproveAction(ctx, entry.ClusterID, entry.PlaybookID, sig, in.ActionName, entry.Meta)
		if err != nil {
			return nil, ApproveActionOut{}, fmt.Errorf("executando aprovação no cluster %s: %w", entry.ClusterID, err)
		}
		if resp.Error != "" {
			return nil, ApproveActionOut{}, fmt.Errorf("agente do cluster %s: %s", entry.ClusterID, resp.Error)
		}
		a := resp.Attempt

		newEntry := audit.Entry{
			ID: audit.NewID(), Time: time.Now().UTC(), ClusterID: entry.ClusterID,
			Groups: s.CallerGroups, PlaybookID: entry.PlaybookID,
			Summary:  fmt.Sprintf("aprovação explícita de %q para o incidente %s", in.ActionName, in.IncidentID),
			Decision: policy.Decision{Allow: true, DryRun: false, Reason: "ação de alto risco aprovada explicitamente por chamador prod-admin", Tier: tier},
			Signal:   entry.Signal,
			Result: &execengine.Result{
				PlaybookID: entry.PlaybookID,
				Resolved:   a.GetValidated(),
				Attempts: []execengine.AttemptResult{{
					Action: a.GetAction(), Description: a.GetDescription(), Risk: policy.RiskLevel(a.GetRisk()),
					Output: a.GetOutput(), Err: a.GetError(), Validated: a.GetValidated(),
					RolledBack: a.GetRolledBack(), Skipped: a.GetSkipped(),
				}},
			},
		}
		if err := s.Audit.Append(ctx, newEntry); err != nil {
			return nil, ApproveActionOut{}, fmt.Errorf("gravando auditoria: %w", err)
		}

		return nil, ApproveActionOut{
			IncidentID: newEntry.ID, OriginalIncidentID: in.IncidentID,
			Attempt: AttemptOut{
				Action: a.GetAction(), Description: a.GetDescription(), Risk: a.GetRisk(),
				Output: a.GetOutput(), Error: a.GetError(), Validated: a.GetValidated(),
				RolledBack: a.GetRolledBack(), Skipped: a.GetSkipped(),
			},
		}, nil
	}
}

type GetPostmortemIn struct {
	IncidentID string `json:"incident_id" jsonschema:"ID do incidente, retornado por troubleshoot"`
}

type GetPostmortemOut struct {
	Text string `json:"text"`
}

func (s *Server) postmortemHandler() mcp.ToolHandlerFor[GetPostmortemIn, GetPostmortemOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetPostmortemIn) (*mcp.CallToolResult, GetPostmortemOut, error) {
		entry, ok, err := s.Audit.Get(ctx, in.IncidentID)
		if err != nil {
			return nil, GetPostmortemOut{}, fmt.Errorf("buscando incidente %s: %w", in.IncidentID, err)
		}
		if !ok {
			return nil, GetPostmortemOut{}, fmt.Errorf("incidente %q não encontrado", in.IncidentID)
		}
		text, err := postmortem.Render(entry)
		if err != nil {
			return nil, GetPostmortemOut{}, err
		}
		return nil, GetPostmortemOut{Text: text}, nil
	}
}
