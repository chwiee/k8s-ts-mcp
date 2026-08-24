package policy

import (
	"context"
	"testing"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	groups := GroupMapping{
		"infra-prod-admins": TierProdAdmin,
		"infra-eng":         TierNonProdAdmin,
		"time-x-readonly":   TierReadOnly,
	}
	e, err := NewEngine(context.Background(), groups)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func TestDecide(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()

	cases := []struct {
		name       string
		req        Request
		wantAllow  bool
		wantDryRun bool
	}{
		{
			name:       "unknown group is always denied",
			req:        Request{Groups: []string{"some-other-group"}, ClusterEnv: EnvNonProd, ActionRisk: RiskSafe, ExecutionFlag: true},
			wantAllow:  false,
			wantDryRun: true,
		},
		{
			name:       "high risk always forces dry-run even for prod-admin with flag on",
			req:        Request{Groups: []string{"infra-prod-admins"}, ClusterEnv: EnvProd, ActionRisk: RiskHigh, ExecutionFlag: true},
			wantAllow:  true,
			wantDryRun: true,
		},
		{
			name:       "readonly tier never executes, even safe risk with flag on",
			req:        Request{Groups: []string{"time-x-readonly"}, ClusterEnv: EnvNonProd, ActionRisk: RiskSafe, ExecutionFlag: true},
			wantAllow:  true,
			wantDryRun: true,
		},
		{
			name:       "safe risk executes for nonprod-admin when flag is on",
			req:        Request{Groups: []string{"infra-eng"}, ClusterEnv: EnvNonProd, ActionRisk: RiskSafe, ExecutionFlag: true},
			wantAllow:  true,
			wantDryRun: false,
		},
		{
			name:       "safe risk stays dry-run when flag is off",
			req:        Request{Groups: []string{"infra-eng"}, ClusterEnv: EnvNonProd, ActionRisk: RiskSafe, ExecutionFlag: false},
			wantAllow:  true,
			wantDryRun: true,
		},
		{
			name:       "medium risk in prod denied for nonprod-admin",
			req:        Request{Groups: []string{"infra-eng"}, ClusterEnv: EnvProd, ActionRisk: RiskMedium, ExecutionFlag: true},
			wantAllow:  false,
			wantDryRun: true,
		},
		{
			name:       "medium risk in prod executes for prod-admin when flag is on",
			req:        Request{Groups: []string{"infra-prod-admins"}, ClusterEnv: EnvProd, ActionRisk: RiskMedium, ExecutionFlag: true},
			wantAllow:  true,
			wantDryRun: false,
		},
		{
			name:       "highest tier wins when caller is in multiple groups",
			req:        Request{Groups: []string{"time-x-readonly", "infra-prod-admins"}, ClusterEnv: EnvProd, ActionRisk: RiskMedium, ExecutionFlag: true},
			wantAllow:  true,
			wantDryRun: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.Decide(ctx, tc.req)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if got.Allow != tc.wantAllow {
				t.Errorf("Allow = %v, want %v (reason: %s)", got.Allow, tc.wantAllow, got.Reason)
			}
			if got.DryRun != tc.wantDryRun {
				t.Errorf("DryRun = %v, want %v (reason: %s)", got.DryRun, tc.wantDryRun, got.Reason)
			}
		})
	}
}
