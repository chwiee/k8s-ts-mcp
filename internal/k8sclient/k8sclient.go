// Package k8sclient wraps the client-go clientset and dynamic client with
// the small, specific set of operations the built-in playbooks need. It is
// the only package in cluster-agent that talks to the Kubernetes API
// directly — playbooks call it, never client-go itself, so every mutating
// call lives in one auditable place.
package k8sclient

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client is the cluster-agent's handle onto its local cluster's API server.
type Client struct {
	Clientset kubernetes.Interface
	Dynamic   dynamic.Interface
}

// New builds a Client. An empty kubeconfigPath means "running inside the
// cluster" (in-cluster config, i.e. the pod's own ServiceAccount) — the
// normal case for cluster-agent; a non-empty path is for local development
// against a kubeconfig.
func New(kubeconfigPath string) (*Client, error) {
	var cfg *rest.Config
	var err error
	if kubeconfigPath != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("building kube config: %w", err)
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client: %w", err)
	}
	return &Client{Clientset: cs, Dynamic: dyn}, nil
}

// ListPods lists every pod in ns ("" for every namespace) — the discovery
// step behind the "scan_cluster" MCP tool, before any specific pod name is
// known.
func (c *Client) ListPods(ctx context.Context, ns string) ([]corev1.Pod, error) {
	pods, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods in %q: %w", ns, err)
	}
	return pods.Items, nil
}

// RestartPod deletes a pod so its controller (ReplicaSet, DaemonSet, ...)
// recreates it fresh. Safe/idempotent for any controller-owned pod.
func (c *Client) RestartPod(ctx context.Context, ns, name string) (string, error) {
	if err := c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return "", fmt.Errorf("deleting pod %s/%s: %w", ns, name, err)
	}
	return fmt.Sprintf("pod %s/%s deleted, controller will recreate it", ns, name), nil
}

// PodPhase reports a pod's current phase (Running, Pending, Failed, ...).
func (c *Client) PodPhase(ctx context.Context, ns, name string) (corev1.PodPhase, error) {
	pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting pod %s/%s: %w", ns, name, err)
	}
	return pod.Status.Phase, nil
}

// PodRestartCount sums container restart counts for a pod — the signal a
// CrashLoopBackOff validator checks to see if restarts are still climbing.
func (c *Client) PodRestartCount(ctx context.Context, ns, name string) (int32, error) {
	pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("getting pod %s/%s: %w", ns, name, err)
	}
	var total int32
	for _, cs := range pod.Status.ContainerStatuses {
		total += cs.RestartCount
	}
	return total, nil
}

// PodForDeployment resolves a Deployment to one of its current pods —
// needed because a Deployment's pod names are generated, not fixed, so
// nothing can reference "the KEDA operator's pod" directly the way it can
// reference the Deployment itself. Prefers a currently-Running pod; falls
// back to whatever's first if none are Running yet, since even a crashing
// pod's log can be worth reading.
func (c *Client) PodForDeployment(ctx context.Context, ns, deployment string) (string, error) {
	dep, err := c.Clientset.AppsV1().Deployments(ns).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting deployment %s/%s: %w", ns, deployment, err)
	}
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return "", fmt.Errorf("deployment %s/%s has an invalid selector: %w", ns, deployment, err)
	}
	pods, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return "", fmt.Errorf("listing pods for deployment %s/%s: %w", ns, deployment, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("deployment %s/%s has no pods right now", ns, deployment)
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			return p.Name, nil
		}
	}
	return pods.Items[0].Name, nil
}

// OwningDeploymentName walks a pod's owner references (Pod -> ReplicaSet ->
// Deployment) to find the Deployment that manages it. Returns an error if
// the pod isn't managed by a Deployment (e.g. it's a bare pod or owned by a
// DaemonSet/StatefulSet/Job instead).
func (c *Client) OwningDeploymentName(ctx context.Context, ns, podName string) (string, error) {
	pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting pod %s/%s: %w", ns, podName, err)
	}
	rsName := ""
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "ReplicaSet" {
			rsName = ref.Name
			break
		}
	}
	if rsName == "" {
		return "", fmt.Errorf("pod %s/%s is not owned by a ReplicaSet", ns, podName)
	}
	rs, err := c.Clientset.AppsV1().ReplicaSets(ns).Get(ctx, rsName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting replicaset %s/%s: %w", ns, rsName, err)
	}
	for _, ref := range rs.OwnerReferences {
		if ref.Kind == "Deployment" {
			return ref.Name, nil
		}
	}
	return "", fmt.Errorf("replicaset %s/%s is not owned by a Deployment", ns, rsName)
}

