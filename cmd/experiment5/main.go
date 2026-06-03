// Experiment 5: Relevance-scored graph walk + cross-package indexing.
// Fixes from experiment 4:
// 1. Walk prioritizes edges where target name contains query terms
// 2. Indexes multiple packages (kernel + control + cmd/runsc)
// 3. Builds cross-package edges via selector matching
package main

import (
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func hash64(s string) int64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return int64(h.Sum64() >> 1)
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: experiment5 <module-dir> <query>\n")
		os.Exit(1)
	}
	moduleDir := os.Args[1]
	query := os.Args[2]

	fmt.Printf("=== Experiment 5: Relevance-Scored Search ===\n")
	fmt.Printf("Module: %s\nQuery: %q\n\n", moduleDir, query)

	dbPath := "experiment5.db"
	os.Remove(dbPath)
	db, _ := sql.Open("sqlite", dbPath)
	defer db.Close()
	defer os.Remove(dbPath)

	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA synchronous=NORMAL")
	db.Exec(`CREATE TABLE idents (id INTEGER PRIMARY KEY, name TEXT, qualname TEXT, pkg TEXT, kind TEXT, file TEXT, line INTEGER)`)
	db.Exec(`CREATE TABLE edges (src INTEGER, dst INTEGER, kind TEXT, file TEXT, line INTEGER)`)
	db.Exec(`CREATE TABLE annotations (file TEXT, line INTEGER, text TEXT, near_type TEXT)`)
	db.Exec(`CREATE INDEX idx_name ON idents(name)`)
	db.Exec(`CREATE INDEX idx_qualname ON idents(qualname)`)
	db.Exec(`CREATE INDEX idx_esrc ON edges(src)`)
	db.Exec(`CREATE INDEX idx_edst ON edges(dst)`)

	// Index key directories
	dirs := []string{
		filepath.Join(moduleDir, "pkg", "sentry", "kernel"),
		filepath.Join(moduleDir, "pkg", "sentry", "control"),
		filepath.Join(moduleDir, "runsc", "cmd"),
		filepath.Join(moduleDir, "runsc", "config"),
		filepath.Join(moduleDir, "runsc", "boot"),
	}

	t0 := time.Now()
	tx, _ := db.Begin()
	insI, _ := tx.Prepare("INSERT OR IGNORE INTO idents (id, name, qualname, pkg, kind, file, line) VALUES (?,?,?,?,?,?,?)")
	insE, _ := tx.Prepare("INSERT INTO edges (src, dst, kind, file, line) VALUES (?,?,?,?,?)")
	insA, _ := tx.Prepare("INSERT INTO annotations (file, line, text, near_type) VALUES (?,?,?,?)")

	fset := token.NewFileSet()
	totalFiles := 0
	totalIdents := 0
	totalEdges := 0

	for _, dir := range dirs {
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return nil
			}
			totalFiles++
			relPath, _ := filepath.Rel(moduleDir, path)
			pkg := f.Name.Name

			// Track current type context for stateify annotation linkage
			var lastTypeName string

			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						if ts, ok := spec.(*ast.TypeSpec); ok {
							lastTypeName = ts.Name.Name
							pos := fset.Position(ts.Pos())
							qn := pkg + "." + ts.Name.Name
							id := hash64(qn)
							insI.Exec(id, ts.Name.Name, qn, pkg, "type", relPath, pos.Line)
							totalIdents++

							if st, ok := ts.Type.(*ast.StructType); ok && st.Fields != nil {
								for _, field := range st.Fields.List {
									for _, name := range field.Names {
										fpos := fset.Position(name.Pos())
										fqn := qn + "." + name.Name
										fid := hash64(fqn)
										insI.Exec(fid, name.Name, fqn, pkg, "field", relPath, fpos.Line)
										insE.Exec(id, fid, "has_field", relPath, fpos.Line)
										totalIdents++
										totalEdges++
									}
								}
							}
						}
					}

				case *ast.FuncDecl:
					pos := fset.Position(d.Pos())
					recv := ""
					if d.Recv != nil && len(d.Recv.List) > 0 {
						recv = extractRecvType(d.Recv.List[0].Type)
					}
					qn := pkg + "."
					if recv != "" {
						qn += recv + "."
					}
					qn += d.Name.Name
					fid := hash64(qn)
					insI.Exec(fid, d.Name.Name, qn, pkg, "func", relPath, pos.Line)
					totalIdents++

					if recv != "" {
						tid := hash64(pkg + "." + recv)
						insE.Exec(tid, fid, "has_method", relPath, pos.Line)
						totalEdges++
					}

					// Walk body for selectors and calls
					if d.Body != nil {
						ast.Inspect(d.Body, func(n ast.Node) bool {
							switch v := n.(type) {
							case *ast.SelectorExpr:
								selPos := fset.Position(v.Pos())
								selName := v.Sel.Name
								// Try to resolve the target
								xName := ""
								if ident, ok := v.X.(*ast.Ident); ok {
									xName = ident.Name
								}
								// Create edge: this function accesses this selector
								// Use qualname pattern for cross-package resolution
								targetQN := selName // partial, will resolve later
								if xName != "" {
									// Heuristic: if xName matches a receiver param, use the recv type
									if recv != "" && (xName == strings.ToLower(recv[:1]) || xName == "k" || xName == "s" || xName == "t" || xName == "f") {
										targetQN = pkg + "." + recv + "." + selName
									} else {
										targetQN = xName + "." + selName
									}
								}
								targetID := hash64(targetQN)
								insE.Exec(fid, targetID, "accesses", relPath, selPos.Line)
								totalEdges++

							case *ast.CallExpr:
								callPos := fset.Position(v.Pos())
								if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
									calleeName := sel.Sel.Name
									// Try multiple resolution strategies
									xName := ""
									if ident, ok := sel.X.(*ast.Ident); ok {
										xName = ident.Name
									}
									calleeQN := calleeName
									if xName != "" {
										if recv != "" && (xName == strings.ToLower(recv[:1]) || xName == "k") {
											calleeQN = pkg + "." + recv + "." + calleeName
										} else {
											calleeQN = xName + "." + calleeName
										}
									}
									calleeID := hash64(calleeQN)
									insE.Exec(fid, calleeID, "calls", relPath, callPos.Line)
									totalEdges++
								}
							}
							return true
						})
					}
				}
			}

			// Annotations
			for _, cg := range f.Comments {
				for _, c := range cg.List {
					text := strings.TrimSpace(c.Text)
					if strings.Contains(text, "+stateify") {
						pos := fset.Position(c.Pos())
						insA.Exec(relPath, pos.Line, text, lastTypeName)
					}
				}
			}
			return nil
		})
	}
	tx.Commit()
	indexTime := time.Since(t0)
	fmt.Printf("Indexed %d files: %d idents, %d edges in %v\n\n", totalFiles, totalIdents, totalEdges, indexTime)

	// === SEARCH ===
	terms := tokenize(query)
	nonStopTerms := filterStopWords(terms)
	fmt.Printf("Search terms: %v\n\n", nonStopTerms)

	// Phase 1: Find seed symbols
	type scored struct {
		id      int64
		name    string
		qn      string
		kind    string
		file    string
		line    int
		score   float64
		matched string
	}
	var seeds []scored

	for _, term := range nonStopTerms {
		// Exact match
		rows, _ := db.Query("SELECT id, name, qualname, kind, file, line FROM idents WHERE LOWER(name) = LOWER(?)", term)
		for rows.Next() {
			var s scored
			rows.Scan(&s.id, &s.name, &s.qn, &s.kind, &s.file, &s.line)
			s.score = 100
			s.matched = "exact:" + term
			seeds = append(seeds, s)
		}
		rows.Close()

		// Substring in qualname (catches SaveRestoreExecConfig from "restore")
		rows, _ = db.Query("SELECT id, name, qualname, kind, file, line FROM idents WHERE LOWER(qualname) LIKE ? LIMIT 20",
			"%"+strings.ToLower(term)+"%")
		for rows.Next() {
			var s scored
			rows.Scan(&s.id, &s.name, &s.qn, &s.kind, &s.file, &s.line)
			s.score = 50
			s.matched = "substr:" + term
			// Boost if multiple query terms match
			matchCount := 0
			qnLower := strings.ToLower(s.qn)
			for _, t := range nonStopTerms {
				if strings.Contains(qnLower, strings.ToLower(t)) {
					matchCount++
				}
			}
			s.score += float64(matchCount) * 20
			seeds = append(seeds, s)
		}
		rows.Close()
	}

	// Deduplicate and sort by score
	seen := make(map[int64]bool)
	var uniqueSeeds []scored
	for _, s := range seeds {
		if !seen[s.id] {
			seen[s.id] = true
			uniqueSeeds = append(uniqueSeeds, s)
		}
	}
	sort.Slice(uniqueSeeds, func(i, j int) bool { return uniqueSeeds[i].score > uniqueSeeds[j].score })

	fmt.Println("=== TOP SEEDS ===")
	for i, s := range uniqueSeeds {
		if i >= 15 {
			break
		}
		fmt.Printf("  %.0f  [%s] %s at %s:%d (%s)\n", s.score, s.kind, s.qn, s.file, s.line, s.matched)
	}

	// Phase 2: Relevance-scored graph walk
	// Only follow edges where the target is relevant to the query
	fmt.Println("\n=== RELEVANCE-SCORED WALK (3 hops) ===")

	type traceNode struct {
		id    int64
		name  string
		qn    string
		kind  string
		file  string
		line  int
		depth int
		edge  string
		score float64
	}

	visited := make(map[int64]bool)
	var results []traceNode

	// Start from top 5 seeds
	topSeeds := uniqueSeeds
	if len(topSeeds) > 5 {
		topSeeds = topSeeds[:5]
	}

	queue := make([]traceNode, 0)
	for _, s := range topSeeds {
		queue = append(queue, traceNode{s.id, s.name, s.qn, s.kind, s.file, s.line, 0, "seed", s.score})
	}

	for len(queue) > 0 && len(results) < 100 {
		// Pop highest-scored item
		bestIdx := 0
		for i, q := range queue {
			if q.score > queue[bestIdx].score {
				bestIdx = i
			}
		}
		node := queue[bestIdx]
		queue = append(queue[:bestIdx], queue[bestIdx+1:]...)

		if visited[node.id] || node.depth > 3 {
			continue
		}
		visited[node.id] = true
		results = append(results, node)

		// Follow edges, scoring targets by relevance
		rows, _ := db.Query(`
			SELECT e.dst, i.name, i.qualname, i.kind, i.file, i.line, e.kind
			FROM edges e JOIN idents i ON e.dst = i.id
			WHERE e.src = ?`, node.id)
		for rows.Next() {
			var t traceNode
			rows.Scan(&t.id, &t.name, &t.qn, &t.kind, &t.file, &t.line, &t.edge)
			if visited[t.id] {
				continue
			}
			t.depth = node.depth + 1
			// Score: how relevant is this target to the query?
			t.score = relevanceScore(t.qn, t.name, t.kind, t.edge, nonStopTerms)
			if t.score > 0 {
				queue = append(queue, t)
			}
		}
		rows.Close()

		// Reverse edges too
		rows, _ = db.Query(`
			SELECT e.src, i.name, i.qualname, i.kind, i.file, i.line, e.kind
			FROM edges e JOIN idents i ON e.src = i.id
			WHERE e.dst = ?`, node.id)
		for rows.Next() {
			var t traceNode
			rows.Scan(&t.id, &t.name, &t.qn, &t.kind, &t.file, &t.line, &t.edge)
			if visited[t.id] {
				continue
			}
			t.depth = node.depth + 1
			t.edge = "rev_" + t.edge
			t.score = relevanceScore(t.qn, t.name, t.kind, t.edge, nonStopTerms)
			if t.score > 0 {
				queue = append(queue, t)
			}
		}
		rows.Close()
	}

	// Print results sorted by score
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	for _, r := range results {
		indent := strings.Repeat("  ", r.depth)
		fmt.Printf("%s%.0f [%s] %s at %s:%d via %s\n", indent, r.score, r.kind, r.qn, r.file, r.line, r.edge)
	}

	// Check for stateify annotations on found types
	fmt.Println("\n=== STATEIFY ANNOTATIONS FOR FOUND TYPES ===")
	for _, r := range results {
		if r.kind == "type" {
			rows, _ := db.Query("SELECT file, line, text FROM annotations WHERE near_type = ?", r.name)
			for rows.Next() {
				var file, text string
				var line int
				rows.Scan(&file, &line, &text)
				fmt.Printf("  %s: %s at %s:%d\n", r.name, text, file, line)
			}
			rows.Close()
		}
	}

	fmt.Printf("\n=== DONE in %v (index: %v) ===\n", time.Since(t0), indexTime)
}

