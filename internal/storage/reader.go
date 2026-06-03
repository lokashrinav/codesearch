package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Reader provides query-time access to the fact graph.
type Reader struct {
	db *sql.DB
}

// NewReader creates a reader for an existing database.
func NewReader(db *sql.DB) *Reader {
	return &Reader{db: db}
}

// FindByName finds identifiers by exact name match.
func (r *Reader) FindByName(name string) ([]Identifier, error) {
	return r.queryIdents("SELECT id, name, pkg_path, kind, file_path, line, col, doc FROM identifiers WHERE name = ?", name)
}

// FindByPrefix finds identifiers by name prefix.
func (r *Reader) FindByPrefix(prefix string) ([]Identifier, error) {
	return r.queryIdents("SELECT id, name, pkg_path, kind, file_path, line, col, doc FROM identifiers WHERE name LIKE ?", prefix+"%")
}

// SearchFTS does a full-text trigram search over identifier names and docs.
func (r *Reader) SearchFTS(query string) ([]Identifier, error) {
	rows, err := r.db.Query(
		`SELECT i.id, i.name, i.pkg_path, i.kind, i.file_path, i.line, i.col, i.doc
		 FROM ident_fts f JOIN identifiers i ON f.rowid = i.id
		 WHERE ident_fts MATCH ?
		 ORDER BY rank LIMIT 50`, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIdents(rows)
}

// GetCallers returns all functions that call the given function.
func (r *Reader) GetCallers(funcID uint64) ([]CallEdge, error) {
	rows, err := r.db.Query(
		"SELECT caller_id, callee_id, file_path, line, dispatch, arg_map FROM call_edges WHERE callee_id = ?", funcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCallEdges(rows)
}

// GetCallees returns all functions called by the given function.
func (r *Reader) GetCallees(funcID uint64) ([]CallEdge, error) {
	rows, err := r.db.Query(
		"SELECT caller_id, callee_id, file_path, line, dispatch, arg_map FROM call_edges WHERE caller_id = ?", funcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCallEdges(rows)
}

// GetFieldWriters returns functions that write to a specific struct field.
func (r *Reader) GetFieldWriters(structType, fieldName string) ([]FieldOp, error) {
	return r.queryFieldOps(
		"SELECT struct_type, field_name, field_idx, op_kind, func_id, file_path, line FROM field_ops WHERE struct_type = ? AND field_name = ? AND op_kind = ?",
		structType, fieldName, FieldWrite)
}

// GetFieldReaders returns functions that read a specific struct field.
func (r *Reader) GetFieldReaders(structType, fieldName string) ([]FieldOp, error) {
	return r.queryFieldOps(
		"SELECT struct_type, field_name, field_idx, op_kind, func_id, file_path, line FROM field_ops WHERE struct_type = ? AND field_name = ? AND op_kind = ?",
		structType, fieldName, FieldRead)
}

// GetFlagBinding finds what a flag name is bound to.
func (r *Reader) GetFlagBinding(flagName string) ([]FlagBinding, error) {
	rows, err := r.db.Query(
		"SELECT flag_name, flag_pkg, bound_to_id, default_val, usage, func_id FROM flag_bindings WHERE flag_name LIKE ?",
		"%"+flagName+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []FlagBinding
	for rows.Next() {
		var fb FlagBinding
		if err := rows.Scan(&fb.FlagName, &fb.FlagPkg, &fb.BoundToID, &fb.DefaultVal, &fb.Usage, &fb.FuncID); err != nil {
			return nil, err
		}
		results = append(results, fb)
	}
	return results, rows.Err()
}

// GetDataFlowFrom returns data-flow edges originating from a function.
func (r *Reader) GetDataFlowFrom(funcID uint64) ([]DataFlowEdge, error) {
	return r.queryDataFlow("SELECT from_func, from_slot, to_func, to_slot, flow_kind, condition FROM data_flow WHERE from_func = ?", funcID)
}

// GetDataFlowTo returns data-flow edges terminating at a function.
func (r *Reader) GetDataFlowTo(funcID uint64) ([]DataFlowEdge, error) {
	return r.queryDataFlow("SELECT from_func, from_slot, to_func, to_slot, flow_kind, condition FROM data_flow WHERE to_func = ?", funcID)
}

// GetCodegenEdges returns codegen relationships for a source type.
func (r *Reader) GetCodegenEdges(sourceID uint64) ([]CodegenEdge, error) {
	rows, err := r.db.Query(
		"SELECT source_id, generated_id, gen_kind, pattern FROM codegen WHERE source_id = ?", sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []CodegenEdge
	for rows.Next() {
		var ce CodegenEdge
		if err := rows.Scan(&ce.SourceID, &ce.GeneratedID, &ce.GenKind, &ce.Pattern); err != nil {
			return nil, err
		}
		results = append(results, ce)
	}
	return results, rows.Err()
}

// WalkResult represents a node visited during graph traversal.
type WalkResult struct {
	Ident    Identifier
	Depth    int
	EdgeKind string
	Path     []uint64
}

// Walk performs a multi-hop BFS from seed identifiers.
func (r *Reader) Walk(seedIDs []uint64, maxDepth int, maxNodes int) ([]WalkResult, error) {
	visited := make(map[uint64]bool)
	var results []WalkResult

	type queueItem struct {
		id       uint64
		depth    int
		edgeKind string
		path     []uint64
	}

	queue := make([]queueItem, 0, len(seedIDs))
	for _, id := range seedIDs {
		queue = append(queue, queueItem{id: id, depth: 0, edgeKind: "seed", path: []uint64{id}})
	}

	for len(queue) > 0 && len(results) < maxNodes {
		item := queue[0]
		queue = queue[1:]

		if visited[item.id] {
			continue
		}
		visited[item.id] = true

		idents, err := r.queryIdents(
			"SELECT id, name, pkg_path, kind, file_path, line, col, doc FROM identifiers WHERE id = ?", item.id)
		if err != nil || len(idents) == 0 {
			continue
		}

		results = append(results, WalkResult{
			Ident:    idents[0],
			Depth:    item.depth,
			EdgeKind: item.edgeKind,
			Path:     item.path,
		})

		if item.depth >= maxDepth {
			continue
		}

		nextPath := make([]uint64, len(item.path)+1)
		copy(nextPath, item.path)

		// Follow edges in priority order
		neighbors := r.getNeighbors(item.id)
		for _, n := range neighbors {
			if !visited[n.id] {
				np := make([]uint64, len(item.path)+1)
				copy(np, item.path)
				np[len(item.path)] = n.id
				queue = append(queue, queueItem{
					id: n.id, depth: item.depth + 1,
					edgeKind: n.edgeKind, path: np,
				})
			}
		}
	}

	return results, nil
}

type neighbor struct {
	id       uint64
	edgeKind string
	priority int
}

func (r *Reader) getNeighbors(id uint64) []neighbor {
	var neighbors []neighbor

	// Data flow (definite) - highest priority
	dfs, _ := r.GetDataFlowFrom(id)
	for _, df := range dfs {
		kind := "data_flow"
		prio := 1
		if df.FlowKind == FlowMay {
			kind = "data_flow_may"
			prio = 4
		}
		neighbors = append(neighbors, neighbor{id: df.ToFunc, edgeKind: kind, priority: prio})
	}

	// Codegen edges
	cges, _ := r.GetCodegenEdges(id)
	for _, cg := range cges {
		neighbors = append(neighbors, neighbor{id: cg.GeneratedID, edgeKind: "codegen", priority: 2})
	}

	// Call edges
	callees, _ := r.GetCallees(id)
	for _, ce := range callees {
		neighbors = append(neighbors, neighbor{id: ce.CalleeID, edgeKind: "calls", priority: 3})
	}
	callers, _ := r.GetCallers(id)
	for _, ce := range callers {
		neighbors = append(neighbors, neighbor{id: ce.CallerID, edgeKind: "called_by", priority: 3})
	}

	// Field ops - find other functions operating on the same fields
	fieldOps, _ := r.queryFieldOps(
		"SELECT struct_type, field_name, field_idx, op_kind, func_id, file_path, line FROM field_ops WHERE func_id = ?", id)
	for _, fo := range fieldOps {
		var related []FieldOp
		if fo.OpKind == FieldWrite {
			related, _ = r.GetFieldReaders(fo.StructType, fo.FieldName)
		} else {
			related, _ = r.GetFieldWriters(fo.StructType, fo.FieldName)
		}
		for _, rel := range related {
			if rel.FuncID != id {
				neighbors = append(neighbors, neighbor{id: rel.FuncID, edgeKind: "shared_field", priority: 5})
			}
		}
	}

	return neighbors
}

// --- internal helpers ---

func (r *Reader) queryIdents(query string, args ...interface{}) ([]Identifier, error) {
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query idents: %w", err)
	}
	defer rows.Close()
	return scanIdents(rows)
}

func scanIdents(rows *sql.Rows) ([]Identifier, error) {
	var results []Identifier
	for rows.Next() {
		var id Identifier
		var file, doc sql.NullString
		if err := rows.Scan(&id.ID, &id.Name, &id.PkgPath, &id.Kind, &file, &id.Line, &id.Col, &doc); err != nil {
			return nil, err
		}
		id.File = file.String
		id.Doc = doc.String
		results = append(results, id)
	}
	return results, rows.Err()
}

func scanCallEdges(rows *sql.Rows) ([]CallEdge, error) {
	var results []CallEdge
	for rows.Next() {
		var ce CallEdge
		var file sql.NullString
		var argMapBlob []byte
		if err := rows.Scan(&ce.CallerID, &ce.CalleeID, &file, &ce.Line, &ce.Dispatch, &argMapBlob); err != nil {
			return nil, err
		}
		ce.File = file.String
		if len(argMapBlob) > 0 {
			json.Unmarshal(argMapBlob, &ce.ArgMap)
		}
		results = append(results, ce)
	}
	return results, rows.Err()
}

func (r *Reader) queryFieldOps(query string, args ...interface{}) ([]FieldOp, error) {
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []FieldOp
	for rows.Next() {
		var fo FieldOp
		var file sql.NullString
		if err := rows.Scan(&fo.StructType, &fo.FieldName, &fo.FieldIdx, &fo.OpKind, &fo.FuncID, &file, &fo.Line); err != nil {
			return nil, err
		}
		fo.File = file.String
		results = append(results, fo)
	}
	return results, rows.Err()
}

func (r *Reader) queryDataFlow(query string, args ...interface{}) ([]DataFlowEdge, error) {
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []DataFlowEdge
	for rows.Next() {
		var df DataFlowEdge
		var fromSlotBlob, toSlotBlob []byte
		var condition sql.NullString
		if err := rows.Scan(&df.FromFunc, &fromSlotBlob, &df.ToFunc, &toSlotBlob, &df.FlowKind, &condition); err != nil {
			return nil, err
		}
		json.Unmarshal(fromSlotBlob, &df.FromSlot)
		json.Unmarshal(toSlotBlob, &df.ToSlot)
		df.Condition = condition.String
		results = append(results, df)
	}
	return results, rows.Err()
}

// CompoundMatch finds identifiers where all query subwords appear in the camelCase-split name.
func (r *Reader) CompoundMatch(queryTerms []string) ([]Identifier, error) {
	if len(queryTerms) == 0 {
		return nil, nil
	}

	// Build a query that checks if all terms appear as substrings of the name
	conditions := make([]string, len(queryTerms))
	args := make([]interface{}, len(queryTerms))
	for i, term := range queryTerms {
		conditions[i] = "LOWER(name) LIKE ?"
		args[i] = "%" + strings.ToLower(term) + "%"
	}

	query := fmt.Sprintf(
		"SELECT id, name, pkg_path, kind, file_path, line, col, doc FROM identifiers WHERE %s LIMIT 50",
		strings.Join(conditions, " AND "),
	)

	return r.queryIdents(query, args...)
}
