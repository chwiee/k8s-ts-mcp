package audit

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/chwiee/k8s-ts-mcp/internal/policy"
)

func TestFileStoreAppendAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	e1 := Entry{ID: NewID(), ClusterID: "cluster-a", PlaybookID: "core/crashloopbackoff", Decision: policy.Decision{Allow: true}}
	e2 := Entry{ID: NewID(), ClusterID: "cluster-b", PlaybookID: "keda/scaledobject-stuck", Decision: policy.Decision{Allow: false}}

	if err := s.Append(ctx, e1); err != nil {
		t.Fatalf("Append e1: %v", err)
	}
	if err := s.Append(ctx, e2); err != nil {
		t.Fatalf("Append e2: %v", err)
	}

	got, ok, err := s.Get(ctx, e1.ID)
	if err != nil || !ok {
		t.Fatalf("Get(%q) = %v, %v, %v", e1.ID, got, ok, err)
	}
	if got.PlaybookID != e1.PlaybookID {
		t.Errorf("Get returned PlaybookID = %q, want %q", got.PlaybookID, e1.PlaybookID)
	}

	list, err := s.ListByCluster(ctx, "cluster-a")
	if err != nil {
		t.Fatalf("ListByCluster: %v", err)
	}
	if len(list) != 1 || list[0].ID != e1.ID {
		t.Errorf("ListByCluster(cluster-a) = %v, want just e1", list)
	}

	if _, ok, err := s.Get(ctx, "does-not-exist"); err != nil || ok {
		t.Errorf("Get(missing) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}
