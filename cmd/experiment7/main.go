// Experiment 7: LLM-as-graph-walker.
// Instead of BFS with heuristic scoring, give Claude the seed symbols
// and their immediate edges, let it choose which to follow.
// Tests: Can the LLM do better multi-hop reasoning than hand-coded scoring?
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
	"strings"

	_ "modernc.org/sqlite"
)

func hash64(s string) int64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return int64(h.Sum64() >> 1)
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: experiment7 <module-dir> <query>\n")
		os.Exit(1)
	}
	moduleDir := os.Args[1]
	query := os.Args[2]
	apiKey := os.Getenv("ANTHROPIC_API_KEY")

	fmt.Printf("=== Experiment 7: LLM-as-Graph-Walker ===\n")
	fmt.Printf("Query: %q\n\n", query)

	// Index (reuse exp5/6 approach)
	dbPath := "experiment7.db"
	os.Remove(dbPath)
	db := indexRepo(moduleDir, dbPath)
	defer db.Close()
	defer os.Remove(dbPath)

	// Phase 1: Find initial seeds (same as before)
	terms := filterStopWords(tokenize(query))
	fmt.Printf("Terms: %v\n\n", terms)

	type ident struct {
		id   int64
		name string
		qn   string
		kind string
		file string
		line int
	}

	var seeds []ident
	seen := make(map[int64]bool)
	for _, term := range terms {
		rows, _ := db.Query("SELECT id, name, qualname, kind, file, line FROM idents WHERE LOWER(qualname) LIKE ? LIMIT 15",
			"%"+strings.ToLower(term)+"%")
		for rows.Next() {
			var s ident
			rows.Scan(&s.id, &s.name, &s.qn, &s.kind, &s.file, &s.line)
			if !seen[s.id] {
				seen[s.id] = true
				seeds = append(seeds, s)
			}
		}
		rows.Close()
	}

	fmt.Printf("Found %d seed symbols\n\n", len(seeds))

	// Phase 2: Build a context of seeds + their edges for the LLM
	var contextBuilder strings.Builder
	contextBuilder.WriteString("## Code Graph Facts\n\n")
	contextBuilder.WriteString("These are symbols found in the codebase related to the query.\n")
	contextBuilder.WriteString("Each symbol has edges showing what it connects to.\n\n")

	for i, s := range seeds {
		if i >= 20 { // Cap at 20 seeds for context size
			break
		}
		contextBuilder.WriteString(fmt.Sprintf("### [%s] %s\n", s.kind, s.qn))
		contextBuilder.WriteString(fmt.Sprintf("  File: %s:%d\n", s.file, s.line))

		// Get edges from this node
		rows, _ := db.Query(`
			SELECT i.qualname, i.kind, e.kind as edge_kind
			FROM edges e JOIN idents i ON e.dst = i.id
			WHERE e.src = ? LIMIT 10`, s.id)
		for rows.Next() {
			var targetQN, targetKind, edgeKind string
			rows.Scan(&targetQN, &targetKind, &edgeKind)
			contextBuilder.WriteString(fmt.Sprintf("  -> [%s] %s (%s)\n", targetKind, targetQN, edgeKind))
		}
		rows.Close()

		// Reverse edges
		rows, _ = db.Query(`
			SELECT i.qualname, i.kind, e.kind as edge_kind
			FROM edges e JOIN idents i ON e.src = i.id
			WHERE e.dst = ? LIMIT 10`, s.id)
		for rows.Next() {
			var srcQN, srcKind, edgeKind string
			rows.Scan(&srcQN, &srcKind, &edgeKind)
			contextBuilder.WriteString(fmt.Sprintf("  <- [%s] %s (%s)\n", srcKind, srcQN, edgeKind))
		}
		rows.Close()

		contextBuilder.WriteString("\n")
	}

	// Check stateify annotations
	contextBuilder.WriteString("### Stateify Annotations (types that persist across save/restore)\n")
	rows, _ := db.Query("SELECT near_type, file, line FROM annotations WHERE text LIKE '%stateify%savable%' LIMIT 20")
	for rows.Next() {
		var nearType, file string
		var line int
		rows.Scan(&nearType, &file, &line)
		contextBuilder.WriteString(fmt.Sprintf("  %s at %s:%d is +stateify savable\n", nearType, file, line))
	}
	rows.Close()

	graphContext := contextBuilder.String()
	fmt.Printf("Graph context: %d chars\n\n", len(graphContext))

	// Phase 3: Ask the LLM to trace the causal chain
	prompt := fmt.Sprintf(`You are a code comprehension engine. A developer is debugging a Go codebase and described their problem as: %q

Below is a graph of relevant code symbols and their relationships extracted from the codebase. Each symbol has edges showing what it connects to (-> outgoing, <- incoming).

%s

Based on this graph, trace the most likely causal chain that explains the developer's symptom. For each step:
1. Name the specific symbol
2. State its file and line
3. Explain WHY this symbol is relevant to the symptom
4. Explain how it connects to the next symbol in the chain

Focus on the mechanism that could cause "nothing to happen" - look for nil checks, missing configuration, conditional paths that might not execute.

Output a numbered trace, most important symbols first. Be specific about file:line locations.`, query, graphContext)

	fmt.Println("=== LLM TRACE ===\n")

	if apiKey == "" {
		fmt.Println("No API key - skipping LLM trace")
	} else {
		body := map[string]interface{}{
			"model":      "claude-sonnet-4-6",
			"max_tokens": 1500,
			"messages":   []map[string]string{{"role": "user", "content": prompt}},
		}
		jsonBody, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Printf("API error: %v\n", err)
		} else {
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)
			var result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			json.Unmarshal(respBody, &result)
			if len(result.Content) > 0 {
				fmt.Println(result.Content[0].Text)
			} else {
				fmt.Printf("No content returned (status %d): %s\n", resp.StatusCode, string(respBody[:min(500, len(respBody))]))
			}
		}
	}

	fmt.Printf("\n=== DONE ===\n")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func indexRepo(moduleDir, dbPath string) *sql.DB {
	db, _ := sql.Open("sqlite", dbPath)
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

	tx, _ := db.Begin()
	insI, _ := tx.Prepare("INSERT OR IGNORE INTO idents (id, name, qualname, pkg, kind, file, line) VALUES (?,?,?,?,?,?,?)")
	insE, _ := tx.Prepare("INSERT INTO edges (src, dst, kind, file, line) VALUES (?,?,?,?,?)")
	insA, _ := tx.Prepare("INSERT INTO annotations (file, line, text, near_type) VALUES (?,?,?,?)")
	fset := token.NewFileSet()

	for _, dir := range dirs {
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return nil
			}
			relPath, _ := filepath.Rel(moduleDir, path)
			pkg := f.Name.Name

			stateifyLines := make(map[int]string)
			for _, cg := range f.Comments {
				for _, c := range cg.List {
					if strings.Contains(c.Text, "+stateify") {
						pos := fset.Position(c.Pos())
						stateifyLines[pos.Line] = strings.TrimSpace(c.Text)
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
							for checkLine := pos.Line - 3; checkLine <= pos.Line; checkLine++ {
								if annot, ok := stateifyLines[checkLine]; ok {
									insA.Exec(relPath, checkLine, annot, ts.Name.Name)
								}
							}
							if st, ok := ts.Type.(*ast.StructType); ok && st.Fields != nil {
								for _, field := range st.Fields.List {
									for _, name := range field.Names {
										fpos := fset.Position(name.Pos())
										fqn := qn + "." + name.Name
										insI.Exec(hash64(fqn), name.Name, fqn, pkg, "field", relPath, fpos.Line)
										insE.Exec(id, hash64(fqn), "has_field", relPath, fpos.Line)
									}
								}
							}
						}
					}
				case *ast.FuncDecl:
					pos := fset.Position(d.Pos())
					recv := ""
					if d.Recv != nil && len(d.Recv.List) > 0 {
						switch t := d.Recv.List[0].Type.(type) {
						case *ast.StarExpr:
							if ident, ok := t.X.(*ast.Ident); ok {
								recv = ident.Name
							}
						case *ast.Ident:
							recv = t.Name
						}
					}
					qn := pkg + "."
					if recv != "" {
						qn += recv + "."
					}
					qn += d.Name.Name
					fid := hash64(qn)
					insI.Exec(fid, d.Name.Name, qn, pkg, "func", relPath, pos.Line)
					if recv != "" {
						insE.Exec(hash64(pkg+"."+recv), fid, "has_method", relPath, pos.Line)
					}
					if d.Body != nil {
						ast.Inspect(d.Body, func(n ast.Node) bool {
							switch v := n.(type) {
							case *ast.SelectorExpr:
								sp := fset.Position(v.Pos())
								xn := ""
								if id, ok := v.X.(*ast.Ident); ok {
									xn = id.Name
								}
								tqn := v.Sel.Name
								if xn != "" && recv != "" && isRecvVar(xn, recv) {
									tqn = pkg + "." + recv + "." + v.Sel.Name
								} else if xn != "" {
									tqn = xn + "." + v.Sel.Name
								}
								insE.Exec(fid, hash64(tqn), "accesses", relPath, sp.Line)
							case *ast.CallExpr:
								cp := fset.Position(v.Pos())
								if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
									xn := ""
									if id, ok := sel.X.(*ast.Ident); ok {
										xn = id.Name
									}
									cqn := sel.Sel.Name
									if xn != "" && recv != "" && isRecvVar(xn, recv) {
										cqn = pkg + "." + recv + "." + sel.Sel.Name
									} else if xn != "" {
										cqn = xn + "." + sel.Sel.Name
									}
									insE.Exec(fid, hash64(cqn), "calls", relPath, cp.Line)
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
	return db
}

func isRecvVar(v, recv string) bool {
	if len(recv) == 0 {
		return false
	}
	return v == strings.ToLower(recv[:1]) || v == "k" || v == "s" || v == "t" || v == "f" || v == "m" || v == "o"
}

func tokenize(s string) []string {
	var w []string
	var c strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			c.WriteRune(r)
		} else if c.Len() > 0 {
			w = append(w, c.String())
			c.Reset()
		}
	}
	if c.Len() > 0 {
		w = append(w, c.String())
	}
	return w
}

func filterStopWords(words []string) []string {
	stops := map[string]bool{"the": true, "a": true, "an": true, "is": true, "was": true, "did": true, "do": true, "on": true, "in": true, "for": true, "to": true, "of": true, "not": true, "it": true, "with": true, "at": true, "by": true, "from": true, "nothing": true}
	var r []string
	for _, w := range words {
		if !stops[strings.ToLower(w)] && len(w) >= 2 {
			r = append(r, w)
		}
	}
	return r
}
