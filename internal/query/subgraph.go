package query

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/lokashrinav/codesearch/internal/extractor"
)

// BuildSubgraph extracts a local subgraph around seed symbols for LLM consumption.
// Returns a formatted string of ~5KB suitable for the LLM context.
func BuildSubgraph(db *sql.DB, query string, maxSeeds int) (string, error) {
	if maxSeeds == 0 {
		maxSeeds = 25
	}

	allWords := tokenize(query)
	var terms []string
	for _, w := range allWords {
		if !isStopWord(strings.ToLower(w)) && len(w) >= 2 {
			terms = append(terms, w)
		}
	}

	// Find seeds
	seen := make(map[int64]bool)
	type seedInfo struct {
		id   int64
		name string
		qn   string
		kind string
		file string
		line int
	}
	var seeds []seedInfo

	for _, term := range terms {
		rows, err := db.Query(
			"SELECT id, name, pkg_path, kind, file_path, line FROM identifiers WHERE LOWER(pkg_path) LIKE ? LIMIT 15",
			"%"+strings.ToLower(term)+"%")
		if err != nil {
			continue
		}
		for rows.Next() {
			var s seedInfo
			rows.Scan(&s.id, &s.name, &s.qn, &s.kind, &s.file, &s.line)
			if !seen[s.id] {
				seen[s.id] = true
				seeds = append(seeds, s)
			}
		}
		rows.Close()
	}

	if len(seeds) == 0 {
		return "", fmt.Errorf("no symbols matched query terms: %v", terms)
	}

	// Build formatted context
	var ctx strings.Builder
	ctx.WriteString("## Code Graph\n\n")

	limit := maxSeeds
	if len(seeds) < limit {
		limit = len(seeds)
	}

	for i := 0; i < limit; i++ {
		s := seeds[i]
		kindLabel := identKindLabel(s.kind)
		ctx.WriteString(fmt.Sprintf("### [%s] %s\n  File: %s:%d\n", kindLabel, s.qn, s.file, s.line))

		// Outgoing edges
		rows, _ := db.Query(`
			SELECT i.pkg_path, i.kind, e.kind
			FROM edges e JOIN identifiers i ON e.dst_id = i.id
			WHERE e.src_id = ? LIMIT 8`, s.id)
		for rows.Next() {
			var tqn, tk, ek string
			rows.Scan(&tqn, &tk, &ek)
			ctx.WriteString(fmt.Sprintf("  -> [%s] %s (%s)\n", identKindLabel(tk), tqn, ek))
		}
		rows.Close()

		// Incoming edges
		rows, _ = db.Query(`
			SELECT i.pkg_path, i.kind, e.kind
			FROM edges e JOIN identifiers i ON e.src_id = i.id
			WHERE e.dst_id = ? LIMIT 8`, s.id)
		for rows.Next() {
			var sqn, sk, ek string
			rows.Scan(&sqn, &sk, &ek)
			ctx.WriteString(fmt.Sprintf("  <- [%s] %s (%s)\n", identKindLabel(sk), sqn, ek))
		}
		rows.Close()

		ctx.WriteString("\n")
	}

	// Add annotations for found types
	ctx.WriteString("### Annotations\n")
	for i := 0; i < limit; i++ {
		s := seeds[i]
		rows, _ := db.Query("SELECT file_path, line, text FROM annotations WHERE near_type = ?", s.name)
		for rows.Next() {
			var f, t string
			var l int
			rows.Scan(&f, &l, &t)
			ctx.WriteString(fmt.Sprintf("  %s: %s at %s:%d\n", s.name, t, f, l))
		}
		rows.Close()
	}

	return ctx.String(), nil
}

func identKindLabel(kind string) string {
	switch kind {
	case "0":
		return "func"
	case "1":
		return "type"
	case "2":
		return "field"
	case "7":
		return "interface"
	default:
		return kind
	}
}

// HashID re-exports from extractor for convenience
func HashID(s string) int64 {
	return extractor.HashID(s)
}
