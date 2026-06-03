package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lokashrinav/codesearch/internal/storage"
)

func main() {
	moduleDir := flag.String("dir", ".", "Path to Go module directory")
	outputDB := flag.String("out", "codesearch.db", "Output SQLite database path")
	flag.Parse()

	fmt.Printf("Indexing %s -> %s\n", *moduleDir, *outputDB)
	t0 := time.Now()

	// Remove existing DB
	os.Remove(*outputDB)

	db, err := storage.OpenDB(*outputDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	storage.SetMeta(db, "indexed_at", time.Now().Format(time.RFC3339))
	storage.SetMeta(db, "module_dir", *moduleDir)

	// TODO: Wire up extractor here
	// extractor.Extract(moduleDir) -> facts -> storage.Writer -> db
	fmt.Printf("Database created. Schema ready.\n")
	fmt.Printf("Extractor not yet implemented. Run the spike first:\n")
	fmt.Printf("  go run ./cmd/spike %s\n", *moduleDir)
	fmt.Printf("Elapsed: %v\n", time.Since(t0))
}
