// Package transport implements the bidirectional mTLS gRPC channel between
// the hub and each cluster-agent. Agents always dial out to the hub — the
// hub never opens an inbound connection to a cluster (see api/proto/k8sts/v1
// for the wire contract). This package only moves messages and tracks which
// cluster is currently connected; it has no knowledge of playbooks, policy
// or execution — cmd/hub-server and cmd/cluster-agent wire those in.
//
// A cluster's agent connection lives on exactly one hub-server replica — a
// gRPC stream can't be shared across processes. When hub-server runs as
// more than one replica, Server uses internal/registry (Redis in
// production) to find out which replica holds a given cluster's connection
// and forwards the request there over InternalService, instead of failing
// just because this particular replica didn't get that connection. See
// docs/ARCHITECTURE.md for the full design.
package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/chwiee/k8s-ts-mcp/internal/registry"
	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

const (
	defaultTTL               = 30 * time.Second
	defaultHeartbeatInterval = 10 * time.Second
)

// Server is the hub side: one gRPC service accepting every cluster-agent's
// long-lived stream, keyed by the cluster_id each agent announces in Hello.
type Server struct {
	pb.UnimplementedAgentServiceServer

	mu     sync.RWMutex
	agents map[string]*agentConn

	// Registry, when set, makes this replica announce/heartbeat every
	// cluster it holds and forward requests for clusters held by other
	// replicas. Nil means single-replica mode: no announcing, and a
	// request for a cluster not connected locally simply fails — there's
	// nowhere else to look.
	Registry registry.Registry
	// SelfAddr is this replica's own address, reachable by other replicas
	// (e.g. "10.0.1.23:7443", a pod IP) — what gets announced to Registry
	// and dialed by peers forwarding a request here.
	SelfAddr string
	// PeerCreds authenticates this replica when it dials another replica's
	// InternalService to forward a request. Required if Registry is set.
	PeerCreds credentials.TransportCredentials
	// TTL and HeartbeatInterval override the defaults (30s / 10s) — mostly
	// for tests that don't want to wait 10 real seconds for a heartbeat.
	TTL               time.Duration
	HeartbeatInterval time.Duration
	// PeerDialOpts is almost always empty in production — it exists so
	// tests can inject grpc.WithContextDialer for in-memory (bufconn)
	// peer-to-peer connections instead of a real network dial.
	PeerDialOpts []grpc.DialOption

	// RoleResolver, when set, is tried before the gRPC agent registry for
	// every cluster_id — implemented by internal/rolecluster.Manager for
	// clusters reached by the hub assuming an AWS IAM Role and talking to
	// their EKS API directly, instead of a cluster-agent dialing in. A
	// cluster_id it doesn't recognize falls through to the normal
	// agent/peer lookup unchanged. Kept as a small interface (not a direct
	// dependency on internal/rolecluster) since that package already
	// depends on this one for the Handler/Server types it wires together.
	RoleResolver RoleResolver

	peersMu sync.Mutex
	peers   map[string]*grpc.ClientConn
}

// RoleResolver resolves a cluster_id straight to a Handler, called
// in-process with no gRPC hop at all — see Server.RoleResolver.
type RoleResolver interface {
	// ClusterIDs lists every cluster_id this resolver can serve, merged
	// into Server.ConnectedClusters alongside gRPC-connected agents.
	ClusterIDs() []string
	// Handler returns clusterID's Handler. ok is false when clusterID
	// isn't one this resolver recognizes at all — the caller should fall
	// back to the gRPC agent registry in that case. A real failure to
	// build the client (bad Role, unreachable EKS, ...) comes back as a
	// non-nil err with ok still true, distinct from "not mine to serve."
	Handler(ctx context.Context, clusterID string) (h Handler, ok bool, err error)
}

func NewServer() *Server {
	return &Server{agents: make(map[string]*agentConn)}
}

// agentConn is one connected cluster-agent's stream, plus the in-flight
// request/response correlation for it.
type agentConn struct {
	clusterID string

	sendMu sync.Mutex // grpc streams require sends to be serialized
	stream pb.AgentService_SessionServer

	pendingMu sync.Mutex
	pending   map[string]chan *pb.AgentMessage // request_id -> reply

	stopHeartbeat context.CancelFunc // nil in single-replica (no Registry) mode
}

