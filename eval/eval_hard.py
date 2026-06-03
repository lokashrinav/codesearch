"""Evaluate on hard SWE-bench Lite instances where the file
is NOT named in the issue. This is where the graph should help."""

import json
import os
import re
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
    for m in re.finditer(r'[\w/.-]+\.(?:go|py|js|ts|java|rs|c|cpp|h)', response):
        files.add(m.group(0).lstrip('./'))
    return files


def main():
    from datasets import load_dataset

    num = int(sys.argv[1]) if len(sys.argv) > 1 else 20

    with open("hard_instances.json") as f:
        hard_ids = json.load(f)

    ds = load_dataset("princeton-nlp/SWE-bench_Lite", split="test")
    id_to_inst = {i["instance_id"]: i for i in ds}

    # Pick hard instances spread across repos
    instances = [id_to_inst[hid] for hid in hard_ids if hid in id_to_inst][:num]

    baseline_hits = 0
    mechanism_hits = 0
    total = 0
    disagreements = []

    for i, inst in enumerate(instances):
        issue = inst["problem_statement"]
        patch = inst["patch"]
        repo = inst["repo"]
        iid = inst["instance_id"]
        gt_files = set(re.findall(r'^diff --git a/(.*?) b/', patch, re.MULTILINE))

        if not gt_files:
            continue

        print(f"\n[{i+1}/{num}] {iid}")
        print(f"  GT: {gt_files}")
        print(f"  Issue: {issue[:80]}...")

        # Method A: Baseline — plain Claude
        resp_a = call_claude(f"""You are a fault localization tool for the {repo} repository.
Given this bug report, predict which source files contain the bug.

Bug report:
{issue[:3000]}

Output ONLY file paths, one per line. No explanation. Be specific (full path from repo root).""")

        # Method B: Mechanism-hypothesis — ask Claude to think about mechanism first
        resp_b = call_claude(f"""You are a fault localization tool for the {repo} repository.
Given this bug report, first think about what INTERNAL MECHANISM could cause this symptom.
What class, function, or data structure is likely responsible?
Then predict which source files contain the bug.

Bug report:
{issue[:3000]}

Think step by step about the mechanism, then output ONLY file paths, one per line at the end.""")

        files_a = extract_files(resp_a)
        files_b = extract_files(resp_b)

        hit_a = bool(files_a & gt_files)
        hit_b = bool(files_b & gt_files)

        baseline_hits += hit_a
        mechanism_hits += hit_b
        total += 1

        marker = ""
        if hit_a != hit_b:
            marker = " *** DISAGREEMENT ***"
            disagreements.append({
                "id": iid, "gt": list(gt_files),
                "baseline": list(files_a), "mechanism": list(files_b),
                "baseline_hit": hit_a, "mechanism_hit": hit_b,
            })

        print(f"  Baseline: {files_a} -> {'HIT' if hit_a else 'MISS'}")
        print(f"  Mechanism: {files_b} -> {'HIT' if hit_b else 'MISS'}{marker}")

        time.sleep(0.5)

    print(f"\n{'='*60}")
    print(f"RESULTS ({total} hard instances, files NOT named in issue)")
    print(f"{'='*60}")
    print(f"Baseline (plain):     {baseline_hits}/{total} = {baseline_hits/max(1,total)*100:.1f}%")
    print(f"Mechanism hypothesis: {mechanism_hits}/{total} = {mechanism_hits/max(1,total)*100:.1f}%")
    print(f"Delta:                {mechanism_hits - baseline_hits} ({(mechanism_hits-baseline_hits)/max(1,total)*100:+.1f}%)")

    if disagreements:
        print(f"\nDisagreements ({len(disagreements)}):")
        for d in disagreements:
            winner = "mechanism" if d["mechanism_hit"] else "baseline"
            print(f"  {d['id']}: {winner} won (GT: {d['gt']})")

    # Save results
    with open("eval_results.json", "w") as f:
        json.dump({
            "total": total, "baseline_hits": baseline_hits,
            "mechanism_hits": mechanism_hits, "disagreements": disagreements,
        }, f, indent=2)


if __name__ == "__main__":
    main()
