"""S3 follow-up: what does it cost to reload the decision model to empty its prompt cache?

Ollama's MLX runner keeps a prompt cache that grows ~30 MB per question per call up to ~8.5 GB. noskill can't
change the server's settings, but it can unload the model and load it again. This times, idle, model alone:
  unload     the unload request
  cold       the first decision after unloading (load + read prompt)
  warm       the next decision on a different request
  python3 spikes/s3/reload.py [--model tev1:0.8b] [--n 8]
Writes results/s3-reload.jsonl.
"""
import argparse
import json
import statistics as st
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(Path(__file__).parent))
from common import ollama  # noqa: E402
from run import ROUTER_QUESTIONS, load, state_of  # noqa: E402

QS = {"task_type": ROUTER_QUESTIONS["task_type"]}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="tev1:0.8b")
    ap.add_argument("--n", type=int, default=8)
    a = ap.parse_args()
    items = load("router-200.jsonl")[100:100 + 2 * a.n]
    for m in [x["name"] for x in ollama.ps()]:
        ollama.unload(m)
    res = {"unload": [], "cold": [], "warm": []}
    with (ROOT / "results" / "s3-reload.jsonl").open("w") as out:
        for i in range(a.n):
            t0 = time.perf_counter()
            ollama.unload(a.model)
            res["unload"].append((time.perf_counter() - t0) * 1000)
            for kind, it in (("cold", items[2 * i]), ("warm", items[2 * i + 1])):
                t0 = time.perf_counter()
                ollama.systemone(a.model, state_of(it), QS)
                res[kind].append((time.perf_counter() - t0) * 1000)
            out.write(json.dumps({"model": a.model, **{k: v[-1] for k, v in res.items()}}) + "\n")
    print("| Step | Median ms | Max ms |\n|---|---|---|")
    for k, v in res.items():
        print(f"| {k} | {st.median(v):.0f} | {max(v):.0f} |")
    ollama.unload(a.model)


if __name__ == "__main__":
    main()