// DeploymentReplicas returns a deployment's desired replica count.
func (c *Client) DeploymentReplicas(ctx context.Context, ns, name string) (int32, error) {
	dep, err := c.Clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("getting deployment %s/%s: %w", ns, name, err)
	}
	if dep.Spec.Replicas == nil {
		return 1, nil
	}
	return *dep.Spec.Replicas, nil
}

// ScaleDeployment sets a deployment's desired replica count.
func (c *Client) ScaleDeployment(ctx context.Context, ns, name string, replicas int32) (string, error) {
	dep, err := c.Clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting deployment %s/%s: %w", ns, name, err)
	}
	dep.Spec.Replicas = &replicas
	if _, err := c.Clientset.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return "", fmt.Errorf("scaling deployment %s/%s to %d: %w", ns, name, replicas, err)
	}
	return fmt.Sprintf("deployment %s/%s scaled to %d replicas", ns, name, replicas), nil
}

// DeploymentReadyReplicas returns how many of a deployment's pods are ready.
func (c *Client) DeploymentReadyReplicas(ctx context.Context, ns, name string) (int32, error) {
	dep, err := c.Clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("getting deployment %s/%s: %w", ns, name, err)
	}
	return dep.Status.ReadyReplicas, nil
}

// RolloutRestartDeployment triggers the same rolling restart as
// `kubectl rollout restart`, by bumping a restart-timestamp annotation on
// the pod template so the controller recreates every pod.
func (c *Client) RolloutRestartDeployment(ctx context.Context, ns, name string) (string, error) {
	dep, err := c.Clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting deployment %s/%s: %w", ns, name, err)
	}
	if dep.Spec.Template.ObjectMeta.Annotations == nil {
		dep.Spec.Template.ObjectMeta.Annotations = map[string]string{}
	}
	dep.Spec.Template.ObjectMeta.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().UTC().Format(time.RFC3339)
	if _, err := c.Clientset.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return "", fmt.Errorf("rollout-restarting deployment %s/%s: %w", ns, name, err)
	}
	return fmt.Sprintf("deployment %s/%s rollout restarted", ns, name), nil
}

// DaemonSetPodOnNode finds the pod a DaemonSet has scheduled onto a given
// node (e.g. calico-node), so a caller can target it for a restart.
func (c *Client) DaemonSetPodOnNode(ctx context.Context, ns, dsName, nodeName string) (*corev1.Pod, error) {
	ds, err := c.Clientset.AppsV1().DaemonSets(ns).Get(ctx, dsName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting daemonset %s/%s: %w", ns, dsName, err)
	}
	sel, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("parsing daemonset %s/%s selector: %w", ns, dsName, err)
	}
	pods, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("listing pods for daemonset %s/%s: %w", ns, dsName, err)
	}
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName == nodeName {
			return &pods.Items[i], nil
		}
	}
	return nil, fmt.Errorf("daemonset %s/%s has no pod on node %s", ns, dsName, nodeName)
}

// DeleteHPA deletes a HorizontalPodAutoscaler. Used to un-stick a KEDA
// ScaledObject: KEDA owns the HPA it creates and recreates it on the next
// reconcile, which is the commonly documented fix for a stuck scaler.
func (c *Client) DeleteHPA(ctx context.Context, ns, name string) (string, error) {
	if err := c.Clientset.AutoscalingV2().HorizontalPodAutoscalers(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return "", fmt.Errorf("deleting hpa %s/%s: %w", ns, name, err)
	}
	return fmt.Sprintf("hpa %s/%s deleted, KEDA will recreate it", ns, name), nil
}

var scaledObjectGVR = schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}

// ScaledObjectReady reads a KEDA ScaledObject's status.conditions and
// reports whether its "Ready" condition is true, plus that condition's
// message. Uses the dynamic client since keda.sh types aren't in
// client-go's built-in scheme.
func (c *Client) ScaledObjectReady(ctx context.Context, ns, name string) (bool, string, error) {
	obj, err := c.Dynamic.Resource(scaledObjectGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, "", fmt.Errorf("getting scaledobject %s/%s: %w", ns, name, err)
	}
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false, "no status.conditions reported yet", nil
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok || cond["type"] != "Ready" {
			continue
		}
		msg, _ := cond["message"].(string)
		return cond["status"] == "True", msg, nil
	}
	return false, "no Ready condition reported yet", nil
}
