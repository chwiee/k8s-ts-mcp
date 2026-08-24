// Package runbooks loads docs/runbooks/kubernetes-errors.md (or any file
// following the same format) — human-authored guidance for problems that
// don't have a compiled playbook. This is knowledge, never automation: a
// Store only ever returns text for a caller to read, the same way a
// playbook's dry-run proposal does. Nothing in this package touches a
// cluster or executes anything.
package runbooks

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// minMatchScore is the lowest TF-IDF/cosine score Find accepts for its
// free-text fallback — below this, the match is too weak to be more useful
// than saying nothing. Chosen empirically against the real
// docs/runbooks/kubernetes-errors.md corpus (see runbooks_test.go); revisit
// if the corpus grows very differently in character.
const minMatchScore = 0.1

// Entry is one runbook: a known problem, its cause, how to confirm it, and
// how to fix it — kept as one opaque Body rather than separate fields
// because the source is meant to stay a plain markdown document a human
// edits directly, not a structured format this package dictates.
type Entry struct {
	Title    string
	Kind     string // matches playbooks.Signal.Kind's vocabulary where a formal signal exists; empty for reference-only entries
	Keywords []string
	Body     string
	// LogSignatures, when non-empty, lets an SRE teach this entry to
	// confirm itself against real log output instead of relying only on
	// kind/keyword text similarity — any one of these substrings (case-
	// insensitive) appearing in the fetched log is treated as a match. No
	// code change needed to add a new one: just edit the markdown.
	LogSignatures []string
	// LogSource says whose log to check: empty (or "self") means the
	// signal's own pod (namespace/name from the Signal being diagnosed);
	// "namespace/deployment" points at a fixed, shared component instead
	// (e.g. "keda-system/keda-operator") — the current pod under that
	// Deployment is resolved at lookup time, since its name is generated.
	LogSource string
}

// MatchesLogs reports whether logs contains any of Entry's LogSignatures
// (case-insensitive substring match) — false for an entry with no
// LogSignatures declared, since that entry has nothing to confirm against.
func (e Entry) MatchesLogs(logs string) bool {
	if len(e.LogSignatures) == 0 {
		return false
	}
	lower := strings.ToLower(logs)
	for _, sig := range e.LogSignatures {
		if strings.Contains(lower, strings.ToLower(sig)) {
			return true
		}
	}
	return false
}

// Store holds every Entry loaded from one markdown file, safe for
// concurrent use.
type Store struct {
	path string

	mu      sync.RWMutex
	entries []Entry
}

// NewStore loads path immediately, so a bad/missing file fails at startup
// rather than on the first lookup.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload re-reads the file from disk — call this to pick up an edited
// runbook without restarting the process (e.g. after a mounted ConfigMap
// updates).
func (s *Store) Reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("reading runbooks file %s: %w", s.path, err)
	}
	entries := parse(string(data))

	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
	return nil
}

// Len reports how many entries are currently loaded.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Find looks for the entry matching kind exactly first (the precise,
// intended lookup when a formal Signal.Kind exists), then falls back to
// ranking every entry by lexical similarity (TF-IDF + cosine, see
// similarity.go) to query and returning the best one — as long as it
// clears minMatchScore. ok is false only when neither approach finds
// anything good enough.
func (s *Store) Find(kind, query string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if kind != "" {
		for _, e := range s.entries {
			if e.Kind == kind {
				return e, true
			}
		}
	}

	if strings.TrimSpace(query) == "" {
		return Entry{}, false
	}
	best, bestScore, ok := score(s.entries, query)
	if !ok || bestScore < minMatchScore {
		return Entry{}, false
	}
	return best, true
}

// WatchAndReload calls Reload every interval until ctx is done — the
// automatic side of "edit the runbook doc, no restart needed" (a mounted
// ConfigMap synced by ArgoCD is the intended production source). A failed
// reload (e.g. caught mid-write) is reported via onError but never stops
// the loop — the previous good set of entries stays in place until the
// next tick succeeds.
func (s *Store) WatchAndReload(ctx context.Context, interval time.Duration, onError func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Reload(); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}

// parse splits data on level-2 ("## ") headings — one Entry per section —
// and pulls the leading "kind:"/"keywords:" metadata lines out of each
// section's body. Entries with neither kind nor keywords (e.g. the
// document's own "## Formato" explanation of this format) are dropped:
// they're not lookup-able and aren't meant to be.
func parse(data string) []Entry {
	var entries []Entry
	for _, block := range strings.Split("\n"+data, "\n## ")[1:] {
		e := parseEntry(block)
		if e.Kind == "" && len(e.Keywords) == 0 {
			continue
		}
		entries = append(entries, e)
	}
	return entries
}

func parseEntry(block string) Entry {
	lines := strings.Split(block, "\n")
	if len(lines) == 0 {
		return Entry{}
	}
	e := Entry{Title: strings.TrimSpace(lines[0])}

	i := 1
metaLoop:
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(trimmed, "kind:"):
			e.Kind = strings.TrimSpace(strings.TrimPrefix(trimmed, "kind:"))
		case strings.HasPrefix(trimmed, "keywords:"):
			for _, kw := range strings.Split(strings.TrimPrefix(trimmed, "keywords:"), ",") {
				if kw = strings.TrimSpace(kw); kw != "" {
					e.Keywords = append(e.Keywords, kw)
				}
			}
		case strings.HasPrefix(trimmed, "log_signatures:"):
			for _, sig := range strings.Split(strings.TrimPrefix(trimmed, "log_signatures:"), ",") {
				if sig = strings.TrimSpace(sig); sig != "" {
					e.LogSignatures = append(e.LogSignatures, sig)
				}
			}
		case strings.HasPrefix(trimmed, "log_source:"):
			e.LogSource = strings.TrimSpace(strings.TrimPrefix(trimmed, "log_source:"))
		case trimmed == "":
			// blank line between/after metadata fields — keep scanning
		default:
			break metaLoop
		}
		i++
	}
	// The "---" separator before the next "## " heading lands inside this
	// block (it's everything up to, not including, that next heading) —
	// strip it so it doesn't leak into every entry's Body.
	body := strings.TrimSpace(strings.Join(lines[i:], "\n"))
	body = strings.TrimSpace(strings.TrimSuffix(body, "---"))
	e.Body = body
	return e
}
