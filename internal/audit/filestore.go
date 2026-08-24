package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileStore is an append-only JSON-lines Store. It's the honest local/dev
// implementation of Store — durable and simple, but a single file isn't
// what should back 1000 clusters' worth of audit trail in production. Swap
// in a Postgres-backed Store (same interface) for that; nothing above this
// package needs to change.
type FileStore struct {
	mu   sync.Mutex
	path string
}

// NewFileStore opens (creating if needed) a JSONL file at path for
// appending audit entries.
func NewFileStore(path string) (*FileStore, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating audit dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening audit file %s: %w", path, err)
	}
	_ = f.Close()
	return &FileStore{path: path}, nil
}

func (s *FileStore) Append(_ context.Context, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening audit file %s: %w", s.path, err)
	}
	defer f.Close()

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshaling audit entry: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("writing audit entry: %w", err)
	}
	return nil
}

func (s *FileStore) all() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("opening audit file %s: %w", s.path, err)
	}
	defer f.Close()

	var entries []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("decoding audit entry: %w", err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading audit file %s: %w", s.path, err)
	}
	return entries, nil
}

func (s *FileStore) ListByCluster(_ context.Context, clusterID string) ([]Entry, error) {
	all, err := s.all()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(all))
	for _, e := range all {
		if e.ClusterID == clusterID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *FileStore) Get(_ context.Context, id string) (Entry, bool, error) {
	all, err := s.all()
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range all {
		if e.ID == id {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

var _ Store = (*FileStore)(nil)
