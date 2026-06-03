// Experiment 8: Full-repo indexer + MCP-ready query tool.
// Indexes the ENTIRE gVisor repo (not just select directories).
// Tests: Does the approach scale? Can we find nvproxy-related facts?
// Also tests a third query: "CUDA context not initialized after restore"
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
		fmt.Fprintf(os.Stderr, "Usage: experiment8 <module-dir> <query>\n")
		os.Exit(1)
	}
	moduleDir := os.Args[1]
	query := os.Args[2]
	apiKey := os.Getenv("ANTHROPIC_API_KEY")

	fmt.Printf("=== Experiment 8: Full-Repo Index + Query ===\n")
	fmt.Printf("Module: %s\nQuery: %q\n\n", moduleDir, query)

	// Phase 1: Index the ENTIRE repo
	dbPath := "experiment8.db"
	os.Remove(dbPath)
	db, _ := sql.Open("sqlite", dbPath)
	defer db.Close()

	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA synchronous=NORMAL")
	db.Exec("PRAGMA cache_size=-64000") // 64MB cache
	db.Exec(`CREATE TABLE idents (id INTEGER PRIMARY KEY, name TEXT, qualname TEXT, pkg TEXT, kind TEXT, file TEXT, line INTEGER)`)
	db.Exec(`CREATE TABLE edges (src INTEGER, dst INTEGER, kind TEXT)`)
	db.Exec(`CREATE TABLE annotations (file TEXT, line INTEGER, text TEXT, near_type TEXT)`)

	t0 := time.Now()
	tx, _ := db.Begin()
	insI, _ := tx.Prepare("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)")
	insE, _ := tx.Prepare("INSERT INTO edges VALUES (?,?,?)")
	insA, _ := tx.Prepare("INSERT INTO annotations VALUES (?,?,?,?)")
	fset := token.NewFileSet()
	totalFiles := 0
	totalIdents := 0
	totalEdges := 0

	// Walk EVERYTHING under moduleDir
	filepath.Walk(moduleDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			// Skip vendor, .git, testdata, tools
			if info != nil && info.IsDir() {
				name := info.Name()
				if name == ".git" || name == "vendor" || name == "tools" || name == "bazel-out" || name == "bazel-bin" || name == "bazel-gvisor" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil
		}
		totalFiles++
		relPath, _ := filepath.Rel(moduleDir, path)
		pkg := f.Name.Name

		// Stateify tracking
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
						totalIdents++

						for cl := pos.Line - 3; cl <= pos.Line; cl++ {
							if annot, ok := stateifyLines[cl]; ok {
								insA.Exec(relPath, cl, annot, ts.Name.Name)
							}
						}

						if st, ok := ts.Type.(*ast.StructType); ok && st.Fields != nil {
							for _, field := range st.Fields.List {
								for _, name := range field.Names {
									fp := fset.Position(name.Pos())
									fqn := qn + "." + name.Name
									fid := hash64(fqn)
									insI.Exec(fid, name.Name, fqn, pkg, "field", relPath, fp.Line)
									insE.Exec(id, fid, "has_field")
									totalIdents++
									totalEdges++
								}
							}
						}
						if iface, ok := ts.Type.(*ast.InterfaceType); ok && iface.Methods != nil {
							for _, m := range iface.Methods.List {
								for _, name := range m.Names {
									mp := fset.Position(name.Pos())
									mqn := qn + "." + name.Name
									mid := hash64(mqn)
									insI.Exec(mid, name.Name, mqn, pkg, "imethod", relPath, mp.Line)
									insE.Exec(id, mid, "has_imethod")
									totalIdents++
									totalEdges++
								}
							}
						}
					}
				}

			case *ast.FuncDecl:
				pos := fset.Position(d.Pos())
				recv := extractRecv(d)
				qn := pkg + "."
				if recv != "" {
					qn += recv + "."
				}
				qn += d.Name.Name
				fid := hash64(qn)
				insI.Exec(fid, d.Name.Name, qn, pkg, "func", relPath, pos.Line)
				totalIdents++

				if recv != "" {
					insE.Exec(hash64(pkg+"."+recv), fid, "has_method")
					totalEdges++
				}

				if d.Body != nil {
					ast.Inspect(d.Body, func(n ast.Node) bool {
						switch v := n.(type) {
						case *ast.SelectorExpr:
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
							insE.Exec(fid, hash64(tqn), "accesses")
							totalEdges++

						case *ast.CallExpr:
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
								insE.Exec(fid, hash64(cqn), "calls")
								totalEdges++
							}
						}
						return true
					})
				}
			}
		}
		return nil
	})

	tx.Commit()

	// Create indexes AFTER bulk insert (faster)
	db.Exec("CREATE INDEX idx_name ON idents(name)")
	db.Exec("CREATE INDEX idx_qn ON idents(qualname)")
	db.Exec("CREATE INDEX idx_esrc ON edges(src)")
	db.Exec("CREATE INDEX idx_edst ON edges(dst)")

	indexTime := time.Since(t0)
	fmt.Printf("Indexed %d files: %d idents, %d edges in %v\n\n", totalFiles, totalIdents, totalEdges, indexTime)

	// Phase 2: Search
	terms := filterStop(tokenize(query))
	fmt.Printf("Search terms: %v\n\n", terms)

	// Find seeds
	seen := make(map[int64]bool)
	type seed struct {
		id   int64
		name string
		qn   string
		kind string
		file string
		line int
	}
	var seeds []seed

	for _, term := range terms {
		rows, _ := db.Query("SELECT id, name, qualname, kind, file, line FROM idents WHERE LOWER(qualname) LIKE ? LIMIT 15",
			"%"+strings.ToLower(term)+"%")
		for rows.Next() {
			var s seed
			rows.Scan(&s.id, &s.name, &s.qn, &s.kind, &s.file, &s.line)
			if !seen[s.id] {
				seen[s.id] = true
				seeds = append(seeds, s)
			}
		}
		rows.Close()
	}
	fmt.Printf("Found %d seeds\n\n", len(seeds))

	// Build subgraph context for LLM
	var ctx strings.Builder
	ctx.WriteString("## Code Graph (top 25 seeds + edges)\n\n")

	limit := 25
	if len(seeds) < limit {
		limit = len(seeds)
	}
	for i := 0; i < limit; i++ {
		s := seeds[i]
		ctx.WriteString(fmt.Sprintf("### [%s] %s\n  File: %s:%d\n", s.kind, s.qn, s.file, s.line))

		rows, _ := db.Query(`SELECT i.qualname, i.kind, e.kind FROM edges e JOIN idents i ON e.dst = i.id WHERE e.src = ? LIMIT 8`, s.id)
		for rows.Next() {
			var tqn, tk, ek string
			rows.Scan(&tqn, &tk, &ek)
			ctx.WriteString(fmt.Sprintf("  -> [%s] %s (%s)\n", tk, tqn, ek))
		}
		rows.Close()

		rows, _ = db.Query(`SELECT i.qualname, i.kind, e.kind FROM edges e JOIN idents i ON e.src = i.id WHERE e.dst = ? LIMIT 8`, s.id)
		for rows.Next() {
			var sqn, sk, ek string
			rows.Scan(&sqn, &sk, &ek)
			ctx.WriteString(fmt.Sprintf("  <- [%s] %s (%s)\n", sk, sqn, ek))
		}
		rows.Close()
		ctx.WriteString("\n")
	}

	// Add stateify annotations for found types
	ctx.WriteString("### Stateify annotations\n")
	for i := 0; i < limit; i++ {
		s := seeds[i]
		if s.kind == "type" {
			rows, _ := db.Query("SELECT file, line, text FROM annotations WHERE near_type = ?", s.name)
			for rows.Next() {
				var f, t string
				var l int
				rows.Scan(&f, &l, &t)
				ctx.WriteString(fmt.Sprintf("  %s: %s at %s:%d\n", s.name, t, f, l))
			}
			rows.Close()
		}
	}

	graphCtx := ctx.String()
	fmt.Printf("Graph context: %d chars\n\n", len(graphCtx))

	// LLM trace
	if apiKey != "" {
		prompt := fmt.Sprintf(`You are a code comprehension engine analyzing a Go codebase (gVisor - a container runtime sandbox).

Developer's problem: %q

Below is a subgraph of relevant code symbols with their relationships:

%s

Trace the causal chain that explains this symptom. For each step, give:
1. The specific symbol and file:line
2. Why it's relevant
3. How it connects to the next step

Focus on mechanisms that could cause the symptom silently (nil checks, missing initialization, conditional paths that skip work). Be specific.`, query, graphCtx)

		body := map[string]interface{}{
			"model": "claude-sonnet-4-6", "max_tokens": 1500,
			"messages": []map[string]string{{"role": "user", "content": prompt}},
		}
		jsonBody, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		fmt.Println("=== LLM TRACE ===\n")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Printf("API error: %v\n", err)
		} else {
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)
			var result struct {
				Content []struct{ Text string `json:"text"` } `json:"content"`
			}
			json.Unmarshal(respBody, &result)
			if len(result.Content) > 0 {
				fmt.Println(result.Content[0].Text)
			} else {
				fmt.Printf("No content (status %d)\n", resp.StatusCode)
			}
		}
	}

	// Keep DB for inspection
	fmt.Printf("\n=== DONE in %v (index: %v) ===\n", time.Since(t0), indexTime)
	fmt.Printf("Database saved: %s\n", dbPath)
}

func extractRecv(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	switch t := d.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func isRecvVar(v, recv string) bool {
	if len(recv) == 0 {
		return false
	}
	return v == strings.ToLower(recv[:1]) || v == "k" || v == "s" || v == "t" || v == "f" || v == "m" || v == "o" || v == "d" || v == "p" || v == "n"
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

func filterStop(words []string) []string {
	stops := map[string]bool{"the": true, "a": true, "an": true, "is": true, "was": true, "did": true, "do": true, "on": true, "in": true, "for": true, "to": true, "of": true, "not": true, "it": true, "with": true, "at": true, "by": true, "from": true, "nothing": true, "after": true}
	var r []string
	for _, w := range words {
		if !stops[strings.ToLower(w)] && len(w) >= 2 {
			r = append(r, w)
		}
	}
	return r
}
