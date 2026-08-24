// Package rolecluster is the alternative to internal/agentcore's usual
// deployment: instead of a cluster-agent process dialing in from inside a
// cluster, a cluster is reached by the hub assuming an AWS IAM Role and
// talking straight to its EKS API — see docs/ARCHITECTURE.md and
// internal/k8sclient.NewFromAWSRole. Everything downstream of that — the
// actual diagnose/execute/scan logic — is internal/agentcore.Handler
// unchanged; this package only decides how its *k8sclient.Client gets built
// and authenticated.
//
// There is deliberately no per-cluster registration step here. Every
// account's Terraform module creates the same fixed-name IRSA role
// (roleARNForAccount) already granted read-only — so given a cluster_id,
// Manager asks internal/inventory which AWS account/region it's in and
// assumes that role directly. Onboarding a cluster in an account the
// company already has therefore requires zero config changes in this
// service; only onboarding a brand new AWS account does (create the role
// there via the Terraform module). See internal/agentauth for the actual
// thing that IS manually configured: which AWS accounts a calling agent's
// token may touch at all — a much smaller, more stable list than one entry
// per cluster.
package rolecluster

import (
	"context"
	"fmt"
	"sync"

	"github.com/chwiee/k8s-ts-mcp/internal/agentcore"
	"github.com/chwiee/k8s-ts-mcp/internal/inventory"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/transport"
)

// readOnlyRoleName is the fixed IRSA role name every account's Terraform
// module creates for k8s-ts-mcp, already granted read-only — only the
// account ID in the ARN ever changes.
const readOnlyRoleName = "k8s-ts-mcp-readonly"

func roleARNForAccount(accountID string) string {
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, readOnlyRoleName)
}

// newClientFunc matches k8sclient.NewFromAWSRole's signature — a field
// (not a direct call) so tests can inject a fake and exercise Manager's
// routing/caching logic without ever touching AWS.
type newClientFunc func(ctx context.Context, roleARN, eksClusterName, region string) (*k8sclient.Client, error)

// Manager reaches a cluster purely by convention: given a cluster_id, it
// asks Inventory for that cluster's AWS account/region, builds the IRSA
// role ARN by fixed naming convention, and assumes it directly — see the
// package doc comment. Each resulting Handler is built lazily on first use
// and cached forever after — the underlying k8sclient.Client refreshes its
// own auth token per-request (see k8sclient.NewFromAWSRole), so there's no
// need to rebuild or expire the Handler itself once it exists.
type Manager struct {
	Inventory inventory.Lookup
	registry  *playbooks.Registry
	newClient newClientFunc

	mu       sync.Mutex
	handlers map[string]*agentcore.Handler
}

// NewManager builds a Manager backed by inv, wiring every resulting Handler
// to the same playbook registry a normal cluster-agent would use — a
// Role-based cluster runs the exact same diagnose/execute/scan logic, just
// without a separate agent process or gRPC hop.
func NewManager(inv inventory.Lookup, registry *playbooks.Registry) *Manager {
	return &Manager{
		Inventory: inv,
		registry:  registry,
		newClient: k8sclient.NewFromAWSRole,
		handlers:  make(map[string]*agentcore.Handler),
	}
}

// ClusterIDs is always empty: discovery-by-convention has no fixed catalog
// to enumerate, only "does this specific cluster_id exist" (answered by
// Handler below, on demand). list_clusters is disabled in internal/mcptools
// for this reason — see docs/ARCHITECTURE.md.
func (m *Manager) ClusterIDs() []string {
	return nil
}

// Handler returns clusterID's transport.Handler, building (and caching) it
// on first use via Inventory + the fixed role-naming convention. ok is
// false when Inventory has no entry for clusterID at all (or Inventory
// itself is nil) — the caller should fall back to the gRPC agent registry
// in that case, same contract as any other RoleResolver. A real failure —
// the inventory lookup itself erroring, or a bad Role/unreachable EKS —
// comes back as a non-nil err with ok still true, distinct from "not mine
// to serve."
func (m *Manager) Handler(ctx context.Context, clusterID string) (h transport.Handler, ok bool, err error) {
	m.mu.Lock()
	if existing, cached := m.handlers[clusterID]; cached {
		m.mu.Unlock()
		return existing, true, nil
	}
	m.mu.Unlock()

	if m.Inventory == nil {
		return nil, false, nil
	}
	info, found, err := m.Inventory.Lookup(ctx, clusterID)
	if err != nil {
		return nil, true, fmt.Errorf("looking up cluster %s in inventory: %w", clusterID, err)
	}
	if !found {
		return nil, false, nil
	}

	eksClusterName := info.EKSClusterName
	if eksClusterName == "" {
		eksClusterName = clusterID
	}
	cli, err := m.newClient(ctx, roleARNForAccount(info.AWSAccountID), eksClusterName, info.Region)
	if err != nil {
		return nil, true, fmt.Errorf("building client for cluster %s (account %s): %w", clusterID, info.AWSAccountID, err)
	}
	built := &agentcore.Handler{Client: cli, Registry: m.registry}

	m.mu.Lock()
	// Another goroutine may have built and cached one first while we were
	// off making AWS calls — keep whichever landed first rather than
	// discarding a perfectly good client.
	if existing, cached := m.handlers[clusterID]; cached {
		m.mu.Unlock()
		return existing, true, nil
	}
	m.handlers[clusterID] = built
	m.mu.Unlock()
	return built, true, nil
}
