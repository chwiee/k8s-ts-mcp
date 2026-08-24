package registry

import (
	"context"
	"sync"
	"time"
)

// InMemory is a single-process Registry: correct for one hub-server replica
// (which is all a local/dev run or a low-scale deployment needs — see
// cmd/hub-server, it's the default when --redis-addr isn't set) but not
// shared across replicas. Use Redis for a multi-replica deployment.
type InMemory struct {
	mu      sync.RWMutex
	records map[string]inMemoryRecord
}

type inMemoryRecord struct {
	addr    string
	expires time.Time
}

func NewInMemory() *InMemory {
	return &InMemory{records: make(map[string]inMemoryRecord)}
}

func (r *InMemory) Announce(_ context.Context, clusterID, replicaAddr string, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[clusterID] = inMemoryRecord{addr: replicaAddr, expires: time.Now().Add(ttl)}
	return nil
}

func (r *InMemory) Heartbeat(ctx context.Context, clusterID, replicaAddr string, ttl time.Duration) error {
	return r.Announce(ctx, clusterID, replicaAddr, ttl)
}

func (r *InMemory) Lookup(_ context.Context, clusterID string) (string, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[clusterID]
	if !ok || time.Now().After(rec.expires) {
		return "", false, nil
	}
	return rec.addr, true, nil
}

func (r *InMemory) Forget(_ context.Context, clusterID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.records, clusterID)
	return nil
}

func (r *InMemory) List(_ context.Context) ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := time.Now()
	out := make([]string, 0, len(r.records))
	for id, rec := range r.records {
		if now.Before(rec.expires) {
			out = append(out, id)
		}
	}
	return out, nil
}
