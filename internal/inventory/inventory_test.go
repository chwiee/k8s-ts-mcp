package inventory

import (
	"context"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	doc := `
clusters:
  - cluster_id: spoke-role-1
    aws_account_id: "000000000000"
    region: us-east-1
    eks_cluster_name: probe-cluster
  - cluster_id: spoke-2
    aws_account_id: "111122223333"
    region: eu-west-1
`
	cfg, err := LoadConfig(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Clusters) != 2 {
		t.Fatalf("len(Clusters) = %d, want 2", len(cfg.Clusters))
	}
	if cfg.Clusters[0].ClusterID != "spoke-role-1" || cfg.Clusters[0].Region != "us-east-1" {
		t.Errorf("Clusters[0] = %+v", cfg.Clusters[0])
	}
	// eks_cluster_name is optional (not every cluster is reached via IAM
	// Role) — spoke-2 omitting it must not be an error.
	if cfg.Clusters[1].EKSClusterName != "" {
		t.Errorf("Clusters[1].EKSClusterName = %q, want empty", cfg.Clusters[1].EKSClusterName)
	}
}

func TestLoadConfig_MissingField(t *testing.T) {
	doc := `
clusters:
  - cluster_id: spoke-role-1
    region: us-east-1
`
	_, err := LoadConfig(strings.NewReader(doc))
	if err == nil {
		t.Fatal("LoadConfig accepted an entry missing aws_account_id, want an error")
	}
}

func TestLookup(t *testing.T) {
	inv := New([]ClusterInfo{
		{ClusterID: "spoke-role-1", AWSAccountID: "000000000000", Region: "us-east-1", EKSClusterName: "probe-cluster"},
	})

	info, ok, err := inv.Lookup(context.Background(), "spoke-role-1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok {
		t.Fatal("Lookup(spoke-role-1) = not found, want found")
	}
	if info.Region != "us-east-1" || info.AWSAccountID != "000000000000" {
		t.Errorf("Lookup(spoke-role-1) = %+v", info)
	}

	if _, ok, err := inv.Lookup(context.Background(), "does-not-exist"); err != nil || ok {
		t.Errorf("Lookup(does-not-exist) = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

func TestLookup_NilInventory(t *testing.T) {
	var inv *Inventory
	if _, ok, err := inv.Lookup(context.Background(), "spoke-role-1"); err != nil || ok {
		t.Errorf("Lookup on a nil *Inventory = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}
