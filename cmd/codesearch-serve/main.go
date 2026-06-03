package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"

	"github.com/lokashrinav/codesearch/internal/mcp"
	"github.com/lokashrinav/codesearch/internal/storage"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := flag.String("db", "", "Path to codesearch SQLite database")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "Usage: codesearch-serve --db <path-to-db>")
		os.Exit(1)
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	reader := storage.NewReader(db)
	server := mcp.NewServerWithDB(db, reader)

	fmt.Fprintf(os.Stderr, "codesearch MCP server started (db: %s)\n", *dbPath)
	if err := server.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
