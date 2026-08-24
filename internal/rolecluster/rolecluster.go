// Package rolecluster is the alternative to internal/agentcore's usual
// deployment: instead of a cluster-agent process dialing in from inside a
// cluster, a cluster listed here is reached by the hub assuming an AWS IAM
// Role and talking straight to its EKS API — see docs/ARCHITECTURE.md and
// internal/k8sclient.NewFromAWSRole. Everything downstream of that — the
// actual diagnose/execute/scan logic — is internal/agentcore.Handler
// unchanged; this package only decides how its *k8sclient.Client gets
// built and authenticated.
package rolecluster

import (
	"context"
	"fmt"
	"io"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/chwiee/k8s-ts-mcp/internal/agentcore"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
	"github.com/chwiee/k8s-ts-mcp/internal/playbooks"
	"github.com/chwiee/k8s-ts-mcp/internal/transport"
)

// ClusterConfig identifies one Role-based cluster: which Role to assume,
// and which EKS cluster (name + region) to reach with it.
type ClusterConfig struct {
	ClusterID  string `yaml:"cluster_id"`
	RoleARN    string `yaml:"role_arn"`
	EKSCluster string `yaml:"eks_cluster_name"`
	Region     string `yaml:"region"`
}

// Config is the top-level shape of --role-clusters-config's YAML file.
type Config struct {
	Clusters []ClusterConfig `yaml:"clusters"`
}

// LoadConfig reads a "clusters: [...]" YAML document.
func LoadConfig(r io.Reader) (Config, error) {
	var cfg Config
	if err := yaml.NewDecoder(r).Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decoding role-clusters config: %w", err)
	}
	for i, c := range cfg.Clusters {
		if c.ClusterID == "" || c.RoleARN == "" || c.EKSCluster == "" || c.Region == "" {
			return Config{}, fmt.Errorf("role-clusters config entry %d is missing a required field (cluster_id/role_arn/eks_cluster_name/region): %+v", i, c)
		}
	}
	return cfg, nil
}

// newClientFunc matches k8sclient.NewFromAWSRole's signature — a field
// (not a direct call) so tests can inject a fake and exercise Manager's
// routing/caching logic without ever touching AWS.
type newClientFunc func(ctx context.Context, roleARN, eksClusterName, region string) (*k8sclient.Client, error)

// Manager holds one transport.Handler per configured Role-based cluster,
// building each lazily on first use and caching it forever after — the
// underlying k8sclient.Client refreshes its own auth token per-request
// (see k8sclient.NewFromAWSRole), so there's no need to rebuild or expire
// the Handler itself once it exists.
type Manager struct {
	byID      map[string]ClusterConfig
	registry  *playbooks.Registry
	newClient newClientFunc

	mu       sync.Mutex
	handlers map[string]*agentcore.Handler
}

// NewManager builds a Manager for the given cluster configs, wiring every
// resulting Handler to the same playbook registry a normal cluster-agent
// would use — a Role-based cluster runs the exact same diagnose/execute/
// scan/approve_action/get_logs logic, just without a separate agent
// process or gRPC hop.
func NewManager(clusters []ClusterConfig, registry *playbooks.Registry) *Manager {
	byID := make(map[string]ClusterConfig, len(clusters))
	for _, c := range clusters {
		byID[c.ClusterID] = c
	}
	return &Manager{
		byID:      byID,
		registry:  registry,
		newClient: k8sclient.NewFromAWSRole,
		handlers:  make(map[string]*agentcore.Handler),
	}
}

// ClusterIDs lists every configured Role-based cluster — merged into
// list_clusters alongside whatever's connected over gRPC.
func (m *Manager) ClusterIDs() []string {
	ids := make([]string, 0, len(m.byID))
	for id := range m.byID {
		ids = append(ids, id)
	}
	return ids
}

// Handler returns the transport.Handler for clusterID, building (and
// caching) it on first use. ok is false when clusterID isn't configured
// here at all — a real error building the client (bad Role, unreachable
// EKS, ...) is returned in err instead, distinct from "not configured".
func (m *Manager) Handler(ctx context.Context, clusterID string) (h transport.Handler, ok bool, err error) {
	cfg, known := m.byID[clusterID]
	if !known {
		return nil, false, nil
	}

	m.mu.Lock()
	if existing, cached := m.handlers[clusterID]; cached {
		m.mu.Unlock()
		return existing, true, nil
	}
	m.mu.Unlock()

	cli, err := m.newClient(ctx, cfg.RoleARN, cfg.EKSCluster, cfg.Region)
	if err != nil {
		return nil, true, fmt.Errorf("building client for role-based cluster %s: %w", clusterID, err)
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
