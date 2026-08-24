package k8sclient

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
