"""S3 follow-up: does running /v1/systemone calls in parallel make tev1:0.8b faster?

Three ways to answer the five router questions for the same requests, idle, model alone:
  one call     one request with all five questions (as in S3)
  split        the five questions as five calls sent at the same time
  throughput   four different requests (all five questions each) sent at the same time
Uses Ollama's own parallel slots (OLLAMA_NUM_PARALLEL); nothing about the server is changed.
  python3 spikes/s3/parallel.py [--model tev1:0.8b] [--n 24]
Writes results/s3-parallel.jsonl.
"""
import argparse
import json
import statistics as st
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(Path(__file__).parent))
from common import ollama  # noqa: E402
from run import ROUTER_QUESTIONS, load, state_of  # noqa: E402

WARM = {"request": "hello there"}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="tev1:0.8b")
    ap.add_argument("--n", type=int, default=24)
    a = ap.parse_args()
    items = load("router-200.jsonl")
    out = (ROOT / "results" / "s3-parallel.jsonl").open("w")
    res = {}

    def fresh():  # empty prompt cache, so every way starts equal
        ollama.unload(a.model)
        ollama.systemone(a.model, WARM, ROUTER_QUESTIONS)

    # each way gets different requests, so no way benefits from another's cache
    batches = {"one call": items[0:a.n], "split": items[a.n:2 * a.n], "throughput": items[2 * a.n:3 * a.n]}

    fresh()
    t = []
    for it in batches["one call"]:
        t0 = time.perf_counter()
        ollama.systemone(a.model, state_of(it), ROUTER_QUESTIONS)
        t.append((time.perf_counter() - t0) * 1000)
    res["one call"] = t

    fresh()
    t = []
    with ThreadPoolExecutor(5) as ex:
        for it in batches["split"]:
            t0 = time.perf_counter()
            list(ex.map(lambda kv: ollama.systemone(a.model, state_of(it), {kv[0]: kv[1]}), ROUTER_QUESTIONS.items()))
            t.append((time.perf_counter() - t0) * 1000)
    res["split"] = t

    fresh()
    t, wall0 = [], time.perf_counter()
    with ThreadPoolExecutor(4) as ex:
        def one(it):
            t0 = time.perf_counter()
            ollama.systemone(a.model, state_of(it), ROUTER_QUESTIONS)
            return (time.perf_counter() - t0) * 1000
        t = list(ex.map(one, batches["throughput"]))
    wall = time.perf_counter() - wall0
    res["throughput"] = t

    print("| Way | Median ms per routing decision | Decisions per second |\n|---|---|---|")
    for k, v in res.items():
        per_s = len(v) / wall if k == "throughput" else 1000 / st.mean(v)
        print(f"| {k} | {st.median(v):.0f} | {per_s:.1f} |")
        out.write(json.dumps({"model": a.model, "way": k, "ms": v, "per_s": per_s}) + "\n")
    ollama.unload(a.model)


if __name__ == "__main__":
    main()
