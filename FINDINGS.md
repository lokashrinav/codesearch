# Code Search Experiments: Findings

## Architecture Validated

**Deterministic indexing + LLM reasoning at query time.**

Raw Go parser (no build system) → SQLite fact graph → seed finding → local subgraph extraction → LLM traces causal chain.

## Experiment Results

| Exp | What | Result |
|-----|------|--------|
| 1 | go/packages + SSA on gVisor | Loads in 1m19s but SSA member iteration empty |
| 2 | AST + types on gVisor | Fails on Windows (unix syscalls, proto imports) |
| 3 | Raw Go parser (no type-checking) | **25ms for 12 files, 290 facts. Works everywhere.** |
| 4 | Full index + BFS search | 678 files, 12K idents, 102K edges in 6s. Finds SaveRestoreExecConfig. |
| 5 | Relevance-scored cross-package | 218 files, 3s. Top result: Kernel.SaveRestoreExecConfig (score 118). Crosses 4 package boundaries. |
| 6 | LLM query expansion | LLM hypothesized mechanism terms. Marginal improvement when symptom/mechanism terms already overlap. Stateify annotations correctly tracked. |
| 7 | **LLM-as-graph-walker** | **The breakthrough.** LLM traces 6-step causal chain from seeds. Identifies IsFlagSafeToOverride as root cause. Produces better explanations than heuristic BFS. |
| 7b | Generalization test (GPU query) | LLM traces 5-step chain for "GPU memory mapping lost." Correctly identifies fd mapping and stateify persistence as the mechanism. Matches the actual bug we fixed. |

## Key Findings

1. **Raw Go parser is the right foundation.** No build system needed, works on any platform, 500ms for 218 files. Type-checked analysis (go/packages + SSA) is more precise but requires Linux for gVisor and is 100x slower.

2. **LLM-as-graph-walker beats heuristic BFS.** Heuristic scoring produces noisy results (too many irrelevant symbols). The LLM reads the subgraph and reasons about WHY symbols are relevant and HOW they connect causally.

3. **The fact graph grounds the LLM.** Without the graph, the LLM would hallucinate symbol names and file locations. The graph provides exact, real symbols that exist in the codebase. The LLM provides the reasoning that connects them.

4. **Stateify annotation tracking is critical for gVisor.** The `+stateify savable` comments determine what persists across save/restore. Detecting and linking these to their types is essential for the causal chain.

5. **Cross-package resolution via heuristic receiver matching works.** Matching `k` → `Kernel`, `s` → `Stack`, etc. based on receiver type in the containing function is a good-enough heuristic for Go codebases.

6. **LLM query expansion has diminishing returns when symptom terms overlap with mechanism terms.** The real value of the LLM is in the graph walk, not in query expansion.

## Proven Architecture

```
[1] Raw Go Parser (go/parser, no build)
    → Extract: identifiers, struct fields, functions, methods
    → Extract: selector expressions (field accesses, method calls)
    → Extract: annotations (+stateify, go:generate)
    → 500ms for 218 files

[2] SQLite Fact Graph
    → Tables: idents, edges, annotations
    → Indexes on name, qualname, src, dst
    → Multi-hop queries via recursive CTEs or BFS

[3] Seed Finding
    → Tokenize query, filter stop words
    → Match against ident names and qualnames (exact, substring)
    → Optional: LLM hypothesizes additional mechanism terms

[4] Subgraph Extraction
    → For top 20 seeds, extract immediate edges (outgoing + incoming)
    → Include stateify annotations for found types
    → Format as structured text (~5KB context)

[5] LLM Causal Trace (query time)
    → Give Claude the subgraph + the symptom description
    → Claude traces the causal chain with file:line citations
    → Claude identifies the root cause mechanism
    → ~5s latency, ~2K output tokens

[6] MCP Server
    → Delivers [1-5] as tools in Claude Code
    → User's subscription pays for [5]
```

## What's Next

1. **Add nvproxy to indexed directories** — the GPU query would find frontendFD.afterLoadImpl directly
2. **Test on non-gVisor codebases** — validate the approach generalizes beyond our benchmark
3. **Add data-flow edges** — track struct field reads/writes to connect writers and readers across functions
4. **Build the MCP server** — package this as a Claude Code tool
5. **Pre-index popular Go modules** — distribute fact graphs via GitHub Releases
