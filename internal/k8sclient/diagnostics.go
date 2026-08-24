package k8sclient

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TerminationInfo is a container's last-known termination — the reason,
// message and exit code the kubelet recorded, e.g. Reason="OOMKilled" or a
// Message containing "exec format error". Assumes a single-container pod
// (the first container's status), which every playbook using this covers;
// a multi-container pod would need to know which container to look at.
type TerminationInfo struct {
	Found    bool
	Reason   string
	Message  string
	ExitCode int32
}

// ContainerTermination reads the first container's termination info,
// preferring the current state (if it's terminated right now) and falling
// back to the last known termination (if it's since been restarted and is
// now in a different state, e.g. waiting to restart under CrashLoopBackOff).
func (c *Client) ContainerTermination(ctx context.Context, ns, pod string) (TerminationInfo, error) {
	p, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return TerminationInfo{}, fmt.Errorf("getting pod %s/%s: %w", ns, pod, err)
	}
	if len(p.Status.ContainerStatuses) == 0 {
		return TerminationInfo{}, nil
	}
	cs := p.Status.ContainerStatuses[0]
	if t := cs.State.Terminated; t != nil {
		return TerminationInfo{Found: true, Reason: t.Reason, Message: t.Message, ExitCode: t.ExitCode}, nil
	}
	if t := cs.LastTerminationState.Terminated; t != nil {
		return TerminationInfo{Found: true, Reason: t.Reason, Message: t.Message, ExitCode: t.ExitCode}, nil
	}
	return TerminationInfo{}, nil
}

// ContainerLogs returns the last tailLines of the first container's log
// output — previous=true reads the crashed instance's log (the one that
// matters right after a restart; the "current" stream is usually empty or
// belongs to the new attempt). Best-effort: some failures (e.g. the
// container never actually started) leave nothing to read, which is not
// itself an error worth failing the caller's diagnosis over.
func (c *Client) ContainerLogs(ctx context.Context, ns, pod string, previous bool, tailLines int64) (string, error) {
	raw, err := c.Clientset.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Previous:  previous,
		TailLines: &tailLines,
	}).DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("reading logs for %s/%s: %w", ns, pod, err)
	}
	return string(raw), nil
}

// WaitingInfo is why a container hasn't started, e.g. Reason="ImagePullBackOff".
type WaitingInfo struct {
	Found   bool
	Reason  string
	Message string
}

// ContainerWaiting reads the first container's current Waiting state, if any.
func (c *Client) ContainerWaiting(ctx context.Context, ns, pod string) (WaitingInfo, error) {
	p, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return WaitingInfo{}, fmt.Errorf("getting pod %s/%s: %w", ns, pod, err)
	}
	if len(p.Status.ContainerStatuses) == 0 {
		return WaitingInfo{}, nil
	}
	w := p.Status.ContainerStatuses[0].State.Waiting
	if w == nil {
		return WaitingInfo{}, nil
	}
	return WaitingInfo{Found: true, Reason: w.Reason, Message: w.Message}, nil
}

// ContainerMemoryLimit returns the first container's memory limit on a
// Deployment's pod template, or the zero Quantity if none is set.
func (c *Client) ContainerMemoryLimit(ctx context.Context, ns, deployment string) (resource.Quantity, error) {
	dep, err := c.Clientset.AppsV1().Deployments(ns).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("getting deployment %s/%s: %w", ns, deployment, err)
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return resource.Quantity{}, fmt.Errorf("deployment %s/%s has no containers", ns, deployment)
	}
	return dep.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory], nil
}

// IncreaseContainerMemoryLimit multiplies the first container's memory
// limit by factor (e.g. 1.5 for +50%) and rolls the deployment out. This is
// always a policy.RiskHigh action in every playbook that offers it — see
// internal/execengine.Run, which refuses to auto-run RiskHigh actions
// regardless of caller — because an arbitrary multiplier is a reasonable
// starting guess, not a substitute for a human deciding the right limit
// (or investigating whether it's actually a memory leak).
func (c *Client) IncreaseContainerMemoryLimit(ctx context.Context, ns, deployment string, factor float64) (string, error) {
	dep, err := c.Clientset.AppsV1().Deployments(ns).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting deployment %s/%s: %w", ns, deployment, err)
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return "", fmt.Errorf("deployment %s/%s has no containers", ns, deployment)
	}
	container := &dep.Spec.Template.Spec.Containers[0]
	current := container.Resources.Limits[corev1.ResourceMemory]
	newLimit := resource.NewQuantity(int64(float64(current.Value())*factor), current.Format)

	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}
	container.Resources.Limits[corev1.ResourceMemory] = *newLimit

	if _, err := c.Clientset.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return "", fmt.Errorf("updating memory limit on %s/%s: %w", ns, deployment, err)
	}
	return fmt.Sprintf("deployment %s/%s memory limit: %s -> %s", ns, deployment, current.String(), newLimit.String()), nil
}
