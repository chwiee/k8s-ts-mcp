package policy

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/open-policy-agent/opa/v1/rego"
)

//go:embed policy.rego
var policySrc string

// Engine evaluates access requests against policy.rego, after resolving the
// caller's AD/OIDC groups to a tier via GroupMapping. It never talks to a
// remote OPA server — the policy is embedded and evaluated in-process, so a
// decision never depends on hub-external availability.
type Engine struct {
	query  rego.PreparedEvalQuery
	groups GroupMapping
}

// NewEngine prepares the embedded policy for evaluation. groups is the
// ops-owned AD-group-to-tier mapping (see LoadGroupMapping).
func NewEngine(ctx context.Context, groups GroupMapping) (*Engine, error) {
	r := rego.New(
		rego.Query("data.k8sts.policy.result"),
		rego.Module("policy.rego", policySrc),
	)
	pq, err := r.PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("preparing policy for eval: %w", err)
	}
	return &Engine{query: pq, groups: groups}, nil
}

// CallerTier resolves groups to the caller's highest known tier, the same
// lookup Decide does internally — exported for callers (e.g. mcptools's
// approve_action tool) that need to gate on tier directly, without going
// through a full Decide, since an explicit-approval request isn't the kind
// of thing policy.rego's dry-run/execute logic reasons about.
func (e *Engine) CallerTier(groups []string) (Tier, bool) {
	return e.groups.HighestTier(groups)
}

// Decide answers whether req is allowed and whether it must run as dry-run.
// A caller whose groups don't map to any known tier is always denied.
func (e *Engine) Decide(ctx context.Context, req Request) (Decision, error) {
	tier, ok := e.groups.HighestTier(req.Groups)
	if !ok {
		return Decision{
			Allow:  false,
			DryRun: true,
			Reason: "chamador não pertence a nenhum grupo AD mapeado para um tier de acesso",
		}, nil
	}

	input := map[string]any{
		"tier":           string(tier),
		"cluster_env":    string(req.ClusterEnv),
		"action_risk":    string(req.ActionRisk),
		"execution_flag": req.ExecutionFlag,
	}

	rs, err := e.query.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return Decision{}, fmt.Errorf("evaluating policy: %w", err)
	}
	if len(rs) == 0 || len(rs[0].Expressions) == 0 {
		return Decision{}, fmt.Errorf("policy produced no result for input %+v", input)
	}

	// rs[0].Expressions[0].Value is a map[string]any shaped like Decision's
	// JSON tags; round-trip through JSON rather than hand-writing the
	// type-assertions so a policy.rego change that adds/renames a field
	// fails loudly here instead of silently.
	raw, err := json.Marshal(rs[0].Expressions[0].Value)
	if err != nil {
		return Decision{}, fmt.Errorf("marshaling policy result: %w", err)
	}
	var d Decision
	if err := json.Unmarshal(raw, &d); err != nil {
		return Decision{}, fmt.Errorf("unmarshaling policy result: %w", err)
	}
	d.Tier = tier
	return d, nil
}
