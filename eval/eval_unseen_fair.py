"""Fair test: graph vs baseline on unseen code.
Both methods get ONLY the bug report. No file-list hints.
Graph gets the actual indexed symbols. Baseline gets nothing extra.
"""
import json, os, re, sys, time, urllib.request

API_KEY = os.environ.get("ANTHROPIC_API_KEY", "")
MODEL = "claude-sonnet-4-6"

TEST_CASES = [
    {
        "symptom": "runsc checkpoint fails with 'failed to load save/restore binary: no such file or directory' even though the binary exists on the host",
        "ground_truth_files": {"cmd/gvisor-gpu-ckpt/main.go"},
    },
    {
        "symptom": "After restoring a GPU container from checkpoint, nvidia-smi shows no GPU and all CUDA calls fail with 'driver not initialized'",
        "ground_truth_files": {"pkg/sentry/devices/nvproxy/save_restore_impl.go"},
    },
    {
        "symptom": "Container checkpoint crashes with 'panic: not implemented' in the nvproxy save path",
        "ground_truth_files": {"pkg/sentry/devices/nvproxy/save_restore_impl.go"},
    },
    {
        "symptom": "GPU memory-mapped regions cause the checkpoint serializer to crash because it doesn't know how to handle device memory",
        "ground_truth_files": {"pkg/sentry/mm/save_restore.go"},
    },
    {
        "symptom": "After restore, the CUDA checkpoint helper binary runs but exits silently without restoring GPU state",
        "ground_truth_files": {"cmd/gvisor-gpu-ckpt/cuda.go", "cmd/gvisor-gpu-ckpt/main.go"},
    },
]

import sqlite3

def call_claude(prompt, max_tokens=500):
    body = json.dumps({"model": MODEL, "max_tokens": max_tokens,
        "messages": [{"role": "user", "content": prompt}]}).encode()
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
    for m in re.finditer(r'[\w/.-]+\.go', response):
        f = m.group(0).lstrip('./')
        if len(f) > 4: files.add(f)
    return files

def build_subgraph(db_path, query, max_seeds=20):
    conn = sqlite3.connect(db_path)
    terms = [t for t in query.lower().split() if len(t) >= 3 and t not in {
        'the','and','for','not','was','did','with','from','that','this',
        'when','are','but','has','been','can','should','even','though',
        'after','all','calls','fail','shows','because','doesn',
    }]
    seeds = []
    seen = set()
    for term in terms:
        try:
            cursor = conn.execute(
                "SELECT id, name, pkg_path, kind, file_path, line FROM identifiers "
                "WHERE LOWER(pkg_path) LIKE ? OR LOWER(name) LIKE ? LIMIT 8",
                (f"%{term}%", f"%{term}%"))
            for row in cursor:
                if row[0] not in seen:
                    seen.add(row[0])
                    seeds.append(row)
        except: pass

    lines = ["## Code Graph (from gpu-checkpoint-gvisor repo)\n"]
    for sid, name, qn, kind, file, line in seeds[:max_seeds]:
        lines.append(f"### [{kind}] {qn}")
        lines.append(f"  File: {file}:{line}")
        try:
            for tqn, tk, ek in conn.execute(
                "SELECT i.pkg_path, i.kind, e.kind FROM edges e JOIN identifiers i ON e.dst_id = i.id WHERE e.src_id = ? LIMIT 5", (sid,)):
                lines.append(f"  -> [{tk}] {tqn} ({ek})")
        except: pass
        try:
            for sqn, sk, ek in conn.execute(
                "SELECT i.pkg_path, i.kind, e.kind FROM edges e JOIN identifiers i ON e.src_id = i.id WHERE e.dst_id = ? LIMIT 5", (sid,)):
                lines.append(f"  <- [{sk}] {sqn} ({ek})")
        except: pass
        lines.append("")

    if "annotations" in [r[0] for r in conn.execute("SELECT name FROM sqlite_master WHERE type='table'")]:
        lines.append("### Annotations")
        for r in conn.execute("SELECT near_type, file_path, line, text FROM annotations LIMIT 10"):
            lines.append(f"  {r[0]}: {r[3]} at {r[1]}:{r[2]}")

    conn.close()
    return "\n".join(lines), len(seeds)

def main():
    db_path = "gpuckpt.db"
    if not os.path.exists(db_path):
        print(f"ERROR: {db_path} not found")
        sys.exit(1)

    print("=" * 60)
    print("FAIR EVAL: Graph vs Baseline on UNSEEN code")
    print("Both get ONLY bug report. No file-list hints to either.")
    print("Graph gets indexed symbols. Baseline gets nothing extra.")
    print("=" * 60)

    baseline_hits = 0
    graph_hits = 0
    total = 0

    for i, tc in enumerate(TEST_CASES):
        symptom = tc["symptom"]
        gt = tc["ground_truth_files"]
        print(f"\n[{i+1}/{len(TEST_CASES)}] {symptom[:70]}...")
        print(f"  GT: {gt}")

        # BASELINE: just the symptom, no hints
        resp_a = call_claude(f"""You are debugging a gVisor GPU checkpoint/restore system.
Bug report: {symptom}
Which source files most likely contain the bug? Output ONLY file paths, one per line.""")

        # GRAPH: symptom + indexed subgraph
        subgraph, seeds = build_subgraph(db_path, symptom)
        resp_b = call_claude(f"""You are debugging a gVisor GPU checkpoint/restore system.
Bug report: {symptom}

Code graph from the actual codebase:
{subgraph[:4000]}

Which source files most likely contain the bug? Output ONLY file paths, one per line.""")

        files_a = extract_files(resp_a)
        files_b = extract_files(resp_b)
        hit_a = bool(files_a & gt)
        hit_b = bool(files_b & gt)
        baseline_hits += hit_a
        graph_hits += hit_b
        total += 1

        marker = ""
        if hit_a != hit_b:
            marker = f" *** {'GRAPH' if hit_b else 'BASELINE'} WINS ***"
        print(f"  Seeds: {seeds}")
        print(f"  Baseline: {files_a} -> {'HIT' if hit_a else 'MISS'}")
        print(f"  Graph:    {files_b} -> {'HIT' if hit_b else 'MISS'}{marker}")
        time.sleep(0.5)

    print(f"\n{'='*60}")
    print(f"FAIR RESULTS: UNSEEN CODE ({total} cases)")
    print(f"{'='*60}")
    print(f"Baseline (no graph, no hints): {baseline_hits}/{total} = {baseline_hits/max(1,total)*100:.1f}%")
    print(f"Graph-backed:                  {graph_hits}/{total} = {graph_hits/max(1,total)*100:.1f}%")
    print(f"Delta:                         {graph_hits-baseline_hits} ({(graph_hits-baseline_hits)/max(1,total)*100:+.1f}%)")

if __name__ == "__main__":
    main()
