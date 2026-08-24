package agentcore

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/chwiee/k8s-ts-mcp/internal/redact"
	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

// Scan implements transport.Handler: it lists every pod in ns and
// classifies the unhealthy ones into the same Signal.Kind vocabulary
// Diagnose/Execute expect, so a caller can go from "what's wrong in
// spoke-1" straight to "fix nginx-abc123" without already knowing a pod's
// name up front.
func (h *Handler) Scan(ctx context.Context, ns string) ([]*pb.PodIssue, error) {
	pods, err := h.Client.ListPods(ctx, ns)
	if err != nil {
		return nil, err
	}

	var issues []*pb.PodIssue
	for _, pod := range pods {
		kind, detail, unhealthy := classifyPod(pod)
		if !unhealthy {
			continue
		}
		if kind == "PodUnknownError" {
			// classifyPod is deliberately I/O-free and pure — this is the
			// one exception, an extra log fetch only for the cases it
			// couldn't already explain, to catch exec-format-error (wrong
			// CPU architecture): the Kubernetes status API often exposes no
			// message at all for it (confirmed live testing an arm64 image
			// on an amd64 node — just an exit code), so the only reliable
			// signal is the runtime's own error text in the container log.
			if refined, refinedDetail, ok := h.refineExecFormatError(ctx, pod); ok {
				kind, detail = refined, refinedDetail
			}
		}
		issues = append(issues, &pb.PodIssue{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Kind:      kind,
			Detail:    redact.Text(detail),
		})
	}
	return issues, nil
}

// refineExecFormatError checks pod's log output for a known exec-format-
// error signature, trying the current container instance's log first (the
// common shape when the failure is recent enough that State.Terminated is
// still set directly) and falling back to the previous instance's log if
// that comes back empty (e.g. the container has since moved on to
// Waiting/CrashLoopBackOff and only LastTerminationState remains). Errors
// fetching logs are swallowed — this is a best-effort refinement of an
// already-reported issue, not something that should fail the whole scan.
func (h *Handler) refineExecFormatError(ctx context.Context, pod corev1.Pod) (kind, detail string, ok bool) {
	logs, err := h.Client.ContainerLogs(ctx, pod.Namespace, pod.Name, false, 20)
	if (err != nil || strings.TrimSpace(logs) == "") && pod.Status.ContainerStatuses[0].RestartCount > 0 {
		logs, err = h.Client.ContainerLogs(ctx, pod.Namespace, pod.Name, true, 20)
	}
	if err != nil || !looksLikeExecFormatError(logs) {
		return "", "", false
	}
	return "PodExecFormatError", strings.TrimSpace(logs), true
}

// transientWaitingReasons are Waiting states every pod passes through
// normally on the way up — never worth reporting as an issue on their own.
var transientWaitingReasons = map[string]bool{
	"ContainerCreating": true,
	"PodInitializing":   true,
}

// classifyPod maps a pod's status to the same Signal.Kind values the
// built-in playbooks Detect on — or, when nothing recognized applies but
// something is clearly still wrong, "PodUnknownError"/"PodPending" so it at
// least surfaces instead of going unnoticed. It only ever returns a
// specific kind it's actually confident about (Waiting.Reason and
// Terminated.Reason are set by the kubelet itself, not guessed).
//
// Order matters: once a pod has failed enough times, Waiting.Reason settles
// on the generic "CrashLoopBackOff" regardless of why it's actually
// crashing — that's just "kubernetes is backing off restarts", not a root
// cause. A specific termination reason like OOMKilled is more useful and
// more actionable (it has its own playbook with a real remediation path),
// so it's checked first; CrashLoopBackOff is the fallback once nothing more
// specific was found.
func classifyPod(pod corev1.Pod) (kind, detail string, unhealthy bool) {
	// No container was ever scheduled — the pod itself never got placed on
	// a node. ContainerStatuses is empty in this case, so nothing below
	// would catch it without this check.
	if pod.Status.Phase == corev1.PodPending && len(pod.Status.ContainerStatuses) == 0 {
		return "PodPending", schedulingFailureMessage(pod), true
	}

	for _, cs := range pod.Status.ContainerStatuses {
		// A container currently Running is not an issue, no matter what
		// LastTerminationState says — every long-lived pod eventually
		// racks up a restart or two (a node reboot, a Docker daemon
		// bounce, a one-off transient failure that self-recovered), and
		// none of that is still "wrong" once the container is actually up
		// and running again. Without this check, LastTerminationState
		// below would flag a healthy pod as PodUnknownError/PodOOMKilled
		// forever after its first-ever restart — seen live on kube-system
		// pods (coredns, kube-proxy, ...) that restarted once when the
		// underlying Docker daemon restarted, then came back up fine.
		if cs.State.Running != nil {
			continue
		}

		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull":
				return "PodImagePullBackOff", w.Message, true
			case "CreateContainerConfigError", "CreateContainerError":
				return "PodCreateContainerError", w.Message, true
			}
		}

		term := cs.State.Terminated
		if term == nil {
			term = cs.LastTerminationState.Terminated
		}
		if term != nil && term.Reason == "OOMKilled" {
			return "PodOOMKilled", term.Message, true
		}

		if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
			return "PodCrashLoopBackOff", w.Message, true
		}

		if term != nil && term.ExitCode != 0 && cs.RestartCount > 0 {
			return "PodUnknownError", term.Message, true
		}

		// Anything else stuck Waiting that isn't just normal startup —
		// report it rather than stay silent, even without a specific kind
		// for it.
		if w := cs.State.Waiting; w != nil && w.Reason != "" && !transientWaitingReasons[w.Reason] {
			return "PodUnknownError", fmt.Sprintf("%s: %s", w.Reason, w.Message), true
		}
	}
	return "", "", false
}

// schedulingFailureMessage pulls the reason the scheduler gave for not
// being able to place a Pending pod, if Kubernetes reported one via the
// pod's PodScheduled condition.
func schedulingFailureMessage(pod corev1.Pod) string {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return c.Message
		}
	}
	return ""
}
