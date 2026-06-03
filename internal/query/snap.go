package query

import (
	"strings"

	"github.com/lokashrinav/codesearch/internal/storage"
)

// SnappedSymbol is a candidate identifier matched from query terms.
type SnappedSymbol struct {
	Ident     storage.Identifier
	Score     float64
	MatchType string // "exact", "prefix", "fts", "compound", "flag"
}

// SnapToSymbols matches expanded query terms against the fact graph.
// Returns scored candidates in descending order.
func SnapToSymbols(eq *ExpandedQuery, reader *storage.Reader) ([]SnappedSymbol, error) {
	seen := make(map[uint64]bool)
	var results []SnappedSymbol

	addResults := func(idents []storage.Identifier, score float64, matchType string) {
		for _, id := range idents {
			if seen[id.ID] {
				continue
			}
			seen[id.ID] = true
			adjustedScore := score * kindBoost(id.Kind)
			results = append(results, SnappedSymbol{
				Ident:     id,
				Score:     adjustedScore,
				MatchType: matchType,
			})
		}
	}

	// Tier 1: Exact name match (score 100)
	for _, term := range eq.Idents {
		idents, err := reader.FindByName(term)
		if err != nil {
			continue
		}
		addResults(idents, 100, "exact")
	}

	// Tier 2: Prefix match (score 60)
	for _, term := range eq.Idents {
		if len(term) < 3 {
			continue
		}
		idents, err := reader.FindByPrefix(term)
		if err != nil {
			continue
		}
		addResults(idents, 60, "prefix")
	}

	// Tier 3: Compound match - all query subwords present (score 80)
	if len(eq.Terms) >= 2 {
		searchTerms := make([]string, 0)
		for _, t := range eq.Terms {
			if !isStopWord(strings.ToLower(t)) && len(t) >= 3 {
				searchTerms = append(searchTerms, t)
			}
		}
		if len(searchTerms) >= 2 {
			idents, err := reader.CompoundMatch(searchTerms)
			if err == nil {
				addResults(idents, 80, "compound")
			}
		}
	}

	// Tier 4: FTS5 trigram search (score 40)
	for _, term := range eq.Idents {
		if len(term) < 3 {
			continue
		}
		idents, err := reader.SearchFTS(term)
		if err != nil {
			continue
		}
		addResults(idents, 40, "fts")
	}

	// Tier 5: Flag name match (score 90)
	for _, term := range eq.Terms {
		flags, err := reader.GetFlagBinding(term)
		if err != nil {
			continue
		}
		for _, fb := range flags {
			boundIdents, err := reader.FindByName(fb.FlagName)
			if err != nil {
				continue
			}
			addResults(boundIdents, 90, "flag")
			// Also add what the flag is bound to
			idents, _ := reader.FindByName("")
			_ = idents // bound_to_id lookup would go here via ID
		}
	}

	// Sort by score descending
	sortByScore(results)

	// Cap at 50
	if len(results) > 50 {
		results = results[:50]
	}

	return results, nil
}

func kindBoost(kind storage.IdentKind) float64 {
	switch kind {
	case storage.IdentType, storage.IdentInterface:
		return 1.5
	case storage.IdentFunc, storage.IdentMethod:
		return 1.3
	case storage.IdentField:
		return 1.2
	default:
		return 1.0
	}
}

func sortByScore(results []SnappedSymbol) {
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].Score > results[j-1].Score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
}
