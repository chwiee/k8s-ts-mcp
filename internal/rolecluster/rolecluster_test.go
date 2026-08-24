package rolecluster

import (
	"context"
	"errors"
	"testing"

	"github.com/chwiee/k8s-ts-mcp/internal/inventory"
	"github.com/chwiee/k8s-ts-mcp/internal/k8sclient"
)

// fakeInventory is a minimal inventory.Lookup for tests — no YAML, no HTTP,
// just a fixed map plus an optional forced error to exercise Manager's
// error handling.
type fakeInventory struct {
	byID map[string]inventory.ClusterInfo
	err  error
}

func (f *fakeInventory) Lookup(_ context.Context, clusterID string) (inventory.ClusterInfo, bool, error) {
	if f.err != nil {
		return inventory.ClusterInfo{}, false, f.err
	}
	info, ok := f.byID[clusterID]
	return info, ok, nil
}

func TestRoleARNForAccount(t *testing.T) {
	got := roleARNForAccount("123456789012")
	want := "arn:aws:iam::123456789012:role/k8s-ts-mcp-readonly"
	if got != want {
		t.Errorf("roleARNForAccount = %q, want %q", got, want)
	}
}

func TestManager_Handler_UnknownClusterNotFound(t *testing.T) {
	m := NewManager(&fakeInventory{}, nil)
	h, ok, err := m.Handler(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if ok {
		t.Error("ok = true for a cluster_id the inventory doesn't know, want false")
	}
	if h != nil {
		t.Error("Handler != nil for an unknown cluster_id, want nil")
	}
}

func TestManager_Handler_NilInventoryNotFound(t *testing.T) {
	m := NewManager(nil, nil)
	_, ok, err := m.Handler(context.Background(), "spoke-role-1")
	if err != nil || ok {
		t.Errorf("Handler with nil Inventory = (ok=%v, err=%v), want (false, nil) — falls through to the gRPC agent path", ok, err)
	}
}

func TestManager_Handler_BuildsFromInventoryByConvention(t *testing.T) {
	inv := &fakeInventory{byID: map[string]inventory.ClusterInfo{
		"spoke-role-1": {AWSAccountID: "000000000000", Region: "us-east-1", EKSClusterName: "probe-cluster"},
	}}
	m := NewManager(inv, nil)

	var gotRoleARN, gotEKSName, gotRegion string
	m.newClient = func(ctx context.Context, roleARN, eksClusterName, region string) (*k8sclient.Client, error) {
		gotRoleARN, gotEKSName, gotRegion = roleARN, eksClusterName, region
		return &k8sclient.Client{}, nil
	}

	h, ok, err := m.Handler(context.Background(), "spoke-role-1")
	if err != nil || !ok || h == nil {
		t.Fatalf("Handler: h=%v ok=%v err=%v", h, ok, err)
	}
	if gotRoleARN != "arn:aws:iam::000000000000:role/k8s-ts-mcp-readonly" {
		t.Errorf("roleARN = %q", gotRoleARN)
	}
	if gotEKSName != "probe-cluster" {
		t.Errorf("eksClusterName = %q, want the inventory's eks_cluster_name", gotEKSName)
	}
	if gotRegion != "us-east-1" {
		t.Errorf("region = %q", gotRegion)
	}
}

func TestManager_Handler_EKSClusterNameFallsBackToClusterID(t *testing.T) {
	// No eks_cluster_name in the inventory entry — by convention, the
	// cluster_id itself is the EKS cluster name.
	inv := &fakeInventory{byID: map[string]inventory.ClusterInfo{
		"mars-prod-1": {AWSAccountID: "123456789012", Region: "us-east-1"},
	}}
	m := NewManager(inv, nil)

	var gotEKSName string
	m.newClient = func(ctx context.Context, roleARN, eksClusterName, region string) (*k8sclient.Client, error) {
		gotEKSName = eksClusterName
		return &k8sclient.Client{}, nil
	}

	if _, ok, err := m.Handler(context.Background(), "mars-prod-1"); err != nil || !ok {
		t.Fatalf("Handler: ok=%v err=%v", ok, err)
	}
	if gotEKSName != "mars-prod-1" {
		t.Errorf("eksClusterName = %q, want mars-prod-1 (falls back to cluster_id)", gotEKSName)
	}
}

func TestManager_Handler_BuildsOnceAndCaches(t *testing.T) {
	calls := 0
	inv := &fakeInventory{byID: map[string]inventory.ClusterInfo{
		"spoke-role-1": {AWSAccountID: "000000000000", Region: "us-east-1"},
	}}
	m := NewManager(inv, nil)
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
	inv := &fakeInventory{byID: map[string]inventory.ClusterInfo{
		"spoke-role-1": {AWSAccountID: "000000000000", Region: "us-east-1"},
	}}
	m := NewManager(inv, nil)
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
		t.Error("ok = false on a build error, want true — the cluster IS in the inventory, it just failed to build")
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

func TestManager_Handler_InventoryLookupError(t *testing.T) {
	inv := &fakeInventory{err: errors.New("inventory API unreachable")}
	m := NewManager(inv, nil)

	h, ok, err := m.Handler(context.Background(), "spoke-role-1")
	if err == nil {
		t.Fatal("Handler: expected the inventory lookup error to propagate")
	}
	if !ok {
		t.Error("ok = false on an inventory lookup error, want true — distinct from 'cluster unknown'")
	}
	if h != nil {
		t.Error("Handler != nil alongside a lookup error, want nil")
	}
}

func TestManager_ClusterIDs_AlwaysEmpty(t *testing.T) {
	inv := &fakeInventory{byID: map[string]inventory.ClusterInfo{
		"spoke-role-1": {AWSAccountID: "000000000000", Region: "us-east-1"},
	}}
	m := NewManager(inv, nil)
	if ids := m.ClusterIDs(); ids != nil {
		t.Errorf("ClusterIDs() = %v, want nil — discovery-by-convention has no catalog to enumerate", ids)
	}
}
