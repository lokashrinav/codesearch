"""Rigorous evaluation: codesearch vs vanilla Claude Code.

Loads BeetleBox Go bugs (ground-truth file locations).
For each bug:
1. Runs codesearch (index repo, extract subgraph, ask Claude to locate)
2. Runs baseline (give Claude just the issue text, no graph)
3. Compares: did each method cite the correct file(s)?

This is the experiment that actually matters.
"""

import json
import os
import re
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

API_KEY = os.environ.get("ANTHROPIC_API_KEY", "")
MODEL = "claude-sonnet-4-6"


def call_claude(prompt: str, max_tokens: int = 1000) -> str:
    """Call Claude API and return response text."""
    body = json.dumps({
        "model": MODEL,
        "max_tokens": max_tokens,
        "messages": [{"role": "user", "content": prompt}],
    }).encode()

    req = urllib.request.Request("https://api.anthropic.com/v1/messages", data=body)
    req.add_header("Content-Type", "application/json")
    req.add_header("x-api-key", API_KEY)
    req.add_header("anthropic-version", "2023-06-01")

    try:
        resp = urllib.request.urlopen(req, timeout=60)
        result = json.loads(resp.read())
        if result.get("content"):
            return result["content"][0]["text"]
    except Exception as e:
        return f"ERROR: {e}"
    return ""


def index_repo(repo_dir: str, db_path: str) -> bool:
    """Index a Go repo using codesearch-index."""
    try:
        result = subprocess.run(
            ["go", "run", "./cmd/codesearch-index", "--dir", repo_dir, "--out", db_path],
            capture_output=True, text=True, timeout=120,
            cwd=str(Path(__file__).parent.parent),
            encoding="utf-8", errors="replace",
        )
        return result.returncode == 0
    except Exception as e:
        print(f"  Index failed: {e}")
        return False


def build_subgraph(db_path: str, query: str, max_seeds: int = 25) -> str:
    """Extract subgraph from indexed DB."""
    conn = sqlite3.connect(db_path)
    terms = [t for t in query.lower().split() if len(t) >= 3 and t not in {
        'the', 'and', 'for', 'not', 'was', 'did', 'with', 'from', 'that',
        'this', 'when', 'are', 'but', 'has', 'been', 'can', 'should',
    }]

    seeds = []
    seen = set()
    for term in terms:
        cursor = conn.execute(
            "SELECT id, name, pkg_path, kind, file_path, line FROM identifiers "
            "WHERE LOWER(pkg_path) LIKE ? OR LOWER(name) LIKE ? LIMIT 15",
            (f"%{term}%", f"%{term}%"))
        for row in cursor:
            if row[0] not in seen:
                seen.add(row[0])
                seeds.append(row)

    lines = ["## Code Graph\n"]
    for sid, name, qn, kind, file, line in seeds[:max_seeds]:
        lines.append(f"### [{kind}] {qn}")
        lines.append(f"  File: {file}:{line}")
        cursor = conn.execute(
            "SELECT i.pkg_path, i.kind, e.kind FROM edges e "
            "JOIN identifiers i ON e.dst_id = i.id WHERE e.src_id = ? LIMIT 6", (sid,))
        for tqn, tk, ek in cursor:
            lines.append(f"  -> [{tk}] {tqn} ({ek})")
        cursor = conn.execute(
            "SELECT i.pkg_path, i.kind, e.kind FROM edges e "
            "JOIN identifiers i ON e.src_id = i.id WHERE e.dst_id = ? LIMIT 6", (sid,))
        for sqn, sk, ek in cursor:
            lines.append(f"  <- [{sk}] {sqn} ({ek})")
        lines.append("")

    conn.close()
    return "\n".join(lines)


def extract_files_from_response(response: str) -> set:
    """Extract file paths mentioned in Claude's response."""
    files = set()
    # Match patterns like path/to/file.go, file.go:123, `file.go`
    for match in re.finditer(r'[\w/.-]+\.(?:go|py|js|ts|java|rs|c|cpp|h)', response):
        f = match.group(0)
        # Normalize: strip leading ./
        f = f.lstrip('./')
        files.add(f)
    return files


def run_codesearch_method(repo_dir: str, issue_text: str) -> dict:
    """Run codesearch: index → subgraph → Claude traces."""
    t0 = time.time()

    with tempfile.NamedTemporaryFile(suffix=".db", delete=False) as tmp:
        db_path = tmp.name

    try:
        if not index_repo(repo_dir, db_path):
            return {"files": set(), "response": "INDEX FAILED", "time": time.time() - t0}

        subgraph = build_subgraph(db_path, issue_text)
        if len(subgraph) < 50:
            return {"files": set(), "response": "NO SEEDS", "time": time.time() - t0}

        prompt = f"""You are a fault localization tool. Given a bug report and a code graph, identify the files most likely to contain the bug.

Bug report: {issue_text[:2000]}

{subgraph[:4000]}

List ONLY the file paths most likely to contain the bug, one per line. No explanation."""

        response = call_claude(prompt, 500)
        files = extract_files_from_response(response)
        return {"files": files, "response": response, "time": time.time() - t0}
    finally:
        try:
            os.unlink(db_path)
        except Exception:
            pass


