package k8sclient

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Well-known node labels a real cloud controller manager (EKS's included)
// sets — see https://kubernetes.io/docs/reference/labels-annotations-taints/.
// A bare local cluster with no cloud-controller-manager (e.g. a kind node,
// or Floci's plain k3s stand-in) simply won't have these, so callers that
// need an authoritative region for a whole cluster should prefer
// internal/inventory over reading it back off individual nodes here.
const (
	labelRegion       = "topology.kubernetes.io/region"
	labelZone         = "topology.kubernetes.io/zone"
	labelInstanceType = "node.kubernetes.io/instance-type"
)

// NodeInfo is one node's identity/placement info, trimmed to what callers
// actually need — never the full corev1.Node (which can carry status
// fields/annotations we have no reason to expose).
type NodeInfo struct {
	Name         string
	Zone         string
	Region       string
	InstanceType string
	Architecture string
	Ready        bool
}

// ListNodes returns every node in the cluster.
func (c *Client) ListNodes(ctx context.Context) ([]NodeInfo, error) {
	nodes, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	out := make([]NodeInfo, 0, len(nodes.Items))
	for _, n := range nodes.Items {
		out = append(out, NodeInfo{
			Name:         n.Name,
			Zone:         n.Labels[labelZone],
			Region:       n.Labels[labelRegion],
			InstanceType: n.Labels[labelInstanceType],
			Architecture: n.Status.NodeInfo.Architecture,
			Ready:        nodeReady(n),
		})
	}
	return out, nil
}

func nodeReady(n corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// PodNodeName returns the node a pod is (or was last) scheduled on.
func (c *Client) PodNodeName(ctx context.Context, ns, name string) (string, error) {
	pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting pod %s/%s: %w", ns, name, err)
	}
	return pod.Spec.NodeName, nil
}

// NodeArchitecture returns one node's CPU architecture (e.g. "amd64",
// "arm64"), as reported by the kubelet.
func (c *Client) NodeArchitecture(ctx context.Context, nodeName string) (string, error) {
	node, err := c.Clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting node %s: %w", nodeName, err)
	}
	return node.Status.NodeInfo.Architecture, nil
}

// ClusterArchitectures returns every distinct CPU architecture present
// among the cluster's nodes (usually just one, e.g. {"amd64": true}; mixed
// clusters report more).
func (c *Client) ClusterArchitectures(ctx context.Context) (map[string]bool, error) {
	nodes, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	archs := make(map[string]bool)
	for _, n := range nodes.Items {
		if a := n.Status.NodeInfo.Architecture; a != "" {
			archs[a] = true
		}
	}
	return archs, nil
}
