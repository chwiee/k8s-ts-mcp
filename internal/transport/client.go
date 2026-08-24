package transport

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

// Handler answers the two request types a hub can send down the stream.
// cmd/cluster-agent implements this by calling into internal/playbooks and
// internal/execengine — transport itself has no knowledge of either.
type Handler interface {
	// Diagnose's noPlaybook return is true when no compiled playbook
	// matched the signal at all — not an error, the caller falls back to
	// its own runbook knowledge base for this case. meta is playbook-
	// specific context (e.g. the owning Deployment name) the hub should
	// persist and pass back unchanged in a later ApproveActionRequest — see
	// internal/playbooks.Recheckable.
	Diagnose(ctx context.Context, sig *pb.Signal) (playbookID, summary string, proposedActions []*pb.ProposedAction, meta map[string]string, noPlaybook bool, err error)
	Execute(ctx context.Context, playbookID string, sig *pb.Signal) (resolved bool, attempts []*pb.Attempt, err error)
	Scan(ctx context.Context, namespace string) (issues []*pb.PodIssue, err error)
	// ApproveAction runs exactly one named action from playbookID's ladder
	// for sig — the only path by which a policy.RiskHigh action ever
	// actually executes. The hub only sends this after a human has
	// explicitly approved that one action on a real, already-diagnosed
	// incident (see internal/mcptools's approve_action tool). meta is
	// whatever Diagnose returned for that same incident.
	ApproveAction(ctx context.Context, playbookID string, sig *pb.Signal, actionName string, meta map[string]string) (attempt *pb.Attempt, err error)
	// GetLogs reads recent log output for namespace/name, or for the
	// current pod under deployment when deployment is non-empty (name is
	// ignored in that case) — see internal/runbooks' log_source field.
	GetLogs(ctx context.Context, namespace, name, deployment string, tailLines int64) (logs string, err error)
	// ListNodes returns every node in the cluster.
	ListNodes(ctx context.Context) (nodes []*pb.NodeInfo, err error)
}

// Client is the agent side: dials the hub, announces its cluster_id, and
// serves Handler requests until ctx is cancelled — reconnecting with
// backoff any time the connection drops, since the hub restarting (or a
// network blip) is a routine event, not a fatal one.
type Client struct {
	addr      string
	clusterID string
	version   string
	creds     credentials.TransportCredentials
	handler   Handler
	dialOpts  []grpc.DialOption
}

// NewClient builds a Client. creds is the agent's mTLS client credentials
// for dialing the hub. extraDialOpts is almost always empty in production —
// it exists so tests can inject grpc.WithContextDialer for an in-memory
// (bufconn) listener instead of a real network dial.
func NewClient(addr, clusterID, version string, creds credentials.TransportCredentials, handler Handler, extraDialOpts ...grpc.DialOption) *Client {
	return &Client{addr: addr, clusterID: clusterID, version: version, creds: creds, handler: handler, dialOpts: extraDialOpts}
}

// Run connects and serves until ctx is done, reconnecting with exponential
// backoff (capped at 30s) on every disconnect. It only returns when ctx is
// cancelled.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		start := time.Now()
		err := c.runOnce(ctx)
		if err != nil {
			log.Printf("transport: sessão com o hub caiu (%v), reconectando em %s", err, backoff)
		}
		// A connection that stayed up a while was healthy — don't let a
		// single drop after hours of uptime reconnect at whatever backoff
		// years of prior flapping left behind.
		if time.Since(start) > maxBackoff {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *Client) runOnce(ctx context.Context) error {
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(c.creds)}, c.dialOpts...)
	conn, err := grpc.NewClient(c.addr, opts...)
	if err != nil {
		return fmt.Errorf("dialing hub at %s: %w", c.addr, err)
	}
	defer conn.Close()

	stream, err := pb.NewAgentServiceClient(conn).Session(ctx)
	if err != nil {
		return fmt.Errorf("opening session: %w", err)
	}

	var sendMu sync.Mutex
	send := func(msg *pb.AgentMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(msg)
	}

	if err := send(&pb.AgentMessage{Payload: &pb.AgentMessage_Hello{
		Hello: &pb.Hello{ClusterId: c.clusterID, AgentVersion: c.version},
	}}); err != nil {
		return fmt.Errorf("sending hello: %w", err)
	}
	log.Printf("transport: conectado ao hub como cluster_id=%s", c.clusterID)

	for {
		msg, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("receiving from hub: %w", err)
		}
		switch p := msg.Payload.(type) {
		case *pb.HubMessage_DiagnoseRequest:
			go c.handleDiagnose(ctx, send, p.DiagnoseRequest)
		case *pb.HubMessage_ExecuteRequest:
			go c.handleExecute(ctx, send, p.ExecuteRequest)
		case *pb.HubMessage_ScanRequest:
			go c.handleScan(ctx, send, p.ScanRequest)
		case *pb.HubMessage_ApproveActionRequest:
			go c.handleApproveAction(ctx, send, p.ApproveActionRequest)
		case *pb.HubMessage_GetLogsRequest:
			go c.handleGetLogs(ctx, send, p.GetLogsRequest)
		case *pb.HubMessage_ListNodesRequest:
			go c.handleListNodes(ctx, send, p.ListNodesRequest)
		}
	}
}

