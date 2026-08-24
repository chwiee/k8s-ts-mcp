// Package calico implements playbooks for the Calico CNI. It is deliberately
// diagnostic-first: BGP/networking incidents are highly version- and
// topology-specific, so the one remediation wired up here (restarting the
// calico-node pod on the affected node) is the single commonly-documented,
// safe, idempotent fix — anything more specific belongs in a playbook this
// team writes with knowledge of its own Calico setup.
package calico

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/policy"
)

// NodeDegraded restarts the calico-node pod on a node reporting BGP/routing
// problems — resetting the BGP mesh from that node is the standard first
// step in Calico's own troubleshooting docs.
type NodeDegraded struct {
	// Namespace is where the calico-node DaemonSet lives: "calico-system"
	// for an operator install, "kube-system" for a manifest install. Set to
	// match this team's install method.
	Namespace string
}

func (n NodeDegraded) namespace() string {
	if n.Namespace != "" {
		return n.Namespace
	}
	return "calico-system"
}

func (NodeDegraded) ID() string       { return "calico/node-degraded" }
func (NodeDegraded) Resource() string { return "calico" }

func (NodeDegraded) Detect(sig playbooks.Signal) bool {
	return sig.Kind == "CalicoNodeDegraded" && sig.NodeName != ""
}

func (n NodeDegraded) Diagnose(ctx context.Context, cli *k8sclient.Client, sig playbooks.Signal) (playbooks.Finding, error) {
	pod, err := cli.DaemonSetPodOnNode(ctx, n.namespace(), "calico-node", sig.NodeName)
	if err != nil {
		return playbooks.Finding{}, fmt.Errorf("diagnosing calico node %s: %w", sig.NodeName, err)
	}
	summary := fmt.Sprintf("nó %s reportando degradação de rede Calico; pod calico-node atual: %s/%s", sig.NodeName, pod.Namespace, pod.Name)
	return playbooks.Finding{Signal: sig, Summary: summary}, nil
}

func (n NodeDegraded) Ladder(cli *k8sclient.Client, finding playbooks.Finding) execengine.Ladder {
	ns, node := n.namespace(), finding.Signal.NodeName
	return execengine.Ladder{
		PlaybookID: NodeDegraded{}.ID(),
		Actions: []execengine.Action{
			{
				Name:        "restart calico-node pod on node",
				Description: fmt.Sprintf("kubectl delete pod -n %s -l k8s-app=calico-node --field-selector spec.nodeName=%s", ns, node),
				Risk:        policy.RiskMedium,
				Run: func(ctx context.Context) (string, error) {
					pod, err := cli.DaemonSetPodOnNode(ctx, ns, "calico-node", node)
					if err != nil {
						return "", err
					}
					return cli.RestartPod(ctx, pod.Namespace, pod.Name)
				},
				Validate: func(ctx context.Context) (bool, error) {
					return waitFor(ctx, 60*time.Second, func() (bool, error) {
						pod, err := cli.DaemonSetPodOnNode(ctx, ns, "calico-node", node)
						if err != nil {
							return false, nil // pod not recreated yet, keep polling
						}
						return pod.Status.Phase == corev1.PodRunning, nil
					})
				},
			},
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
