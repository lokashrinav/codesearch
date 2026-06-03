package query

import (
	"fmt"

	"github.com/lokashrinav/codesearch/internal/storage"
)

// Engine orchestrates the full query pipeline: expand → snap → walk → narrate.
type Engine struct {
	reader *storage.Reader
}

// NewEngine creates a query engine backed by an indexed database.
func NewEngine(db *storage.Reader) *Engine {
	return &Engine{reader: db}
}

// Search runs the full symptom→mechanism pipeline.
func (e *Engine) Search(query string, maxHops int) (*NarratedResult, error) {
	if maxHops == 0 {
		maxHops = 5
	}

	// Phase 1: Expand symptom
	expanded := ExpandSymptom(query)

	// Phase 2: Snap to symbols
	snapped, err := SnapToSymbols(expanded, e.reader)
	if err != nil {
		return nil, fmt.Errorf("snap symbols: %w", err)
	}
	if len(snapped) == 0 {
		return &NarratedResult{Summary: fmt.Sprintf("No symbols matched query: %q", query)}, nil
	}

	// Use top 10 seeds for the walk
	seeds := snapped
	if len(seeds) > 10 {
		seeds = seeds[:10]
	}

	// Phase 3: Walk graph
	steps, err := WalkFromSeeds(seeds, e.reader, maxHops)
	if err != nil {
		return nil, fmt.Errorf("graph walk: %w", err)
	}

	// Phase 4: Narrate
	return Narrate(query, steps), nil
}

// Trace follows data flow forward or backward from a specific symbol.
func (e *Engine) Trace(symbolName string, direction string, maxHops int) (*NarratedResult, error) {
	if maxHops == 0 {
		maxHops = 5
	}

	idents, err := e.reader.FindByName(symbolName)
	if err != nil || len(idents) == 0 {
		return &NarratedResult{Summary: fmt.Sprintf("Symbol not found: %q", symbolName)}, nil
	}

	seeds := make([]SnappedSymbol, 0, len(idents))
	for _, id := range idents {
		seeds = append(seeds, SnappedSymbol{Ident: id, Score: 100, MatchType: "exact"})
	}

	steps, err := WalkFromSeeds(seeds, e.reader, maxHops)
	if err != nil {
		return nil, fmt.Errorf("trace walk: %w", err)
	}

	return Narrate(symbolName, steps), nil
}

// Explain finds the connection between two symbols.
func (e *Engine) Explain(fromSymbol, toSymbol string) (*NarratedResult, error) {
	fromIdents, err := e.reader.FindByName(fromSymbol)
	if err != nil || len(fromIdents) == 0 {
		return &NarratedResult{Summary: fmt.Sprintf("Source symbol not found: %q", fromSymbol)}, nil
	}

	toIdents, err := e.reader.FindByName(toSymbol)
	if err != nil || len(toIdents) == 0 {
		return &NarratedResult{Summary: fmt.Sprintf("Target symbol not found: %q", toSymbol)}, nil
	}

	// Walk from source and check if target appears
	seeds := []SnappedSymbol{{Ident: fromIdents[0], Score: 100, MatchType: "exact"}}
	steps, err := WalkFromSeeds(seeds, e.reader, 8)
	if err != nil {
		return nil, err
	}

	// Filter to steps that lead toward the target
	targetID := toIdents[0].ID
	var relevantSteps []TraceStep
	for _, step := range steps {
		relevantSteps = append(relevantSteps, step)
		if step.Ident.ID == targetID {
			break
		}
	}

	query := fmt.Sprintf("%s → %s", fromSymbol, toSymbol)
	return Narrate(query, relevantSteps), nil
}

// FieldFlow finds all readers and writers of a specific struct field.
func (e *Engine) FieldFlow(structType, fieldName string) (*NarratedResult, error) {
	writers, err := e.reader.GetFieldWriters(structType, fieldName)
	if err != nil {
		return nil, err
	}

	readers, err := e.reader.GetFieldReaders(structType, fieldName)
	if err != nil {
		return nil, err
	}

	var steps []TraceStep
	stepNum := 0

	for _, w := range writers {
		idents, _ := e.reader.FindByName("")
		_ = idents
		steps = append(steps, TraceStep{
			StepNum:  stepNum,
			EdgeKind: "field_write",
			Detail:   fmt.Sprintf("Writes %s.%s in %s:%d", structType, fieldName, w.File, w.Line),
		})
		stepNum++
	}

	for _, r := range readers {
		steps = append(steps, TraceStep{
			StepNum:  stepNum,
			EdgeKind: "field_read",
			Detail:   fmt.Sprintf("Reads %s.%s in %s:%d", structType, fieldName, r.File, r.Line),
		})
		stepNum++
	}

	query := fmt.Sprintf("%s.%s", structType, fieldName)
	return Narrate(query, steps), nil
}
