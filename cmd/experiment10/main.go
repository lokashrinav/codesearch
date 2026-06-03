// Experiment 10: Cross-dependency search.
// Index the autoresearch-gpu project AND its key dependency (anthropic SDK)
// to test if the search can cross the project→dependency boundary.
// Query: "API calls failing with max_tokens error" — answer is in the anthropic SDK.
package main

import (
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
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: experiment10 <query>\n")
		os.Exit(1)
	}
	searchQuery := os.Args[1]

	fmt.Printf("=== Experiment 10: Cross-Dependency Search ===\n")
	fmt.Printf("Query: %q\n\n", searchQuery)

	// Find Python site-packages for anthropic SDK
	out, err := exec.Command("python", "-c", "import anthropic; import os; print(os.path.dirname(anthropic.__file__))").Output()
	if err != nil {
		fmt.Printf("Can't find anthropic SDK: %v\n", err)
		os.Exit(1)
	}
	anthropicPath := strings.TrimSpace(string(out))
	fmt.Printf("Anthropic SDK: %s\n", anthropicPath)

	// We'll index the autoresearch-gpu project as Go files won't work here
	// Instead, let's use the Python extractor approach but in Go
	// Actually, let's just test cross-directory Go indexing

	// Index multiple Go directories into one DB
	dbPath := "experiment10.db"
	os.Remove(dbPath)
	db, err := storage.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "DB error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	defer os.Remove(dbPath)

	// Index gVisor's sentry + control + nvproxy (the project)
	// AND gVisor's pkg/state (the dependency that handles stateify)
	gvisorDir := `C:\Users\lokas\gvisor`

	dirs := []string{
		filepath.Join(gvisorDir, "pkg", "sentry", "kernel"),
		filepath.Join(gvisorDir, "pkg", "sentry", "control"),
		filepath.Join(gvisorDir, "pkg", "sentry", "devices", "nvproxy"),
		filepath.Join(gvisorDir, "pkg", "state"),
		filepath.Join(gvisorDir, "runsc", "cmd"),
		filepath.Join(gvisorDir, "runsc", "boot"),
	}

	t0 := time.Now()
	var totalStats extractor.Stats

	for _, dir := range dirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			fmt.Printf("  Skipping (not found): %s\n", dir)
			continue
		}
		stats, err := extractor.Index(db, dir, nil)
		if err != nil {
			fmt.Printf("  Error indexing %s: %v\n", dir, err)
			continue
		}
		fmt.Printf("  Indexed %s: %s\n", filepath.Base(dir), stats)
		totalStats.Files += stats.Files
		totalStats.Idents += stats.Idents
		totalStats.Edges += stats.Edges
		totalStats.Annots += stats.Annots
	}

	indexTime := time.Since(t0)
	fmt.Printf("\nTotal: %d files, %d idents, %d edges, %d annotations in %v\n\n",
		totalStats.Files, totalStats.Idents, totalStats.Edges, totalStats.Annots, indexTime)

	// Search using subgraph extraction
	subgraph, err := query.BuildSubgraph(db, searchQuery, 25)
	if err != nil {
		fmt.Printf("Search error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Subgraph: %d chars\n\n", len(subgraph))

	// Use LLM to trace
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey != "" {
		trace, err := query.LLMTrace(query.LLMConfig{APIKey: apiKey}, searchQuery, subgraph)
		if err != nil {
			fmt.Printf("LLM error: %v\n", err)
		} else {
			fmt.Println("=== LLM TRACE ===\n")
			fmt.Println(trace)
		}
	} else {
		fmt.Println("No API key — printing subgraph only:")
		fmt.Println(subgraph[:3000])
	}

	fmt.Printf("\n=== DONE in %v ===\n", time.Since(t0))
}
