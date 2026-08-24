package agentcore

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func waitingPod(reason, message string) corev1.Pod {
	return corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}},
	}}}
}

func terminatedPod(reason, message string, exitCode int32, restarts int32) corev1.Pod {
	return corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{
			State:        corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, Message: message, ExitCode: exitCode}},
			RestartCount: restarts,
		},
	}}}
}

// backingOffAfterOOM simulates a container that crashed with a specific
// termination reason (like OOMKilled) enough times that it's now settled
// into the generic CrashLoopBackOff waiting state — Kubernetes' own status
// for "keeps failing", regardless of why. This is the actual live shape
// seen after a pod has been unhealthy for a while, not just the first
// failure.
func backingOffAfterOOM(restarts int32) corev1.Pod {
	return corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
			RestartCount:         restarts,
		},
	}}}
}

func TestClassifyPod(t *testing.T) {
	cases := []struct {
		name       string
		pod        corev1.Pod
		wantKind   string
		wantHealth bool
	}{
		{"crashloopbackoff", waitingPod("CrashLoopBackOff", "back-off restarting failed container"), "PodCrashLoopBackOff", true},
		{"image pull backoff", waitingPod("ImagePullBackOff", "..."), "PodImagePullBackOff", true},
		{"err image pull", waitingPod("ErrImagePull", "..."), "PodImagePullBackOff", true},
		{"oomkilled", terminatedPod("OOMKilled", "", 137, 1), "PodOOMKilled", true},
		{"oomkilled but already backing off — OOMKilled must still win over the generic backoff status", backingOffAfterOOM(3), "PodOOMKilled", true},
		{"unknown crash with restarts", terminatedPod("Error", "", 255, 2), "PodUnknownError", true},
		{"terminated cleanly, no restarts — not unhealthy", terminatedPod("Completed", "", 0, 0), "", false},
		{"create container config error (missing secret/configmap)", waitingPod("CreateContainerConfigError", "secret \"x\" not found"), "PodCreateContainerError", true},
		{"create container error (bad securityContext etc)", waitingPod("CreateContainerError", "..."), "PodCreateContainerError", true},
		{"normal startup, ContainerCreating — not unhealthy", waitingPod("ContainerCreating", ""), "", false},
		{"normal startup, PodInitializing — not unhealthy", waitingPod("PodInitializing", ""), "", false},
		{"unrecognized waiting reason still gets reported", waitingPod("SomeFutureReasonWeDontKnowYet", "..."), "PodUnknownError", true},
		{
			name: "healthy running pod",
			pod: corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			}}},
			wantKind: "", wantHealth: false,
		},
		{
			// Live bug found testing against a real kind cluster: every
			// kube-system pod (coredns, kube-proxy, kindnet, ...) restarts
			// once whenever the underlying Docker daemon bounces, then
			// comes back up fine — but LastTerminationState.Terminated
			// stays set to that old restart forever. Before the fix, this
			// shape (currently Running, but with a nonzero-exit-code
			// LastTerminationState) got misclassified as PodUnknownError
			// even though the pod is completely healthy right now.
			name: "running now, but has a past restart from an unrelated node/daemon bounce",
			pod: corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{
					State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 255}},
					RestartCount:         1,
				},
			}}},
			wantKind: "", wantHealth: false,
		},
		{
			name: "pending, never scheduled — no container statuses at all",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/3 nodes are available: insufficient cpu"},
				},
			}},
			wantKind: "PodPending", wantHealth: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, _, unhealthy := classifyPod(tc.pod)
			if unhealthy != tc.wantHealth {
				t.Fatalf("classifyPod() unhealthy = %v, want %v", unhealthy, tc.wantHealth)
			}
			if kind != tc.wantKind {
				t.Errorf("classifyPod() kind = %q, want %q", kind, tc.wantKind)
			}
		})
	}
}
