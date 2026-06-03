package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Writer batches fact inserts into the SQLite database.
type Writer struct {
	db *sql.DB
	tx *sql.Tx
}

// NewWriter creates a writer with an active transaction.
func NewWriter(db *sql.DB) (*Writer, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	return &Writer{db: db, tx: tx}, nil
}

// Commit commits the batch transaction.
func (w *Writer) Commit() error {
	return w.tx.Commit()
}

// Rollback aborts the batch transaction.
func (w *Writer) Rollback() error {
	return w.tx.Rollback()
}

func (w *Writer) WriteIdentifier(id Identifier) error {
	_, err := w.tx.Exec(
		`INSERT OR IGNORE INTO identifiers (id, name, pkg_path, kind, file_path, line, col, doc) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id.ID, id.Name, id.PkgPath, id.Kind, id.File, id.Line, id.Col, id.Doc,
	)
	if err != nil {
		return fmt.Errorf("write identifier %s: %w", id.Name, err)
	}
	// Update FTS index
	_, err = w.tx.Exec(
		`INSERT INTO ident_fts (rowid, name, doc, pkg_path) VALUES (?, ?, ?, ?)`,
		id.ID, id.Name, id.Doc, id.PkgPath,
	)
	return err
}

func (w *Writer) WriteDefRef(dr DefRef) error {
	_, err := w.tx.Exec(
		`INSERT INTO def_refs (def_id, ref_id, ref_kind) VALUES (?, ?, ?)`,
		dr.DefID, dr.RefID, dr.RefKind,
	)
	return err
}

func (w *Writer) WriteCallEdge(ce CallEdge) error {
	argMapJSON, _ := json.Marshal(ce.ArgMap)
	_, err := w.tx.Exec(
		`INSERT INTO call_edges (caller_id, callee_id, file_path, line, dispatch, arg_map) VALUES (?, ?, ?, ?, ?, ?)`,
		ce.CallerID, ce.CalleeID, ce.File, ce.Line, ce.Dispatch, argMapJSON,
	)
	return err
}

func (w *Writer) WriteFieldOp(fo FieldOp) error {
	_, err := w.tx.Exec(
		`INSERT INTO field_ops (struct_type, field_name, field_idx, op_kind, func_id, file_path, line) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		fo.StructType, fo.FieldName, fo.FieldIdx, fo.OpKind, fo.FuncID, fo.File, fo.Line,
	)
	return err
}

func (w *Writer) WriteFlagBinding(fb FlagBinding) error {
	_, err := w.tx.Exec(
		`INSERT INTO flag_bindings (flag_name, flag_pkg, bound_to_id, default_val, usage, func_id) VALUES (?, ?, ?, ?, ?, ?)`,
		fb.FlagName, fb.FlagPkg, fb.BoundToID, fb.DefaultVal, fb.Usage, fb.FuncID,
	)
	return err
}

func (w *Writer) WriteDataFlow(df DataFlowEdge) error {
	fromSlot, _ := json.Marshal(df.FromSlot)
	toSlot, _ := json.Marshal(df.ToSlot)
	_, err := w.tx.Exec(
		`INSERT INTO data_flow (from_func, from_slot, to_func, to_slot, flow_kind, condition) VALUES (?, ?, ?, ?, ?, ?)`,
		df.FromFunc, fromSlot, df.ToFunc, toSlot, df.FlowKind, df.Condition,
	)
	return err
}

func (w *Writer) WriteFuncSummary(funcID uint64, summary FuncSummary) error {
	blob, _ := json.Marshal(summary)
	_, err := w.tx.Exec(
		`INSERT OR REPLACE INTO func_summaries (func_id, summary) VALUES (?, ?)`,
		funcID, blob,
	)
	return err
}

func (w *Writer) WriteCodegen(ce CodegenEdge) error {
	_, err := w.tx.Exec(
		`INSERT INTO codegen (source_id, generated_id, gen_kind, pattern) VALUES (?, ?, ?, ?)`,
		ce.SourceID, ce.GeneratedID, ce.GenKind, ce.Pattern,
	)
	return err
}

func (w *Writer) WriteTypeFact(tf TypeFact) error {
	_, err := w.tx.Exec(
		`INSERT INTO type_facts (type_id, fact_kind, related_id) VALUES (?, ?, ?)`,
		tf.TypeID, tf.FactKind, tf.RelatedID,
	)
	return err
}

func (w *Writer) WriteExternalRef(localID uint64, extPkg, extName string, extKind IdentKind) error {
	_, err := w.tx.Exec(
		`INSERT INTO external_refs (local_id, ext_pkg, ext_name, ext_kind) VALUES (?, ?, ?, ?)`,
		localID, extPkg, extName, extKind,
	)
	return err
}