func (c *Client) handleDiagnose(ctx context.Context, send func(*pb.AgentMessage) error, req *pb.DiagnoseRequest) {
	resp := diagnoseResponse(ctx, c.handler, req.Signal)
	resp.RequestId = req.RequestId
	if err := send(&pb.AgentMessage{Payload: &pb.AgentMessage_DiagnoseResponse{DiagnoseResponse: resp}}); err != nil {
		log.Printf("transport: falha ao enviar diagnose response %s: %v", req.RequestId, err)
	}
}

func (c *Client) handleExecute(ctx context.Context, send func(*pb.AgentMessage) error, req *pb.ExecuteRequest) {
	resp := executeResponse(ctx, c.handler, req.PlaybookId, req.Signal)
	resp.RequestId = req.RequestId
	if err := send(&pb.AgentMessage{Payload: &pb.AgentMessage_ExecuteResponse{ExecuteResponse: resp}}); err != nil {
		log.Printf("transport: falha ao enviar execute response %s: %v", req.RequestId, err)
	}
}

func (c *Client) handleScan(ctx context.Context, send func(*pb.AgentMessage) error, req *pb.ScanRequest) {
	resp := scanResponse(ctx, c.handler, req.Namespace)
	resp.RequestId = req.RequestId
	if err := send(&pb.AgentMessage{Payload: &pb.AgentMessage_ScanResponse{ScanResponse: resp}}); err != nil {
		log.Printf("transport: falha ao enviar scan response %s: %v", req.RequestId, err)
	}
}

func (c *Client) handleApproveAction(ctx context.Context, send func(*pb.AgentMessage) error, req *pb.ApproveActionRequest) {
	resp := approveActionResponse(ctx, c.handler, req.PlaybookId, req.Signal, req.ActionName, req.Meta)
	resp.RequestId = req.RequestId
	if err := send(&pb.AgentMessage{Payload: &pb.AgentMessage_ApproveActionResponse{ApproveActionResponse: resp}}); err != nil {
		log.Printf("transport: falha ao enviar approve_action response %s: %v", req.RequestId, err)
	}
}

func (c *Client) handleGetLogs(ctx context.Context, send func(*pb.AgentMessage) error, req *pb.GetLogsRequest) {
	resp := getLogsResponse(ctx, c.handler, req.Namespace, req.Name, req.Deployment, req.TailLines)
	resp.RequestId = req.RequestId
	if err := send(&pb.AgentMessage{Payload: &pb.AgentMessage_GetLogsResponse{GetLogsResponse: resp}}); err != nil {
		log.Printf("transport: falha ao enviar get_logs response %s: %v", req.RequestId, err)
	}
}

func (c *Client) handleListNodes(ctx context.Context, send func(*pb.AgentMessage) error, req *pb.ListNodesRequest) {
	resp := listNodesResponse(ctx, c.handler)
	resp.RequestId = req.RequestId
	if err := send(&pb.AgentMessage{Payload: &pb.AgentMessage_ListNodesResponse{ListNodesResponse: resp}}); err != nil {
		log.Printf("transport: falha ao enviar list_nodes response %s: %v", req.RequestId, err)
	}
}
