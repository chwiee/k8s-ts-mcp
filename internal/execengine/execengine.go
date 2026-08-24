package execengine

import (
	"context"
	"fmt"

	"github.com/chwiee/k8s-ts-mcp/internal/policy"
	"github.com/chwiee/k8s-ts-mcp/internal/redact"
)

// MaxActions is the hard cap on a playbook's escalation ladder: up to three
// distinct actions, in increasing order of intervention. This is not a
// "same command 3x" retry — each action is expected to be a different,
// more invasive remediation than the last.
const MaxActions = 3

// State is opaque, playbook-defined pre-action state captured by Snapshot
// and handed back to Rollback unchanged. It must only ever contain
// non-sensitive fields (replica counts, image tags, resource limits) — never
// a full manifest, which could carry Secret-derived values.
type State map[string]any

// Action is one rung of a playbook's escalation ladder.
type Action struct {
	Name string
	// Description is a human-readable statement of what Run actually does
	// (e.g. "kubectl delete pod nginx-abc123 -n default"), shown to the
	// caller when the request is (or must be) dry-run — Run itself is never
	// called in that case, so this is the only thing describing it.
	Description string
	Risk        policy.RiskLevel

	// Snapshot captures the state needed to undo Run, if Run's effect turns
	// out not to fix the problem. Optional: nil means Rollback is never
	// called for this action.
	Snapshot func(ctx context.Context) (State, error)

	// Run performs the remediation and returns raw command output. Run
	// itself is never called when the caller only asked for a dry-run —
	// the caller decides that before invoking Run, execengine only orders
	// and records the ladder.
	Run func(ctx context.Context) (output string, err error)

	// Validate reports whether the incident is resolved after Run. A nil
	// Validate means Run's own success/error is trusted as the verdict.
	Validate func(ctx context.Context) (healthy bool, err error)

	// Rollback undoes Run using the snapshot taken before it, when Run
	// either failed outright or Validate reports the incident persists.
	Rollback func(ctx context.Context, snap State) error
}

// Ladder is one playbook's ordered set of actions for a single incident.
type Ladder struct {
	PlaybookID string
	Actions    []Action
}

// AttemptResult is the redacted, audit-ready record of one action attempt —
// or, when Skipped is true, of one action execengine refused to attempt at
// all.
type AttemptResult struct {
	Action      string           `json:"action"`
	Description string           `json:"description,omitempty"`
	Risk        policy.RiskLevel `json:"risk"`
	Output      string           `json:"output,omitempty"`
	Err         string           `json:"error,omitempty"`
	Snapshot    map[string]any   `json:"snapshot,omitempty"`
	Validated   bool             `json:"validated"`
	RolledBack  bool             `json:"rolled_back"`
	// Skipped is true when this action was never run because its Risk is
	// policy.RiskHigh — execengine refuses to auto-run those unconditionally
	// (see Run), regardless of what policy already allowed at the hub. This
	// is a hard invariant, not a caller-configurable option: a high-risk
	// step always needs a human running its Description by hand.
	Skipped bool `json:"skipped,omitempty"`
}

// Result is the outcome of running (up to) a full escalation ladder.
type Result struct {
	PlaybookID string          `json:"playbook_id"`
	Resolved   bool            `json:"resolved"`
	Attempts   []AttemptResult `json:"attempts"`
}

// Run executes ladder.Actions in order, stopping at the first one that
// Validate reports as healthy. Every action that doesn't resolve the
// incident is rolled back (best-effort) before the next one runs. All
// output, errors and snapshots recorded in the result are already redacted
// — this is the last point before the result crosses the agent boundary.
//
// A policy.RiskHigh action is never run — its Description is recorded as a
// skipped attempt needing manual approval, and the ladder stops there
// (actions are meant to be in increasing risk order, so nothing after a
// high-risk step should be lower-risk). This holds even if the caller was
// authorized to execute: the risk tag itself, not just who's asking, is
// what gates auto-execution for that one step.
func Run(ctx context.Context, ladder Ladder) (Result, error) {
	if len(ladder.Actions) == 0 {
		return Result{}, fmt.Errorf("execengine: ladder %q has no actions", ladder.PlaybookID)
	}
	if len(ladder.Actions) > MaxActions {
		return Result{}, fmt.Errorf("execengine: ladder %q has %d actions, max is %d", ladder.PlaybookID, len(ladder.Actions), MaxActions)
	}

	result := Result{PlaybookID: ladder.PlaybookID}

	for _, action := range ladder.Actions {
		if action.Risk == policy.RiskHigh {
			result.Attempts = append(result.Attempts, AttemptResult{
				Action:      action.Name,
				Description: action.Description,
				Risk:        action.Risk,
				Skipped:     true,
				Err:         "ação de alto risco — não executada automaticamente, requer aprovação manual",
			})
			return result, nil
		}

		attempt := runOne(ctx, action)
		result.Attempts = append(result.Attempts, attempt)
		if attempt.Validated {
			result.Resolved = true
			return result, nil
		}
	}

	return result, nil
}

// RunApproved runs exactly one action, unconditionally — including a
// policy.RiskHigh action, which Run refuses to auto-run. This is the only
// path by which a high-risk action ever actually executes: the hub only
// calls it after a human has explicitly approved that one named action on a
// real, already-diagnosed incident (see internal/mcptools's approve_action
// tool and internal/transport's ApproveAction). Unlike Run, it doesn't walk
// a ladder — there's exactly one action, so there's nothing to escalate to
// and nothing after it to skip.
func RunApproved(ctx context.Context, action Action) (AttemptResult, error) {
	return runOne(ctx, action), nil
}

// runOne runs a single action to completion — snapshot, run, validate,
// rollback-on-failure — and returns its redacted, audit-ready record. It
// never itself decides whether the action's risk allows running; that's the
// caller's job (Run refuses RiskHigh before calling this, RunApproved
// always calls it).
func runOne(ctx context.Context, action Action) AttemptResult {
	attempt := AttemptResult{Action: action.Name, Description: action.Description, Risk: action.Risk}

	var snap State
	if action.Snapshot != nil {
		s, err := action.Snapshot(ctx)
		if err != nil {
			attempt.Err = redact.Text(fmt.Sprintf("snapshot failed: %v", err))
			return attempt
		}
		snap = s
		attempt.Snapshot = redact.Object(map[string]any(s)).(map[string]any)
	}

	out, runErr := action.Run(ctx)
	attempt.Output = redact.Text(out)
	if runErr != nil {
		attempt.Err = redact.Text(runErr.Error())
		rollback(ctx, action, snap, &attempt)
		return attempt
	}

	healthy := true
	if action.Validate != nil {
		h, valErr := action.Validate(ctx)
		healthy = h
		if valErr != nil {
			attempt.Err = redact.Text(valErr.Error())
		}
	}
	attempt.Validated = healthy

	if !healthy {
		rollback(ctx, action, snap, &attempt)
	}
	return attempt
}

func rollback(ctx context.Context, action Action, snap State, attempt *AttemptResult) {
	if action.Rollback == nil || snap == nil {
		return
	}
	if err := action.Rollback(ctx, snap); err != nil {
		attempt.Err = redact.Text(fmt.Sprintf("%s; rollback failed: %v", attempt.Err, err))
		return
	}
	attempt.RolledBack = true
}
