package execengine

import (
	"context"
	"errors"
	"testing"

	"github.com/chwiee/k8s-ts-mcp/internal/policy"
)

func snapshotOK(v int) func(context.Context) (State, error) {
	return func(context.Context) (State, error) { return State{"replicas": v}, nil }
}

func TestRun_NeverAutoRunsHighRiskAction(t *testing.T) {
	var ranHighRisk bool
	ladder := Ladder{
		PlaybookID: "oomkilled",
		Actions: []Action{
			{
				Name:     "restart pod",
				Risk:     policy.RiskSafe,
				Run:      func(context.Context) (string, error) { return "restarted", nil },
				Validate: func(context.Context) (bool, error) { return false, nil }, // doesn't fix it
			},
			{
				Name:        "increase memory limit",
				Description: "kubectl set resources deployment/x --limits=memory=1536Mi",
				Risk:        policy.RiskHigh,
				Run:         func(context.Context) (string, error) { ranHighRisk = true; return "bumped", nil },
			},
		},
	}

	got, err := Run(context.Background(), ladder)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Resolved {
		t.Fatalf("Resolved = true, want false — the only two actions were a failed safe one and a skipped high-risk one")
	}
	if ranHighRisk {
		t.Fatal("execengine ran a policy.RiskHigh action's Run — it must never do that automatically")
	}
	if len(got.Attempts) != 2 {
		t.Fatalf("len(Attempts) = %d, want 2 (the safe attempt plus the skip record)", len(got.Attempts))
	}
	skip := got.Attempts[1]
	if !skip.Skipped {
		t.Errorf("Attempts[1].Skipped = false, want true")
	}
	if skip.Description == "" {
		t.Errorf("skipped attempt lost its Description — that's the only way the caller learns what to run by hand")
	}
}

func TestRun_HighRiskFirstActionSkipsImmediately(t *testing.T) {
	ladder := Ladder{
		PlaybookID: "execformaterror",
		Actions: []Action{
			{Name: "rebuild image for the right architecture", Risk: policy.RiskHigh, Run: func(context.Context) (string, error) {
				t.Fatal("Run must never be called for a RiskHigh action")
				return "", nil
			}},
		},
	}
	got, err := Run(context.Background(), ladder)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Resolved {
		t.Fatal("Resolved = true, want false")
	}
	if len(got.Attempts) != 1 || !got.Attempts[0].Skipped {
		t.Fatalf("Attempts = %+v, want exactly one skipped attempt", got.Attempts)
	}
}

func TestRunApproved_RunsHighRiskActionUnlikeRun(t *testing.T) {
	var ran bool
	action := Action{
		Name:        "increase memory limit",
		Description: "kubectl set resources deployment/x --limits=memory=1536Mi",
		Risk:        policy.RiskHigh,
		Run:         func(context.Context) (string, error) { ran = true; return "bumped", nil },
		Validate:    func(context.Context) (bool, error) { return true, nil },
	}

	got, err := RunApproved(context.Background(), action)
	if err != nil {
		t.Fatalf("RunApproved: %v", err)
	}
	if !ran {
		t.Fatal("RunApproved did not call the high-risk action's Run — that's the whole point of explicit approval")
	}
	if got.Skipped {
		t.Errorf("Skipped = true, want false — RunApproved must never skip the one action it was asked to run")
	}
	if !got.Validated {
		t.Errorf("Validated = false, want true")
	}
}

func TestRunApproved_RollsBackOnFailedValidation(t *testing.T) {
	var rolledBack bool
	action := Action{
		Name:     "increase memory limit",
		Risk:     policy.RiskHigh,
		Snapshot: snapshotOK(1),
		Run:      func(context.Context) (string, error) { return "bumped", nil },
		Validate: func(context.Context) (bool, error) { return false, nil },
		Rollback: func(context.Context, State) error { rolledBack = true; return nil },
	}

	got, err := RunApproved(context.Background(), action)
	if err != nil {
		t.Fatalf("RunApproved: %v", err)
	}
	if got.Validated {
		t.Errorf("Validated = true, want false")
	}
	if !rolledBack {
		t.Error("RunApproved did not roll back an action whose Validate reported the incident persists")
	}
}