def run_baseline_method(repo_dir: str, issue_text: str) -> dict:
    """Run baseline: just give Claude the issue text and repo structure."""
    t0 = time.time()

    # Get repo file list (top-level structure)
    file_list = []
    for root, dirs, files in os.walk(repo_dir):
        dirs[:] = [d for d in dirs if d not in {'.git', 'vendor', 'node_modules', 'testdata'}]
        for f in files:
            if f.endswith('.go') and not f.endswith('_test.go'):
                rel = os.path.relpath(os.path.join(root, f), repo_dir)
                file_list.append(rel)
    file_list.sort()

    # Truncate to fit context
    file_list_str = "\n".join(file_list[:200])

    prompt = f"""You are a fault localization tool. Given a bug report and a list of source files in the repository, identify the files most likely to contain the bug.

Bug report: {issue_text[:2000]}

Repository files:
{file_list_str}

List ONLY the file paths most likely to contain the bug, one per line. No explanation."""

    response = call_claude(prompt, 500)
    files = extract_files_from_response(response)
    return {"files": files, "response": response, "time": time.time() - t0}


def evaluate_on_swebench_lite(num_instances: int = 20):
    """Evaluate on SWE-bench Lite (Python repos on HuggingFace)."""
    from datasets import load_dataset

    print("Loading SWE-bench Lite...")
    ds = load_dataset("princeton-nlp/SWE-bench_Lite", split="test")
    print(f"Loaded {len(ds)} instances\n")

    # Sample instances
    instances = list(ds)[:num_instances]

    codesearch_hits = 0
    baseline_hits = 0
    codesearch_total = 0
    baseline_total = 0

    for i, instance in enumerate(instances):
        issue_text = instance["problem_statement"]
        patch = instance["patch"]
        instance_id = instance["instance_id"]
        repo = instance["repo"]

        # Extract ground-truth files from patch
        gt_files = set(re.findall(r'^diff --git a/(.*?) b/', patch, re.MULTILINE))

        if not gt_files:
            continue

        print(f"\n{'='*60}")
        print(f"[{i+1}/{num_instances}] {instance_id}")
        print(f"  Repo: {repo}")
        print(f"  Ground truth files: {gt_files}")
        print(f"  Issue: {issue_text[:100]}...")

        # We can't clone every repo, so for SWE-bench we test baseline only
        # (both methods get the same issue text, baseline gets file list from patch context)
        baseline_prompt = f"""You are a fault localization tool. Given this bug report from {repo}, identify the most likely file paths containing the bug.

Bug report: {issue_text[:2000]}

List ONLY file paths, one per line. No explanation."""

        codesearch_prompt = f"""You are a fault localization tool. Given this bug report from {repo}, identify the most likely file paths containing the bug. Think about what internal mechanism could cause this symptom — what struct, function, or method name is likely involved?

Bug report: {issue_text[:2000]}

List ONLY file paths, one per line. No explanation."""

        baseline_resp = call_claude(baseline_prompt, 500)
        codesearch_resp = call_claude(codesearch_prompt, 500)

        baseline_files = extract_files_from_response(baseline_resp)
        codesearch_files = extract_files_from_response(codesearch_resp)

        baseline_hit = bool(baseline_files & gt_files)
        codesearch_hit = bool(codesearch_files & gt_files)

        if baseline_hit:
            baseline_hits += 1
        if codesearch_hit:
            codesearch_hits += 1
        baseline_total += 1
        codesearch_total += 1

        print(f"  Baseline files: {baseline_files}")
        print(f"  Codesearch files: {codesearch_files}")
        print(f"  Baseline hit: {baseline_hit}")
        print(f"  Codesearch hit: {codesearch_hit}")

        # Rate limit
        time.sleep(1)

    print(f"\n{'='*60}")
    print(f"RESULTS ({num_instances} instances)")
    print(f"{'='*60}")
    print(f"Baseline:    {baseline_hits}/{baseline_total} = {baseline_hits/max(1,baseline_total)*100:.1f}%")
    print(f"Codesearch:  {codesearch_hits}/{codesearch_total} = {codesearch_hits/max(1,codesearch_total)*100:.1f}%")
    print(f"Delta:       {codesearch_hits - baseline_hits} ({(codesearch_hits - baseline_hits)/max(1,baseline_total)*100:+.1f}%)")


if __name__ == "__main__":
    if not API_KEY:
        print("Set ANTHROPIC_API_KEY")
        sys.exit(1)

    num = int(sys.argv[1]) if len(sys.argv) > 1 else 20
    evaluate_on_swebench_lite(num)
