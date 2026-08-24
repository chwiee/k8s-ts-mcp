package postmortem

import (
	"strings"
	"testing"
	"time"

	"github.com/chwiee/k8s-ts-mcp/internal/audit"
	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/policy"
)

func TestRender_DryRun(t *testing.T) {
	e := audit.Entry{
		ID: "abc", ClusterID: "cluster-a", Time: time.Now(), Groups: []string{"time-x-readonly"},
		PlaybookID: "core/crashloopbackoff", Summary: "pod x em CrashLoopBackOff",
		Decision:         policy.Decision{Allow: true, DryRun: true, Reason: "tier readonly: apenas diagnóstico"},
		ProposedCommands: []string{"kubectl delete pod x -n default"},
	}
	out, err := Render(e)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"dry-run", "kubectl delete pod x -n default", "pod x em CrashLoopBackOff"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRender_Resolved(t *testing.T) {
	e := audit.Entry{
		ID: "abc", ClusterID: "cluster-a", Time: time.Now(), Groups: []string{"infra-eng"},
		PlaybookID: "core/crashloopbackoff", Summary: "pod x em CrashLoopBackOff",
		Decision: policy.Decision{Allow: true, DryRun: false, Reason: "ação segura"},
		Result: &execengine.Result{
			PlaybookID: "core/crashloopbackoff",
			Resolved:   true,
			Attempts: []execengine.AttemptResult{
				{Action: "restart pod", Risk: policy.RiskSafe, Output: "pod deleted, recreated", Validated: true},
			},
		},
	}
	out, err := Render(e)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"Resolvido", "restart pod", "pod deleted, recreated"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRender_Unresolved(t *testing.T) {
	e := audit.Entry{
		ID: "abc", ClusterID: "cluster-a", Time: time.Now(), Groups: []string{"infra-prod-admins"},
		PlaybookID: "core/crashloopbackoff", Summary: "pod x em CrashLoopBackOff",
		Decision: policy.Decision{Allow: true, DryRun: false, Reason: "ação de risco médio em prod"},
		Result: &execengine.Result{
			PlaybookID: "core/crashloopbackoff",
			Resolved:   false,
			Attempts: []execengine.AttemptResult{
				{Action: "restart pod", Validated: false, Err: "still crashing"},
				{Action: "scale deployment down and up", Validated: false, Err: "still crashing", RolledBack: true},
			},
		},
	}
	out, err := Render(e)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"Intervenção manual necessária", "still crashing", "Revertido: sim"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
