package transport

import (
	"context"

	pb "github.com/chwiee/k8s-ts-mcp/internal/transport/gen/k8sts/v1"
)

// The functions below turn one Handler call into the matching *pb.XResponse
// — the same conversion Client's gRPC handlers need to send a response back
// to the hub, and what Server needs to answer a request served by a
// directly-held Handler (e.g. a Role-based cluster, see role.go) without
// any gRPC hop at all. Kept here, shared, so there's exactly one place that
// knows how a Handler's Go return values become wire messages.

func diagnoseResponse(ctx context.Context, h Handler, sig *pb.Signal) *pb.DiagnoseResponse {
	playbookID, summary, proposed, meta, noPlaybook, err := h.Diagnose(ctx, sig)
	resp := &pb.DiagnoseResponse{PlaybookId: playbookID, Summary: summary, ProposedActions: proposed, Meta: meta, NoPlaybook: noPlaybook}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}

func executeResponse(ctx context.Context, h Handler, playbookID string, sig *pb.Signal) *pb.ExecuteResponse {
	resolved, attempts, err := h.Execute(ctx, playbookID, sig)
	resp := &pb.ExecuteResponse{Resolved: resolved, Attempts: attempts}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}

func scanResponse(ctx context.Context, h Handler, namespace string) *pb.ScanResponse {
	issues, err := h.Scan(ctx, namespace)
	resp := &pb.ScanResponse{Issues: issues}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}

func approveActionResponse(ctx context.Context, h Handler, playbookID string, sig *pb.Signal, actionName string, meta map[string]string) *pb.ApproveActionResponse {
	attempt, err := h.ApproveAction(ctx, playbookID, sig, actionName, meta)
	resp := &pb.ApproveActionResponse{Attempt: attempt}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}

func getLogsResponse(ctx context.Context, h Handler, namespace, name, deployment string, tailLines int64) *pb.GetLogsResponse {
	logs, err := h.GetLogs(ctx, namespace, name, deployment, tailLines)
	resp := &pb.GetLogsResponse{Logs: logs}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}

func listNodesResponse(ctx context.Context, h Handler) *pb.ListNodesResponse {
	nodes, err := h.ListNodes(ctx)
	resp := &pb.ListNodesResponse{Nodes: nodes}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}
