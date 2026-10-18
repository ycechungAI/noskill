"""Summarise results/s1-raw.jsonl as markdown and apply S1's decision rules."""
import json
import statistics as st
import sys
from collections import defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
path = Path(sys.argv[1]) if len(sys.argv) > 1 else ROOT / "results" / "s1-raw.jsonl"
raw = [json.loads(l) for l in path.read_text().splitlines() if l.strip()]
# One row per (model, strategy, seed, task): the last successful attempt, else the last attempt.
# A run that timed out (model kept generating until the 300 s request limit) counts as a failure,
# because a real run would have failed too. Other errors (harness/server) are left out.
last = {}
for r in raw:
    k = (r["model"], r["strategy"], r["seed"], r["task"])
    if k not in last or last[k].get("error") or not r.get("error"):
        last[k] = r
rows, timeouts, errors = [], [], 0
for r in last.values():
    if not r.get("error"):
        rows.append(r)
    elif "timed out" in r["error"]:
        rows.append({**r, "success": False})
        timeouts.append(r)
    else:
        errors += 1

g = defaultdict(list)
for r in rows:
    g[(r["model"], r["strategy"])].append(r)


def pct(xs):
    return 100 * sum(xs) / len(xs) if xs else float("nan")


summary = {}
print(f"Runs: {len(rows)} scored ({len(timeouts)} timed out, counted as failures), {errors} left out after errors. Source: {path.name}")
for r in timeouts:
    print(f"- timed out: {r['model']} {r['strategy']} seed {r['seed']} {r['task']} after {len(r['calls'])} calls")
print()
print("| Model | Strategy | Valid calls | Right tool | Right args | Success (all) | Single | Two-step | No-tool | False calls | Median ms | Success by seed |")
print("|---|---|---|---|---|---|---|---|---|---|---|---|")
for (m, s), rs in sorted(g.items()):
    calls = [c for r in rs for c in r["calls"]]
    tool_tasks = [r for r in rs if r["kind"] != "none"]
    none_tasks = [r for r in rs if r["kind"] == "none"]
    by_seed = defaultdict(list)
    for r in rs:
        by_seed[r["seed"]].append(r["success"])
    d = {
        "valid": pct([c["valid"] for c in calls]),
        "tool": pct([r["right_tool"] for r in tool_tasks]),
        "args": pct([r["right_args"] for r in tool_tasks]),
        "succ": pct([r["success"] for r in rs]),
        "single": pct([r["success"] for r in rs if r["kind"] == "single"]),
        "two": pct([r["success"] for r in rs if r["kind"] == "two_step"]),
        "none": pct([r["success"] for r in none_tasks]),
        "false": pct([r["false_call"] for r in none_tasks]),
        "ms": st.median(r["ms"] for r in rs),
        "seeds": " / ".join(f"{pct(v):.0f}" for _, v in sorted(by_seed.items())),
        "n": len(rs),
    }
    summary[(m, s)] = d
    print(f"| {m} | {s} | {d['valid']:.0f}% | {d['tool']:.0f}% | {d['args']:.0f}% | **{d['succ']:.0f}%** | {d['single']:.0f}% | {d['two']:.0f}% | "
          f"{d['none']:.0f}% | {d['false']:.0f}% | {d['ms']:.0f} | {d['seeds']} |")

print("\n### Decision rules (all tested models are < 7B)\n")
for m in sorted({k[0] for k in summary}):
    nat, con, pro = (summary.get((m, s)) for s in ("native", "constrained", "prompted"))
    if not (nat and con):
        continue
    diff = con["succ"] - nat["succ"]
    if diff >= 10:
        verdict = "constrained ≥ native + 10 → keep plan: constrained default for < 7B"
    elif diff <= -10:
        verdict = "native ≥ constrained + 10 → native everywhere; constrained only for models without tool support"
    else:
        verdict = "within ±10 → native default; constrained as fallback after a malformed call"
    overhead = 100 * (con["ms"] / nat["ms"] - 1)
    print(f"- **{m}**: constrained − native = {diff:+.0f} pts → {verdict}. "
          f"Constrained latency overhead {overhead:+.0f}% (limit 15%)." + (f" Prompted: {pro['succ']:.0f}%." if pro else ""))
    worst = max((summary[(m, s)]["false"], s) for s in ("native", "constrained", "prompted") if (m, s) in summary)
    if worst[0] > 10:
        print(f"  - False-call rate {worst[0]:.0f}% ({worst[1]}) > 10% → add a \"tool needed?\" decision before tool selection.")

print("\n### Per-task success (all models, strategies and seeds)\n")
task = defaultdict(list)
for r in rows:
    task[r["task"]].append(r["success"])
hard = sorted(task.items(), key=lambda kv: pct(kv[1]))[:12]
print("Hardest tasks: " + ", ".join(f"{t} {pct(v):.0f}%" for t, v in hard))
