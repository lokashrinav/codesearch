package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lokashrinav/codesearch/internal/extractor"
	"github.com/lokashrinav/codesearch/internal/storage"
)

func main() {
	moduleDir := flag.String("dir", ".", "Path to Go source directory")
	outputDB := flag.String("out", "codesearch.db", "Output SQLite database path")
	flag.Parse()

	fmt.Printf("codesearch-index: %s -> %s\n", *moduleDir, *outputDB)

	os.Remove(*outputDB)
	db, err := storage.OpenDB(*outputDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	storage.SetMeta(db, "indexed_at", time.Now().Format(time.RFC3339))
	storage.SetMeta(db, "source_dir", *moduleDir)

	stats, err := extractor.Index(db, *moduleDir, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Indexing failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Done: %s\n", stats)
}
