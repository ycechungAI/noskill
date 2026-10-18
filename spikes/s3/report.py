"""Summarise S3 raw results as markdown and apply S3's decision rules."""
import json
import statistics as st
from collections import Counter, defaultdict
from pathlib import Path

RES = Path(__file__).resolve().parents[1] / "results"
NOULS = ["needs_web", "needs_vision", "needs_tools"]


def load(name):
    p = RES / name
    return [json.loads(l) for l in p.read_text().splitlines() if l.strip()] if p.exists() else []


def q(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(p * len(xs)))] if xs else float("nan")


def f1(pairs):
    tp = sum(1 for y, p in pairs if y and p)
    fp = sum(1 for y, p in pairs if not y and p)
    fn = sum(1 for y, p in pairs if y and not p)
    return 2 * tp / (2 * tp + fp + fn) if tp else 0.0


def ece(pairs, bins=10):
    pairs = [(c, ok) for c, ok in pairs if c is not None]
    if not pairs:
        return None
    total = 0.0
    for b in range(bins):
        lo, hi = b / bins, (b + 1) / bins
        inb = [(c, ok) for c, ok in pairs if lo <= c < hi or (b == bins - 1 and c == 1.0)]
        if inb:
            total += abs(st.mean(ok for _, ok in inb) - st.mean(c for c, _ in inb)) * len(inb) / len(pairs)
    return total


CURRENT = {}
_p = Path(__file__).resolve().parents[1] / "data" / "router-200.jsonl"
if _p.exists():
    CURRENT = {i["id"]: i["labels"] for i in map(json.loads, filter(str.strip, _p.read_text().splitlines()))}


def gold(rec):
    """The settled label (labels.final) if there is one; else labeller a, or the agreed label when
    labeller b exists (disagreements dropped).
    Uses the current labels in data/router-200.jsonl, so labelling can finish after S3 has run."""
    L = CURRENT.get(rec["id"], rec["labels"])
    if "final" in L:  # settled with spikes/data/adjudicate.py
        return L["final"]
    if "b" in L:
        a, b = L["a"], L["b"]
        if a["task_type"] != b["task_type"]:
            return None
        return {"task_type": a["task_type"], "difficulty": (a["difficulty"] + b["difficulty"]) / 2,
                **{k: a[k] for k in NOULS if a[k] == b[k]}}  # needs_* left out where a and b disagree
    return L["a"]


# ---------------- noul ----------------
noul = load("s3-noul.jsonl")
summary = {}
if noul:
    print("## noul-40\n\n| Model | Mean on yes items | Mean on no items | Min yes | Max no | Accuracy @0.5 | p50 ms |\n|---|---|---|---|---|---|---|")
    by = defaultdict(list)
    for r in noul:
        by[r["model"]].append(r)
    for m, rs in by.items():
        yes = [r["noul"] for r in rs if r["expected"]]
        no = [r["noul"] for r in rs if not r["expected"]]
        acc = sum((r["noul"] >= 0.5) == r["expected"] for r in rs) / len(rs)
        summary[("noul", m)] = (st.mean(yes), st.mean(no))
        print(f"| {m} | {st.mean(yes):.3f} | {st.mean(no):.3f} | {min(yes):.3f} | {max(no):.3f} | {acc:.0%} | {q([r['ms'] for r in rs], .5):.0f} |")
    for m, rs in by.items():
        wrong = [r for r in rs if (r["noul"] >= 0.5) != r["expected"]]
        if wrong:
            print(f"\n{m} wrong: " + "; ".join(f"{r['id']} ({r['noul']:.2f})" for r in wrong))


# ---------------- router ----------------
def router_table(rows, title):
    if not rows:
        return {}
    print(f"\n## {title}\n")
    print("| Model | Mode | n | Task type acc. | Difficulty MAE | F1 web | F1 vision | F1 tools | ECE (task type) | p50 ms | p95 ms | Errors |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|")
    by = defaultdict(list)
    for r in rows:
        by[(r["model"], r["mode"])].append(r)
    out = {}
    for (m, mode), rs in by.items():
        ok = [r for r in rs if not r["error"] and gold(r)]
        errs = sum(1 for r in rs if r["error"])
        acc = st.mean(r["pred"]["task_type"] == gold(r)["task_type"] for r in ok)
        mae = st.mean(abs(r["pred"]["difficulty"] - gold(r)["difficulty"]) for r in ok)
        f1s = {k: f1([(gold(r)[k], r["pred"][k] >= 0.5) for r in ok if k in gold(r)]) for k in NOULS}
        if mode == "jev":
            conf = [(max(r["pred"]["tt_probs"].values()), r["pred"]["task_type"] == gold(r)["task_type"]) for r in ok]
        else:
            conf = [(r["pred"].get("tt_conf_first_token"), r["pred"]["task_type"] == gold(r)["task_type"]) for r in ok]
        e = ece(conf)
        ms = [r["pred"]["ms"] for r in ok]
        out[m] = {"acc": acc, "p50": q(ms, .5), "mode": mode}
        print(f"| {m} | {mode} | {len(ok)} | **{acc:.1%}** | {mae:.2f} | {f1s['needs_web']:.2f} | {f1s['needs_vision']:.2f} | {f1s['needs_tools']:.2f} | "
              f"{'' if e is None else f'{e:.3f}'} | {q(ms, .5):.0f} | {q(ms, .95):.0f} | {errs} |")
    for (m, mode), rs in by.items():
        conf = Counter((gold(r)["task_type"], r["pred"]["task_type"]) for r in rs if not r["error"] and gold(r) and r["pred"]["task_type"] != gold(r)["task_type"])
        if conf:
            print(f"\n{m} top confusions (label → predicted): " + ", ".join(f"{a}→{b} ×{n}" for (a, b), n in conf.most_common(6)))
    return out


router = router_table(load("s3-router.jsonl"), "router-200 (idle machine)")
cont = router_table(load("s3-contention.jsonl"), "router-200 with a chat model generating at the same time")

mem = load("s3-memory.jsonl")
if mem:
    print("\n## Memory snapshots\n\n| When | Free memory | Loaded models |\n|---|---|---|")
    for s in mem:
        print(f"| {s['tag']} | {s['free_pct']}% | " + ", ".join(f"{m['name']} {m['gb']} GB ({m['runner']}, ctx {m['ctx']})" for m in s["loaded"]) + " |")

print("\n## Decision rules\n")
for (_, m), (y, n) in ((k, v) for k, v in summary.items() if k[0] == "noul"):
    print(f"- noul ({m}): yes-mean {y:.2f}, no-mean {n:.2f} → " + ("confirms noul = P(yes)" if y >= .9 and n <= .1 else "does NOT meet ≥0.9 / ≤0.1; treat noul as a score and calibrate a threshold"))
for m, d in cont.items():
    rule = "≥ 85% and p50 ≤ 150 ms under contention"
    ok = d["acc"] >= .85 and d["p50"] <= 150
    print(f"- {m} under contention: {d['acc']:.1%}, p50 {d['p50']:.0f} ms → {'meets' if ok else 'does not meet'} '{rule}'")
jev_best = max((d["acc"] for d in router.values() if d["mode"] == "jev"), default=None)
for m, d in router.items():
    if d["mode"] == "emulated" and jev_best is not None:
        gap = 100 * (jev_best - d["acc"])
        print(f"- emulated {m}: {d['acc']:.1%} vs best decision model {jev_best:.1%} ({gap:+.1f} pts) → " +
              ("within 5 pts: decision model optional, recommended for speed" if gap <= 5 else "decision model clearly better"))
