package transport

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

// fakeHandler simulates a cluster-agent's playbook/execengine wiring
// without needing a real Kubernetes API.
type fakeHandler struct{}

func (fakeHandler) Diagnose(_ context.Context, sig *pb.Signal) (string, string, []*pb.ProposedAction, map[string]string, bool, error) {
	actions := []*pb.ProposedAction{
		{Name: "restart pod", Description: "kubectl delete pod " + sig.GetName(), Risk: "safe"},
	}
	return "core/crashloopbackoff", "pod " + sig.GetName() + " está crashando", actions, nil, false, nil
}

func (fakeHandler) Execute(_ context.Context, _ string, sig *pb.Signal) (bool, []*pb.Attempt, error) {
	return true, []*pb.Attempt{{Action: "restart pod", Output: "pod " + sig.GetName() + " deleted, recreated", Validated: true}}, nil
}

func (fakeHandler) Scan(_ context.Context, ns string) ([]*pb.PodIssue, error) {
	return []*pb.PodIssue{{Namespace: ns, Name: "nginx-abc", Kind: "PodCrashLoopBackOff", Detail: "back-off restarting failed container"}}, nil
}

func (fakeHandler) GetLogs(_ context.Context, ns, name, deployment string, _ int64) (string, error) {
	if deployment != "" {
		return "log do deployment " + deployment, nil
	}
	return "log de " + ns + "/" + name, nil
}

func (fakeHandler) ApproveAction(_ context.Context, _ string, sig *pb.Signal, actionName string, _ map[string]string) (*pb.Attempt, error) {
	return &pb.Attempt{Action: actionName, Output: "increased memory limit for " + sig.GetName(), Validated: true}, nil
}

// newTestPair starts an in-memory (bufconn) hub Server and connects one
// Client to it as cluster "spoke-1", returning the Server and a cancel func.
// It blocks until the agent has registered.
func newTestPair(t *testing.T) (*Server, context.CancelFunc) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	hub := NewServer()
	pb.RegisterAgentServiceServer(grpcServer, hub)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }

	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient("passthrough:///bufnet", "spoke-1", "test", insecure.NewCredentials(), fakeHandler{},
		grpc.WithContextDialer(dialer))

	go func() { _ = client.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := hub.get("spoke-1"); ok {
			return hub, cancel
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agent never registered with the hub")
	return nil, cancel
}

func TestDiagnose_RoundTrip(t *testing.T) {
	hub, cancel := newTestPair(t)
	defer cancel()

	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()

	resp, err := hub.Diagnose(ctx, "spoke-1", &pb.Signal{Kind: "PodCrashLoopBackOff", Namespace: "default", Name: "nginx-abc"})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if resp.PlaybookId != "core/crashloopbackoff" {
		t.Errorf("PlaybookId = %q, want core/crashloopbackoff", resp.PlaybookId)
	}
	if len(resp.ProposedActions) != 1 {
		t.Errorf("ProposedActions = %v, want 1 entry", resp.ProposedActions)
	}
}

func TestExecute_RoundTrip(t *testing.T) {
	hub, cancel := newTestPair(t)
	defer cancel()

	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()

	resp, err := hub.Execute(ctx, "spoke-1", "core/crashloopbackoff", &pb.Signal{Kind: "PodCrashLoopBackOff", Namespace: "default", Name: "nginx-abc"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !resp.Resolved {
		t.Errorf("Resolved = false, want true")
	}
	if len(resp.Attempts) != 1 || resp.Attempts[0].Action != "restart pod" {
		t.Errorf("Attempts = %v, want one 'restart pod' attempt", resp.Attempts)
	}
}

func TestDiagnose_UnknownCluster(t *testing.T) {
	hub, cancel := newTestPair(t)
	defer cancel()

	_, err := hub.Diagnose(context.Background(), "does-not-exist", &pb.Signal{})
	if err == nil {
		t.Fatal("expected an error for an unconnected cluster")
	}
	if _, ok := err.(ErrClusterNotConnected); !ok {
		t.Errorf("err = %v (%T), want ErrClusterNotConnected", err, err)
	}
}

func TestConnectedClusters_FallsBackToLocalWithoutRegistry(t *testing.T) {
	hub, cancel := newTestPair(t)
	defer cancel()

	got, err := hub.ConnectedClusters(context.Background())
	if err != nil {
		t.Fatalf("ConnectedClusters: %v", err)
	}
	if len(got) != 1 || got[0] != "spoke-1" {
		t.Errorf("ConnectedClusters() = %v, want [spoke-1]", got)
	}
}
