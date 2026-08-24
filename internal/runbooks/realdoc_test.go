package runbooks

import "testing"

// TestRealDocument loads the actual docs/runbooks/kubernetes-errors.md
// shipped in this repo — catches a parsing regression against the real
// content, not just the small fixture in runbooks_test.go.
func TestRealDocument(t *testing.T) {
	s, err := NewStore("../../docs/runbooks/kubernetes-errors.md")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := s.Len(); got < 15 {
		t.Fatalf("Len() = %d, want at least 15 real entries", got)
	}

	for _, kind := range []string{
		"PodCrashLoopBackOff", "PodOOMKilled", "PodImagePullBackOff", "PodExecFormatError",
		"PodPending", "NodeNotReady", "CalicoNodeDegraded", "KEDAScaledObjectStuck",
	} {
		if _, ok := s.Find(kind, ""); !ok {
			t.Errorf("real document has no entry for kind %q", kind)
		}
	}

	if _, ok := s.Find("", "formato"); ok {
		t.Error("the \"## Formato\" section leaked into the real document's searchable entries")
	}
}
