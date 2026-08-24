// Package agentcore implements transport.Handler by wiring the wire
// protocol (api/proto/k8sts/v1) to internal/playbooks, internal/execengine
// and internal/k8sclient — this is the glue cmd/cluster-agent's main.go
// wires together, kept separate so it's testable without a real gRPC
// connection.
package agentcore

import (
	"context"
	"fmt"
	"strings"

	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/redact"
	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

// Handler implements transport.Handler for one cluster-agent process.
type Handler struct {
	Client   *k8sclient.Client
	Registry *playbooks.Registry
}

func fromProtoSignal(sig *pb.Signal) playbooks.Signal {
	return playbooks.Signal{
		Kind:      sig.GetKind(),
		Namespace: sig.GetNamespace(),
		Name:      sig.GetName(),
		NodeName:  sig.GetNodeName(),
	}
}

// diagnose finds the matching playbook and produces both its Finding and
// the Ladder it would run — Ladder only builds the action closures, it
// never calls Run, so this is safe to call for a dry-run/proposal only.
func (h *Handler) diagnose(ctx context.Context, sig playbooks.Signal) (playbooks.Playbook, playbooks.Finding, execengine.Ladder, error) {
	pbk, ok := h.Registry.Find(sig)
	if !ok {
		return nil, playbooks.Finding{}, execengine.Ladder{}, playbooks.ErrNoPlaybook(sig)
	}
	finding, err := pbk.Diagnose(ctx, h.Client, sig)
	if err != nil {
		return nil, playbooks.Finding{}, execengine.Ladder{}, fmt.Errorf("playbook %s: %w", pbk.ID(), err)
	}
	return pbk, finding, pbk.Ladder(h.Client, finding), nil
}

// Diagnose implements transport.Handler.
func (h *Handler) Diagnose(ctx context.Context, sig *pb.Signal) (playbookID, summary string, proposedActions []*pb.ProposedAction, meta map[string]string, noPlaybook bool, err error) {
	domainSig := fromProtoSignal(sig)
	if _, ok := h.Registry.Find(domainSig); !ok {
		return "", "", nil, nil, true, nil
	}

	pbk, finding, ladder, err := h.diagnose(ctx, domainSig)
	if err != nil {
		return "", "", nil, nil, false, err
	}
	actions := make([]*pb.ProposedAction, 0, len(ladder.Actions))
	for _, a := range ladder.Actions {
		actions = append(actions, &pb.ProposedAction{
			Name:        a.Name,
			Description: redact.Text(a.Description),
			Risk:        string(a.Risk),
		})
	}
	return pbk.ID(), redact.Text(finding.Summary), actions, redactMeta(finding.Meta), false, nil
}

// redactMeta applies redact.Text to every value of a Finding.Meta map
// before it crosses the agent boundary — same discipline as every other
// piece of playbook-produced text, even though Meta today only ever holds
// non-sensitive identifiers (a Deployment name and the like).
func redactMeta(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = redact.Text(v)
	}
	return out
}

// Execute implements transport.Handler. It re-derives the finding and
// ladder itself rather than trusting anything cached from a prior Diagnose
// call — the hub only ever calls Execute after its own policy check, but
// the ladder must still reflect live cluster state at execution time, not
// whatever it looked like when the caller last asked.
func (h *Handler) Execute(ctx context.Context, playbookID string, sig *pb.Signal) (resolved bool, attempts []*pb.Attempt, err error) {
	pbk, _, ladder, err := h.diagnose(ctx, fromProtoSignal(sig))
	if err != nil {
		return false, nil, err
	}
	if pbk.ID() != playbookID {
		return false, nil, fmt.Errorf("playbook mismatch: signal now matches %q, execute was requested for %q (cluster state likely changed)", pbk.ID(), playbookID)
	}

	result, err := execengine.Run(ctx, ladder)
	if err != nil {
		return false, nil, err
	}
	out := make([]*pb.Attempt, 0, len(result.Attempts))
	for _, a := range result.Attempts {
		out = append(out, &pb.Attempt{
			Action:      a.Action,
			Description: a.Description,
			Risk:        string(a.Risk),
			Output:      a.Output,
			Error:       a.Err,
			Validated:   a.Validated,
			RolledBack:  a.RolledBack,
			Skipped:     a.Skipped,
		})
	}
	return result.Resolved, out, nil
}

