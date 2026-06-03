"""The definitive test: graph vs baseline on code Claude has NEVER seen.

Uses the gpu-checkpoint-gvisor code we wrote this session.
Claude has not seen this code in training.
We know the real bugs and ground-truth fix locations.

Test cases are real bugs we encountered during development:
1. "checkpoint binary not found in container" -> cmd/gvisor-gpu-ckpt/main.go (binary path issue)
2. "device files not reopened after restore" -> pkg/sentry/devices/nvproxy/save_restore_impl.go
3. "GPU memory regions crash serializer" -> pkg/sentry/mm/save_restore.go
4. "stateify annotations missing on nvproxy types" -> pkg/sentry/devices/nvproxy/object.go

These are from our ACTUAL debugging experience, not synthetic.
"""

import json
import os
import re
import sqlite3
import sys
import time
import urllib.request

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'cmd', 'experiment9'))
from extract_python import hash_id  # just for the hash function

API_KEY = os.environ.get("ANTHROPIC_API_KEY", "")
MODEL = "claude-sonnet-4-6"

# Ground-truth bug cases from our actual development
TEST_CASES = [
    {
        "symptom": "runsc checkpoint fails with 'failed to load save/restore binary: no such file or directory' even though the binary exists on the host",
        "ground_truth_files": {"cmd/gvisor-gpu-ckpt/main.go"},
        "explanation": "The binary must be inside the container rootfs, not on the host. The --save-restore-exec-argv flag specifies the path inside the sandbox.",
    },
    {
        "symptom": "After restoring a GPU container from checkpoint, nvidia-smi shows no GPU and all CUDA calls fail with 'driver not initialized'",
        "ground_truth_files": {"pkg/sentry/devices/nvproxy/save_restore_impl.go"},
        "explanation": "The nvproxy frontend and UVM file descriptors are not reopened after restore. save_restore_impl.go needs afterLoadImpl to reopen /dev/nvidia0 and /dev/nvidia-uvm.",
    },
    {
        "symptom": "Container checkpoint crashes with 'panic: not implemented' in the nvproxy save path",
        "ground_truth_files": {"pkg/sentry/devices/nvproxy/save_restore_impl.go"},
        "explanation": "All 6 save/restore methods in save_restore_impl.go were panic stubs. They need real implementations.",
    },
    {
        "symptom": "GPU memory-mapped regions cause the checkpoint serializer to crash because it doesn't know how to handle device memory",
        "ground_truth_files": {"pkg/sentry/mm/save_restore.go"},
        "explanation": "GPU-backed PMAs need to be dropped before serialization. save_restore.go invalidates GPU memory regions before the checkpoint.",
    },
    {
        "symptom": "After restore, the CUDA checkpoint helper binary runs but exits silently without restoring GPU state",
        "ground_truth_files": {"cmd/gvisor-gpu-ckpt/cuda.go", "cmd/gvisor-gpu-ckpt/main.go"},
        "explanation": "The helper binary reads GVISOR_SAVE_RESTORE_AUTO_EXEC_MODE env var to decide save vs restore mode. If the env var is missing, it exits 0 silently.",
    },
]


def call_claude(prompt, max_tokens=500):
    body = json.dumps({
        "model": MODEL, "max_tokens": max_tokens,
        "messages": [{"role": "user", "content": prompt}],
    }).encode()
    req = urllib.request.Request("https://api.anthropic.com/v1/messages", data=body)
    req.add_header("Content-Type", "application/json")
    req.add_header("x-api-key", API_KEY)
    req.add_header("anthropic-version", "2023-06-01")
    try:
        resp = urllib.request.urlopen(req, timeout=60)
        result = json.loads(resp.read())
        return result["content"][0]["text"] if result.get("content") else ""
    except Exception as e:
        return f"ERROR: {e}"


def extract_files(response):
    files = set()
    for m in re.finditer(r'[\w/.-]+\.(?:go|py)', response):
        f = m.group(0).lstrip('./')
        if len(f) > 4:
            files.add(f)
    return files


