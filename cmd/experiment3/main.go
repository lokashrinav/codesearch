// Experiment 3: Raw Go parser extraction (no type-checking).
// Works on any Go source file regardless of platform or missing dependencies.
// Extracts: identifiers, struct fields, function names, comments (+stateify),
// selector expressions, and string literals (flag names).
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Fact struct {
	Kind    string
	Name    string
	File    string
	Line    int
	Detail  string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: experiment3 <directory>\n")
		os.Exit(1)
	}
	dir := os.Args[1]

	fmt.Printf("=== Experiment 3: Raw Parser Extraction ===\n")
	fmt.Printf("Directory: %s\n\n", dir)

	t0 := time.Now()
	fset := token.NewFileSet()
	var facts []Fact
	fileCount := 0

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip test files and generated files for now
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil // skip unparseable files
		}
		fileCount++
		relPath, _ := filepath.Rel(dir, path)

		// 1. Package-level declarations
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						pos := fset.Position(s.Pos())
						facts = append(facts, Fact{"TYPE", s.Name.Name, relPath, pos.Line, ""})

						// Struct fields
						if st, ok := s.Type.(*ast.StructType); ok && st.Fields != nil {
							for _, field := range st.Fields.List {
								for _, name := range field.Names {
									fpos := fset.Position(name.Pos())
									tag := ""
									if field.Tag != nil {
										tag = field.Tag.Value
									}
									facts = append(facts, Fact{"FIELD", s.Name.Name + "." + name.Name, relPath, fpos.Line, tag})
								}
							}
						}

						// Interface methods
						if iface, ok := s.Type.(*ast.InterfaceType); ok && iface.Methods != nil {
							for _, method := range iface.Methods.List {
								for _, name := range method.Names {
									mpos := fset.Position(name.Pos())
									facts = append(facts, Fact{"IMETHOD", s.Name.Name + "." + name.Name, relPath, mpos.Line, ""})
								}
							}
						}

					case *ast.ValueSpec:
						for _, name := range s.Names {
							pos := fset.Position(name.Pos())
							kind := "VAR"
							if d.Tok.String() == "const" {
								kind = "CONST"
							}
							facts = append(facts, Fact{kind, name.Name, relPath, pos.Line, ""})
						}
					}
				}

			case *ast.FuncDecl:
				pos := fset.Position(d.Pos())
				recv := ""
				if d.Recv != nil && len(d.Recv.List) > 0 {
					if t, ok := d.Recv.List[0].Type.(*ast.StarExpr); ok {
						if ident, ok := t.X.(*ast.Ident); ok {
							recv = ident.Name + "."
						}
					} else if ident, ok := d.Recv.List[0].Type.(*ast.Ident); ok {
						recv = ident.Name + "."
					}
				}
				facts = append(facts, Fact{"FUNC", recv + d.Name.Name, relPath, pos.Line, ""})
			}
		}

		// 2. Comments with +stateify or other annotations
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				text := strings.TrimSpace(c.Text)
				if strings.Contains(text, "+stateify") || strings.Contains(text, "+checklocks") || strings.Contains(text, "go:generate") {
					pos := fset.Position(c.Pos())
					facts = append(facts, Fact{"ANNOTATION", text, relPath, pos.Line, ""})
				}
			}
		}

		// 3. Selector expressions referencing interesting names
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				name := v.Sel.Name
				if strings.Contains(name, "SaveRestore") || strings.Contains(name, "afterLoad") ||
					strings.Contains(name, "beforeSave") || strings.Contains(name, "Checkpoint") {
					pos := fset.Position(v.Pos())
					xName := ""
					if ident, ok := v.X.(*ast.Ident); ok {
						xName = ident.Name
					}
					facts = append(facts, Fact{"SELECTOR", xName + "." + name, relPath, pos.Line, ""})
				}

			case *ast.CallExpr:
				// Check for flag-related function calls
				if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
					fname := sel.Sel.Name
					if strings.Contains(fname, "StringVar") || strings.Contains(fname, "BoolVar") ||
						strings.Contains(fname, "IntVar") || strings.Contains(fname, "DurationVar") ||
						fname == "String" || fname == "Bool" || fname == "Int" {
						if xIdent, ok := sel.X.(*ast.Ident); ok {
							if xIdent.Name == "flag" || strings.Contains(xIdent.Name, "flag") || strings.Contains(xIdent.Name, "Flag") {
								pos := fset.Position(v.Pos())
								// Try to extract flag name from first string argument
								flagName := ""
								for _, arg := range v.Args {
									if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
										flagName = strings.Trim(lit.Value, "\"")
										break
									}
								}
								facts = append(facts, Fact{"FLAG_CALL", xIdent.Name + "." + fname, relPath, pos.Line, flagName})
							}
						}
					}
				}
			}
			return true
		})

		return nil
	})

	// Print results
	fmt.Printf("Parsed %d files in %v\n", fileCount, time.Since(t0))
	fmt.Printf("Extracted %d facts\n\n", len(facts))

	// Group by kind
	kindCounts := make(map[string]int)
	for _, f := range facts {
		kindCounts[f.Kind]++
	}
	fmt.Println("Fact counts by kind:")
	for kind, count := range kindCounts {
		fmt.Printf("  %s: %d\n", kind, count)
	}

	// Print SaveRestore-related facts
	fmt.Println("\nSaveRestore-related facts:")
	for _, f := range facts {
		if strings.Contains(f.Name, "SaveRestore") || strings.Contains(f.Name, "afterLoad") ||
			strings.Contains(f.Name, "beforeSave") || strings.Contains(f.Name, "Checkpoint") ||
			strings.Contains(f.Detail, "save") || strings.Contains(f.Detail, "restore") {
			fmt.Printf("  [%s] %s at %s:%d", f.Kind, f.Name, f.File, f.Line)
			if f.Detail != "" {
				fmt.Printf(" (%s)", f.Detail)
			}
			fmt.Println()
		}
	}

	// Print stateify annotations
	fmt.Println("\nStateify annotations:")
	for _, f := range facts {
		if f.Kind == "ANNOTATION" && strings.Contains(f.Name, "stateify") {
			fmt.Printf("  %s at %s:%d\n", f.Name, f.File, f.Line)
		}
	}

	// Print flag bindings
	fmt.Println("\nFlag bindings:")
	for _, f := range facts {
		if f.Kind == "FLAG_CALL" {
			fmt.Printf("  %s at %s:%d (flag: %s)\n", f.Name, f.File, f.Line, f.Detail)
		}
	}

	fmt.Printf("\n=== DONE in %v ===\n", time.Since(t0))
}
