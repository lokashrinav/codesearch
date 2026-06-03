"""The final experiment. Kill-test for the whole project.

Mechanism-hypothesis prompt + grep grounding on unseen code.
If this doesn't beat 20%, the investigation is done.
"""
import json, os, re, subprocess, sys, time, urllib.request

API_KEY = os.environ.get("ANTHROPIC_API_KEY", "")
MODEL = "claude-sonnet-4-6"
REPO = r"C:\Users\lokas\gpu-checkpoint-gvisor"

TEST_CASES = [
    {"symptom": "runsc checkpoint fails with 'failed to load save/restore binary: no such file or directory' even though the binary exists on the host",
     "gt": {"cmd/gvisor-gpu-ckpt/main.go"}},
    {"symptom": "After restoring a GPU container from checkpoint, nvidia-smi shows no GPU and all CUDA calls fail with 'driver not initialized'",
     "gt": {"pkg/sentry/devices/nvproxy/save_restore_impl.go"}},
    {"symptom": "Container checkpoint crashes with 'panic: not implemented' in the nvproxy save path",
     "gt": {"pkg/sentry/devices/nvproxy/save_restore_impl.go"}},
    {"symptom": "GPU memory-mapped regions cause the checkpoint serializer to crash because it doesn't know how to handle device memory",
     "gt": {"pkg/sentry/mm/save_restore.go"}},
    {"symptom": "After restore, the CUDA checkpoint helper binary runs but exits silently without restoring GPU state",
     "gt": {"cmd/gvisor-gpu-ckpt/cuda.go", "cmd/gvisor-gpu-ckpt/main.go"}},
]


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
        if len(f) > 4:
            files.add(f)
    return files


def grep_repo(terms, repo_dir):
    """Grep the repo for hypothesized mechanism terms. Return matching files."""
    hits = {}
    for term in terms:
        if len(term) < 3:
            continue
        try:
            result = subprocess.run(
                ["grep", "-ril", "--include=*.go", term, repo_dir],
                capture_output=True, text=True, timeout=10,
                encoding="utf-8", errors="replace")
            for line in result.stdout.strip().split("\n"):
                if line:
                    rel = os.path.relpath(line, repo_dir).replace("\\", "/")
                    if not rel.endswith("_test.go"):
                        hits[rel] = hits.get(rel, 0) + 1
        except Exception:
            pass
    # Sort by hit count, return top files
    sorted_hits = sorted(hits.items(), key=lambda x: -x[1])
    return sorted_hits[:15]


def main():
    print("=" * 60)
    print("FINAL EXPERIMENT: Mechanism prompt + grep grounding")
    print("On unseen code (gpu-checkpoint-gvisor)")
    print("If this doesn't beat 20%, the investigation is done.")
    print("=" * 60)

    baseline_hits = 0
    prompt_hits = 0
    grep_hits = 0
    total = 0

    for i, tc in enumerate(TEST_CASES):
        symptom = tc["symptom"]
        gt = tc["gt"]
        print(f"\n[{i+1}/{len(TEST_CASES)}] {symptom[:70]}...")
        print(f"  GT: {gt}")

        # Method A: Baseline (just the symptom)
        resp_a = call_claude(f"""You are debugging a gVisor GPU checkpoint/restore system.
Bug report: {symptom}
Which source files most likely contain the bug? Output ONLY file paths, one per line.""")
        files_a = extract_files(resp_a)

        # Method B: Mechanism-hypothesis prompt (the +15% winner)
        resp_b = call_claude(f"""You are debugging a gVisor GPU checkpoint/restore system.
First, think about what INTERNAL MECHANISM could cause this symptom.
What function, struct, or code path is likely responsible?
Then predict which source files contain the bug.

Bug report: {symptom}

Think step by step about the mechanism, then output ONLY file paths at the end, one per line.""")
        files_b = extract_files(resp_b)

        # Extract mechanism terms from response B for grep
        mechanism_terms = set()
        for word in resp_b.split():
            word = word.strip(".,;:()[]{}\"'`")
            if (len(word) >= 4 and
                any(c.isupper() for c in word[1:]) and  # camelCase or PascalCase
                not word.startswith("http") and
                not word.startswith("//") and
                word not in {"This", "That", "When", "With", "From", "After", "Before", "Each", "Such", "Most", "Some"}):
                mechanism_terms.add(word)
        # Also grab anything that looks like a Go identifier
        for m in re.finditer(r'\b[A-Z][a-zA-Z]+(?:\.[A-Z][a-zA-Z]+)+\b', resp_b):
            mechanism_terms.add(m.group(0).split(".")[-1])

        print(f"  Mechanism terms: {sorted(mechanism_terms)[:10]}")

        # Method C: Mechanism prompt + grep grounding
        grep_results = grep_repo(list(mechanism_terms), REPO)
        grep_context = "\n".join(f"  {f} ({count} matches)" for f, count in grep_results)

        if grep_context.strip():
            resp_c = call_claude(f"""You are debugging a gVisor GPU checkpoint/restore system.
Bug report: {symptom}

I searched the codebase for terms related to the likely mechanism and found these files:
{grep_context}

Based on these grep results, which file(s) most likely contain the bug?
Output ONLY full file paths, one per line.""")
        else:
            resp_c = resp_b  # fallback to mechanism prompt if no grep hits

        files_c = extract_files(resp_c)

        hit_a = bool(files_a & gt)
        hit_b = bool(files_b & gt)
        hit_c = bool(files_c & gt)

        baseline_hits += hit_a
        prompt_hits += hit_b
        grep_hits += hit_c
        total += 1

        print(f"  Baseline:      {files_a} -> {'HIT' if hit_a else 'MISS'}")
        print(f"  Mechanism:     {files_b} -> {'HIT' if hit_b else 'MISS'}")
        print(f"  Grep-grounded: {files_c} -> {'HIT' if hit_c else 'MISS'}")
        if grep_results:
            print(f"  Grep top hits: {[f for f,_ in grep_results[:5]]}")

        time.sleep(0.5)

    print(f"\n{'='*60}")
    print(f"FINAL RESULTS ({total} cases, unseen code)")
    print(f"{'='*60}")
    print(f"Baseline (plain):        {baseline_hits}/{total} = {baseline_hits/max(1,total)*100:.1f}%")
    print(f"Mechanism prompt:        {prompt_hits}/{total} = {prompt_hits/max(1,total)*100:.1f}%")
    print(f"Grep-grounded:           {grep_hits}/{total} = {grep_hits/max(1,total)*100:.1f}%")
    print(f"")
    print(f"Grep vs baseline delta:  {grep_hits-baseline_hits} ({(grep_hits-baseline_hits)/max(1,total)*100:+.1f}%)")
    print(f"Grep vs mechanism delta: {grep_hits-prompt_hits} ({(grep_hits-prompt_hits)/max(1,total)*100:+.1f}%)")
    print(f"")
    if grep_hits > baseline_hits:
        print(f"VERDICT: Grep-grounding BEATS baseline. There's a thread to pull.")
    elif grep_hits == baseline_hits:
        print(f"VERDICT: Grep-grounding TIES baseline. No evidence external context helps.")
    else:
        print(f"VERDICT: Grep-grounding LOSES to baseline. Investigation is done.")
    print(f"         Ship the mechanism prompt. Delete the graph.")


if __name__ == "__main__":
    main()
