package transport

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/chwiee/k8s-ts-mcp/internal/registry"
	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

// newReplica starts a fully wired hub-server replica (AgentService +
// InternalService) over an in-memory bufconn listener identified by name,
// sharing reg with any other replica built the same way — this is the
// scenario a real multi-pod hub-server deployment faces: one shared
// Registry, N independent processes, each only holding the agent
// connections that happened to dial it.
//
// SelfAddr is set to "passthrough:///"+name: grpc's default resolver tries
// real DNS for a bare hostname like "replica-a" and fails before the
// dialer even runs, so tests need the passthrough scheme exactly like
// production's literal pod-IP addresses skip DNS resolution naturally.
func newReplica(t *testing.T, reg registry.Registry, name string, dial func(context.Context, string) (net.Conn, error)) *Server {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	hub := NewServer()
	hub.Registry = reg
	hub.SelfAddr = "passthrough:///" + name
	hub.PeerCreds = insecure.NewCredentials()
	hub.PeerDialOpts = []grpc.DialOption{grpc.WithContextDialer(dial)}
	hub.TTL = time.Minute
	hub.HeartbeatInterval = 10 * time.Millisecond

	pb.RegisterAgentServiceServer(grpcServer, hub)
	pb.RegisterInternalServiceServer(grpcServer, hub.InternalHandler())
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	// Register this replica's own listener as reachable at name — the same
	// dial func peers use to forward to it must also work for connecting
	// an agent directly to it in tests below.
	replicaListeners[name] = lis
	t.Cleanup(func() { delete(replicaListeners, name) })

	return hub
}

// replicaListeners backs the shared dial func every replica in a test uses:
// a peer-to-peer or agent-to-replica dial by address just looks up the
// matching bufconn.Listener here. Fine for tests (single goroutine setup
// per test), never used outside _test.go.
var replicaListeners = map[string]*bufconn.Listener{}

func dialByReplicaAddr(_ context.Context, addr string) (net.Conn, error) {
	lis, ok := replicaListeners[addr]
	if !ok {
		return nil, fmt.Errorf("no test listener registered for peer address %q", addr)
	}
	return lis.Dial()
}

func TestServer_ForwardsDiagnoseToPeerReplica(t *testing.T) {
	reg := registry.NewInMemory()
	hubA := newReplica(t, reg, "replica-a", dialByReplicaAddr)
	hubB := newReplica(t, reg, "replica-b", dialByReplicaAddr)

	// The agent connects only to replica A.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := NewClient("passthrough:///replica-a", "spoke-1", "test", insecure.NewCredentials(), fakeHandler{},
		grpc.WithContextDialer(dialByReplicaAddr))
	go func() { _ = client.Run(ctx) }()

	waitForRegistryEntry(t, reg, "spoke-1")

	// Asking replica B — which has no local connection for spoke-1 — must
	// transparently forward to replica A over InternalService and return
	// the real result, not ErrClusterNotConnected.
	resp, err := hubB.Diagnose(context.Background(), "spoke-1", &pb.Signal{Kind: "PodCrashLoopBackOff", Name: "nginx-abc"})
	if err != nil {
		t.Fatalf("Diagnose via peer replica: %v", err)
	}
	if resp.PlaybookId != "core/crashloopbackoff" {
		t.Errorf("PlaybookId = %q, want core/crashloopbackoff", resp.PlaybookId)
	}

	// And directly on replica A (the one actually holding the connection)
	// it must still work exactly as before, without any forwarding.
	resp, err = hubA.Diagnose(context.Background(), "spoke-1", &pb.Signal{Kind: "PodCrashLoopBackOff", Name: "nginx-abc"})
	if err != nil {
		t.Fatalf("Diagnose via owning replica: %v", err)
	}
	if resp.PlaybookId != "core/crashloopbackoff" {
		t.Errorf("PlaybookId = %q, want core/crashloopbackoff", resp.PlaybookId)
	}
}

func TestServer_ForwardsExecuteToPeerReplica(t *testing.T) {
	reg := registry.NewInMemory()
	newReplica(t, reg, "replica-a", dialByReplicaAddr)
	hubB := newReplica(t, reg, "replica-b", dialByReplicaAddr)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := NewClient("passthrough:///replica-a", "spoke-1", "test", insecure.NewCredentials(), fakeHandler{},
		grpc.WithContextDialer(dialByReplicaAddr))
	go func() { _ = client.Run(ctx) }()

	waitForRegistryEntry(t, reg, "spoke-1")

	resp, err := hubB.Execute(context.Background(), "spoke-1", "core/crashloopbackoff", &pb.Signal{Kind: "PodCrashLoopBackOff", Name: "nginx-abc"})
	if err != nil {
		t.Fatalf("Execute via peer replica: %v", err)
	}
	if !resp.Resolved {
		t.Errorf("Resolved = false, want true")
	}
}

func TestServer_ConnectedClusters_FleetWideAcrossReplicas(t *testing.T) {
	reg := registry.NewInMemory()
	newReplica(t, reg, "replica-a", dialByReplicaAddr)
	hubB := newReplica(t, reg, "replica-b", dialByReplicaAddr)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := NewClient("passthrough:///replica-a", "spoke-1", "test", insecure.NewCredentials(), fakeHandler{},
		grpc.WithContextDialer(dialByReplicaAddr))
	go func() { _ = client.Run(ctx) }()

	waitForRegistryEntry(t, reg, "spoke-1")

	// hubB never had spoke-1's connection, but ConnectedClusters must still
	// report it — that's the whole point of a shared registry over
	// LocalClusters.
	got, err := hubB.ConnectedClusters(context.Background())
	if err != nil {
		t.Fatalf("ConnectedClusters: %v", err)
	}
	found := false
	for _, id := range got {
		if id == "spoke-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("ConnectedClusters() from replica B = %v, want it to include spoke-1 (connected to replica A)", got)
	}
}

func TestServer_ForwardStillFailsForTrulyUnknownCluster(t *testing.T) {
	reg := registry.NewInMemory()
	hubB := newReplica(t, reg, "replica-b", dialByReplicaAddr)

	_, err := hubB.Diagnose(context.Background(), "does-not-exist-anywhere", &pb.Signal{})
	if _, ok := err.(ErrClusterNotConnected); !ok {
		t.Errorf("err = %v (%T), want ErrClusterNotConnected", err, err)
	}
}

func waitForRegistryEntry(t *testing.T, reg registry.Registry, clusterID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _ := reg.Lookup(context.Background(), clusterID); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cluster %q never showed up in the shared registry", clusterID)
}
