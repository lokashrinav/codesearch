# Codesearch: Cross-Procedural Code Comprehension Engine

## Context

Developers search with symptom language ("flag did nothing on restore") but the answer is in mechanism language ("SaveRestoreExecConfig persists across kernel state"). 89.7% of real bugs need 2+ hops from issue description to the actual code. The #1 failure mode in existing systems (45% of failures) is data-flow stopping at function boundaries. No existing tool does cross-boundary data-flow analysis.

## Architectural Camp: Compiler-Accurate (Not File-Incremental)

This system uses `go/packages` + `go/ssa` + `go/callgraph` (VTA). This is the **SCIP/Glean camp**: whole-program analysis, compiler-accurate, expensive index, precise data-flow. This is NOT the stack-graphs camp. Indexing requires type-checking the full transitive dependency set. Changing one file invalidates SSA of everything downstream in the package graph.

**Why this tradeoff**: Go compiles fast. SSA gives us cross-procedural data-flow that stack-graphs structurally cannot. For a single-language Go MVP where we control the corpus, precision is worth the index cost. File-incrementality is not a property of this architecture. Per-module SQLite caching amortizes cost across projects that share dependencies, but re-indexing a changed module is a full rebuild of that module.

## Architecture

```
[1] Fact Extractor  --> [2] Data-Flow Analyzer --> [3] Storage Layer
         ^                       ^                       |
         |                       |                       v
                              [4] Query Engine <-- [5] MCP Server <-- Claude Code
```

Dependency pre-indexing (GitHub Releases registry) is **deferred from MVP**. For the gVisor benchmark, `go/packages` with LoadAllSyntax pulls dependencies through the module cache automatically. No separate registry needed.

## Project Structure

```
codesearch/
  cmd/
    codesearch-index/       # CLI: index a module
    codesearch-serve/       # CLI: run MCP server
  internal/
    extractor/
      extractor.go          # Orchestrator: go/packages + SSA + callgraph
      identifiers.go        # Identifier def/ref extraction
      calls.go              # Call edges (static + dynamic dispatch)
      fields.go             # Struct field read/write extraction
      flags.go              # Flag→config binding extraction
      codegen.go            # Codegen pattern detection + synthetic edge injection
      types.go              # Type hierarchy, interface satisfaction
    dataflow/
      dataflow.go           # Main analyzer
      summary.go            # Function summary computation + composition
      tracker.go            # Intra-procedural value tracking via SSA
      dispatch.go           # Unresolved dispatch handling (VTA candidates + codegen synthesis)
    storage/
      schema.go             # SQLite schema
      writer.go             # Batch writer
      reader.go             # Query-time reader with graph traversal
    query/
      engine.go             # Main query engine
      symptom.go            # Thin: tokenize + camelCase split only
      snap.go               # Symbol snapping: text → graph nodes
      walk.go               # Multi-hop graph traversal
      narrator.go           # Format results for LLM
    mcp/
      server.go             # MCP server setup
      tools.go              # Tool implementations
```

## Pre-Build Spike (Days 1-3)

Before writing any schema or extractor, answer two questions that can sink the plan:

**Spike 1**: Can `go/packages` + `go/ssa` + VTA process gVisor on a 32GB machine? How long, how much RAM? If VTA exceeds memory, fall back to CHA/RTA or scope SSA to non-test packages.

**Spike 2**: Does VTA produce the `afterLoad` callback edge for `+stateify savable` types? The `afterLoad` call chain is: `pkg/state` reflective decoder → `checkComplete` → `callbackRun` → `StateLoad.func1` → `afterLoad`. This is a runtime callback through the deserializer. VTA may or may not model it. If not, we MUST synthesize it from the codegen pattern (option b below), and that becomes a mandatory component.

## Component 1: Fact Extractor

Uses `go/packages` (LoadAllSyntax) + `go/ssa` + `go/callgraph` (VTA) to extract:

- **Identifiers**: every named thing with location + doc
- **Def/Ref edges**: what references what
- **Call edges**: who calls whom, with dispatch kind and arg mapping
- **Field ops**: reads and writes to struct fields, per-function
- **Flag bindings**: CLI flag name → config struct field (flag.StringVar, cobra, pflag, struct tags)
- **Type facts**: interface satisfaction, embedding
- **Codegen patterns + synthetic edges**: see below

### Codegen Pattern Detection (codegen.go) — Critical

For reflection/codegen frameworks where VTA cannot produce call edges, `codegen.go` synthesizes them from known patterns:

- **`+stateify savable`**: Scan comments. For each savable struct T, synthesize edges: `state.decoder → T.StateLoad → T.afterLoad` and `state.encoder → T.StateSave → T.beforeSave`. These are the exact dispatch paths the runtime uses but that static analysis cannot see.
- **protobuf**: `.pb.go` files → map generated types to proto message definitions
- **`go:generate`**: Parse directives, record generator → target relationship

This is the moat: hard-coding dispatch facts for the specific codegen frameworks in the wedge domain (gVisor, systems code) rather than solving dispatch resolution in general.

## Component 2: Data-Flow Analyzer (The Hard Part)

Pre-computes **function summaries** recording how inputs flow to outputs.

### Phase 1 — Intra-procedural

Walk SSA blocks in dominator order. Track value provenance from params/receiver through field accesses, stores, calls, returns. Phi nodes merge provenances from branches.

### Phase 2 — Cross-procedural composition

Bottom-up through call graph (topological sort, SCC fixed-point with 3 iteration cap).

**For statically-resolved call sites**: Compose caller's summary with callee's summary directly.

**For unresolved dispatch (Interface/FuncValue)**: VTA provides a candidate set (sound, over-approximate). Compose against ALL candidates. Tag resulting flow edges as `may_flow` (not `definite_flow`). The narrator surfaces the distinction: "this value MAY flow to X (via interface dispatch)."

