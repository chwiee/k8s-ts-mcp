package runbooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchAndReload_PicksUpEditedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runbooks.md")
	if err := os.WriteFile(path, []byte(testRunbookDocV1), 0o644); err != nil {
		t.Fatalf("writing initial doc: %v", err)
	}
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, ok := s.Find("PodOOMKilled", ""); ok {
		t.Fatal("v1 doc shouldn't have a PodOOMKilled entry yet")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lastErr error
	go s.WatchAndReload(ctx, 20*time.Millisecond, func(err error) { lastErr = err })

	if err := os.WriteFile(path, []byte(testRunbookDocV2), 0o644); err != nil {
		t.Fatalf("writing updated doc: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.Find("PodOOMKilled", ""); ok {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("WatchAndReload never picked up the edited file (lastErr=%v)", lastErr)
}

const testRunbookDocV1 = `# Doc

## Pod em CrashLoopBackOff
kind: PodCrashLoopBackOff
keywords: crashloopbackoff

corpo v1
`

const testRunbookDocV2 = `# Doc

## Pod em CrashLoopBackOff
kind: PodCrashLoopBackOff
keywords: crashloopbackoff

corpo v1

## Pod morto por OOM
kind: PodOOMKilled
keywords: oomkilled

corpo novo, adicionado depois do reload
`