func relevanceScore(qn, name, kind, edgeKind string, queryTerms []string) float64 {
	score := 1.0 // base score for any connected node

	qnLower := strings.ToLower(qn)
	nameLower := strings.ToLower(name)

	// Boost for each query term that matches
	for _, term := range queryTerms {
		tl := strings.ToLower(term)
		if strings.Contains(qnLower, tl) {
			score += 30
		}
		if strings.Contains(nameLower, tl) {
			score += 20
		}
	}

	// Boost certain edge types
	switch edgeKind {
	case "accesses", "calls":
		score += 10
	case "has_field":
		score += 5
	case "has_method":
		score += 3
	case "rev_accesses", "rev_calls":
		score += 8
	}

	// Boost certain identifier kinds
	switch kind {
	case "func":
		score += 5
	case "type":
		score += 3
	case "field":
		score += 2
	}

	// Boost names containing save/restore/checkpoint/config/flag patterns
	boostPatterns := []string{"save", "restore", "checkpoint", "config", "flag", "exec", "load", "state"}
	for _, p := range boostPatterns {
		if strings.Contains(nameLower, p) {
			score += 15
		}
	}

	return score
}

func extractRecvType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		if ident, ok := t.X.(*ast.Ident); ok {
			return ident.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func tokenize(s string) []string {
	var words []string
	var cur strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			cur.WriteRune(r)
		} else if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		words = append(words, cur.String())
	}
	return words
}

func filterStopWords(words []string) []string {
	stops := map[string]bool{"the": true, "a": true, "an": true, "is": true, "was": true, "did": true, "do": true, "on": true, "in": true, "for": true, "to": true, "of": true, "not": true, "it": true, "with": true, "at": true, "by": true, "from": true, "nothing": true}
	var result []string
	for _, w := range words {
		if !stops[strings.ToLower(w)] && len(w) >= 2 {
			result = append(result, w)
		}
	}
	return result
}
