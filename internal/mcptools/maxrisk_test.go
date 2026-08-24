package mcptools

import (
	"testing"

	"github.com/chwiee/k8s-ts-mcp/internal/policy"
	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

func TestMaxRisk(t *testing.T) {
	cases := []struct {
		name    string
		actions []*pb.ProposedAction
		want    policy.RiskLevel
	}{
		{"no actions", nil, policy.RiskHigh},
		{"only safe", []*pb.ProposedAction{{Risk: "safe"}}, policy.RiskSafe},
		{"safe then medium", []*pb.ProposedAction{{Risk: "safe"}, {Risk: "medium"}}, policy.RiskMedium},
		{
			name:    "safe then high — high must not win, or the safe step never gets to run for real",
			actions: []*pb.ProposedAction{{Risk: "safe"}, {Risk: "high"}},
			want:    policy.RiskSafe,
		},
		{
			name:    "medium then high — same reasoning",
			actions: []*pb.ProposedAction{{Risk: "medium"}, {Risk: "high"}},
			want:    policy.RiskMedium,
		},
		{
			name:    "only high — nothing safe/medium exists, stays diagnostic-only",
			actions: []*pb.ProposedAction{{Risk: "high"}},
			want:    policy.RiskHigh,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maxRisk(tc.actions); got != tc.want {
				t.Errorf("maxRisk(%+v) = %q, want %q", tc.actions, got, tc.want)
			}
		})
	}
}
