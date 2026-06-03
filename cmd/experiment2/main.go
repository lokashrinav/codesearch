// Experiment 2: Direct AST + types extraction — skip SSA, test raw fact extraction.
// Validates: can we extract identifiers, struct fields, and references
// from go/packages alone (without SSA)?
package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: experiment2 <module-dir> <package-pattern>\n")
		os.Exit(1)
	}
	moduleDir := os.Args[1]
	pattern := os.Args[2]

	fmt.Printf("=== Experiment 2: AST + Types Extraction ===\n")
	fmt.Printf("Module: %s, Pattern: %s\n\n", moduleDir, pattern)

	t0 := time.Now()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports | packages.NeedModule,
		Dir: moduleDir,
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Load failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Loaded %d packages in %v\n\n", len(pkgs), time.Since(t0))

	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			for _, e := range pkg.Errors {
				fmt.Printf("  ERROR: %v\n", e)
			}
			continue
		}

		fmt.Printf("--- Package: %s (%d files) ---\n", pkg.PkgPath, len(pkg.Syntax))

		// 1. Extract exported identifiers from scope
		scope := pkg.Types.Scope()
		fmt.Printf("\n  EXPORTED IDENTIFIERS:\n")
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			pos := pkg.Fset.Position(obj.Pos())
			fmt.Printf("    [%s] %s at %s:%d\n", classifyObj(obj), name, pos.Filename, pos.Line)

			// Struct fields
			if tn, ok := obj.(*types.TypeName); ok {
				if st, ok := tn.Type().Underlying().(*types.Struct); ok {
					for i := 0; i < st.NumFields(); i++ {
						f := st.Field(i)
						fpos := pkg.Fset.Position(f.Pos())
						tag := st.Tag(i)
						marker := ""
						if strings.Contains(f.Name(), "SaveRestore") {
							marker = " *** TARGET ***"
						}
						if tag != "" {
							fmt.Printf("      FIELD %s.%s [tag: %s] at %s:%d%s\n", name, f.Name(), tag, fpos.Filename, fpos.Line, marker)
						} else {
							fmt.Printf("      FIELD %s.%s at %s:%d%s\n", name, f.Name(), fpos.Filename, fpos.Line, marker)
						}
					}
				}
				// Interface methods
				if iface, ok := tn.Type().Underlying().(*types.Interface); ok {
					for i := 0; i < iface.NumMethods(); i++ {
						m := iface.Method(i)
						fmt.Printf("      METHOD %s.%s\n", name, m.Name())
					}
				}
			}
		}

		// 2. Walk AST for comments containing +stateify
		fmt.Printf("\n  STATEIFY ANNOTATIONS:\n")
		for _, file := range pkg.Syntax {
			for _, cg := range file.Comments {
				for _, c := range cg.List {
					if strings.Contains(c.Text, "+stateify") {
						pos := pkg.Fset.Position(c.Pos())
						fmt.Printf("    %s at %s:%d\n", strings.TrimSpace(c.Text), pos.Filename, pos.Line)
					}
				}
			}
		}

		// 3. Walk AST for function declarations and find references to SaveRestoreExecConfig
		fmt.Printf("\n  FUNCTIONS WITH SAVERESTORE REFERENCES:\n")
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.FuncDecl:
					fnName := v.Name.Name
					// Check if function body references SaveRestore
					if v.Body != nil {
						src := nodeToString(pkg.Fset, v.Body)
						if strings.Contains(src, "SaveRestore") {
							pos := pkg.Fset.Position(v.Pos())
							fmt.Printf("    FUNC %s at %s:%d references SaveRestore\n", fnName, pos.Filename, pos.Line)
						}
					}
				case *ast.SelectorExpr:
					if v.Sel != nil && strings.Contains(v.Sel.Name, "SaveRestore") {
						pos := pkg.Fset.Position(v.Pos())
						fmt.Printf("    SELECTOR .%s at %s:%d\n", v.Sel.Name, pos.Filename, pos.Line)
					}
				}
				return true
			})
		}

		// 4. Use TypesInfo to find all uses of specific objects
		fmt.Printf("\n  TYPESINFO USES (SaveRestore-related):\n")
		for ident, obj := range pkg.TypesInfo.Uses {
			if strings.Contains(obj.Name(), "SaveRestore") || strings.Contains(obj.Name(), "afterLoad") || strings.Contains(obj.Name(), "beforeSave") {
				pos := pkg.Fset.Position(ident.Pos())
				fmt.Printf("    USE %s (type: %s) at %s:%d\n", obj.Name(), classifyObj(obj), pos.Filename, pos.Line)
			}
		}

		// 5. Find flag-related calls
		fmt.Printf("\n  FLAG-RELATED CALLS:\n")
		for ident, obj := range pkg.TypesInfo.Uses {
			name := obj.Name()
			if strings.Contains(name, "StringVar") || strings.Contains(name, "BoolVar") ||
				strings.Contains(name, "IntVar") || strings.Contains(name, "DurationVar") ||
				name == "Flag" || name == "Flags" {
				pos := pkg.Fset.Position(ident.Pos())
				fmt.Printf("    %s at %s:%d\n", name, pos.Filename, pos.Line)
			}
		}
	}

	fmt.Printf("\n=== DONE in %v ===\n", time.Since(t0))
}

func classifyObj(obj types.Object) string {
	switch obj.(type) {
	case *types.Func:
		return "func"
	case *types.TypeName:
		return "type"
	case *types.Var:
		return "var"
	case *types.Const:
		return "const"
	default:
		return "other"
	}
}

func nodeToString(fset *token.FileSet, node ast.Node) string {
	// Quick and dirty: just check the source range
	start := fset.Position(node.Pos())
	end := fset.Position(node.End())
	_ = start
	_ = end
	// Can't easily get source text without reading the file,
	// so we'll use ast.Inspect for targeted checks instead
	var buf strings.Builder
	ast.Inspect(node, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			buf.WriteString(sel.Sel.Name)
			buf.WriteString(" ")
		}
		if ident, ok := n.(*ast.Ident); ok {
			buf.WriteString(ident.Name)
			buf.WriteString(" ")
		}
		return true
	})
	return buf.String()
}