func TestRun_ResolvesOnFirstAction(t *testing.T) {
	var rolledBack bool
	ladder := Ladder{
		PlaybookID: "crashloop-restart",
		Actions: []Action{
			{
				Name:     "restart pod",
				Risk:     policy.RiskSafe,
				Snapshot: snapshotOK(2),
				Run:      func(context.Context) (string, error) { return "pod deleted, recreated", nil },
				Validate: func(context.Context) (bool, error) { return true, nil },
				Rollback: func(context.Context, State) error { rolledBack = true; return nil },
			},
		},
	}

	got, err := Run(context.Background(), ladder)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !got.Resolved {
		t.Fatalf("Resolved = false, want true")
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("len(Attempts) = %d, want 1", len(got.Attempts))
	}
	if rolledBack {
		t.Errorf("rollback was called for a successful action")
	}
}

func TestRun_EscalatesAndRollsBackFailedAttempts(t *testing.T) {
	var rollbacks []string
	mkAction := func(name string, healthy bool) Action {
		return Action{
			Name:     name,
			Risk:     policy.RiskMedium,
			Snapshot: snapshotOK(1),
			Run:      func(context.Context) (string, error) { return name + " ran", nil },
			Validate: func(context.Context) (bool, error) { return healthy, nil },
			Rollback: func(context.Context, State) error { rollbacks = append(rollbacks, name); return nil },
		}
	}
	ladder := Ladder{
		PlaybookID: "scale-fix",
		Actions: []Action{
			mkAction("restart pod", false),
			mkAction("scale down/up", false),
			mkAction("recreate deployment", true),
		},
	}

	got, err := Run(context.Background(), ladder)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !got.Resolved {
		t.Fatalf("Resolved = false, want true")
	}
	if len(got.Attempts) != 3 {
		t.Fatalf("len(Attempts) = %d, want 3", len(got.Attempts))
	}
	if want := []string{"restart pod", "scale down/up"}; len(rollbacks) != len(want) || rollbacks[0] != want[0] || rollbacks[1] != want[1] {
		t.Errorf("rollbacks = %v, want %v", rollbacks, want)
	}
	if got.Attempts[2].RolledBack {
		t.Errorf("the successful (3rd) attempt should not be rolled back")
	}
}

func TestRun_AllThreeFail(t *testing.T) {
	ladder := Ladder{
		PlaybookID: "unfixable",
		Actions: []Action{
			{Name: "a1", Run: func(context.Context) (string, error) { return "", errors.New("boom") }},
			{Name: "a2", Run: func(context.Context) (string, error) { return "", errors.New("boom") }},
			{Name: "a3", Run: func(context.Context) (string, error) { return "", errors.New("boom") }},
		},
	}

	got, err := Run(context.Background(), ladder)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Resolved {
		t.Fatalf("Resolved = true, want false")
	}
	if len(got.Attempts) != 3 {
		t.Fatalf("len(Attempts) = %d, want 3", len(got.Attempts))
	}
	for _, a := range got.Attempts {
		if a.Err == "" {
			t.Errorf("attempt %q has no recorded error", a.Action)
		}
	}
}

func TestRun_RejectsTooManyActions(t *testing.T) {
	actions := make([]Action, MaxActions+1)
	for i := range actions {
		actions[i] = Action{Name: "a", Run: func(context.Context) (string, error) { return "", nil }}
	}
	_, err := Run(context.Background(), Ladder{PlaybookID: "too-long", Actions: actions})
	if err == nil {
		t.Fatal("Run did not reject a ladder with more than MaxActions actions")
	}
}

func TestRun_RedactsSecretsInOutputAndSnapshot(t *testing.T) {
	ladder := Ladder{
		PlaybookID: "leaky",
		Actions: []Action{
			{
				Name:     "leaky action",
				Snapshot: func(context.Context) (State, error) { return State{"token": "Bearer abc123.def456-ghi789"}, nil },
				Run:      func(context.Context) (string, error) { return "used AKIAABCDEFGHIJKLMNOP to authenticate", nil },
				Validate: func(context.Context) (bool, error) { return true, nil },
			},
		},
	}
	got, err := Run(context.Background(), ladder)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	attempt := got.Attempts[0]
	if _, ok := attempt.Snapshot["token"]; ok {
		t.Errorf("snapshot still has sensitive key %q: %v", "token", attempt.Snapshot)
	}
	if got.Attempts[0].Output == "" {
		t.Fatal("expected non-empty redacted output")
	}
}