func (c *agentConn) send(msg *pb.HubMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(msg)
}

func (c *agentConn) await(requestID string) chan *pb.AgentMessage {
	ch := make(chan *pb.AgentMessage, 1)
	c.pendingMu.Lock()
	c.pending[requestID] = ch
	c.pendingMu.Unlock()
	return ch
}

func (c *agentConn) forget(requestID string) {
	c.pendingMu.Lock()
	delete(c.pending, requestID)
	c.pendingMu.Unlock()
}

func (c *agentConn) dispatch(msg *pb.AgentMessage) {
	var requestID string
	switch p := msg.Payload.(type) {
	case *pb.AgentMessage_DiagnoseResponse:
		requestID = p.DiagnoseResponse.GetRequestId()
	case *pb.AgentMessage_ExecuteResponse:
		requestID = p.ExecuteResponse.GetRequestId()
	case *pb.AgentMessage_ScanResponse:
		requestID = p.ScanResponse.GetRequestId()
	case *pb.AgentMessage_ApproveActionResponse:
		requestID = p.ApproveActionResponse.GetRequestId()
	case *pb.AgentMessage_GetLogsResponse:
		requestID = p.GetLogsResponse.GetRequestId()
	case *pb.AgentMessage_ListNodesResponse:
		requestID = p.ListNodesResponse.GetRequestId()
	default:
		return
	}
	c.pendingMu.Lock()
	ch, ok := c.pending[requestID]
	if ok {
		delete(c.pending, requestID)
	}
	c.pendingMu.Unlock()
	if ok {
		ch <- msg
	}
}

// Session implements pb.AgentServiceServer: it registers the connecting
// agent under the cluster_id from its first (Hello) message, then loops
// receiving responses until the agent disconnects.
func (s *Server) Session(stream pb.AgentService_SessionServer) error {
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receiving hello: %w", err)
	}
	hello := first.GetHello()
	if hello == nil || hello.ClusterId == "" {
		return fmt.Errorf("first message on a session must be a Hello with a cluster_id")
	}

	conn := &agentConn{clusterID: hello.ClusterId, stream: stream, pending: make(map[string]chan *pb.AgentMessage)}
	s.register(conn)
	defer s.unregister(hello.ClusterId)

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		conn.dispatch(msg)
	}
}

func (s *Server) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return defaultTTL
}

func (s *Server) heartbeatInterval() time.Duration {
	if s.HeartbeatInterval > 0 {
		return s.HeartbeatInterval
	}
	return defaultHeartbeatInterval
}

func (s *Server) register(c *agentConn) {
	s.mu.Lock()
	s.agents[c.clusterID] = c
	s.mu.Unlock()

	if s.Registry == nil {
		return
	}
	ttl := s.ttl()
	ctx, cancel := context.WithCancel(context.Background())
	c.stopHeartbeat = cancel
	if err := s.Registry.Announce(ctx, c.clusterID, s.SelfAddr, ttl); err != nil {
		log.Printf("transport: falha ao anunciar %s no registry: %v", c.clusterID, err)
	}
	go s.heartbeatLoop(ctx, c.clusterID, ttl)
}

func (s *Server) heartbeatLoop(ctx context.Context, clusterID string, ttl time.Duration) {
	ticker := time.NewTicker(s.heartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Registry.Heartbeat(ctx, clusterID, s.SelfAddr, ttl); err != nil {
				log.Printf("transport: falha no heartbeat de %s: %v", clusterID, err)
			}
		}
	}
}

func (s *Server) unregister(clusterID string) {
	s.mu.Lock()
	c, ok := s.agents[clusterID]
	delete(s.agents, clusterID)
	s.mu.Unlock()
	if ok && c.stopHeartbeat != nil {
		c.stopHeartbeat()
	}

	if s.Registry == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Registry.Forget(ctx, clusterID); err != nil {
		log.Printf("transport: falha ao remover %s do registry: %v", clusterID, err)
	}
}

func (s *Server) get(clusterID string) (*agentConn, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.agents[clusterID]
	return c, ok
}

