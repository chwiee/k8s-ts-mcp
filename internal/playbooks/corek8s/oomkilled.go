package corek8s

import (
	"context"
	"fmt"

	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/policy"
)

// OOMKilled tries a cheap restart first (covers a transient memory spike),
// then proposes — but never auto-runs — raising the container's memory
// limit. Deciding the right limit (or whether this is actually a leak) is a
// human call, not something to guess a multiplier for and apply silently.
type OOMKilled struct {
	// LimitIncreaseFactor multiplies the current memory limit for the
	// proposed (never auto-run) second action. Defaults to 1.5 (+50%).
	LimitIncreaseFactor float64
}

func (o OOMKilled) factor() float64 {
	if o.LimitIncreaseFactor > 0 {
		return o.LimitIncreaseFactor
	}
	return 1.5
}

func (OOMKilled) ID() string       { return "core/oomkilled" }
func (OOMKilled) Resource() string { return "core" }

func (OOMKilled) Detect(sig playbooks.Signal) bool {
	return sig.Kind == "PodOOMKilled"
}

func (OOMKilled) Diagnose(ctx context.Context, cli *k8sclient.Client, sig playbooks.Signal) (playbooks.Finding, error) {
	info, err := cli.ContainerTermination(ctx, sig.Namespace, sig.Name)
	if err != nil {
		return playbooks.Finding{}, fmt.Errorf("diagnosing %s/%s: %w", sig.Namespace, sig.Name, err)
	}

	summary := fmt.Sprintf("pod %s/%s foi morto por OOM (Reason=%s)", sig.Namespace, sig.Name, info.Reason)
	meta := map[string]string{}
	if dep, err := cli.OwningDeploymentName(ctx, sig.Namespace, sig.Name); err == nil {
		meta["deployment"] = dep
		if limit, err := cli.ContainerMemoryLimit(ctx, sig.Namespace, dep); err == nil {
			summary += fmt.Sprintf(", limite de memória atual: %s", limit.String())
		}
	}
	return playbooks.Finding{Signal: sig, Summary: summary, Meta: meta}, nil
}

func (o OOMKilled) Ladder(cli *k8sclient.Client, finding playbooks.Finding) execengine.Ladder {
	ns, pod := finding.Signal.Namespace, finding.Signal.Name
	dep, hasDep := finding.Meta["deployment"]
	hasDep = hasDep && dep != ""

	actions := []execengine.Action{restartPodAction(cli, ns, pod, dep, hasDep)}

	if hasDep {
		factor := o.factor()
		actions = append(actions, execengine.Action{
			Name:        "increase memory limit",
			Description: fmt.Sprintf("kubectl set resources deployment/%s -n %s --limits=memory=<%.0f%% do atual> && kubectl rollout restart deployment/%s -n %s", dep, ns, factor*100, dep, ns),
			// Always high risk: execengine refuses to auto-run this (see
			// internal/execengine.Run) no matter who's asking or whether
			// execution is otherwise enabled — a human decides the real
			// limit.
			Risk: policy.RiskHigh,
			Run: func(ctx context.Context) (string, error) {
				out, err := cli.IncreaseContainerMemoryLimit(ctx, ns, dep, factor)
				if err != nil {
					return "", err
				}
				if _, err := cli.RolloutRestartDeployment(ctx, ns, dep); err != nil {
					return out, err
				}
				return out, nil
			},
		})
	}

	return execengine.Ladder{PlaybookID: OOMKilled{}.ID(), Actions: actions}
}

// Recheck implements playbooks.Recheckable: by the time a human approves
// "increase memory limit", the pod restartPodAction deleted is gone — so
// instead of re-diagnosing that (vanished) pod, this checks whether the
// Deployment meta identifies is currently missing ready replicas, the same
// "healthy" definition restartPodAction's own Validate uses elsewhere in
// this package. If it's already fully ready, the restart alone fixed it (or
// it wasn't a persistent problem) and there's nothing left to approve.
func (o OOMKilled) Recheck(ctx context.Context, cli *k8sclient.Client, sig playbooks.Signal, meta map[string]string) (playbooks.Finding, execengine.Ladder, bool, error) {
	dep := meta["deployment"]
	if dep == "" {
		return playbooks.Finding{}, execengine.Ladder{}, false, fmt.Errorf("incidente original não tem deployment registrado — não é possível reavaliar sem o pod, que já não existe mais")
	}

	want, err := cli.DeploymentReplicas(ctx, sig.Namespace, dep)
	if err != nil {
		return playbooks.Finding{}, execengine.Ladder{}, false, fmt.Errorf("checando deployment %s/%s: %w", sig.Namespace, dep, err)
	}
	ready, err := cli.DeploymentReadyReplicas(ctx, sig.Namespace, dep)
	if err != nil {
		return playbooks.Finding{}, execengine.Ladder{}, false, fmt.Errorf("checando deployment %s/%s: %w", sig.Namespace, dep, err)
	}
	if ready >= want {
		return playbooks.Finding{}, execengine.Ladder{}, false, nil
	}

	finding := playbooks.Finding{
		Signal:  sig,
		Summary: fmt.Sprintf("deployment %s/%s ainda não está saudável (%d/%d réplicas prontas) — ação de alto risco continua necessária", sig.Namespace, dep, ready, want),
		Meta:    meta,
	}
	return finding, o.Ladder(cli, finding), true, nil
}
