// Experiment 6: LLM-assisted query expansion + improved graph walk.
// Tests: Can Claude hypothesize mechanism terms from symptom language,
// and does that improve search results compared to raw keyword matching?
// Also fixes: stateify annotation tracking, noise reduction in walk.
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type scoredResult struct {
	id, score            int
	name, qn, kind, file string
	line                 int
	matched              string
}

func hash64(s string) int64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return int64(h.Sum64() >> 1)
}

// LLM query expansion: ask Claude to hypothesize mechanism terms
func llmExpand(query, apiKey string) []string {
	if apiKey == "" {
		return nil
	}

	body := map[string]interface{}{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 200,
		"messages": []map[string]string{
			{"role": "user", "content": fmt.Sprintf(`Given this developer's problem description about a Go codebase: %q

List 5-10 Go identifier names (types, functions, fields, variables) that might be related to the root cause. Think about what internal mechanism could cause this symptom. Output ONLY the identifiers, one per line, no explanation.`, query)},
		},
	}

	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("  LLM expansion failed: %v\n", err)
		return nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(respBody, &result)

	if len(result.Content) == 0 {
		fmt.Printf("  LLM expansion: no content returned (status %d)\n", resp.StatusCode)
		return nil
	}

	text := result.Content[0].Text
	var terms []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		// Remove numbering like "1. " or "1) "
		for i, c := range line {
			if c == '.' || c == ')' {
				if i > 0 && i < 3 {
					line = strings.TrimSpace(line[i+1:])
				}
				break
			}
			if c < '0' || c > '9' {
				break
			}
		}
		if len(line) >= 3 && !strings.Contains(line, " ") {
			terms = append(terms, line)
		} else if len(line) >= 3 {
			// Split multi-word into individual identifiers
			for _, part := range strings.Fields(line) {
				part = strings.Trim(part, "`,.'\"()[]")
				if len(part) >= 3 {
					terms = append(terms, part)
				}
			}
		}
	}
	return terms
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: experiment6 <module-dir> <query>\n")
		os.Exit(1)
	}
	moduleDir := os.Args[1]
	query := os.Args[2]
	apiKey := os.Getenv("ANTHROPIC_API_KEY")

	fmt.Printf("=== Experiment 6: LLM-Assisted Search ===\n")
	fmt.Printf("Module: %s\nQuery: %q\nAPI key: %v\n\n", moduleDir, query, apiKey != "")

	// Phase 0: LLM query expansion
	fmt.Println("Phase 0: LLM Query Expansion")
	rawTerms := filterStopWords(tokenize(query))
	fmt.Printf("  Raw terms: %v\n", rawTerms)

	llmTerms := llmExpand(query, apiKey)
	fmt.Printf("  LLM terms: %v\n", llmTerms)

	allTerms := append(rawTerms, llmTerms...)
	// Deduplicate
	seen := make(map[string]bool)
	var uniqueTerms []string
	for _, t := range allTerms {
		tl := strings.ToLower(t)
		if !seen[tl] {
			seen[tl] = true
			uniqueTerms = append(uniqueTerms, t)
		}
	}
	fmt.Printf("  Combined terms: %v\n\n", uniqueTerms)

	// Phase 1: Index (same as exp5 but with stateify fix)
	dbPath := "experiment6.db"
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
	db.Exec(`CREATE INDEX idx_qn ON idents(qualname)`)
	db.Exec(`CREATE INDEX idx_esrc ON edges(src)`)
	db.Exec(`CREATE INDEX idx_edst ON edges(dst)`)

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

			// Track stateify annotations with position-based type linkage
			stateifyPositions := make(map[int]string) // line -> annotation text
			for _, cg := range f.Comments {
				for _, c := range cg.List {
					if strings.Contains(c.Text, "+stateify") {
						pos := fset.Position(c.Pos())
						stateifyPositions[pos.Line] = strings.TrimSpace(c.Text)
					}
				}
			}

			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						if ts, ok := spec.(*ast.TypeSpec); ok {
							pos := fset.Position(ts.Pos())
							qn := pkg + "." + ts.Name.Name
							id := hash64(qn)
							insI.Exec(id, ts.Name.Name, qn, pkg, "type", relPath, pos.Line)

							// Check if stateify annotation is near this type (within 3 lines above)
							for checkLine := pos.Line - 3; checkLine <= pos.Line; checkLine++ {
								if annot, ok := stateifyPositions[checkLine]; ok {
									insA.Exec(relPath, checkLine, annot, ts.Name.Name)
									// Create a synthetic edge: type -> stateify
									stateifyID := hash64("stateify:" + qn)
									insI.Exec(stateifyID, "stateify_savable", "stateify:"+qn, pkg, "annotation", relPath, checkLine)
									insE.Exec(id, stateifyID, "stateify_savable", relPath, checkLine)
								}
							}

							if st, ok := ts.Type.(*ast.StructType); ok && st.Fields != nil {
								for _, field := range st.Fields.List {
									for _, name := range field.Names {
										fpos := fset.Position(name.Pos())
										fqn := qn + "." + name.Name
										fid := hash64(fqn)
										insI.Exec(fid, name.Name, fqn, pkg, "field", relPath, fpos.Line)
										insE.Exec(id, fid, "has_field", relPath, fpos.Line)
									}
								}
							}
						}
					}

				case *ast.FuncDecl:
					pos := fset.Position(d.Pos())
					recv := extractRecvType(d)
					qn := pkg + "."
					if recv != "" {
						qn += recv + "."
					}
					qn += d.Name.Name
					fid := hash64(qn)
					insI.Exec(fid, d.Name.Name, qn, pkg, "func", relPath, pos.Line)

					if recv != "" {
						tid := hash64(pkg + "." + recv)
						insE.Exec(tid, fid, "has_method", relPath, pos.Line)
					}

					if d.Body != nil {
						ast.Inspect(d.Body, func(n ast.Node) bool {
							switch v := n.(type) {
							case *ast.SelectorExpr:
								selPos := fset.Position(v.Pos())
								xName := ""
								if ident, ok := v.X.(*ast.Ident); ok {
									xName = ident.Name
								}
								targetQN := v.Sel.Name
								if xName != "" && recv != "" && isReceiverVar(xName, recv) {
									targetQN = pkg + "." + recv + "." + v.Sel.Name
								} else if xName != "" {
									targetQN = xName + "." + v.Sel.Name
								}
								insE.Exec(fid, hash64(targetQN), "accesses", relPath, selPos.Line)

							case *ast.CallExpr:
								callPos := fset.Position(v.Pos())
								if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
									xName := ""
									if ident, ok := sel.X.(*ast.Ident); ok {
										xName = ident.Name
									}
									calleeQN := sel.Sel.Name
									if xName != "" && recv != "" && isReceiverVar(xName, recv) {
										calleeQN = pkg + "." + recv + "." + sel.Sel.Name
									} else if xName != "" {
										calleeQN = xName + "." + sel.Sel.Name
									}
									insE.Exec(fid, hash64(calleeQN), "calls", relPath, callPos.Line)
								}
							}
							return true
						})
					}
				}
			}
			return nil
		})
	}
	tx.Commit()
	indexTime := time.Since(t0)
	fmt.Printf("Indexed %d files in %v\n\n", totalFiles, indexTime)

	// Phase 2: Search with combined terms
	fmt.Println("=== SEARCH RESULTS ===\n")

	var seeds []scoredResult
	seenIDs := make(map[int64]bool)

	for _, term := range uniqueTerms {
		rows, _ := db.Query("SELECT id, name, qualname, kind, file, line FROM idents WHERE LOWER(qualname) LIKE ? LIMIT 20", "%"+strings.ToLower(term)+"%")
		for rows.Next() {
			var id int64
			var s scoredResult
			rows.Scan(&id, &s.name, &s.qn, &s.kind, &s.file, &s.line)
			if seenIDs[id] {
				continue
			}
			seenIDs[id] = true
			s.id = int(id)
			s.score = 50
			s.matched = term

			// Score based on how many terms match
			qnLower := strings.ToLower(s.qn)
			for _, t := range uniqueTerms {
				if strings.Contains(qnLower, strings.ToLower(t)) {
					s.score += 20
				}
			}
			// Bonus for LLM terms (they're hypothesized mechanism terms)
			for _, lt := range llmTerms {
				if strings.Contains(qnLower, strings.ToLower(lt)) {
					s.score += 15 // extra boost for LLM-hypothesized terms
				}
			}
			seeds = append(seeds, s)
		}
		rows.Close()
	}

	sort.Slice(seeds, func(i, j int) bool { return seeds[i].score > seeds[j].score })

	fmt.Println("Top 20 results (with LLM expansion):")
	for i, s := range seeds {
		if i >= 20 {
			break
		}
		isLLM := ""
		for _, lt := range llmTerms {
			if strings.Contains(strings.ToLower(s.qn), strings.ToLower(lt)) {
				isLLM = " [LLM]"
				break
			}
		}
		fmt.Printf("  %3d  [%s] %s at %s:%d (matched: %s)%s\n", s.score, s.kind, s.qn, s.file, s.line, s.matched, isLLM)
	}

	// Check stateify annotations
	fmt.Println("\nStateify-savable types found:")
	rows, _ := db.Query("SELECT near_type, file, line, text FROM annotations WHERE text LIKE '%stateify%'")
	for rows.Next() {
		var nearType, file, text string
		var line int
		rows.Scan(&nearType, &file, &line, &text)
		fmt.Printf("  %s: %s at %s:%d\n", nearType, text, file, line)
	}
	rows.Close()

	// Show comparison: raw terms only vs raw + LLM
	fmt.Println("\n=== COMPARISON: Raw vs LLM-expanded ===")
	fmt.Printf("Raw terms only would find: %d seeds\n", countMatches(seeds, rawTerms))
	fmt.Printf("With LLM expansion: %d seeds (%.0f%% more)\n", len(seeds),
		float64(len(seeds)-countMatches(seeds, rawTerms))/float64(max(1, countMatches(seeds, rawTerms)))*100)

	fmt.Printf("\n=== DONE in %v (index: %v) ===\n", time.Since(t0), indexTime)
}

func extractRecvType(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	switch t := d.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if ident, ok := t.X.(*ast.Ident); ok {
			return ident.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func isReceiverVar(varName, recvType string) bool {
	if len(recvType) == 0 {
		return false
	}
	first := strings.ToLower(recvType[:1])
	return varName == first || varName == "k" || varName == "s" || varName == "t" || varName == "f" || varName == "m" || varName == "o"
}

func countMatches(seeds []scoredResult, terms []string) int {
	count := 0
	for _, s := range seeds {
		for _, t := range terms {
			if strings.Contains(strings.ToLower(s.qn), strings.ToLower(t)) {
				count++
				break
			}
		}
	}
	return count
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
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
