// Package audit records every troubleshooting request and execution as an
// immutable trail. Every value it stores is expected to already be
// redacted (see internal/redact) by the time it reaches this package —
// audit never redacts on the way in, it only ever appends and reads back.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/chwiee/k8s-ts-mcp/internal/execengine"
	"github.com/chwiee/k8s-ts-mcp/internal/policy"
)

// Entry is one complete record of a troubleshooting request: who asked,
// what the policy engine decided, and — when execution was allowed — what
// actually happened. It is also the source data for postmortem generation.
type Entry struct {
	ID         string          `json:"id"`
	Time       time.Time       `json:"time"`
	ClusterID  string          `json:"cluster_id"`
	Groups     []string        `json:"groups"`
	PlaybookID string          `json:"playbook_id"`
	Summary    string          `json:"summary"`
	Decision   policy.Decision `json:"decision"`
	// Signal is the exact trigger this incident diagnosed — kept so a later
	// approve_action call can re-send the same signal to the cluster-agent,
	// which re-derives the ladder from live state before running anything
	// (see internal/agentcore.Handler.ApproveAction). Empty for entries with
	// no compiled playbook at all (the runbook-only fallback).
	Signal SignalRecord `json:"signal,omitempty"`
	// Meta is whatever the original Diagnose call returned as playbook-
	// specific context (e.g. the owning Deployment name) — passed back
	// unchanged to approve_action's ApproveActionRequest, since by approval
	// time the pod named in Signal is expected to already be gone for
	// playbooks whose ladder starts with a pod-mutating safe action (see
	// internal/playbooks.Recheckable).
	Meta map[string]string `json:"meta,omitempty"`
	// ProposedActions is the full escalation ladder Diagnose proposed for
	// this signal, regardless of whether it was allowed to run — this is
	// what approve_action looks up actionName against, since
	// ProposedCommands below only keeps the already-formatted display
	// string, not the structured name/risk a later approval needs to
	// validate against.
	ProposedActions []ProposedActionRecord `json:"proposed_actions,omitempty"`
	// ProposedCommands is populated instead of Result when Decision.DryRun
	// is true: the commands the caller would need to run themselves.
	ProposedCommands []string           `json:"proposed_commands,omitempty"`
	Result           *execengine.Result `json:"result,omitempty"`
}

// SignalRecord is the audit-persisted shape of the playbooks.Signal /
// pb.Signal this incident diagnosed.
type SignalRecord struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	NodeName  string `json:"node_name,omitempty"`
}

// ProposedActionRecord is the audit-persisted shape of one rung of the
// ladder Diagnose proposed — enough for approve_action to find a named
// action again and confirm it was really proposed (and at what risk) for
// this incident, without re-trusting whatever the caller claims.
type ProposedActionRecord struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Risk        string `json:"risk"`
}

// NewID returns a time-sortable, unique entry ID.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%d-%s", time.Now().UTC().UnixNano(), hex.EncodeToString(b[:]))
}

// Store persists Entries. Implementations must be safe for concurrent use.
type Store interface {
	Append(ctx context.Context, e Entry) error
	ListByCluster(ctx context.Context, clusterID string) ([]Entry, error)
	Get(ctx context.Context, id string) (Entry, bool, error)
}
