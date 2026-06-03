package query

import (
	"github.com/lokashrinav/codesearch/internal/storage"
)

// TraceStep is one step in a narrated causal trace.
type TraceStep struct {
	StepNum  int
	Ident    storage.Identifier
	EdgeKind string
	Detail   string
	FlowKind storage.FlowKind
}

// WalkFromSeeds performs a multi-hop BFS from seed symbols and returns
// the visited subgraph as an ordered trace.
func WalkFromSeeds(seeds []SnappedSymbol, reader *storage.Reader, maxHops int) ([]TraceStep, error) {
	if maxHops == 0 {
		maxHops = 5
	}

	seedIDs := make([]uint64, 0, len(seeds))
	for _, s := range seeds {
		seedIDs = append(seedIDs, s.Ident.ID)
	}

	walkResults, err := reader.Walk(seedIDs, maxHops, 1000)
	if err != nil {
		return nil, err
	}

	steps := make([]TraceStep, len(walkResults))
	for i, wr := range walkResults {
		steps[i] = TraceStep{
			StepNum:  i,
			Ident:    wr.Ident,
			EdgeKind: wr.EdgeKind,
		}
	}

	return steps, nil
}
