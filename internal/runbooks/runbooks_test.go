package runbooks

import (
	"os"
	"strings"
	"testing"
)

const testDoc = `# Catálogo de teste

Texto de introdução, ignorado pelo parser.

## Formato

Esse é o exemplo de formato, sem kind nem keywords — não deve virar uma entrada buscável.

---

## Pod em CrashLoopBackOff (causas gerais)
kind: PodCrashLoopBackOff
keywords: crashloopbackoff, crash loop, reiniciando

**Causa comum**: o container morre e reinicia repetidamente.

**Diagnóstico**: kubectl logs --previous

**Solução**: depende do exit code.

---

## Nó do Calico degradado
kind: CalicoNodeDegraded
keywords: calico, bgp

**Causa comum**: BGP não estabelecido.

**Solução**: reiniciar o calico-node do nó.

---

## KEDA não escalando
kind: KEDAScalingStuck
keywords: keda, scaledobject, escalonamento
log_signatures: failed to get scaler, error getting scale target
log_source: keda-system/keda-operator

**Causa comum**: o keda-operator não conseguiu falar com a métrica externa.

**Solução**: verifique credenciais do TriggerAuthentication.
`

func TestParse(t *testing.T) {
	entries := parse(testDoc)
	if len(entries) != 3 {
		t.Fatalf("parse() = %d entries, want 3 (Formato must be dropped): %+v", len(entries), entries)
	}

	crash := entries[0]
	if crash.Title != "Pod em CrashLoopBackOff (causas gerais)" {
		t.Errorf("entries[0].Title = %q", crash.Title)
	}
	if crash.Kind != "PodCrashLoopBackOff" {
		t.Errorf("entries[0].Kind = %q", crash.Kind)
	}
	wantKeywords := []string{"crashloopbackoff", "crash loop", "reiniciando"}
	if len(crash.Keywords) != len(wantKeywords) {
		t.Fatalf("entries[0].Keywords = %v, want %v", crash.Keywords, wantKeywords)
	}
	for i, kw := range wantKeywords {
		if crash.Keywords[i] != kw {
			t.Errorf("entries[0].Keywords[%d] = %q, want %q", i, crash.Keywords[i], kw)
		}
	}
	if !strings.Contains(crash.Body, "Causa comum") || strings.Contains(crash.Body, "kind:") {
		t.Errorf("entries[0].Body doesn't look right:\n%s", crash.Body)
	}
}

func TestParse_BodyExcludesTrailingSeparator(t *testing.T) {
	entries := parse(testDoc)
	for _, e := range entries {
		if strings.HasSuffix(strings.TrimSpace(e.Body), "---") {
			t.Errorf("entry %q Body ends with the \"---\" separator, want it stripped:\n%s", e.Title, e.Body)
		}
	}
}

func TestParse_LogSignaturesAndLogSource(t *testing.T) {
	entries := parse(testDoc)
	var keda Entry
	for _, e := range entries {
		if e.Kind == "KEDAScalingStuck" {
			keda = e
		}
	}
	if keda.Kind == "" {
		t.Fatal("KEDAScalingStuck entry not found")
	}
	wantSigs := []string{"failed to get scaler", "error getting scale target"}
	if len(keda.LogSignatures) != len(wantSigs) {
		t.Fatalf("LogSignatures = %v, want %v", keda.LogSignatures, wantSigs)
	}
	for i, sig := range wantSigs {
		if keda.LogSignatures[i] != sig {
			t.Errorf("LogSignatures[%d] = %q, want %q", i, keda.LogSignatures[i], sig)
		}
	}
	if keda.LogSource != "keda-system/keda-operator" {
		t.Errorf("LogSource = %q, want %q", keda.LogSource, "keda-system/keda-operator")
	}
}

func TestEntry_MatchesLogs(t *testing.T) {
	e := Entry{LogSignatures: []string{"failed to get scaler", "error getting scale target"}}

	if !e.MatchesLogs("2026-01-01 ERROR failed to get scaler for prometheus-scaler\n") {
		t.Error("MatchesLogs = false for a log line containing a real signature, want true")
	}
	if !e.MatchesLogs("FAILED TO GET SCALER (uppercase)") {
		t.Error("MatchesLogs should be case-insensitive")
	}
	if e.MatchesLogs("everything's fine, scaled to 3 replicas") {
		t.Error("MatchesLogs = true for unrelated log text, want false")
	}
	if (Entry{}).MatchesLogs("failed to get scaler") {
		t.Error("an entry with no LogSignatures must never match, regardless of log content")
	}
}

func TestStore_FindByKind(t *testing.T) {
	s := newTestStore(t)

	e, ok := s.Find("CalicoNodeDegraded", "")
	if !ok {
		t.Fatal("Find by kind: not found")
	}
	if e.Title != "Nó do Calico degradado" {
		t.Errorf("Find by kind returned %q", e.Title)
	}
}

func TestStore_FindByKeyword(t *testing.T) {
	s := newTestStore(t)

	e, ok := s.Find("", "meu pod fica em crash loop toda hora")
	if !ok {
		t.Fatal("Find by keyword: not found")
	}
	if e.Kind != "PodCrashLoopBackOff" {
		t.Errorf("Find by keyword returned kind %q, want PodCrashLoopBackOff", e.Kind)
	}
}

func TestStore_FindNothing(t *testing.T) {
	s := newTestStore(t)

	if _, ok := s.Find("SomethingNotInTheDoc", "totally unrelated query"); ok {
		t.Error("Find matched something it shouldn't have")
	}
}

func TestStore_FormatoNeverMatches(t *testing.T) {
	s := newTestStore(t)

	if _, ok := s.Find("", "formato"); ok {
		t.Error("the document's own \"## Formato\" section must never be returned as a match")
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := t.TempDir() + "/runbooks.md"
	if err := os.WriteFile(path, []byte(testDoc), 0o644); err != nil {
		t.Fatalf("writing test doc: %v", err)
	}
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}