// LocalClusters lists cluster_ids connected to this replica specifically.
func (s *Server) LocalClusters() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.agents))
	for id := range s.agents {
		out = append(out, id)
	}
	return out
}

// ConnectedClusters lists every cluster_id connected fleet-wide — backed by
// Registry when set (the correct answer in a multi-replica deployment), and
// falling back to LocalClusters when it isn't (single-replica mode) — plus
// every Role-based cluster from RoleResolver, which every replica can serve
// independently and so never needs Registry to discover.
func (s *Server) ConnectedClusters(ctx context.Context) ([]string, error) {
	var ids []string
	if s.Registry == nil {
		ids = s.LocalClusters()
	} else {
		agentIDs, err := s.Registry.List(ctx)
		if err != nil {
			return nil, err
		}
		ids = agentIDs
	}
	if s.RoleResolver != nil {
		ids = append(ids, s.RoleResolver.ClusterIDs()...)
	}
	return ids, nil
}

// tryRole checks RoleResolver for clusterID before the caller falls back to
// the gRPC agent registry. found is false whenever there's nothing to try
// here (no RoleResolver configured, or it doesn't recognize clusterID) —
// the normal "keep going, check the agent registry instead" case, not an
// error.
func (s *Server) tryRole(ctx context.Context, clusterID string) (h Handler, found bool, err error) {
	if s.RoleResolver == nil {
		return nil, false, nil
	}
	return s.RoleResolver.Handler(ctx, clusterID)
}

// ErrClusterNotConnected is returned by Diagnose/Execute when no agent for
// the requested cluster currently has a live session anywhere in the fleet.
type ErrClusterNotConnected string

func (e ErrClusterNotConnected) Error() string {
	return fmt.Sprintf("cluster %q não está conectado ao hub agora", string(e))
}

