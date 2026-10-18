"""Summarise S4 raw results (results/s4-*.jsonl) as markdown tables.  python3 spikes/s4/report.py"""
import json
import statistics as st
from pathlib import Path

RES = Path(__file__).resolve().parents[1] / "results"


def load(name):
    p = RES / name
    return [json.loads(l) for l in p.read_text().splitlines() if l.strip()] if p.exists() else []


def models(rs, key="model"):
    return list(dict.fromkeys(r[key] for r in rs if r.get(key)))


def gb(r, m):
    return next((l["gb"] for l in r["loaded"] if l["name"] == m), None)


# ---- load
rs = load("s4-load.jsonl")
if rs:
    print("### Load times\n| Model | Warm median / max ms | After the other models median / max ms |\n|---|---|---|")
    for m in models(rs):
        w = [r["load_ms"] for r in rs if r["model"] == m and r["kind"] == "warm"]
        c = [r["load_ms"] for r in rs if r["model"] == m and r["kind"] == "after others"]
        print(f"| {m} | {st.median(w):.0f} / {max(w):.0f} | {st.median(c):.0f} / {max(c):.0f} |")

# ---- reload
rs = load("s4-reload.jsonl")
if rs:
    print("\n### Changing num_ctx\n| Model | First load (4096) | Same ctx again | 4096 → 8192 | Same again | 8192 → 4096 |\n|---|---|---|---|---|---|")
    for m in models(rs):
        x = {r["step"]: r["load_ms"] for r in rs if r["model"] == m}
        print(f"| {m} | {x['first load at 4096']:.0f} ms | {x['same 4096']:.0f} ms | {x['change to 8192']:.0f} ms | {x['same 8192']:.0f} ms | {x['back to 4096']:.0f} ms |")

# ---- ctx
rs = load("s4-ctx.jsonl")
if rs:
    print("\n### Memory by context size and KV cache type (Ollama's reported size, GB)\n| Model | f16 8k | f16 16k | q8_0 8k | q8_0 16k |\n|---|---|---|---|---|")
    for m in models(rs):
        cell = {}
        for r in rs:
            if r["model"] == m:
                cell[(r["label"], r["ctx"])] = gb(r, m)
        f = lambda k: f"{cell[k]:.2f}" if cell.get(k) else "–"
        print(f"| {m} | {f(('f16', 8192))} | {f(('f16', 16384))} | {f(('q8_0', 8192))} | {f(('q8_0', 16384))} |")

# ---- swap
rs = [r for r in load("s4-swap.jsonl") if r.get("from")]
if rs:
    print("\n### Swaps (tev1:0.8b resident; unload the current primary, load the next)\n| To | Runs | Median ms | Max ms | Lowest free % | Highest pressure level |\n|---|---|---|---|---|---|")
    for m in models(rs, "to"):
        xs = [r for r in rs if r["to"] == m]
        t = [r["total_ms"] for r in xs]
        print(f"| {m} | {len(xs)} | {st.median(t):.0f} | {max(t):.0f} | {min(r['free_pct'] for r in xs)} | {max(r.get('pressure') or 0 for r in xs)} |")

# ---- cache
rs = [r for r in load("s4-cache.jsonl") if r.get("i") is not None]
if rs:
    print("\n### 200 varied requests, model alone (8k context)\n| Model | Runner | Size at start → end (GB) | Median ms first 10 / last 10 | Lowest free % |\n|---|---|---|---|---|")
    for m in models(rs):
        xs = [r for r in rs if r["model"] == m]
        ms = [r["median_ms_last10"] for r in xs if r.get("median_ms_last10")]
        runner = next((l["runner"] for l in xs[-1]["loaded"] if l["name"] == m), "?")
        print(f"| {m} | {runner} | {gb(xs[0], m)} → {gb(xs[-1], m)} | {ms[0]:.0f} / {ms[-1]:.0f} | {min(r['free_pct'] for r in xs)} |")

# ---- resident
rs = load("s4-resident.jsonl")
if rs:
    print("\n### tev1:0.8b resident (reloaded when its cache passes 1 GB) + one primary, 200 varied requests\n"
          "| Primary | Lowest / median free % | Samples at warning level (of 21) | Swap used GB start → end | Resident reloads | Decision median ms | Primary reply median ms (24 tokens) |\n|---|---|---|---|---|---|---|")
    for m in models(rs, "primary"):
        allx = [r for r in rs if r.get("primary") == m and r.get("mode") == "ceiling"]
        xs = [r for r in allx if r.get("i")]
        if not xs:
            continue
        free = [r["free_pct"] for r in allx]
        warn = sum(1 for r in allx if (r.get("pressure") or 0) >= 2)
        print(f"| {m} | {min(free)} / {st.median(free):.0f} | {warn} | {allx[0]['swap_gb']} → {xs[-1]['swap_gb']} | {xs[-1].get('reloads')} | "
              f"{st.median(r['decision_median_ms'] for r in xs):.0f} | {st.median(r['primary_median_ms'] for r in xs):.0f} |")
    stops = [r for r in rs if r.get("stopped")]
    print(f"\nRuns stopped by the memory guard: {len(stops)}")

# ---- S1 subset on q8_0
s1 = load("s4-s1-q8.jsonl")
if s1:
    ok = [r for r in s1 if r.get("success")]
    print(f"\n### S1 subset with q8_0 KV: {len(ok)}/{len(s1)} = {100 * len(ok) / len(s1):.0f}% success")
    for seed in sorted({r.get("seed") for r in s1}):
        xs = [r for r in s1 if r.get("seed") == seed]
        print(f"- seed {seed}: {sum(1 for r in xs if r.get('success'))}/{len(xs)}")
