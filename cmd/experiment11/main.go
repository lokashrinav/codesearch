// Experiment 11: Auto-index from go.mod.
// Given a go.mod file, automatically index the module and its top N dependencies.
// Tests: Can we automatically discover and index the right code?
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lokashrinav/codesearch/internal/extractor"
	"github.com/lokashrinav/codesearch/internal/query"
	"github.com/lokashrinav/codesearch/internal/storage"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: experiment11 <go-module-dir> <query>\n")
		os.Exit(1)
	}
	moduleDir := os.Args[1]
	searchQuery := os.Args[2]
	apiKey := os.Getenv("ANTHROPIC_API_KEY")

	fmt.Printf("=== Experiment 11: Auto-Index from go.mod ===\n")
	fmt.Printf("Module: %s\nQuery: %q\n\n", moduleDir, searchQuery)

	// Step 1: Read go.mod to find dependencies
	gomodPath := filepath.Join(moduleDir, "go.mod")
	deps, err := parseGoMod(gomodPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse go.mod: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Found %d dependencies in go.mod\n", len(deps))

	// Step 2: Find dependency source in module cache
	gomodcache := os.Getenv("GOMODCACHE")
	if gomodcache == "" {
		gomodcache = filepath.Join(os.Getenv("GOPATH"), "pkg", "mod")
		if os.Getenv("GOPATH") == "" {
			home, _ := os.UserHomeDir()
			gomodcache = filepath.Join(home, "go", "pkg", "mod")
		}
	}
	fmt.Printf("Module cache: %s\n\n", gomodcache)

	// Step 3: Download deps if needed
	fmt.Println("Ensuring dependencies are downloaded...")
	cmd := exec.Command("go", "mod", "download")
	cmd.Dir = moduleDir
	cmd.Run() // ignore errors, deps might already be cached

	// Step 4: Index the module itself + top dependencies
	dbPath := "experiment11.db"
	os.Remove(dbPath)
	db, err := storage.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "DB error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	defer os.Remove(dbPath)

	t0 := time.Now()

	// Index the module itself
	fmt.Printf("Indexing module: %s\n", moduleDir)
	stats, err := extractor.Index(db, moduleDir, nil)
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
	} else {
		fmt.Printf("  %s\n", stats)
	}

	// Index top dependencies (limit to avoid huge index times)
	maxDeps := 10
	indexed := 0
	for _, dep := range deps {
		if indexed >= maxDeps {
			break
		}

		// Convert module path to filesystem path in cache
		depDir := modulePathToDir(gomodcache, dep.path, dep.version)
		if _, err := os.Stat(depDir); os.IsNotExist(err) {
			// Try without the version encoding
			depDir = filepath.Join(gomodcache, dep.path+"@"+dep.version)
			if _, err := os.Stat(depDir); os.IsNotExist(err) {
				continue
			}
		}

		fmt.Printf("Indexing dep: %s@%s\n", dep.path, dep.version)
		depStats, err := extractor.Index(db, depDir, nil)
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			continue
		}
		fmt.Printf("  %s\n", depStats)
		indexed++
	}

	indexTime := time.Since(t0)
	fmt.Printf("\nTotal index time: %v\n\n", indexTime)

	// Step 5: Search
	subgraph, err := query.BuildSubgraph(db, searchQuery, 25)
	if err != nil {
		fmt.Printf("Search error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Subgraph: %d chars\n\n", len(subgraph))

	if apiKey != "" {
		trace, err := query.LLMTrace(query.LLMConfig{APIKey: apiKey}, searchQuery, subgraph)
		if err != nil {
			fmt.Printf("LLM error: %v\n", err)
		} else {
			fmt.Println("=== LLM TRACE ===\n")
			fmt.Println(trace)
		}
	} else {
		fmt.Println(subgraph[:min(3000, len(subgraph))])
	}

	fmt.Printf("\n=== DONE in %v ===\n", time.Since(t0))
}

type dependency struct {
	path    string
	version string
}

func parseGoMod(path string) ([]dependency, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var deps []dependency
	inRequire := false
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "require (" {
			inRequire = true
			continue
		}
		if line == ")" {
			inRequire = false
			continue
		}

		if inRequire || strings.HasPrefix(line, "require ") {
			// Parse "module/path v1.2.3"
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				path := parts[0]
				if path == "require" && len(parts) >= 3 {
					path = parts[1]
					parts[1] = parts[2]
				}
				version := parts[1]

				// Skip indirect dependencies for now
				if strings.Contains(line, "// indirect") {
					continue
				}

				deps = append(deps, dependency{path: path, version: version})
			}
		}
	}

	return deps, scanner.Err()
}

func modulePathToDir(cache, modPath, version string) string {
	// Go module cache uses case-encoded paths
	// e.g., github.com/User/Repo -> github.com/!user/!repo
	encoded := encodeModulePath(modPath)
	return filepath.Join(cache, encoded+"@"+version)
}

func encodeModulePath(path string) string {
	var buf strings.Builder
	for _, r := range path {
		if r >= 'A' && r <= 'Z' {
			buf.WriteByte('!')
			buf.WriteRune(r + 32) // lowercase
		} else {
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