// ApproveAction implements transport.Handler. It runs exactly one named
// action from the ladder currently produced for sig — including a
// policy.RiskHigh action, which Execute (via execengine.Run) refuses to
// auto-run.
//
// For a playbooks.Recheckable playbook (e.g. core/oomkilled), re-validation
// goes through Recheck instead of a full re-Diagnose: by the time a human
// approves a high-risk step, an earlier ladder action (e.g. "restart pod")
// is expected to have already deleted/recreated the pod named in sig, so
// re-Diagnosing that exact pod would just fail with "not found". Recheck
// uses meta (captured by the original Diagnose call) to check the CURRENT
// state of the owning resource instead. Every other playbook falls back to
// the normal re-Diagnose-by-pod path, which is correct as long as nothing
// earlier in its ladder already changed that pod's identity.
//
// Either way, an actionName not on the (re-derived) ladder is rejected — a
// stale or tampered request can't run something the current playbook state
// wouldn't actually propose.
func (h *Handler) ApproveAction(ctx context.Context, playbookID string, sig *pb.Signal, actionName string, meta map[string]string) (*pb.Attempt, error) {
	domainSig := fromProtoSignal(sig)
	pbk, ok := h.Registry.Find(domainSig)
	if !ok {
		return nil, playbooks.ErrNoPlaybook(domainSig)
	}
	if pbk.ID() != playbookID {
		return nil, fmt.Errorf("playbook mismatch: signal now matches %q, approve_action was requested for %q (cluster state likely changed)", pbk.ID(), playbookID)
	}

	var ladder execengine.Ladder
	if rc, ok := pbk.(playbooks.Recheckable); ok {
		_, l, stillNeeded, err := rc.Recheck(ctx, h.Client, domainSig, meta)
		if err != nil {
			return nil, fmt.Errorf("playbook %s: rechecando antes de aprovar: %w", pbk.ID(), err)
		}
		if !stillNeeded {
			return nil, fmt.Errorf("playbook %s: o problema já não está mais presente — aprovação cancelada, nada a rodar", pbk.ID())
		}
		ladder = l
	} else {
		_, _, l, err := h.diagnose(ctx, domainSig)
		if err != nil {
			return nil, err
		}
		ladder = l
	}

	var action *execengine.Action
	for i := range ladder.Actions {
		if ladder.Actions[i].Name == actionName {
			action = &ladder.Actions[i]
			break
		}
	}
	if action == nil {
		return nil, fmt.Errorf("action %q não está na ladder atual do playbook %q", actionName, playbookID)
	}

	result, err := execengine.RunApproved(ctx, *action)
	if err != nil {
		return nil, err
	}
	return &pb.Attempt{
		Action:      result.Action,
		Description: result.Description,
		Risk:        string(result.Risk),
		Output:      result.Output,
		Error:       result.Err,
		Validated:   result.Validated,
		RolledBack:  result.RolledBack,
		Skipped:     result.Skipped,
	}, nil
}

// GetLogs implements transport.Handler. When deployment is set it resolves
// to that Deployment's current pod first — the caller (a runbook entry's
// log_source, see internal/runbooks and internal/mcptools's
// runbookFallback) names a fixed, shared component like the KEDA operator,
// never a specific generated pod name. Tries the current container
// instance's log first, falling back to the previous instance's if that
// comes back empty (the common shape right after a crash — see
// internal/agentcore/scan.go's refineExecFormatError for the same pattern).
func (h *Handler) GetLogs(ctx context.Context, namespace, name, deployment string, tailLines int64) (string, error) {
	if tailLines <= 0 {
		tailLines = 50
	}
	podName := name
	if deployment != "" {
		resolved, err := h.Client.PodForDeployment(ctx, namespace, deployment)
		if err != nil {
			return "", fmt.Errorf("resolvendo pod do deployment %s/%s: %w", namespace, deployment, err)
		}
		podName = resolved
	}

	logs, err := h.Client.ContainerLogs(ctx, namespace, podName, false, tailLines)
	if strings.TrimSpace(logs) == "" {
		if prev, prevErr := h.Client.ContainerLogs(ctx, namespace, podName, true, tailLines); prevErr == nil && strings.TrimSpace(prev) != "" {
			logs, err = prev, nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("lendo log de %s/%s: %w", namespace, podName, err)
	}
	return redact.Text(logs), nil
}
