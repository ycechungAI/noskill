"""S3 follow-up: can tev1:0.8b reach ~150 ms with fewer/shorter questions, and what does it cost in accuracy?

Each /v1/systemone question is read as its own "request + question" prompt, so time grows with the number
and length of questions (see latency_probe.py). This runs three question sets on router-200, idle (model
alone, unloaded between sets) and on the first --n-load requests while a chat model generates.
  python3 spikes/s3/lean.py [--model tev1:0.8b] [--chat qwen3.5:4b] [--n-load 50]
Writes results/s3-lean.jsonl (same pred format as s3-router.jsonl, plus "set" and "load").
"""
import argparse
import json
import sys
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(Path(__file__).parent))
from common import ollama  # noqa: E402
from run import NOULS, ROUTER_QUESTIONS, TYPES, labels, load, memory_snapshot, state_of  # noqa: E402

NAMES_ONLY = {"type": "choice", "instructions": "Which kind of task is this request?", "criteria": {k: k for k in TYPES}}
SHORT = {"type": "choice", "instructions": "Which kind of task is this request?", "criteria": {  # fits laya's 176-token option budget
    "chat": "Casual talk, advice or general knowledge",
    "code": "Write, explain, fix or review code or commands",
    "reasoning": "Logic, trade-offs, diagnosis, multi-step thinking",
    "math": "Calculations, algebra, probability or proofs",
    "research": "Needs current information from the web",
    "summarise": "Condense given text, a page or a document",
    "extract": "Pull structured data out of given text or a file",
    "translate": "Translate between languages",
    "creative": "Stories, poems, names or other creative writing",
    "vision": "About an attached image",
    "plan": "A plan, schedule, itinerary or step-by-step strategy"}}
SETS = {
    "all five (S3)": ROUTER_QUESTIONS,
    "type, names only": {"task_type": NAMES_ONLY},
    "type names + tools": {"task_type": NAMES_ONLY, "needs_tools": ROUTER_QUESTIONS["needs_tools"]},
    "type with descriptions only": {"task_type": ROUTER_QUESTIONS["task_type"]},
    "type, short descriptions": {"task_type": SHORT},
    "all five, short descriptions": {**ROUTER_QUESTIONS, "task_type": SHORT},
}
WARM = {"request": "hello there"}  # not in router-200, so warming up caches nothing we measure


def ask(model, item, qs):
    r, ms = ollama.systemone(model, state_of(item), qs)
    a = r["answers"]
    pred = {"task_type": a["task_type"]["choice"], "tt_probs": a["task_type"].get("probabilities"), "ms": ms,
            "in_tok": r.get("usage", {}).get("input_tokens")}
    if "difficulty" in a:
        pred["difficulty"] = a["difficulty"]["score"] + 1
    for k in NOULS:
        if k in a:
            pred[k] = a[k]["noul"]
    return pred


def run(model, items, load_tag, out, sets):
    for name in sets:
        qs = SETS[name]
        ollama.unload(model)
        ollama.systemone(model, WARM, qs)
        memory_snapshot(f"lean:{load_tag}:{name}")
        for it in items:
            try:
                pred, err = ask(model, it, qs), None
            except Exception as e:  # noqa: BLE001
                pred, err = {}, str(e)[:300]
            out.write(json.dumps({"model": model, "set": name, "load": load_tag, "id": it["id"], "labels": labels(it),
                                  "pred": pred, "error": err}) + "\n")
            out.flush()
        print(f"lean: {load_tag} · {name} done", flush=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="tev1:0.8b")
    ap.add_argument("--chat", default="qwen3.5:4b")
    ap.add_argument("--n-load", type=int, default=50)
    ap.add_argument("--sets", default="|".join(list(SETS)[:3]), help="set names separated by |")
    ap.add_argument("--append", action="store_true", help="add to s3-lean.jsonl instead of starting over")
    a = ap.parse_args()
    sets = a.sets.split("|")
    items = load("router-200.jsonl")
    for m in [x["name"] for x in ollama.ps()]:
        ollama.unload(m)  # start with nothing else resident
    with (ROOT / "results" / "s3-lean.jsonl").open("a" if a.append else "w") as out:
        run(a.model, items, "idle", out, sets)
        stop = threading.Event()

        def churn():
            think = False if "thinking" in ollama.show(a.chat).get("capabilities", []) else None
            while not stop.is_set():
                ollama.chat(a.chat, [{"role": "user", "content": "Write a long, detailed story about a lighthouse keeper. At least 1500 words."}],
                            options={"num_predict": 600, "num_ctx": 8192, "temperature": 0.8}, think=think)

        ollama.chat(a.chat, [{"role": "user", "content": "hi"}], options={"num_ctx": 8192})
        th = threading.Thread(target=churn, daemon=True)
        th.start()
        time.sleep(3)
        try:
            run(a.model, items[:a.n_load], "under load", out, sets)
        finally:
            stop.set()
            th.join(timeout=120)


if __name__ == "__main__":
    main()
