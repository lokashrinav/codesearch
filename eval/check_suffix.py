import json

with open("eval_graph_results.json") as f:
    results = json.load(f)

print("Checking if graph losses are just path truncation:\n")
suffix_matches = 0
for d in results["disagreements"]:
    if d["winner"] == "baseline":
        gt = d["gt"][0] if d["gt"] else ""
        for gf in d["graph"]:
            if gt.endswith(gf) and gf != gt:
                print(f"  {d['id']}: graph='{gf}' is suffix of gt='{gt}' -- PATH BUG")
                suffix_matches += 1
                break
        else:
            print(f"  {d['id']}: graph={d['graph']} vs gt={d['gt']} -- REAL MISS")

print(f"\nPath bugs: {suffix_matches}/{len([d for d in results['disagreements'] if d['winner']=='baseline'])}")
