// Spike: Test whether go/packages + go/ssa + VTA can process gVisor.
// Answers two questions:
// 1. Can VTA process gVisor in <16GB RAM and <10 min?
// 2. Does VTA produce the afterLoad callback edge for +stateify savable types?
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

func memUsageMB() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Alloc / 1024 / 1024
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: spike <module-dir>\n")
		fmt.Fprintf(os.Stderr, "  e.g.: spike /path/to/gvisor\n")
		os.Exit(1)
	}
	moduleDir := os.Args[1]

	fmt.Printf("=== VTA Spike on %s ===\n", moduleDir)
	fmt.Printf("Initial memory: %d MB\n\n", memUsageMB())

	// Step 1: Load packages
	fmt.Println("Step 1: Loading packages with go/packages...")
	t0 := time.Now()

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports | packages.NeedModule,
		Dir: moduleDir,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load packages: %v\n", err)
		os.Exit(1)
	}

	loadTime := time.Since(t0)
	fmt.Printf("  Loaded %d packages in %v\n", len(pkgs), loadTime)
	fmt.Printf("  Memory after load: %d MB\n\n", memUsageMB())

	// Count errors
	errCount := 0
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			errCount++
			if errCount <= 5 {
				fmt.Printf("  Error in %s: %v\n", pkg.PkgPath, e)
			}
		}
	}
	if errCount > 5 {
		fmt.Printf("  ... and %d more errors\n", errCount-5)
	}
	fmt.Println()

	// Step 2: Build SSA
	fmt.Println("Step 2: Building SSA...")
	t1 := time.Now()

	prog, ssaPkgs := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()

	ssaTime := time.Since(t1)
	fmt.Printf("  Built SSA for %d packages in %v\n", len(ssaPkgs), ssaTime)
	fmt.Printf("  Memory after SSA: %d MB\n\n", memUsageMB())

	// Step 3: Try CHA first (cheaper)
	fmt.Println("Step 3: Building CHA call graph...")
	t2 := time.Now()

	chaCG := cha.CallGraph(prog)
	chaTime := time.Since(t2)

	chaEdges := 0
	chaCG.DeleteSyntheticNodes()
	callgraph.GraphVisitEdges(chaCG, func(e *callgraph.Edge) error {
		chaEdges++
		return nil
	})
	fmt.Printf("  CHA: %d edges in %v\n", chaEdges, chaTime)
	fmt.Printf("  Memory after CHA: %d MB\n\n", memUsageMB())

	// Step 4: Check for afterLoad in CHA
	fmt.Println("Step 4: Searching for afterLoad edges in CHA...")
	afterLoadFound := false
	stateifyEdges := 0
	callgraph.GraphVisitEdges(chaCG, func(e *callgraph.Edge) error {
		calleeName := ""
		if e.Callee != nil && e.Callee.Func != nil {
			calleeName = e.Callee.Func.Name()
		}
		if strings.Contains(calleeName, "afterLoad") || strings.Contains(calleeName, "afterLoad") {
			afterLoadFound = true
			stateifyEdges++
			if stateifyEdges <= 10 {
				callerName := "?"
				if e.Caller != nil && e.Caller.Func != nil {
					callerName = e.Caller.Func.String()
				}
				fmt.Printf("  Found: %s -> %s\n", callerName, e.Callee.Func.String())
			}
		}
		return nil
	})
	fmt.Printf("  afterLoad edges in CHA: %d (found: %v)\n\n", stateifyEdges, afterLoadFound)

	// Step 5: Try VTA (more precise but more expensive)
	fmt.Println("Step 5: Building VTA call graph...")
	t3 := time.Now()

	allFuncs := ssautil.AllFunctions(prog)
	vtaCG := vta.CallGraph(allFuncs, chaCG)
	vtaTime := time.Since(t3)

	vtaEdges := 0
	vtaCG.DeleteSyntheticNodes()
	callgraph.GraphVisitEdges(vtaCG, func(e *callgraph.Edge) error {
		vtaEdges++
		return nil
	})
	fmt.Printf("  VTA: %d edges in %v\n", vtaEdges, vtaTime)
	fmt.Printf("  Memory after VTA: %d MB\n\n", memUsageMB())

	// Step 6: Check for afterLoad in VTA
	fmt.Println("Step 6: Searching for afterLoad edges in VTA...")
	afterLoadFoundVTA := false
	stateifyEdgesVTA := 0
	callgraph.GraphVisitEdges(vtaCG, func(e *callgraph.Edge) error {
		calleeName := ""
		if e.Callee != nil && e.Callee.Func != nil {
			calleeName = e.Callee.Func.Name()
		}
		if strings.Contains(calleeName, "afterLoad") {
			afterLoadFoundVTA = true
			stateifyEdgesVTA++
			if stateifyEdgesVTA <= 10 {
				callerName := "?"
				if e.Caller != nil && e.Caller.Func != nil {
					callerName = e.Caller.Func.String()
				}
				fmt.Printf("  Found: %s -> %s\n", callerName, e.Callee.Func.String())
			}
		}
		return nil
	})
	fmt.Printf("  afterLoad edges in VTA: %d (found: %v)\n\n", stateifyEdgesVTA, afterLoadFoundVTA)

	// Step 7: Search for SaveRestoreExecConfig references
	fmt.Println("Step 7: Searching for SaveRestoreExecConfig in SSA...")
	for fn := range allFuncs {
		if fn == nil || fn.Blocks == nil {
			continue
		}
		fnName := fn.String()
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				s := instr.String()
				if strings.Contains(s, "SaveRestoreExecConfig") || strings.Contains(s, "SaveRestoreExec") {
					fmt.Printf("  In %s: %s\n", fnName, s)
				}
			}
		}
	}

	// Summary
	fmt.Printf("\n=== SPIKE RESULTS ===\n")
	fmt.Printf("Packages loaded:     %d\n", len(pkgs))
	fmt.Printf("Load errors:         %d\n", errCount)
	fmt.Printf("Load time:           %v\n", loadTime)
	fmt.Printf("SSA build time:      %v\n", ssaTime)
	fmt.Printf("CHA time:            %v (edges: %d)\n", chaTime, chaEdges)
	fmt.Printf("VTA time:            %v (edges: %d)\n", vtaTime, vtaEdges)
	fmt.Printf("Peak memory:         %d MB\n", memUsageMB())
	fmt.Printf("afterLoad in CHA:    %v (%d edges)\n", afterLoadFound, stateifyEdges)
	fmt.Printf("afterLoad in VTA:    %v (%d edges)\n", afterLoadFoundVTA, stateifyEdgesVTA)
	fmt.Printf("\nVERDICT:\n")
	if memUsageMB() > 16000 {
		fmt.Println("  MEMORY: EXCEEDED 16GB - need CHA/RTA fallback or scoped analysis")
	} else {
		fmt.Println("  MEMORY: OK")
	}
	if loadTime+ssaTime+vtaTime > 10*time.Minute {
		fmt.Println("  TIME: EXCEEDED 10min - consider scoping or using CHA only")
	} else {
		fmt.Println("  TIME: OK")
	}
	if afterLoadFoundVTA {
		fmt.Println("  afterLoad: VTA FOUND IT - no synthetic edges needed")
	} else if afterLoadFound {
		fmt.Println("  afterLoad: CHA found it but VTA pruned it - need synthetic edges from codegen.go")
	} else {
		fmt.Println("  afterLoad: NEITHER FOUND IT - codegen.go synthetic edges are MANDATORY")
	}
}
