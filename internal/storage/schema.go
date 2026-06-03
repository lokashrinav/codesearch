package storage

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const ddl = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT
);

CREATE TABLE IF NOT EXISTS edges (
	src_id INTEGER NOT NULL,
	dst_id INTEGER NOT NULL,
	kind   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS annotations (
	file_path TEXT,
	line      INTEGER,
	text      TEXT,
	near_type TEXT
);

CREATE TABLE IF NOT EXISTS identifiers (
	id        INTEGER PRIMARY KEY,
	name      TEXT NOT NULL,
	pkg_path  TEXT NOT NULL,
	kind      INTEGER NOT NULL,
	file_path TEXT,
	line      INTEGER,
	col       INTEGER,
	doc       TEXT
);
CREATE INDEX IF NOT EXISTS idx_ident_name ON identifiers(name);
CREATE INDEX IF NOT EXISTS idx_ident_pkg ON identifiers(pkg_path);
CREATE INDEX IF NOT EXISTS idx_ident_kind ON identifiers(kind);

CREATE VIRTUAL TABLE IF NOT EXISTS ident_fts USING fts5(
	name, doc, pkg_path,
	content=identifiers,
	content_rowid=id,
	tokenize="trigram"
);

CREATE TABLE IF NOT EXISTS def_refs (
	def_id   INTEGER NOT NULL,
	ref_id   INTEGER NOT NULL,
	ref_kind INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_defref_def ON def_refs(def_id);
CREATE INDEX IF NOT EXISTS idx_defref_ref ON def_refs(ref_id);

CREATE TABLE IF NOT EXISTS call_edges (
	caller_id INTEGER NOT NULL,
	callee_id INTEGER NOT NULL,
	file_path TEXT,
	line      INTEGER,
	dispatch  INTEGER NOT NULL,
	arg_map   BLOB
);
CREATE INDEX IF NOT EXISTS idx_call_caller ON call_edges(caller_id);
CREATE INDEX IF NOT EXISTS idx_call_callee ON call_edges(callee_id);

CREATE TABLE IF NOT EXISTS field_ops (
	struct_type TEXT NOT NULL,
	field_name  TEXT NOT NULL,
	field_idx   INTEGER NOT NULL,
	op_kind     INTEGER NOT NULL,
	func_id     INTEGER NOT NULL,
	file_path   TEXT,
	line        INTEGER
);
CREATE INDEX IF NOT EXISTS idx_fieldop_type ON field_ops(struct_type, field_name);
CREATE INDEX IF NOT EXISTS idx_fieldop_func ON field_ops(func_id);

CREATE TABLE IF NOT EXISTS flag_bindings (
	flag_name   TEXT NOT NULL,
	flag_pkg    TEXT NOT NULL,
	bound_to_id INTEGER NOT NULL,
	default_val TEXT,
	usage       TEXT,
	func_id     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_flag_name ON flag_bindings(flag_name);
CREATE INDEX IF NOT EXISTS idx_flag_bound ON flag_bindings(bound_to_id);

CREATE TABLE IF NOT EXISTS data_flow (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	from_func INTEGER NOT NULL,
	from_slot BLOB NOT NULL,
	to_func   INTEGER NOT NULL,
	to_slot   BLOB NOT NULL,
	flow_kind INTEGER NOT NULL,
	condition TEXT
);
CREATE INDEX IF NOT EXISTS idx_df_from ON data_flow(from_func);
CREATE INDEX IF NOT EXISTS idx_df_to ON data_flow(to_func);

CREATE TABLE IF NOT EXISTS func_summaries (
	func_id INTEGER PRIMARY KEY,
	summary BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS codegen (
	source_id    INTEGER NOT NULL,
	generated_id INTEGER NOT NULL,
	gen_kind     INTEGER NOT NULL,
	pattern      TEXT
);
CREATE INDEX IF NOT EXISTS idx_codegen_source ON codegen(source_id);
CREATE INDEX IF NOT EXISTS idx_codegen_gen ON codegen(generated_id);

CREATE TABLE IF NOT EXISTS type_facts (
	type_id    INTEGER NOT NULL,
	fact_kind  INTEGER NOT NULL,
	related_id INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_typefact_type ON type_facts(type_id);
CREATE INDEX IF NOT EXISTS idx_typefact_related ON type_facts(related_id);

CREATE TABLE IF NOT EXISTS external_refs (
	local_id INTEGER NOT NULL,
	ext_pkg  TEXT NOT NULL,
	ext_name TEXT NOT NULL,
	ext_kind INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_extref_pkg ON external_refs(ext_pkg, ext_name);

PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
`

// OpenDB opens or creates a codesearch SQLite database.
func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if _, err := db.Exec(ddl); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return db, nil
}

// SetMeta stores a metadata key-value pair.
func SetMeta(db *sql.DB, key, value string) error {
	_, err := db.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)", key, value)
	return err
}

// GetMeta retrieves a metadata value.
func GetMeta(db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&value)
	return value, err
}
