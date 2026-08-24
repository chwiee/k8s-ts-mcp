package policy

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Tier is the access level resolved from the caller's AD/OIDC groups.
type Tier string

const (
	TierReadOnly     Tier = "readonly"
	TierNonProdAdmin Tier = "nonprod-admin"
	TierProdAdmin    Tier = "prod-admin"
)

// RiskLevel is declared by each playbook action, not by the caller.
type RiskLevel string

const (
	RiskSafe   RiskLevel = "safe"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// ClusterEnv classifies the target cluster for policy purposes.
type ClusterEnv string

const (
	EnvProd    ClusterEnv = "prod"
	EnvNonProd ClusterEnv = "nonprod"
)

// GroupMapping maps an AD/OIDC group name to an access tier. This is
// ops-owned data, not policy logic — it is loaded from YAML (see
// deployments/hub-server/group-mapping.example.yaml) so security can
// change who's in which tier without touching policy.rego.
type GroupMapping map[string]Tier

// LoadGroupMapping reads a "groups: {ad-group-name: tier}" YAML document.
func LoadGroupMapping(r io.Reader) (GroupMapping, error) {
	var doc struct {
		Groups GroupMapping `yaml:"groups"`
	}
	if err := yaml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding group mapping: %w", err)
	}
	return doc.Groups, nil
}

// tierRank orders tiers from least to most privileged, used to pick the
// highest tier when a caller belongs to more than one AD group.
var tierRank = map[Tier]int{
	TierReadOnly:     0,
	TierNonProdAdmin: 1,
	TierProdAdmin:    2,
}

// HighestTier returns the most privileged tier among the caller's groups.
// ok is false when none of the groups map to a known tier.
func (m GroupMapping) HighestTier(groups []string) (tier Tier, ok bool) {
	best := -1
	for _, g := range groups {
		t, known := m[g]
		if !known {
			continue
		}
		if rank := tierRank[t]; rank > best {
			best = rank
			tier = t
			ok = true
		}
	}
	return tier, ok
}

// Request is one authorization question: can this caller run this action,
// tagged with this risk level, against a cluster of this environment, given
// the current global execution flag?
type Request struct {
	Groups        []string
	ClusterEnv    ClusterEnv
	ActionRisk    RiskLevel
	ExecutionFlag bool
}

// Decision is the policy engine's answer. Allow only means the action may be
// proposed/requested; DryRun independently decides whether it actually runs.
type Decision struct {
	Allow  bool   `json:"allow"`
	DryRun bool   `json:"dry_run"`
	Reason string `json:"reason"`
	Tier   Tier   `json:"-"`
}
