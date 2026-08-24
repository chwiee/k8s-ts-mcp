// Package corek8s implements playbooks for problems that only need the
// built-in Kubernetes API (no Calico/KEDA CRDs).
package corek8s

import (
	"context"
	"fmt"
	"time"

	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/policy"
)

// CrashLoopBackOff escalates from the cheapest fix (restart the one pod) to
// the most invasive (roll the whole deployment), each step validated before
// trying the next.
type CrashLoopBackOff struct{}

func (CrashLoopBackOff) ID() string       { return "core/crashloopbackoff" }
func (CrashLoopBackOff) Resource() string { return "core" }

func (CrashLoopBackOff) Detect(sig playbooks.Signal) bool {
	return sig.Kind == "PodCrashLoopBackOff"
}

func (CrashLoopBackOff) Diagnose(ctx context.Context, cli *k8sclient.Client, sig playbooks.Signal) (playbooks.Finding, error) {
	restarts, err := cli.PodRestartCount(ctx, sig.Namespace, sig.Name)
	if err != nil {
		return playbooks.Finding{}, fmt.Errorf("diagnosing %s/%s: %w", sig.Namespace, sig.Name, err)
	}

	summary := fmt.Sprintf("pod %s/%s está em CrashLoopBackOff, %d restarts registrados", sig.Namespace, sig.Name, restarts)
	meta := map[string]string{}
	if dep, err := cli.OwningDeploymentName(ctx, sig.Namespace, sig.Name); err == nil {
		meta["deployment"] = dep
	}
	return playbooks.Finding{Signal: sig, Summary: summary, Meta: meta}, nil
}

func (CrashLoopBackOff) Ladder(cli *k8sclient.Client, finding playbooks.Finding) execengine.Ladder {
	ns, pod := finding.Signal.Namespace, finding.Signal.Name
	dep, hasDep := finding.Meta["deployment"]
	hasDep = hasDep && dep != ""

	actions := []execengine.Action{restartPodAction(cli, ns, pod, dep, hasDep)}

	if hasDep {
		actions = append(actions,
			execengine.Action{
				Name:        "scale deployment down and up",
				Description: fmt.Sprintf("kubectl scale deployment %s -n %s --replicas=0 && kubectl scale deployment %s -n %s --replicas=<original>", dep, ns, dep, ns),
				Risk:        policy.RiskMedium,
				Snapshot: func(ctx context.Context) (execengine.State, error) {
					n, err := cli.DeploymentReplicas(ctx, ns, dep)
					return execengine.State{"replicas": n}, err
				},
				Run: func(ctx context.Context) (string, error) {
					n, err := cli.DeploymentReplicas(ctx, ns, dep)
					if err != nil {
						return "", err
					}
					if _, err := cli.ScaleDeployment(ctx, ns, dep, 0); err != nil {
						return "", err
					}
					time.Sleep(2 * time.Second)
					out, err := cli.ScaleDeployment(ctx, ns, dep, n)
					return out, err
				},
				Validate: func(ctx context.Context) (bool, error) {
					want, err := cli.DeploymentReplicas(ctx, ns, dep)
					if err != nil {
						return false, err
					}
					return waitFor(ctx, 60*time.Second, func() (bool, error) {
						ready, err := cli.DeploymentReadyReplicas(ctx, ns, dep)
						return ready >= want, err
					})
				},
				Rollback: func(ctx context.Context, snap execengine.State) error {
					n, _ := snap["replicas"].(int32)
					_, err := cli.ScaleDeployment(ctx, ns, dep, n)
					return err
				},
			},
			execengine.Action{
				Name:        "rollout restart deployment",
				Description: fmt.Sprintf("kubectl rollout restart deployment %s -n %s", dep, ns),
				Risk:        policy.RiskMedium,
				Run: func(ctx context.Context) (string, error) {
					return cli.RolloutRestartDeployment(ctx, ns, dep)
				},
				Validate: func(ctx context.Context) (bool, error) {
					want, err := cli.DeploymentReplicas(ctx, ns, dep)
					if err != nil {
						return false, err
					}
					return waitFor(ctx, 90*time.Second, func() (bool, error) {
						ready, err := cli.DeploymentReadyReplicas(ctx, ns, dep)
						return ready >= want, err
					})
				},
			},
		)
	}

	return execengine.Ladder{PlaybookID: CrashLoopBackOff{}.ID(), Actions: actions}
}
