"""S3 follow-up: where does /v1/systemone time go? (prompt size, question count, prefix caching)

Run on an otherwise idle machine, after s3/run.py:
  python3 spikes/s3/latency_probe.py [--models tev1:0.8b,tev1] [--n 20]
Writes results/s3-latency.jsonl and prints a table.
"""
import argparse
import json
import statistics as st
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(Path(__file__).parent))
from common import ollama  # noqa: E402
from run import LEVELS, NOULS, ROUTER_QUESTIONS, TYPES, load, state_of  # noqa: E402

SHORT_TYPES = {k: k for k in TYPES}  # names only, no descriptions
VARIANTS = {
    "all five (as in S3)": ROUTER_QUESTIONS,
    "task_type only": {"task_type": ROUTER_QUESTIONS["task_type"]},
    "task_type, names only": {"task_type": {"type": "choice", "instructions": "Which kind of task is this request?", "criteria": SHORT_TYPES}},
    "difficulty only": {"difficulty": ROUTER_QUESTIONS["difficulty"]},
    "needs_tools only": {"needs_tools": ROUTER_QUESTIONS["needs_tools"]},
    "all five, short type names": {**ROUTER_QUESTIONS, "task_type": {"type": "choice", "instructions": "Which kind of task is this request?", "criteria": SHORT_TYPES}},
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--models", default="tev1:0.8b,tev1")
    ap.add_argument("--n", type=int, default=20, help="requests per variant")
    a = ap.parse_args()
    items = load("router-200.jsonl")[:a.n]
    out = (ROOT / "results" / "s3-latency.jsonl").open("w")
    print("| Model | Variant | Input tokens | p50 ms (new request each time) | Same request again, p50 ms |\n|---|---|---|---|---|")
    for model in a.models.split(","):
        ollama.systemone(model, state_of(items[0]), ROUTER_QUESTIONS)  # load
        for name, qs in VARIANTS.items():
            ms, toks, again = [], [], []
            for it in items:
                r, t = ollama.systemone(model, state_of(it), qs)
                ms.append(t)
                toks.append(r.get("usage", {}).get("input_tokens"))
                _, t2 = ollama.systemone(model, state_of(it), qs)  # identical call right after
                again.append(t2)
                out.write(json.dumps({"model": model, "variant": name, "id": it["id"], "ms": t, "again_ms": t2,
                                      "in_tok": toks[-1]}) + "\n")
                out.flush()
            print(f"| {model} | {name} | {st.median([x for x in toks if x is not None] or [0]):.0f} | "
                  f"{st.median(ms):.0f} | {st.median(again):.0f} |", flush=True)
        ollama.unload(model)


if __name__ == "__main__":
    main()
