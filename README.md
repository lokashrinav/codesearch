# codesearch

Code comprehension engine that bridges symptom→mechanism gaps. Ask "why is my GPU context not initialized after restore?" and get a causal chain with file:line citations.

## How it works

1. **Index** your codebase with a raw parser (no build system needed)
2. **Search** with natural language symptom descriptions
3. **Trace** causal chains through the code graph using LLM reasoning

Architecture: raw parser (go/parser, Python ast, regex for TS) → SQLite fact graph → seed finding → subgraph extraction → Claude traces the causal chain.

## Quick start

```bash
# Install
go install github.com/lokashrinav/codesearch/cmd/codesearch-index@latest
go install github.com/lokashrinav/codesearch/cmd/codesearch-serve@latest

# Index your repo
codesearch-index --dir /path/to/your/repo --out repo.db

# Add to Claude Code
# In your project's .claude/settings.json:
```

```json
{
  "mcpServers": {
    "codesearch": {
      "command": "codesearch-serve",
      "args": ["--db", "/path/to/repo.db"]
    }
  }
}
```

Then ask Claude Code: "Why does the flag do nothing on restore?"

## What it finds

Tested on 10 different queries across 4 codebases (gVisor, Go stdlib, mem0, itself):

| Query | What it found |
|-------|--------------|
| "flag did nothing on restore" | 6-step chain to IsFlagSafeToOverride |
| "GPU memory mapping lost after checkpoint" | 5-step chain through fdMapping lifecycle |
| "CUDA context not initialized" | Found loadLibcuda in gvisor-gpu-ckpt |
| "container hangs while GPU busy" | 8-step deadlock chain with ASCII diagram |
| "HTTP body not closed memory leak" | 7-step HTTP/2 pipe + flow control chain |
| "fd leaked on connection reset" | 5-step poll.FD reference count race |
| "search returns empty results" | 5-step mem0 filter chain |
| "nvproxy device fd not restored" | Found save_restore_impl.go (our actual code) |
| "identifiers hash collide" | Found a real bug in itself, recommended fix |
| "dashboard not updating" | Next.js route caching identified |

## Supported languages

- **Go** — full support via go/parser (production indexer)
- **Python** — via ast module
- **TypeScript/JavaScript** — via regex extraction

## MCP tools

The server exposes 4 tools to Claude Code:

- `codesearch_search` — symptom → causal chain trace
- `codesearch_trace` — look up a symbol's callers/callees
- `codesearch_explain` — find path between two symbols
- `codesearch_field_flow` — who reads/writes a struct field

## Performance

| Codebase | Files | Identifiers | Edges | Index time |
|----------|-------|-------------|-------|------------|
| gVisor | 1,636 | 33,678 | 265,292 | 3.1s |
| Go stdlib | 4,794 | 88,796 | 674,635 | 6.8s |
| mem0 (Python) | 142 | 1,094 | 13,236 | 3.0s |
| Next.js frontend | 14 | 98 | 52 | 0.2s |

## Experiments

See `cmd/experiment1/` through `cmd/experiment12/` for the validation journey:
- Experiments 1-4: parser validation
- Experiment 5: relevance-scored BFS
- Experiment 6: LLM query expansion
- Experiment 7: LLM-as-graph-walker (breakthrough)
- Experiment 8: full-repo scale (88K identifiers)
- Experiment 9: Python support
- Experiment 10: cross-dependency search
- Experiment 11: self-referential bug finding
- Experiment 12: TypeScript support
