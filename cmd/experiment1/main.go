// Experiment 1: Test fact extraction on a small Go package.
// Validates: can we extract identifiers, call edges, field ops from go/packages + SSA?
// Target: gVisor's pkg/sentry/kernel (the package with SaveRestoreExecConfig)
package main

import (
	"fmt"
	"go/token"
	"go/types"
	"os"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: experiment1 <module-dir> <package-pattern>\n")
		fmt.Fprintf(os.Stderr, "  e.g.: experiment1 /path/to/gvisor ./pkg/sentry/kernel\n")
		os.Exit(1)
	}
	moduleDir := os.Args[1]
	pattern := os.Args[2]

	fmt.Printf("=== Experiment 1: Fact Extraction ===\n")
	fmt.Printf("Module: %s\n", moduleDir)
	fmt.Printf("Pattern: %s\n\n", pattern)

	// Load packages
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

	// Build SSA for loaded packages only
	t1 := time.Now()
	prog, ssaPkgs := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	fmt.Printf("SSA built in %v\n\n", time.Since(t1))

	for _, ssaPkg := range ssaPkgs {
		if ssaPkg == nil {
			continue
		}
		fmt.Printf("--- Package: %s ---\n", ssaPkg.Pkg.Path())

		// Extract identifiers
		identCount := 0
		scope := ssaPkg.Pkg.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			pos := prog.Fset.Position(obj.Pos())
			kind := classifyObj(obj)
			fmt.Printf("  IDENT [%s] %s at %s:%d\n", kind, name, pos.Filename, pos.Line)
			identCount++

			// For struct types, extract fields
			if tn, ok := obj.(*types.TypeName); ok {
				if st, ok := tn.Type().Underlying().(*types.Struct); ok {
					for i := 0; i < st.NumFields(); i++ {
						f := st.Field(i)
						fpos := prog.Fset.Position(f.Pos())
						fmt.Printf("    FIELD %s.%s at %s:%d\n", name, f.Name(), fpos.Filename, fpos.Line)

						// Check for stateify annotations
						if strings.Contains(name, "SaveRestore") || strings.Contains(f.Name(), "SaveRestore") {
							fmt.Printf("    *** FOUND SaveRestore-related: %s.%s ***\n", name, f.Name())
						}
					}
				}
			}
		}
		fmt.Printf("  Total identifiers: %d\n\n", identCount)

		// Extract function facts
		for _, member := range ssaPkg.Members {
			fn, ok := member.(*ssa.Function)
			if !ok {
				continue
			}
			if fn.Blocks == nil {
				continue
			}

			// Count call edges and field ops
			calls := 0
			fieldOps := 0
			flagBindings := 0

			for _, block := range fn.Blocks {
				for _, instr := range block.Instrs {
					switch v := instr.(type) {
					case *ssa.Call:
						calls++
						// Check for flag-related calls
						calleeName := ""
						if sc := v.Call.StaticCallee(); sc != nil {
							calleeName = sc.Name()
						}
						if strings.Contains(calleeName, "StringVar") || strings.Contains(calleeName, "BoolVar") {
							flagBindings++
							fmt.Printf("  FLAG_BIND in %s: %s\n", fn.Name(), v.String())
						}

					case *ssa.FieldAddr:
						fieldOps++
						// Check what field is being accessed
						structType := v.X.Type().String()
						if strings.Contains(structType, "SaveRestore") || strings.Contains(structType, "Kernel") {
							fmt.Printf("  FIELD_OP in %s: %s (field %d)\n", fn.Name(), v.String(), v.Field)
						}

					case *ssa.Store:
						// Check if storing to a field
						if fa, ok := v.Addr.(*ssa.FieldAddr); ok {
							structType := fa.X.Type().String()
							if strings.Contains(structType, "SaveRestore") || strings.Contains(structType, "Kernel") {
								fmt.Printf("  FIELD_WRITE in %s: storing to field %d of %s\n", fn.Name(), fa.Field, structType)
							}
						}
					}
				}
			}

			if calls > 0 || fieldOps > 0 || flagBindings > 0 {
				fmt.Printf("  FUNC %s: %d calls, %d field_ops, %d flag_bindings\n", fn.Name(), calls, fieldOps, flagBindings)
			}
		}

		// Look for +stateify comments
		for _, file := range ssaPkg.Pkg.Scope().Names() {
			obj := scope.Lookup(file)
			if tn, ok := obj.(*types.TypeName); ok {
				pos := prog.Fset.Position(tn.Pos())
				if pos.IsValid() {
					// Read the source file to check for stateify comments
					// (In a real implementation, we'd parse the AST comments)
					_ = pos
				}
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

func posString(fset *token.FileSet, pos token.Pos) string {
	p := fset.Position(pos)
	return fmt.Sprintf("%s:%d", p.Filename, p.Line)
}