**For codegen-synthesized edges**: Treat synthetic call edges from `codegen.go` as `definite_flow`. These are known dispatch patterns, not approximations. The `+stateify savable → afterLoad` edge is definite because the codegen guarantees it.

### Phase 3 — Field-flow closure

For each struct field, collect all writers and readers across all functions. Create cross-function edges through shared state. This spans function boundaries without call edges.

### Properties

- Field-sensitive (individual struct fields, not whole structs)
- Flow-sensitive intra-procedurally (SSA dominator ordering)
- Context-insensitive inter-procedurally (one summary per function, not per call-site)
- May-flow for interface/func-value dispatch, definite-flow for static + codegen-synthesized

## Component 3: Storage Layer

One SQLite database per indexed module. Core tables:

- `identifiers` + FTS5 trigram index
- `def_refs`, `call_edges`, `field_ops`, `flag_bindings`
- `data_flow` (from_func, from_slot, to_func, to_slot, flow_kind: definite|may, condition)
- `func_summaries` (func_id, summary_blob)
- `codegen` (source_id, generated_id, gen_kind, pattern)
- `type_facts`

No git-blob-hash incrementality in MVP. Re-indexing is a full rebuild per module. Go compiles fast enough that this is acceptable for the initial wedge.

## Component 4: Query Engine

### Symptom expansion (symptom.go) — deliberately thin

Tokenize, split camelCase/snake_case, strip stop words. No synonym thesaurus, no NLP. The LLM in Claude Code does the reasoning; this layer just produces search terms. Don't over-build it.

### Symbol snapping (snap.go)

Exact → prefix → FTS5 trigram → compound match (all camelCase subwords present) → flag name match. Scored and ranked. Top 50 candidates.

### Multi-hop graph walk (walk.go)

BFS from seed symbols. Edge priority: data_flow (definite) > codegen > call_edges > data_flow (may) > field_ops > def_refs. Max 5 hops, 1000 nodes. Cross-module stitching via `external_refs` when module dependencies are co-indexed.

### Narration (narrator.go)

Step-by-step trace with file:line locations. Distinguishes definite-flow from may-flow. The LLM interprets this; the narrator just structures it.

## Component 5: MCP Server

4 tools:
- `codesearch_search(query, module)` — symptom → narrated trace
- `codesearch_trace(symbol, module, direction)` — data flow forward/backward
- `codesearch_explain(from, to, module)` — how two symbols connect
- `codesearch_field_flow(type, field, module)` — who reads/writes a field

Stdio transport, launched by Claude Code.

## gVisor Benchmark — Corrected Expected Trace

Query: "flag did nothing on restore"

**Expected trace** (verified against source):

1. **Flag binding**: `--save-restore-exec-argv` is defined on the `checkpoint` subcommand in `runsc/cmd/checkpoint.go:76`
2. **Config write**: `checkpoint.Execute` calls `preSave` which calls `ConfigureSaveRestoreExec` (`pkg/sentry/control/state.go:405`), writing `k.SaveRestoreExecConfig`
3. **Stateify persistence**: `kernel.Kernel` is marked `// +stateify savable`. The codegen generates `StateSave`/`StateLoad` methods. `SaveRestoreExecConfig` (a field of Kernel) is serialized across the save/restore boundary.
4. **Restore-side exec**: After restore, `SaveRestoreExec(k, mode)` is called at `control/state.go:314`. It reads `k.SaveRestoreExecConfig`.
5. **The silent no-op**: At `state.go:315-317`, if `SaveRestoreExecConfig == nil`, `SaveRestoreExec` returns nil immediately. No error, no log. The restore-side exec silently does nothing.
6. **Root cause**: The flag lives on `checkpoint`, so it IS set during save. But if the config was never populated (e.g., flag not passed, or `preSave` path not taken), the restore side sees nil and silently no-ops.

This trace terminates at a **specific, citable line** (`state.go:315`) with a **precise mechanism** (nil-check silent return), not a vague "config is empty."

## Build Order

**Spike (Days 1-3)**: VTA on gVisor feasibility. Does afterLoad edge exist in VTA output?

**Phase 1 (Weeks 1-3)**: storage/schema.go, storage/writer.go, storage/reader.go, extractor/extractor.go, extractor/identifiers.go, extractor/calls.go, extractor/codegen.go (stateify synthetic edges — needed early for benchmark)

**Phase 2 (Weeks 4-6)**: extractor/fields.go, extractor/flags.go, extractor/types.go, dataflow/tracker.go, dataflow/summary.go, dataflow/dispatch.go, dataflow/dataflow.go

**Phase 3 (Weeks 7-9)**: query/symptom.go, query/snap.go, query/walk.go, query/narrator.go, query/engine.go, mcp/server.go, mcp/tools.go

**Phase 4 (Week 10+)**: gVisor benchmark verification, performance tuning, dependency pre-indexing (deferred)

## Verification

1. **Spike verification**: VTA processes gVisor in <10 min, <16GB RAM
2. Index gVisor with `codesearch-index gvisor.dev/gvisor /path/to/gvisor`
3. Start MCP server with `codesearch-serve --index-dir ~/.codesearch/indexes`
4. Query: "flag did nothing on restore"
5. Verify: trace includes `SaveRestoreExecConfig`, the `preSave → ConfigureSaveRestoreExec` write path, the `+stateify savable` persistence, and the `state.go:315` nil-check no-op
6. Verify: data-flow edges cross function boundaries (flag value → config → kernel field → restore check)
7. Verify: codegen-synthesized `afterLoad` edge exists even if VTA misses it
8. Query latency < 500ms cold, < 100ms warm
