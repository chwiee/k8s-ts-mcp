package runbooks

import (
	"math"
	"strings"
	"unicode"
)

// score ranks entries by lexical similarity (TF-IDF + cosine) to query —
// same technique already validated in the sibling second-brain-mcp project
// (internal/notes/similarity.go), ported here rather than shared as a
// module since these are two independent repos. Deliberately not
// neural-embedding-based: dependency-free, fully local, good enough to
// catch shared vocabulary even when the wording doesn't share Find's exact
// kind/keyword strings — won't catch pure synonyms with zero word overlap.
func score(entries []Entry, query string) (best Entry, bestScore float64, ok bool) {
	queryTokens := tokenize(query)
	if len(queryTokens) == 0 || len(entries) == 0 {
		return Entry{}, 0, false
	}

	docs := make([][]string, len(entries))
	for i, e := range entries {
		docs[i] = tokenize(e.Title + " " + strings.Join(e.Keywords, " ") + " " + e.Body)
	}

	idf := buildIDF(docs)
	queryVec := tfidfVector(queryTokens, idf)

	for i, e := range entries {
		s := cosine(queryVec, tfidfVector(docs[i], idf))
		if s > bestScore {
			bestScore, best, ok = s, e, true
		}
	}
	return best, bestScore, ok
}

// stopwords are grammatical filler (articles, prepositions, pronouns,
// conjunctions) — removed before scoring because they appear in nearly
// every entry (Portuguese prose, plus this corpus's own repeated
// "Causa/Diagnóstico/Solução" structure) and otherwise dilute the actual
// content words enough to flip which entry looks most similar. This is a
// real gap the naive TF-IDF port from second-brain-mcp didn't need to
// handle — that corpus's notes are longer and more varied, so the signal
// wasn't drowned out the same way.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(
		"a o os as um uma uns umas de do da dos das em no na nos nas por para com sem sob sobre entre " +
			"e ou mas que se não sim é são foi foram ser estar tem têm ter isso essa esse essas esses " +
			"ao aos à às pra pro ate até como quando onde qual quais seu sua seus suas meu minha meus " +
			"minhas ele ela eles elas eu tu você vc voce vocês nós lhe lhes já mais menos muito pouco " +
			"aqui ali lá depois antes então ainda também só apenas caso ex etc",
	) {
		stopwords[w] = true
	}
}

func tokenize(s string) []string {
	raw := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	tokens := make([]string, 0, len(raw))
	for _, t := range raw {
		if !stopwords[t] {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

// buildIDF computes inverse document frequency, smoothed by +1 so terms
// appearing in every document still get a small positive weight instead of
// vanishing to log(1) = 0.
func buildIDF(docs [][]string) map[string]float64 {
	df := map[string]int{}
	for _, doc := range docs {
		seen := map[string]bool{}
		for _, tok := range doc {
			if !seen[tok] {
				df[tok]++
				seen[tok] = true
			}
		}
	}
	n := float64(len(docs))
	idf := make(map[string]float64, len(df))
	for term, count := range df {
		idf[term] = math.Log(1+n/float64(count)) + 1
	}
	return idf
}

func tfidfVector(tokens []string, idf map[string]float64) map[string]float64 {
	tf := map[string]float64{}
	for _, tok := range tokens {
		tf[tok]++
	}
	vec := make(map[string]float64, len(tf))
	for term, count := range tf {
		vec[term] = count * idf[term] // idf[term] is 0 for terms unseen in the corpus
	}
	return vec
}

func cosine(a, b map[string]float64) float64 {
	var dot, normA, normB float64
	for term, va := range a {
		normA += va * va
		if vb, ok := b[term]; ok {
			dot += va * vb
		}
	}
	for _, vb := range b {
		normB += vb * vb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
