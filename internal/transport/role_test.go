package transport

import (
	"context"
	"errors"
	"sort"
	"testing"

	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

// fakeRoleResolver simulates internal/rolecluster.Manager without ever
// touching AWS — Server only depends on the small RoleResolver interface,
// so a fake here is enough to prove the wiring, independent of whether
// internal/rolecluster's own build logic works (that's rolecluster_test.go's
// job).
type fakeRoleResolver struct {
	ids      []string
	handler  Handler // returned for every known cluster_id
	known    map[string]bool
	buildErr error // if set, Handler(known cluster) returns this instead
}

func (f *fakeRoleResolver) ClusterIDs() []string { return f.ids }

func (f *fakeRoleResolver) Handler(_ context.Context, clusterID string) (Handler, bool, error) {
	if !f.known[clusterID] {
		return nil, false, nil
	}
	if f.buildErr != nil {
		return nil, true, f.buildErr
	}
	return f.handler, true, nil
}

func TestServer_Diagnose_UsesRoleResolverWithoutAnyAgentConnected(t *testing.T) {
	hub := NewServer()
	hub.RoleResolver = &fakeRoleResolver{
		ids:     []string{"role-cluster-1"},
		known:   map[string]bool{"role-cluster-1": true},
		handler: fakeHandler{},
	}

	resp, err := hub.Diagnose(context.Background(), "role-cluster-1", &pb.Signal{Kind: "PodCrashLoopBackOff", Name: "nginx-abc"})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if resp.PlaybookId != "core/crashloopbackoff" {
		t.Errorf("PlaybookId = %q, want core/crashloopbackoff (from fakeHandler, called directly — no gRPC agent was ever connected)", resp.PlaybookId)
	}
}

func TestServer_Diagnose_FallsBackToAgentWhenRoleResolverDoesntKnowCluster(t *testing.T) {
	hub, cancel := newTestPair(t) // connects a real bufconn agent as "spoke-1"
	defer cancel()
	hub.RoleResolver = &fakeRoleResolver{known: map[string]bool{}} // knows nothing

	resp, err := hub.Diagnose(context.Background(), "spoke-1", &pb.Signal{Kind: "PodCrashLoopBackOff", Name: "nginx-abc"})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if resp.PlaybookId != "core/crashloopbackoff" {
		t.Errorf("PlaybookId = %q, want core/crashloopbackoff (from the gRPC-connected agent)", resp.PlaybookId)
	}
}

func TestServer_Diagnose_PropagatesRoleResolverBuildErrorWithoutFallback(t *testing.T) {
	hub, cancel := newTestPair(t) // "spoke-1" is connected via gRPC too, but must not be tried
	defer cancel()
	hub.RoleResolver = &fakeRoleResolver{
		known:    map[string]bool{"spoke-1": true},
		buildErr: errors.New("sts:AssumeRole denied"),
	}

	_, err := hub.Diagnose(context.Background(), "spoke-1", &pb.Signal{Kind: "PodCrashLoopBackOff", Name: "nginx-abc"})
	if err == nil {
		t.Fatal("Diagnose: expected the RoleResolver build error to propagate, not silently fall back to the gRPC agent")
	}
}

func TestServer_ConnectedClusters_MergesRoleResolverIDs(t *testing.T) {
	hub, cancel := newTestPair(t) // "spoke-1" connected via gRPC
	defer cancel()
	hub.RoleResolver = &fakeRoleResolver{ids: []string{"role-cluster-1", "role-cluster-2"}}

	ids, err := hub.ConnectedClusters(context.Background())
	if err != nil {
		t.Fatalf("ConnectedClusters: %v", err)
	}
	sort.Strings(ids)
	want := []string{"role-cluster-1", "role-cluster-2", "spoke-1"}
	if len(ids) != len(want) {
		t.Fatalf("ConnectedClusters() = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ConnectedClusters()[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}
