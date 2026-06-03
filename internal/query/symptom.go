// Package query implements the symptom→mechanism query engine.
package query

import (
	"strings"
	"unicode"
)

// ExpandedQuery holds the tokenized and split query terms.
type ExpandedQuery struct {
	Original string
	Terms    []string // individual words
	Idents   []string // camelCase-split identifier candidates
}

// ExpandSymptom tokenizes and splits a natural language query into search terms.
// Deliberately thin: tokenize + camelCase split. No synonyms, no NLP.
// The LLM in Claude Code does the reasoning.
func ExpandSymptom(query string) *ExpandedQuery {
	eq := &ExpandedQuery{Original: query}

	// Tokenize: split on whitespace and punctuation
	words := tokenize(query)
	eq.Terms = words

	// For each word, try camelCase split and generate identifier candidates
	seen := make(map[string]bool)
	for _, w := range words {
		parts := splitCamelCase(w)
		for _, p := range parts {
			lower := strings.ToLower(p)
			if len(lower) >= 2 && !isStopWord(lower) && !seen[lower] {
				eq.Idents = append(eq.Idents, p)
				seen[lower] = true
			}
		}
		// Also keep the original word as a candidate
		if len(w) >= 2 && !isStopWord(strings.ToLower(w)) && !seen[strings.ToLower(w)] {
			eq.Idents = append(eq.Idents, w)
			seen[strings.ToLower(w)] = true
		}
	}

	// Generate compound candidates by pairing adjacent terms
	for i := 0; i < len(words)-1; i++ {
		a := capitalize(words[i])
		b := capitalize(words[i+1])
		compound := a + b
		if !seen[strings.ToLower(compound)] {
			eq.Idents = append(eq.Idents, compound)
			seen[strings.ToLower(compound)] = true
		}
	}

	return eq
}

func tokenize(s string) []string {
	var words []string
	var current strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			current.WriteRune(r)
		} else {
			if current.Len() > 0 {
				words = append(words, current.String())
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		words = append(words, current.String())
	}
	return words
}

func splitCamelCase(s string) []string {
	var parts []string
	var current strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && (i+1 < len(runes) && unicode.IsLower(runes[i+1]) || unicode.IsLower(runes[i-1])) {
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	// Also split on underscores
	var result []string
	for _, p := range parts {
		for _, sub := range strings.Split(p, "_") {
			if len(sub) > 0 {
				result = append(result, sub)
			}
		}
	}
	return result
}

func capitalize(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "was": true,
	"are": true, "be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "do": true, "does": true,
	"did": true, "will": true, "would": true, "could": true,
	"should": true, "may": true, "might": true, "shall": true,
	"can": true, "to": true, "of": true, "in": true, "for": true,
	"on": true, "with": true, "at": true, "by": true, "from": true,
	"it": true, "its": true, "this": true, "that": true, "not": true,
	"but": true, "and": true, "or": true, "if": true, "my": true,
	"i": true, "me": true, "we": true, "you": true, "he": true,
	"she": true, "they": true, "what": true, "when": true,
	"where": true, "how": true, "why": true, "all": true,
}

func isStopWord(s string) bool {
	return stopWords[s]
}
