package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/lokashrinav/codesearch/internal/mcp"
	"github.com/lokashrinav/codesearch/internal/storage"
)

func main() {
	dbPath := flag.String("db", "", "Path to codesearch SQLite database")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "Usage: codesearch-serve --db <path-to-db>")
		os.Exit(1)
	}

	db, err := storage.OpenDB(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	reader := storage.NewReader(db)
	server := mcp.NewServer(reader)

	if err := server.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
