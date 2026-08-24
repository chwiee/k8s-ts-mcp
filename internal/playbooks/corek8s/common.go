package corek8s

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
	"github.com/chwiee/k8s-ts-mcp/internal/policy"
)

// restartPodAction is the shared first rung of every core playbook that can
// try "delete the pod and let its controller recreate it" as a cheap first
// attempt — CrashLoopBackOff, OOMKilled and ImagePullBackOff all offer it.
func restartPodAction(cli *k8sclient.Client, ns, pod, dep string, hasDep bool) execengine.Action {
	return execengine.Action{
		Name:        "restart pod",
		Description: fmt.Sprintf("kubectl delete pod %s -n %s", pod, ns),
		Risk:        policy.RiskSafe,
		Run: func(ctx context.Context) (string, error) {
			return cli.RestartPod(ctx, ns, pod)
		},
		// A pod's Status.Phase is not a reliable "it's healthy now" signal
		// here: a Terminating pod (mid-delete) still reads Phase=Running
		// until it's actually gone, and the replacement pod has a
		// different, generated name. So this waits for the deleted pod to
		// disappear, then — when the pod is Deployment-managed — for the
		// Deployment's ReadyReplicas to recover, which does require the
		// replacement to actually pass its container's readiness, not just
		// briefly exist.
		Validate: func(ctx context.Context) (bool, error) {
			gone, err := waitFor(ctx, 30*time.Second, func() (bool, error) {
				_, err := cli.PodPhase(ctx, ns, pod)
				return apierrors.IsNotFound(err), nil
			})
			if err != nil || !gone {
				return false, err
			}
			if !hasDep {
				return true, nil // no Deployment to check further against — best effort
			}
			want, err := cli.DeploymentReplicas(ctx, ns, dep)
			if err != nil {
				return false, err
			}
			return waitFor(ctx, 60*time.Second, func() (bool, error) {
				ready, err := cli.DeploymentReadyReplicas(ctx, ns, dep)
				return ready >= want, err
			})
		},
	}
}

// waitFor polls check every second until it reports true, ctx is done, or
// timeout elapses.
func waitFor(ctx context.Context, timeout time.Duration, check func() (bool, error)) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := check()
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
