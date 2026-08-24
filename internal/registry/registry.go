// Package registry is the fleet-wide directory of which hub-server replica
// currently holds each cluster's live agent connection. internal/transport
// keeps the connection itself local to one replica (a gRPC stream can't be
// shared across processes) — this package is what lets any replica answer
// "who has cluster X right now" so it can forward the request there instead
// of failing, which is what makes running the hub as more than one replica
// actually work.
package registry

import (
	"context"
	"time"
)

// Registry is implemented by InMemory (single-replica / tests, no external
// dependency) and Redis (the real, multi-replica-safe implementation).
type Registry interface {
	// Announce records that clusterID's agent is connected to this replica
	// (replicaAddr), valid for ttl. Call Heartbeat repeatedly to keep the
	// record alive for as long as the connection stays up.
	Announce(ctx context.Context, clusterID, replicaAddr string, ttl time.Duration) error
	// Heartbeat refreshes clusterID's TTL without changing its value. If the
	// record has already expired (e.g. this replica restarted and lost the
	// in-memory connection, or Redis evicted it), Heartbeat re-announces it.
	Heartbeat(ctx context.Context, clusterID, replicaAddr string, ttl time.Duration) error
	// Lookup returns the replica address currently holding clusterID's
	// connection, if any. ok is false if no live record exists — either the
	// cluster has never connected, or its agent (and every replica that had
	// announced it) is gone and the record expired.
	Lookup(ctx context.Context, clusterID string) (replicaAddr string, ok bool, err error)
	// Forget removes clusterID's record immediately, on graceful
	// disconnect — don't wait for the TTL when we already know it's gone.
	Forget(ctx context.Context, clusterID string) error
	// List returns every cluster_id currently announced by any replica —
	// the fleet-wide view backing the "list_clusters" tool.
	List(ctx context.Context) ([]string, error)
}
