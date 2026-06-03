// Experiment 4: Full indexer + search prototype.
// Indexes a Go directory tree into SQLite, then runs a search query.
// Tests the full pipeline: parse → extract → store → query.
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
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func hash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: experiment4 <directory> <query>\n")
		fmt.Fprintf(os.Stderr, "  e.g.: experiment4 /path/to/gvisor/pkg/sentry \"flag did nothing on restore\"\n")
		os.Exit(1)
	}
	dir := os.Args[1]
	query := os.Args[2]

	fmt.Printf("=== Experiment 4: Index + Search ===\n")
	fmt.Printf("Directory: %s\n", dir)
	fmt.Printf("Query: %q\n\n", query)

	// Phase 1: Index
	dbPath := "experiment4.db"
	os.Remove(dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "DB error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	defer os.Remove(dbPath)

	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA synchronous=NORMAL")

	mustExec(db, `CREATE TABLE identifiers (
		id INTEGER PRIMARY KEY, name TEXT, pkg TEXT, kind TEXT,
		file TEXT, line INTEGER, detail TEXT
	)`)
	mustExec(db, `CREATE TABLE edges (
		src_id INTEGER, dst_id INTEGER, kind TEXT, file TEXT, line INTEGER
	)`)
	mustExec(db, `CREATE TABLE annotations (
		file TEXT, line INTEGER, text TEXT
	)`)
	mustExec(db, `CREATE INDEX idx_name ON identifiers(name)`)
	mustExec(db, `CREATE INDEX idx_name_lower ON identifiers(name COLLATE NOCASE)`)
	mustExec(db, `CREATE INDEX idx_edge_src ON edges(src_id)`)
	mustExec(db, `CREATE INDEX idx_edge_dst ON edges(dst_id)`)

	t0 := time.Now()
	tx, err := db.Begin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Begin tx: %v\n", err)
		os.Exit(1)
	}

	insertIdent, _ := tx.Prepare("INSERT OR IGNORE INTO identifiers (id, name, pkg, kind, file, line, detail) VALUES (?,?,?,?,?,?,?)")
	insertEdge, _ := tx.Prepare("INSERT INTO edges (src_id, dst_id, kind, file, line) VALUES (?,?,?,?,?)")
	insertAnnotation, _ := tx.Prepare("INSERT INTO annotations (file, line, text) VALUES (?,?,?)")

	fileCount := 0
	identCount := 0
	edgeCount := 0

	fset := token.NewFileSet()

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil
		}
		fileCount++
		relPath, _ := filepath.Rel(dir, path)
		pkg := f.Name.Name

		// Current file context for receiver resolution
		var currentRecvType string

		// Declarations
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						pos := fset.Position(s.Pos())
						id := hash(pkg + "." + s.Name.Name)
						insertIdent.Exec(id, s.Name.Name, pkg, "type", relPath, pos.Line, "")

						identCount++

						if st, ok := s.Type.(*ast.StructType); ok && st.Fields != nil {
							for _, field := range st.Fields.List {
								for _, name := range field.Names {
									fpos := fset.Position(name.Pos())
									fid := hash(pkg + "." + s.Name.Name + "." + name.Name)
									tag := ""
									if field.Tag != nil {
										tag = field.Tag.Value
									}
									insertIdent.Exec(fid, name.Name, pkg, "field", relPath, fpos.Line, s.Name.Name+"."+name.Name+" "+tag)

									// Edge: type -> field
									insertEdge.Exec(id, fid, "has_field", relPath, fpos.Line)
									identCount++
									edgeCount++
								}
							}
						}
					}
				}

			case *ast.FuncDecl:
				pos := fset.Position(d.Pos())
				recv := ""
				currentRecvType = ""
				if d.Recv != nil && len(d.Recv.List) > 0 {
					if t, ok := d.Recv.List[0].Type.(*ast.StarExpr); ok {
						if ident, ok := t.X.(*ast.Ident); ok {
							recv = ident.Name
							currentRecvType = ident.Name
						}
					} else if ident, ok := d.Recv.List[0].Type.(*ast.Ident); ok {
						recv = ident.Name
						currentRecvType = ident.Name
					}
				}
				fullName := d.Name.Name
				if recv != "" {
					fullName = recv + "." + d.Name.Name
				}
				fid := hash(pkg + "." + fullName)
				insertIdent.Exec(fid, d.Name.Name, pkg, "func", relPath, pos.Line, fullName)

				identCount++

				// Edge: type -> method
				if recv != "" {
					tid := hash(pkg + "." + recv)
					insertEdge.Exec(tid, fid, "has_method", relPath, pos.Line)
					edgeCount++
				}

				// Walk function body for selectors and calls
				if d.Body != nil {
					ast.Inspect(d.Body, func(n ast.Node) bool {
						switch v := n.(type) {
						case *ast.SelectorExpr:
							selName := v.Sel.Name
							selPos := fset.Position(v.Pos())

							// Infer source type from receiver context
							srcType := ""
							if ident, ok := v.X.(*ast.Ident); ok {
								if ident.Name == "k" && currentRecvType != "" {
									srcType = currentRecvType
								} else {
									srcType = ident.Name
								}
							}

							if srcType != "" {
								// Edge: function -> accesses field
								fieldID := hash(pkg + "." + srcType + "." + selName)
								insertEdge.Exec(fid, fieldID, "accesses", relPath, selPos.Line)
								edgeCount++
							}

						case *ast.CallExpr:
							if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
								calleeName := sel.Sel.Name
								callPos := fset.Position(v.Pos())
								calleeID := hash(pkg + "." + calleeName)
								insertEdge.Exec(fid, calleeID, "calls", relPath, callPos.Line)
								edgeCount++
							}
						}
						return true
					})
				}
				_ = currentRecvType
			}
		}

		// Annotations
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				text := strings.TrimSpace(c.Text)
				if strings.Contains(text, "+stateify") || strings.Contains(text, "+checklocks") {
					pos := fset.Position(c.Pos())
					insertAnnotation.Exec(relPath, pos.Line, text)
				}
			}
		}

		return nil
	})

	tx.Commit()
	indexTime := time.Since(t0)
	fmt.Printf("Indexed %d files: %d identifiers, %d edges in %v\n\n", fileCount, identCount, edgeCount, indexTime)

	// Phase 2: Search
	fmt.Printf("=== SEARCH: %q ===\n\n", query)

	// Tokenize query
	terms := tokenize(query)
	fmt.Printf("Query terms: %v\n\n", terms)

	// Step 1: Find matching identifiers
	fmt.Println("Step 1: Symbol snapping")
	var seedIDs []uint64
	for _, term := range terms {
		if len(term) < 3 || isStop(term) {
			continue
		}

		// Exact match
		rows, _ := db.Query("SELECT id, name, kind, file, line, detail FROM identifiers WHERE LOWER(name) = LOWER(?)", term)
		for rows.Next() {
			var id uint64
			var name, kind, file, detail string
			var line int
			rows.Scan(&id, &name, &kind, &file, &line, &detail)
			fmt.Printf("  EXACT: [%s] %s at %s:%d\n", kind, name, file, line)
			seedIDs = append(seedIDs, id)
		}
		rows.Close()

		// Compound match (term appears as substring in name)
		rows, _ = db.Query("SELECT id, name, kind, file, line, detail FROM identifiers WHERE LOWER(name) LIKE ? OR LOWER(detail) LIKE ? LIMIT 10",
			"%"+strings.ToLower(term)+"%", "%"+strings.ToLower(term)+"%")
		for rows.Next() {
			var id uint64
			var name, kind, file, detail string
			var line int
			rows.Scan(&id, &name, &kind, &file, &line, &detail)
			fmt.Printf("  SUBSTR: [%s] %s at %s:%d (%s)\n", kind, name, file, line, detail)
			seedIDs = append(seedIDs, id)
		}
		rows.Close()
	}

	// Deduplicate seeds
	seen := make(map[uint64]bool)
	var uniqueSeeds []uint64
	for _, id := range seedIDs {
		if !seen[id] {
			seen[id] = true
			uniqueSeeds = append(uniqueSeeds, id)
		}
	}
	fmt.Printf("\n  %d unique seed symbols\n\n", len(uniqueSeeds))

	// Step 2: Walk graph from seeds (2 hops)
	fmt.Println("Step 2: Graph walk (2 hops)")
	visited := make(map[uint64]bool)
	type walkNode struct {
		id    uint64
		depth int
		edge  string
	}
	queue := make([]walkNode, 0)
	for _, id := range uniqueSeeds {
		queue = append(queue, walkNode{id, 0, "seed"})
	}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if visited[node.id] || node.depth > 2 {
			continue
		}
		visited[node.id] = true

		var name, kind, file, detail string
		var line int
		err := db.QueryRow("SELECT name, kind, file, line, detail FROM identifiers WHERE id = ?", node.id).Scan(&name, &kind, &file, &line, &detail)
		if err != nil {
			continue
		}

		indent := strings.Repeat("  ", node.depth+1)
		fmt.Printf("%s[%s] %s (%s) at %s:%d via %s\n", indent, kind, name, detail, file, line, node.edge)

		// Follow edges
		rows, _ := db.Query("SELECT dst_id, kind, file, line FROM edges WHERE src_id = ?", node.id)
		for rows.Next() {
			var dstID uint64
			var edgeKind, edgeFile string
			var edgeLine int
			rows.Scan(&dstID, &edgeKind, &edgeFile, &edgeLine)
			if !visited[dstID] {
				queue = append(queue, walkNode{dstID, node.depth + 1, edgeKind})
			}
		}
		rows.Close()

		// Also follow reverse edges
		rows, _ = db.Query("SELECT src_id, kind, file, line FROM edges WHERE dst_id = ?", node.id)
		for rows.Next() {
			var srcID uint64
			var edgeKind, edgeFile string
			var edgeLine int
			rows.Scan(&srcID, &edgeKind, &edgeFile, &edgeLine)
			if !visited[srcID] {
				queue = append(queue, walkNode{srcID, node.depth + 1, "rev_" + edgeKind})
			}
		}
		rows.Close()
	}

	fmt.Printf("\n=== DONE in %v (index: %v) ===\n", time.Since(t0), indexTime)
}

func mustExec(db *sql.DB, query string) {
	if _, err := db.Exec(query); err != nil {
		fmt.Fprintf(os.Stderr, "SQL error: %v\nQuery: %s\n", err, query)
		os.Exit(1)
	}
}

func tokenize(s string) []string {
	var words []string
	var current strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			current.WriteRune(r)
		} else if current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		words = append(words, current.String())
	}
	return words
}

func isStop(s string) bool {
	stops := map[string]bool{"the": true, "a": true, "an": true, "is": true, "was": true, "did": true, "do": true, "on": true, "in": true, "for": true, "to": true, "of": true, "not": true, "it": true, "with": true, "at": true, "by": true, "from": true}
	return stops[strings.ToLower(s)]
}
