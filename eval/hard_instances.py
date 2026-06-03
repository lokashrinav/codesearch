"""Find SWE-bench Lite instances where the issue text does NOT
name the ground-truth file. These are the hard cases where
a code graph should actually help."""

import re
import json
from datasets import load_dataset

ds = load_dataset("princeton-nlp/SWE-bench_Lite", split="test")

easy = []
hard = []

for instance in ds:
    issue = instance["problem_statement"].lower()
    patch = instance["patch"]
    gt_files = set(re.findall(r'^diff --git a/(.*?) b/', patch, re.MULTILINE))

    # Check if any ground-truth file is mentioned in the issue text
    mentioned = False
    for f in gt_files:
        # Check full path, filename only, and module path
        fname = f.split('/')[-1]
        module_path = f.replace('/', '.').replace('.py', '')
        if f.lower() in issue or fname.lower() in issue or module_path.lower() in issue:
            mentioned = True
            break

    if mentioned:
        easy.append(instance["instance_id"])
    else:
        hard.append(instance["instance_id"])

print(f"Total: {len(ds)}")
print(f"Easy (file mentioned in issue): {len(easy)} ({len(easy)/len(ds)*100:.1f}%)")
print(f"Hard (file NOT mentioned): {len(hard)} ({len(hard)/len(ds)*100:.1f}%)")
print(f"\nFirst 30 hard instances:")
for h in hard[:30]:
    inst = [i for i in ds if i["instance_id"] == h][0]
    patch = inst["patch"]
    gt_files = set(re.findall(r'^diff --git a/(.*?) b/', patch, re.MULTILINE))
    print(f"  {h}: {gt_files}")
    print(f"    Issue: {inst['problem_statement'][:80]}...")

# Save hard instance IDs
with open("hard_instances.json", "w") as f:
    json.dump(hard, f)
print(f"\nSaved {len(hard)} hard instance IDs to hard_instances.json")
