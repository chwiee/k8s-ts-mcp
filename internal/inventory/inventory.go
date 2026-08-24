// Package inventory answers "what cloud account/region is this cluster in?"
// for a cluster_id — every cluster the company runs is an EKS cluster in one
// AWS account/region, so any tool that reasons about region/account (e.g.
// list_nodes) needs an authoritative source for that instead of guessing
// from what a caller mentioned in passing. There is deliberately no
// per-cluster registration step: Lookup is called on demand ("does this
// cluster_id exist, and if so what account/region is it in?"), not read
// from a pre-enumerated catalog — see internal/rolecluster, which is the
// other half of this design (it turns a Lookup's answer into an IRSA role
// ARN by fixed naming convention, with nothing per-cluster to configure
// there either). Two implementations: the YAML-backed *Inventory below
// (local/dev, e.g. against Floci) and HTTPClient (the company's real
// inventory API).
package inventory

import (
	"context"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ClusterInfo is what's known about one cluster.
type ClusterInfo struct {
	ClusterID      string `yaml:"cluster_id"`
	AWSAccountID   string `yaml:"aws_account_id"`
	Region         string `yaml:"region"`
	EKSClusterName string `yaml:"eks_cluster_name,omitempty"`
}

// Lookup resolves a cluster_id to its ClusterInfo. found is false when the
// cluster is simply unknown — not an error, the normal outcome for a typo'd
// or nonexistent cluster_id. err is reserved for the lookup itself failing
// (the inventory API unreachable, a malformed response, ...).
type Lookup interface {
	Lookup(ctx context.Context, clusterID string) (info ClusterInfo, found bool, err error)
}

// Config is the top-level shape of --cluster-inventory-path's YAML file.
type Config struct {
	Clusters []ClusterInfo `yaml:"clusters"`
}

// LoadConfig reads a "clusters: [...]" YAML document.
func LoadConfig(r io.Reader) (Config, error) {
	var cfg Config
	if err := yaml.NewDecoder(r).Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decoding cluster inventory config: %w", err)
	}
	for i, c := range cfg.Clusters {
		if c.ClusterID == "" || c.AWSAccountID == "" || c.Region == "" {
			return Config{}, fmt.Errorf("cluster inventory entry %d is missing a required field (cluster_id/aws_account_id/region): %+v", i, c)
		}
	}
	return cfg, nil
}

// Inventory is a read-only, in-memory Lookup — meant for local/dev use (see
// docs/ARCHITECTURE.md), where there's no real inventory API to call, e.g.
// testing against Floci. Production points mcptools.Server.Inventory and
// rolecluster.Manager.Inventory at HTTPClient instead.
type Inventory struct {
	byID map[string]ClusterInfo
}

// New builds an Inventory from a list of entries — typically Config.Clusters
// loaded once at hub-server startup.
func New(clusters []ClusterInfo) *Inventory {
	byID := make(map[string]ClusterInfo, len(clusters))
	for _, c := range clusters {
		byID[c.ClusterID] = c
	}
	return &Inventory{byID: byID}
}

// Lookup implements Lookup. ctx is unused — this never does I/O.
func (inv *Inventory) Lookup(_ context.Context, clusterID string) (ClusterInfo, bool, error) {
	if inv == nil {
		return ClusterInfo{}, false, nil
	}
	info, ok := inv.byID[clusterID]
	return info, ok, nil
}

var _ Lookup = (*Inventory)(nil)