// Diagnose asks the given cluster's agent to diagnose sig, blocking until it
// replies or ctx is done. If the cluster isn't connected to this replica, it
// is looked up in Registry and the request forwarded to whichever replica
// does have it.
func (s *Server) Diagnose(ctx context.Context, clusterID string, sig *pb.Signal) (*pb.DiagnoseResponse, error) {
	if h, found, err := s.tryRole(ctx, clusterID); found {
		if err != nil {
			return nil, err
		}
		return diagnoseResponse(ctx, h, sig), nil
	}
	if conn, ok := s.get(clusterID); ok {
		return s.diagnoseLocal(ctx, conn, sig)
	}
	peer, err := s.peerFor(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return peer.Diagnose(ctx, &pb.DiagnoseRequest{RequestId: newRequestID(), Signal: sig, ClusterId: clusterID})
}

// Execute asks the given cluster's agent to run playbookID's escalation
// ladder for real, blocking until it replies or ctx is done — forwarded to
// another replica the same way as Diagnose when needed. Callers must
// already have checked policy — Execute has no opinion on whether this
// should have been allowed.
func (s *Server) Execute(ctx context.Context, clusterID, playbookID string, sig *pb.Signal) (*pb.ExecuteResponse, error) {
	if h, found, err := s.tryRole(ctx, clusterID); found {
		if err != nil {
			return nil, err
		}
		return executeResponse(ctx, h, playbookID, sig), nil
	}
	if conn, ok := s.get(clusterID); ok {
		return s.executeLocal(ctx, conn, playbookID, sig)
	}
	peer, err := s.peerFor(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return peer.Execute(ctx, &pb.ExecuteRequest{RequestId: newRequestID(), PlaybookId: playbookID, Signal: sig, ClusterId: clusterID})
}

// Scan asks the given cluster's agent what's currently unhealthy, blocking
// until it replies or ctx is done — forwarded to another replica the same
// way as Diagnose/Execute when needed.
func (s *Server) Scan(ctx context.Context, clusterID, namespace string) (*pb.ScanResponse, error) {
	if h, found, err := s.tryRole(ctx, clusterID); found {
		if err != nil {
			return nil, err
		}
		return scanResponse(ctx, h, namespace), nil
	}
	if conn, ok := s.get(clusterID); ok {
		return s.scanLocal(ctx, conn, namespace)
	}
	peer, err := s.peerFor(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return peer.Scan(ctx, &pb.ScanRequest{RequestId: newRequestID(), Namespace: namespace, ClusterId: clusterID})
}

// GetLogs asks the given cluster's agent for a component's recent log
// output — the signal's own pod (namespace/name) by default, or the
// current pod under deployment when deployment is set — forwarded to
// another replica the same way as Diagnose/Execute/Scan when needed.
func (s *Server) GetLogs(ctx context.Context, clusterID, namespace, name, deployment string, tailLines int64) (*pb.GetLogsResponse, error) {
	if h, found, err := s.tryRole(ctx, clusterID); found {
		if err != nil {
			return nil, err
		}
		return getLogsResponse(ctx, h, namespace, name, deployment, tailLines), nil
	}
	if conn, ok := s.get(clusterID); ok {
		return s.getLogsLocal(ctx, conn, namespace, name, deployment, tailLines)
	}
	peer, err := s.peerFor(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return peer.GetLogs(ctx, &pb.GetLogsRequest{RequestId: newRequestID(), Namespace: namespace, Name: name, Deployment: deployment, TailLines: tailLines, ClusterId: clusterID})
}

// ApproveAction asks the given cluster's agent to run exactly one named
// action from playbookID's ladder for real, blocking until it replies or
// ctx is done — forwarded to another replica the same way as
// Diagnose/Execute/Scan when needed. Callers must already have verified the
// approval is authorized (see internal/mcptools's approve_action tool) —
// ApproveAction has no opinion on who's asking, only the agent-side ladder
// re-derivation guards against running something that isn't actually
// proposed for the signal's current state.
func (s *Server) ApproveAction(ctx context.Context, clusterID, playbookID string, sig *pb.Signal, actionName string, meta map[string]string) (*pb.ApproveActionResponse, error) {
	if h, found, err := s.tryRole(ctx, clusterID); found {
		if err != nil {
			return nil, err
		}
		return approveActionResponse(ctx, h, playbookID, sig, actionName, meta), nil
	}
	if conn, ok := s.get(clusterID); ok {
		return s.approveActionLocal(ctx, conn, playbookID, sig, actionName, meta)
	}
	peer, err := s.peerFor(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return peer.ApproveAction(ctx, &pb.ApproveActionRequest{RequestId: newRequestID(), PlaybookId: playbookID, Signal: sig, ActionName: actionName, ClusterId: clusterID, Meta: meta})
}

// ListNodes asks the given cluster's agent for every node it has, blocking
// until it replies or ctx is done — forwarded to another replica the same
// way as Diagnose/Execute/Scan when needed.
func (s *Server) ListNodes(ctx context.Context, clusterID string) (*pb.ListNodesResponse, error) {
	if h, found, err := s.tryRole(ctx, clusterID); found {
		if err != nil {
			return nil, err
		}
		return listNodesResponse(ctx, h), nil
	}
	if conn, ok := s.get(clusterID); ok {
		return s.listNodesLocal(ctx, conn)
	}
	peer, err := s.peerFor(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return peer.ListNodes(ctx, &pb.ListNodesRequest{RequestId: newRequestID(), ClusterId: clusterID})
}

func (s *Server) diagnoseLocal(ctx context.Context, conn *agentConn, sig *pb.Signal) (*pb.DiagnoseResponse, error) {
	reqID := newRequestID()
	ch := conn.await(reqID)
	defer conn.forget(reqID)

	if err := conn.send(&pb.HubMessage{Payload: &pb.HubMessage_DiagnoseRequest{
		DiagnoseRequest: &pb.DiagnoseRequest{RequestId: reqID, Signal: sig},
	}}); err != nil {
		return nil, fmt.Errorf("sending diagnose request to %s: %w", conn.clusterID, err)
	}

	select {
	case msg := <-ch:
		resp := msg.GetDiagnoseResponse()
		if resp == nil {
			return nil, fmt.Errorf("cluster %s sent an unexpected reply to a diagnose request", conn.clusterID)
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) executeLocal(ctx context.Context, conn *agentConn, playbookID string, sig *pb.Signal) (*pb.ExecuteResponse, error) {
	reqID := newRequestID()
	ch := conn.await(reqID)
	defer conn.forget(reqID)

	if err := conn.send(&pb.HubMessage{Payload: &pb.HubMessage_ExecuteRequest{
		ExecuteRequest: &pb.ExecuteRequest{RequestId: reqID, PlaybookId: playbookID, Signal: sig},
	}}); err != nil {
		return nil, fmt.Errorf("sending execute request to %s: %w", conn.clusterID, err)
	}

	select {
	case msg := <-ch:
		resp := msg.GetExecuteResponse()
		if resp == nil {
			return nil, fmt.Errorf("cluster %s sent an unexpected reply to an execute request", conn.clusterID)
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) scanLocal(ctx context.Context, conn *agentConn, namespace string) (*pb.ScanResponse, error) {
	reqID := newRequestID()
	ch := conn.await(reqID)
	defer conn.forget(reqID)

	if err := conn.send(&pb.HubMessage{Payload: &pb.HubMessage_ScanRequest{
		ScanRequest: &pb.ScanRequest{RequestId: reqID, Namespace: namespace},
	}}); err != nil {
		return nil, fmt.Errorf("sending scan request to %s: %w", conn.clusterID, err)
	}

	select {
	case msg := <-ch:
		resp := msg.GetScanResponse()
		if resp == nil {
			return nil, fmt.Errorf("cluster %s sent an unexpected reply to a scan request", conn.clusterID)
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) approveActionLocal(ctx context.Context, conn *agentConn, playbookID string, sig *pb.Signal, actionName string, meta map[string]string) (*pb.ApproveActionResponse, error) {
	reqID := newRequestID()
	ch := conn.await(reqID)
	defer conn.forget(reqID)

	if err := conn.send(&pb.HubMessage{Payload: &pb.HubMessage_ApproveActionRequest{
		ApproveActionRequest: &pb.ApproveActionRequest{RequestId: reqID, PlaybookId: playbookID, Signal: sig, ActionName: actionName, Meta: meta},
	}}); err != nil {
		return nil, fmt.Errorf("sending approve_action request to %s: %w", conn.clusterID, err)
	}

	select {
	case msg := <-ch:
		resp := msg.GetApproveActionResponse()
		if resp == nil {
			return nil, fmt.Errorf("cluster %s sent an unexpected reply to an approve_action request", conn.clusterID)
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) getLogsLocal(ctx context.Context, conn *agentConn, namespace, name, deployment string, tailLines int64) (*pb.GetLogsResponse, error) {
	reqID := newRequestID()
	ch := conn.await(reqID)
	defer conn.forget(reqID)

	if err := conn.send(&pb.HubMessage{Payload: &pb.HubMessage_GetLogsRequest{
		GetLogsRequest: &pb.GetLogsRequest{RequestId: reqID, Namespace: namespace, Name: name, Deployment: deployment, TailLines: tailLines},
	}}); err != nil {
		return nil, fmt.Errorf("sending get_logs request to %s: %w", conn.clusterID, err)
	}

	select {
	case msg := <-ch:
		resp := msg.GetGetLogsResponse()
		if resp == nil {
			return nil, fmt.Errorf("cluster %s sent an unexpected reply to a get_logs request", conn.clusterID)
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) listNodesLocal(ctx context.Context, conn *agentConn) (*pb.ListNodesResponse, error) {
	reqID := newRequestID()
	ch := conn.await(reqID)
	defer conn.forget(reqID)

	if err := conn.send(&pb.HubMessage{Payload: &pb.HubMessage_ListNodesRequest{
		ListNodesRequest: &pb.ListNodesRequest{RequestId: reqID},
	}}); err != nil {
		return nil, fmt.Errorf("sending list_nodes request to %s: %w", conn.clusterID, err)
	}

	select {
	case msg := <-ch:
		resp := msg.GetListNodesResponse()
		if resp == nil {
			return nil, fmt.Errorf("cluster %s sent an unexpected reply to a list_nodes request", conn.clusterID)
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// peerFor resolves clusterID to another replica via Registry and returns a
// client for that replica's InternalService. Returns ErrClusterNotConnected
// if Registry doesn't know about clusterID either.
func (s *Server) peerFor(ctx context.Context, clusterID string) (pb.InternalServiceClient, error) {
	if s.Registry == nil {
		return nil, ErrClusterNotConnected(clusterID)
	}
	addr, ok, err := s.Registry.Lookup(ctx, clusterID)
	if err != nil {
		return nil, fmt.Errorf("looking up cluster %s in registry: %w", clusterID, err)
	}
	if !ok {
		return nil, ErrClusterNotConnected(clusterID)
	}
	conn, err := s.peerConn(addr)
	if err != nil {
		return nil, fmt.Errorf("dialing peer replica %s for cluster %s: %w", addr, clusterID, err)
	}
	return pb.NewInternalServiceClient(conn), nil
}

func (s *Server) peerConn(addr string) (*grpc.ClientConn, error) {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()

	if conn, ok := s.peers[addr]; ok {
		return conn, nil
	}
	if s.PeerCreds == nil {
		return nil, fmt.Errorf("no PeerCreds configured for replica-to-replica forwarding")
	}
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(s.PeerCreds)}, s.PeerDialOpts...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, err
	}
	if s.peers == nil {
		s.peers = make(map[string]*grpc.ClientConn)
	}
	s.peers[addr] = conn
	return conn, nil
}

// internalServiceServer implements pb.InternalServiceServer by only ever
// looking at this replica's own local connections — if the cluster isn't
// here, the caller (another replica, via Server.Diagnose/Execute) picked
// the wrong peer, which Registry shouldn't let happen; this returns
// ErrClusterNotConnected rather than forwarding again, so a stale registry
// entry fails fast instead of looping.
type internalServiceServer struct {
	pb.UnimplementedInternalServiceServer
	s *Server
}

// InternalHandler returns the pb.InternalServiceServer implementation for
// this Server, for cmd/hub-server to register on its gRPC server alongside
// AgentService.
func (s *Server) InternalHandler() pb.InternalServiceServer {
	return &internalServiceServer{s: s}
}

func (h *internalServiceServer) Diagnose(ctx context.Context, req *pb.DiagnoseRequest) (*pb.DiagnoseResponse, error) {
	conn, ok := h.s.get(req.ClusterId)
	if !ok {
		return nil, ErrClusterNotConnected(req.ClusterId)
	}
	return h.s.diagnoseLocal(ctx, conn, req.Signal)
}

func (h *internalServiceServer) Execute(ctx context.Context, req *pb.ExecuteRequest) (*pb.ExecuteResponse, error) {
	conn, ok := h.s.get(req.ClusterId)
	if !ok {
		return nil, ErrClusterNotConnected(req.ClusterId)
	}
	return h.s.executeLocal(ctx, conn, req.PlaybookId, req.Signal)
}

func (h *internalServiceServer) Scan(ctx context.Context, req *pb.ScanRequest) (*pb.ScanResponse, error) {
	conn, ok := h.s.get(req.ClusterId)
	if !ok {
		return nil, ErrClusterNotConnected(req.ClusterId)
	}
	return h.s.scanLocal(ctx, conn, req.Namespace)
}

func (h *internalServiceServer) ApproveAction(ctx context.Context, req *pb.ApproveActionRequest) (*pb.ApproveActionResponse, error) {
	conn, ok := h.s.get(req.ClusterId)
	if !ok {
		return nil, ErrClusterNotConnected(req.ClusterId)
	}
	return h.s.approveActionLocal(ctx, conn, req.PlaybookId, req.Signal, req.ActionName, req.Meta)
}

func (h *internalServiceServer) GetLogs(ctx context.Context, req *pb.GetLogsRequest) (*pb.GetLogsResponse, error) {
	conn, ok := h.s.get(req.ClusterId)
	if !ok {
		return nil, ErrClusterNotConnected(req.ClusterId)
	}
	return h.s.getLogsLocal(ctx, conn, req.Namespace, req.Name, req.Deployment, req.TailLines)
}

func (h *internalServiceServer) ListNodes(ctx context.Context, req *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	conn, ok := h.s.get(req.ClusterId)
	if !ok {
		return nil, ErrClusterNotConnected(req.ClusterId)
	}
	return h.s.listNodesLocal(ctx, conn)
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
