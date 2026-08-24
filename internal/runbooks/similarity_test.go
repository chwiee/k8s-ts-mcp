package runbooks

import "testing"

func TestFind_FreeTextUsesSimilarityNotJustSubstring(t *testing.T) {
	s := newTestStore(t)

	// "reiniciando toda hora" shares no exact substring with the fixture's
	// keywords ("crashloopbackoff, crash loop, reiniciando") as a whole
	// phrase, but does share the token "reiniciando" — TF-IDF/cosine should
	// still surface it, proving this isn't literal substring matching.
	e, ok := s.Find("", "meu pod fica reiniciando toda hora, o que eu faço?")
	if !ok {
		t.Fatal("Find: no match, want the CrashLoopBackOff entry")
	}
	if e.Kind != "PodCrashLoopBackOff" {
		t.Errorf("Find returned kind %q, want PodCrashLoopBackOff", e.Kind)
	}
}

func TestFind_WeakMatchIsRejected(t *testing.T) {
	s := newTestStore(t)

	if _, ok := s.Find("", "qual a previsão do tempo amanhã em São Paulo"); ok {
		t.Error("Find matched something for a query with zero real overlap — minMatchScore should have rejected it")
	}
}

func TestFind_RealDocument_SimilarityMatch(t *testing.T) {
	s, err := NewStore("../../docs/runbooks/kubernetes-errors.md")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Deliberately shares real vocabulary with the entry's own keywords
	// ("pending", "cpu") — this is lexical (TF-IDF), not semantic, matching:
	// it finds shared words, not paraphrases with zero word overlap (see
	// score's doc comment).
	e, ok := s.Find("", "meu pod tá pending, acho que é falta de cpu disponível no cluster")
	if !ok {
		t.Fatal("Find: no match against the real document, want the Pending entry")
	}
	if e.Kind != "PodPending" {
		t.Errorf("Find returned kind %q, want PodPending", e.Kind)
	}
}
