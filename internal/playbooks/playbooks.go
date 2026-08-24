// Package playbooks defines the diagnostic/remediation plugin interface
// (core Kubernetes, Calico, KEDA, ...) and the registry cluster-agent uses
// to find the right playbook for a signal. Playbook bundles themselves are
// meant to be versioned OCI artifacts published to ECR and delivered to
// clusters via ArgoCD (see docs/ARCHITECTURE.md) — the ones built into this
// package are the starting set, kept in-tree because they only depend on
// core Kubernetes/Calico/KEDA APIs the agent already links against.
package playbooks

import (
	"context"
	"fmt"

	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
)

// Signal is one piece of evidence a playbook can react to — either a live
// event/metric from the predictive pipeline, or a direct question from an
// operator via the MCP tool (e.g. "diagnose crashlooping pods in ns X").
type Signal struct {
	Kind      string // e.g. "PodCrashLoopBackOff", "CalicoNodeDegraded", "KEDAScaledObjectStuck"
	Namespace string
	Name      string // the affected resource's name
	NodeName  string // set for node-scoped signals (e.g. Calico)
}

// Finding is what Diagnose produces: a human-readable summary plus the
// signal it explains, ready to hand to execengine.Run or to return as text
// when the caller is in dry-run.
type Finding struct {
	Signal  Signal
	Summary string
	// Meta carries playbook-specific context resolved during Diagnose (e.g.
	// the owning Deployment name for a crashlooping pod) forward to Ladder,
	// so Ladder doesn't have to re-derive it.
	Meta map[string]string
}

// Playbook is one pluggable unit of troubleshooting knowledge for a
// supported resource (core Kubernetes, Calico, KEDA, ...).
type Playbook interface {
	// ID is a stable, unique identifier, e.g. "core/crashloopbackoff".
	ID() string
	// Resource names what this playbook covers, e.g. "core", "calico", "keda".
	Resource() string
	// Detect reports whether this playbook knows how to handle sig.
	Detect(sig Signal) bool
	// Diagnose turns a detected signal into a Finding.
	Diagnose(ctx context.Context, cli *k8sclient.Client, sig Signal) (Finding, error)
	// Ladder builds the escalation ladder (execengine.MaxActions at most)
	// for a Finding this playbook diagnosed.
	Ladder(cli *k8sclient.Client, finding Finding) execengine.Ladder
}

// Recheckable is implemented by playbooks whose Ladder can include a
// policy.RiskHigh action that a human approves later, after Ladder's
// earlier actions already ran — for a playbook like core/oomkilled, whose
// first action deletes/recreates the pod, the Signal's original pod name is
// expected to already be gone by the time approval happens, so re-running
// Diagnose (which needs that exact pod to exist) would just fail. Recheck
// re-diagnoses against the CURRENT state of the resource identified by meta
// (the Finding.Meta the original Diagnose call produced, e.g. the owning
// Deployment name) instead of the vanished pod, and rebuilds the ladder
// from that current state. ok is false when the resource no longer shows
// the problem the high-risk action was proposed for — approval must be
// refused rather than running a high-risk action against something that
// already recovered on its own.
//
// A playbook that doesn't implement this interface is re-diagnosed the
// normal way (by the pod named in Signal) when its high-risk action is
// approved — correct for playbooks whose ladder never changes the pod's
// identity before a high-risk step, like core/execformaterror (a single,
// purely diagnostic high-risk action, no restart before it).
type Recheckable interface {
	Recheck(ctx context.Context, cli *k8sclient.Client, sig Signal, meta map[string]string) (finding Finding, ladder execengine.Ladder, ok bool, err error)
}

// Registry holds every playbook cluster-agent knows about.
type Registry struct {
	playbooks []Playbook
}

// NewRegistry builds a Registry from the given playbooks.
func NewRegistry(pb ...Playbook) *Registry {
	return &Registry{playbooks: pb}
}

// Find returns the first registered playbook that detects sig.
func (r *Registry) Find(sig Signal) (Playbook, bool) {
	for _, p := range r.playbooks {
		if p.Detect(sig) {
			return p, true
		}
	}
	return nil, false
}

// List returns every registered playbook, for the MCP "list_playbooks" tool.
func (r *Registry) List() []Playbook {
	out := make([]Playbook, len(r.playbooks))
	copy(out, r.playbooks)
	return out
}

// ErrNoPlaybook is returned by callers that look up a playbook via Find and
// get none back — kept here so callers don't have to hand-roll the message.
func ErrNoPlaybook(sig Signal) error {
	return fmt.Errorf("nenhum playbook registrado sabe lidar com o sinal %q (namespace=%s, name=%s)", sig.Kind, sig.Namespace, sig.Name)
}