def build_subgraph_from_go_index(db_path, query, max_seeds=20):
    """Build subgraph from our Go-indexed gVisor repo."""
    if not os.path.exists(db_path):
        return "", 0

    conn = sqlite3.connect(db_path)

    # Check which tables exist
    tables = [r[0] for r in conn.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()]

    # Determine table/column names
    if "idents" in tables:
        ident_table = "idents"
        qn_col = "qualname"
    elif "identifiers" in tables:
        ident_table = "identifiers"
        qn_col = "pkg_path"
    else:
        conn.close()
        return "", 0

    # Check edge table
    if "edges" in tables:
        edge_table = "edges"
        # Check column names
        cols = [r[1] for r in conn.execute(f"PRAGMA table_info({edge_table})").fetchall()]
        src_col = "src" if "src" in cols else "src_id"
        dst_col = "dst" if "dst" in cols else "dst_id"
    else:
        conn.close()
        return "", 0

    terms = [t for t in query.lower().split() if len(t) >= 3 and t not in {
        'the', 'and', 'for', 'not', 'was', 'did', 'with', 'from', 'that',
        'this', 'when', 'are', 'but', 'has', 'been', 'can', 'should',
        'even', 'though', 'after', 'all', 'calls', 'fail',
    }]

    seeds = []
    seen = set()
    for term in terms:
        try:
            cursor = conn.execute(
                f"SELECT id, name, {qn_col}, kind, file, line FROM {ident_table} "
                f"WHERE LOWER({qn_col}) LIKE ? OR LOWER(name) LIKE ? LIMIT 8",
                (f"%{term}%", f"%{term}%"))
            for row in cursor:
                if row[0] not in seen:
                    seen.add(row[0])
                    seeds.append(row)
        except Exception:
            continue

    # Score and sort
    scored = []
    for sid, name, qn, kind, file, line in seeds:
        score = sum(2 for t in terms if t in str(qn).lower()) + sum(1 for t in terms if t in str(name).lower())
        scored.append((score, sid, name, qn, kind, file, line))
    scored.sort(reverse=True)

    lines = ["## Code Graph (indexed from gVisor GPU checkpoint code)\n"]
    lines.append("These are real symbols from the codebase with their relationships.\n")
    count = 0
    for score, sid, name, qn, kind, file, line in scored[:max_seeds]:
        lines.append(f"### [{kind}] {qn}")
        lines.append(f"  File: {file}:{line}")
        try:
            cursor = conn.execute(
                f"SELECT i.{qn_col}, i.kind, e.kind FROM {edge_table} e "
                f"JOIN {ident_table} i ON e.{dst_col} = i.id WHERE e.{src_col} = ? LIMIT 5", (sid,))
            for tqn, tk, ek in cursor:
                lines.append(f"  -> [{tk}] {tqn} ({ek})")
        except Exception:
            pass
        try:
            cursor = conn.execute(
                f"SELECT i.{qn_col}, i.kind, e.kind FROM {edge_table} e "
                f"JOIN {ident_table} i ON e.{src_col} = i.id WHERE e.{dst_col} = ? LIMIT 5", (sid,))
            for sqn, sk, ek in cursor:
                lines.append(f"  <- [{sk}] {sqn} ({ek})")
        except Exception:
            pass
        lines.append("")
        count += 1

    # Add annotations
    if "annotations" in tables:
        lines.append("### Stateify annotations")
        try:
            for row in conn.execute("SELECT near_type, file, line, text FROM annotations LIMIT 10"):
                lines.append(f"  {row[0]}: {row[3]} at {row[1]}:{row[2]}")
        except Exception:
            pass

    conn.close()
    return "\n".join(lines), count


def main():
    if not API_KEY:
        print("Set ANTHROPIC_API_KEY")
        sys.exit(1)

    # Use the gVisor index we already built
    db_path = "gvisor.db"
    if not os.path.exists(db_path):
        print(f"ERROR: {db_path} not found. Run: codesearch-index --dir /path/to/gvisor --out gvisor.db")
        sys.exit(1)

    print("=" * 60)
    print("EVAL: Graph vs Baseline on UNSEEN code")
    print("(gpu-checkpoint-gvisor, written this session, not in training data)")
    print("=" * 60)

    baseline_hits = 0
    graph_hits = 0
    total = 0

    for i, tc in enumerate(TEST_CASES):
        symptom = tc["symptom"]
        gt_files = tc["ground_truth_files"]

        print(f"\n[{i+1}/{len(TEST_CASES)}] {symptom[:80]}...")
        print(f"  GT: {gt_files}")

        # Baseline: just Claude, no graph
        resp_a = call_claude(f"""You are debugging a gVisor container runtime that has been modified to support GPU checkpoint/restore via nvproxy.

The codebase structure:
- cmd/gvisor-gpu-ckpt/ - helper binary for GPU checkpoint (main.go, cuda.go)
- pkg/sentry/devices/nvproxy/ - GPU device proxy (save_restore_impl.go, frontend.go, uvm.go, object.go, nvproxy.go)
- pkg/sentry/mm/ - memory management (save_restore.go)
- pkg/sentry/kernel/ - kernel state (kernel.go, kernel_restore.go)
- pkg/sentry/control/ - control commands (state.go)
- runsc/cmd/ - CLI commands (checkpoint.go, restore.go)

Bug report: {symptom}

Which file(s) most likely contain the bug? Output ONLY full file paths, one per line.""")

        # Graph-backed: with actual indexed subgraph
        subgraph, seed_count = build_subgraph_from_go_index(db_path, symptom)

        resp_b = call_claude(f"""You are debugging a gVisor container runtime that has been modified to support GPU checkpoint/restore via nvproxy.

Bug report: {symptom}

Here is a code graph extracted from the actual codebase showing relevant symbols and their relationships:

{subgraph[:4000]}

Based on the code graph, which file(s) most likely contain the bug? Output ONLY full file paths, one per line.""")

        files_a = extract_files(resp_a)
        files_b = extract_files(resp_b)

        hit_a = bool(files_a & gt_files)
        hit_b = bool(files_b & gt_files)

        baseline_hits += hit_a
        graph_hits += hit_b
        total += 1

        marker = ""
        if hit_a != hit_b:
            marker = f" *** {'GRAPH' if hit_b else 'BASELINE'} WINS ***"

        print(f"  Seeds: {seed_count}")
        print(f"  Baseline: {files_a} -> {'HIT' if hit_a else 'MISS'}")
        print(f"  Graph:    {files_b} -> {'HIT' if hit_b else 'MISS'}{marker}")

        time.sleep(0.5)

    print(f"\n{'='*60}")
    print(f"RESULTS: UNSEEN CODE ({total} cases)")
    print(f"{'='*60}")
    print(f"Baseline (no graph):  {baseline_hits}/{total} = {baseline_hits/max(1,total)*100:.1f}%")
    print(f"Graph-backed:         {graph_hits}/{total} = {graph_hits/max(1,total)*100:.1f}%")
    print(f"Delta:                {graph_hits - baseline_hits} ({(graph_hits-baseline_hits)/max(1,total)*100:+.1f}%)")
    print(f"\nNote: baseline gets a file-list hint (codebase structure)")
    print(f"Graph gets actual indexed symbols + edges")


if __name__ == "__main__":
    main()
