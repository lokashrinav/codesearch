"""Definitive eval: graph-backed search vs baseline on Django.
Uses actual indexed Django repo, extracts real subgraphs,
compares against prompt-only baseline on the same hard instances."""

import json
import os
import re
import sqlite3
import sys
import time
import urllib.request

API_KEY = os.environ.get("ANTHROPIC_API_KEY", "")
MODEL = "claude-sonnet-4-6"


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
    for m in re.finditer(r'[\w/.-]+\.py', response):
        f = m.group(0).lstrip('./')
        if not f.startswith('test') and len(f) > 4:
            files.add(f)
    return files


def build_subgraph(db_path, query, max_seeds=25):
    conn = sqlite3.connect(db_path)
    terms = [t for t in query.lower().split() if len(t) >= 3 and t not in {
        'the', 'and', 'for', 'not', 'was', 'did', 'with', 'from', 'that',
        'this', 'when', 'are', 'but', 'has', 'been', 'can', 'should',
        'does', 'have', 'instead', 'using', 'used', 'use',
    }]

    seeds = []
    seen = set()
    for term in terms:
        cursor = conn.execute(
            "SELECT id, name, qualname, kind, file, line FROM idents "
            "WHERE LOWER(qualname) LIKE ? OR LOWER(name) LIKE ? LIMIT 15",
            (f"%{term}%", f"%{term}%"))
        for row in cursor:
            if row[0] not in seen:
                seen.add(row[0])
                seeds.append(row)

    lines = ["## Code Graph (indexed from actual Django repo)\n"]
    for sid, name, qn, kind, file, line in seeds[:max_seeds]:
        lines.append(f"### [{kind}] {qn}")
        lines.append(f"  File: {file}:{line}")
        cursor = conn.execute(
            "SELECT i.qualname, i.kind, e.kind FROM edges e "
            "JOIN idents i ON e.dst = i.id WHERE e.src = ? LIMIT 6", (sid,))
        for tqn, tk, ek in cursor:
            lines.append(f"  -> [{tk}] {tqn} ({ek})")
        cursor = conn.execute(
            "SELECT i.qualname, i.kind, e.kind FROM edges e "
            "JOIN idents i ON e.src = i.id WHERE e.dst = ? LIMIT 6", (sid,))
        for sqn, sk, ek in cursor:
            lines.append(f"  <- [{sk}] {sqn} ({ek})")
        lines.append("")

    conn.close()
    return "\n".join(lines), len(seeds)


def main():
    from datasets import load_dataset

    db_path = "django.db"
    if not os.path.exists(db_path):
        print(f"ERROR: {db_path} not found. Run the indexer first.")
        sys.exit(1)

    num = int(sys.argv[1]) if len(sys.argv) > 1 else 20

    with open("hard_instances.json") as f:
        hard_ids = json.load(f)

    ds = load_dataset("princeton-nlp/SWE-bench_Lite", split="test")
    id_to_inst = {i["instance_id"]: i for i in ds}

    # Filter to Django-only hard instances
    django_hard = [hid for hid in hard_ids if hid.startswith("django__")]
    instances = [id_to_inst[hid] for hid in django_hard if hid in id_to_inst][:num]

    print(f"Evaluating {len(instances)} hard Django instances with actual graph\n")

    baseline_hits = 0
    graph_hits = 0
    total = 0
    disagreements = []

    for i, inst in enumerate(instances):
        issue = inst["problem_statement"]
        patch = inst["patch"]
        iid = inst["instance_id"]
        gt_files = set(re.findall(r'^diff --git a/(.*?) b/', patch, re.MULTILINE))

        if not gt_files:
            continue

        print(f"\n[{i+1}/{len(instances)}] {iid}")
        print(f"  GT: {gt_files}")

        # Method A: Baseline (no graph)
        resp_a = call_claude(f"""You are a fault localization tool for the Django web framework.
Given this bug report, predict which source files contain the bug.

Bug report:
{issue[:3000]}

Output ONLY file paths from the Django repo, one per line. No explanation.""")

        # Method B: Graph-backed (real indexed subgraph)
        subgraph, seed_count = build_subgraph(db_path, issue[:2000])

        resp_b = call_claude(f"""You are a fault localization tool for the Django web framework.
Given this bug report AND a code graph extracted from the actual Django repository, predict which source files contain the bug.

Bug report:
{issue[:2000]}

{subgraph[:4000]}

Based on the code graph, identify which files most likely contain the bug. Output ONLY file paths, one per line. No explanation.""")

        files_a = extract_files(resp_a)
        files_b = extract_files(resp_b)

        hit_a = bool(files_a & gt_files)
        hit_b = bool(files_b & gt_files)

        baseline_hits += hit_a
        graph_hits += hit_b
        total += 1

        marker = ""
        if hit_a != hit_b:
            marker = " *** DISAGREEMENT ***"
            winner = "graph" if hit_b else "baseline"
            disagreements.append({"id": iid, "winner": winner, "gt": list(gt_files),
                                  "baseline": list(files_a), "graph": list(files_b), "seeds": seed_count})

        print(f"  Seeds: {seed_count}")
        print(f"  Baseline: {files_a} -> {'HIT' if hit_a else 'MISS'}")
        print(f"  Graph:    {files_b} -> {'HIT' if hit_b else 'MISS'}{marker}")

        time.sleep(0.5)

    print(f"\n{'='*60}")
    print(f"DEFINITIVE RESULTS ({total} hard Django instances)")
    print(f"{'='*60}")
    print(f"Baseline (no graph):  {baseline_hits}/{total} = {baseline_hits/max(1,total)*100:.1f}%")
    print(f"Graph-backed:         {graph_hits}/{total} = {graph_hits/max(1,total)*100:.1f}%")
    print(f"Delta:                {graph_hits - baseline_hits} ({(graph_hits-baseline_hits)/max(1,total)*100:+.1f}%)")

    if disagreements:
        print(f"\nDisagreements ({len(disagreements)}):")
        for d in disagreements:
            print(f"  {d['id']}: {d['winner']} won (GT: {d['gt']}, seeds: {d['seeds']})")

    with open("eval_graph_results.json", "w") as f:
        json.dump({"total": total, "baseline": baseline_hits, "graph": graph_hits,
                    "disagreements": disagreements}, f, indent=2)


if __name__ == "__main__":
    main()
