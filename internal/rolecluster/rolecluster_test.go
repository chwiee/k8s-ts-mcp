package rolecluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
)

func TestLoadConfig(t *testing.T) {
	doc := `
clusters:
  - cluster_id: spoke-role-1
    role_arn: arn:aws:iam::111122223333:role/eks-readonly
    eks_cluster_name: prod-1
    region: us-east-1
  - cluster_id: spoke-role-2
    role_arn: arn:aws:iam::444455556666:role/eks-readonly
    eks_cluster_name: prod-2
    region: eu-west-1
`
	cfg, err := LoadConfig(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Clusters) != 2 {
		t.Fatalf("len(Clusters) = %d, want 2", len(cfg.Clusters))
	}
	if cfg.Clusters[0].ClusterID != "spoke-role-1" || cfg.Clusters[0].RoleARN != "arn:aws:iam::111122223333:role/eks-readonly" {
		t.Errorf("Clusters[0] = %+v", cfg.Clusters[0])
	}
}

func TestLoadConfig_MissingField(t *testing.T) {
	doc := `
clusters:
  - cluster_id: spoke-role-1
    role_arn: arn:aws:iam::111122223333:role/eks-readonly
    region: us-east-1
`
	_, err := LoadConfig(strings.NewReader(doc))
	if err == nil {
		t.Fatal("LoadConfig accepted an entry missing eks_cluster_name, want an error")
	}
}

func TestManager_Handler_UnknownClusterNotFound(t *testing.T) {
	m := NewManager(nil, nil)
	h, ok, err := m.Handler(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if ok {
		t.Error("ok = true for an unconfigured cluster_id, want false")
	}
	if h != nil {
		t.Error("Handler != nil for an unconfigured cluster_id, want nil")
	}
}

func TestManager_Handler_BuildsOnceAndCaches(t *testing.T) {
	calls := 0
	m := NewManager([]ClusterConfig{{ClusterID: "spoke-role-1", RoleARN: "arn:x", EKSCluster: "prod-1", Region: "us-east-1"}}, nil)
	m.newClient = func(ctx context.Context, roleARN, eksClusterName, region string) (*k8sclient.Client, error) {
		calls++
		return &k8sclient.Client{}, nil
	}

	ctx := context.Background()
	h1, ok, err := m.Handler(ctx, "spoke-role-1")
	if err != nil || !ok || h1 == nil {
		t.Fatalf("Handler (1st call): h=%v ok=%v err=%v", h1, ok, err)
	}
	h2, ok, err := m.Handler(ctx, "spoke-role-1")
	if err != nil || !ok || h2 == nil {
		t.Fatalf("Handler (2nd call): h=%v ok=%v err=%v", h2, ok, err)
	}
	if h1 != h2 {
		t.Error("Handler returned a different instance on the 2nd call, want the cached one")
	}
	if calls != 1 {
		t.Errorf("newClient called %d times, want 1 (cached after the first build)", calls)
	}
}

func TestManager_Handler_BuildErrorNotCached(t *testing.T) {
	calls := 0
	m := NewManager([]ClusterConfig{{ClusterID: "spoke-role-1", RoleARN: "arn:x", EKSCluster: "prod-1", Region: "us-east-1"}}, nil)
	m.newClient = func(ctx context.Context, roleARN, eksClusterName, region string) (*k8sclient.Client, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("sts:AssumeRole denied")
		}
		return &k8sclient.Client{}, nil
	}

	ctx := context.Background()
	h, ok, err := m.Handler(ctx, "spoke-role-1")
	if err == nil {
		t.Fatal("Handler: expected the build error to propagate")
	}
	if !ok {
		t.Error("ok = false on a build error, want true — the cluster IS configured here, it just failed to build")
	}
	if h != nil {
		t.Error("Handler != nil alongside a build error, want nil")
	}

	// A later call should retry — a transient AWS failure shouldn't
	// permanently wedge this cluster_id.
	h, ok, err = m.Handler(ctx, "spoke-role-1")
	if err != nil || !ok || h == nil {
		t.Fatalf("Handler (retry after a prior build error): h=%v ok=%v err=%v", h, ok, err)
	}
	if calls != 2 {
		t.Errorf("newClient called %d times, want 2 (failed build must not be cached)", calls)
	}
}

func TestManager_ClusterIDs(t *testing.T) {
	m := NewManager([]ClusterConfig{
		{ClusterID: "spoke-role-1", RoleARN: "arn:x", EKSCluster: "prod-1", Region: "us-east-1"},
		{ClusterID: "spoke-role-2", RoleARN: "arn:y", EKSCluster: "prod-2", Region: "eu-west-1"},
	}, nil)
	ids := m.ClusterIDs()
	if len(ids) != 2 {
		t.Fatalf("ClusterIDs() = %v, want 2 entries", ids)
	}
	want := map[string]bool{"spoke-role-1": true, "spoke-role-2": true}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected cluster id %q", id)
		}
	}
}
