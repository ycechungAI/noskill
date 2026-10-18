"""S3: JEV decision models on 16 GB (PHASE0_TESTS.md#s3-jev-decision-models-on-16-gb).

  python3 spikes/s3/run.py noul        # noul-40 on each decision model
  python3 spikes/s3/run.py router      # router-200 on decision models + emulated on chat models
  python3 spikes/s3/run.py contention  # router-200 on decision models while a chat model keeps generating
  python3 spikes/s3/report.py

Raw results: results/s3-<part>.jsonl (one line per item per model). Memory snapshots
(/api/ps + macOS memory_pressure) are written to results/s3-memory.jsonl.
"""
import argparse
import json
import math
import re
import subprocess
import sys
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from common import ollama  # noqa: E402

DATA, RES = ROOT / "data", ROOT / "results"
RES.mkdir(exist_ok=True)

TYPES = {
    "chat": "Casual conversation, opinions, advice or general knowledge a chat model can answer directly",
    "code": "Writing, explaining, fixing or reviewing code, shell commands, regexes or config files",
    "reasoning": "Logic puzzles, trade-offs, diagnosis and multi-step thinking without heavy calculation",
    "math": "Calculations, algebra, calculus, probability or proofs",
    "research": "Needs current or external information gathered from the web, with sources",
    "summarise": "Condensing provided text, a web page or a document",
    "extract": "Pulling structured data (fields, entities, tables, JSON) out of provided text or a file",
    "translate": "Translating between languages or explaining the meaning of foreign words",
    "creative": "Stories, poems, lyrics, names, marketing copy and other creative writing",
    "vision": "The request is about an attached image or screenshot",
    "plan": "Making a plan, schedule, itinerary, roadmap or step-by-step strategy",
}
LEVELS = ["trivial", "easy", "moderate", "hard", "expert"]
NOULS = {
    "needs_web": "Does answering need up-to-date or external information from the internet?",
    "needs_vision": "Does the request include an attached image that must be looked at?",
    "needs_tools": "Does answering need something outside the model itself: web access, the user's files, the current time, or running code for an exact calculation?",
}
ROUTER_QUESTIONS = {
    "task_type": {"type": "choice", "instructions": "Which kind of task is this request?", "criteria": TYPES},
    "difficulty": {"type": "score", "instructions": "How difficult is this request for a small (4B) AI model to do well?", "criteria": LEVELS},
    **{k: {"type": "noul", "instructions": v} for k, v in NOULS.items()},
}
EMU_SCHEMA = {"type": "object", "properties": {
    "task_type": {"type": "string", "enum": list(TYPES)},
    "difficulty": {"type": "integer", "enum": [1, 2, 3, 4, 5]},
    **{k: {"type": "boolean"} for k in NOULS}}, "required": ["task_type", "difficulty", *NOULS]}
EMU_SYS = ("You classify requests for a model router. Reply with JSON only.\n"
           "task_type, one of:\n" + "\n".join(f"- {k}: {v}" for k, v in TYPES.items()) +
           "\ndifficulty for a small (4B) model: 1 trivial, 2 easy, 3 moderate, 4 hard, 5 expert.\n" +
           "\n".join(f"{k}: {v}" for k, v in NOULS.items()))


def load(name):
    return [json.loads(l) for l in (DATA / name).read_text().splitlines() if l.strip()]


def state_of(item):
    s = {"request": item["prompt"]}
    if item["attachments"]:
        s["attachments"] = item["attachments"]
    return s


def memory_snapshot(tag):
    try:
        mp = subprocess.run(["memory_pressure", "-Q"], capture_output=True, text=True, timeout=10).stdout
        free = re.search(r"free percentage:\s*(\d+)%", mp)
        free = int(free.group(1)) if free else None
    except (OSError, subprocess.TimeoutExpired):
        free = None
    vm = subprocess.run(["vm_stat"], capture_output=True, text=True).stdout
    swapouts = re.search(r"Swapouts:\s+(\d+)", vm)
    snap = {"tag": tag, "t": time.time(), "free_pct": free, "swapouts": int(swapouts.group(1)) if swapouts else None,
            "loaded": [{"name": m["name"], "gb": round(m["size"] / 1e9, 2), "ctx": m.get("context_length"), "runner": m.get("runner")} for m in ollama.ps()]}
    with open(RES / "s3-memory.jsonl", "a") as f:
        f.write(json.dumps(snap) + "\n")
    print("memory", tag, snap["free_pct"], "% free;", ", ".join(f"{m['name']} {m['gb']} GB" for m in snap["loaded"]), flush=True)
    return snap


def write(path, rec):
    with open(path, "a") as f:
        f.write(json.dumps(rec) + "\n")


# ---------------- decision models ----------------
def jev_router(model, item):
    r, ms = ollama.systemone(model, state_of(item), ROUTER_QUESTIONS)
    a = r["answers"]
    return {"task_type": a["task_type"]["choice"], "tt_probs": a["task_type"]["probabilities"], "tt_conf": a["task_type"].get("confidence"),
            "difficulty": a["difficulty"]["score"] + 1, "diff_conf": a["difficulty"].get("confidence"),
            **{k: a[k]["noul"] for k in NOULS}, "ms": ms, "in_tok": r.get("usage", {}).get("input_tokens")}


# ---------------- emulated with a chat model ----------------
def value_token_prob(logprobs, content, key):
    """P of the first token of the JSON value for `key`, from Ollama logprobs."""
    m = re.search(r'"%s"\s*:\s*"?' % key, content)
    if not m or not logprobs:
        return None, None
    pos, off = m.end(), 0
    for lp in logprobs:
        tok = lp.get("token", "")
        if off <= pos < off + max(len(tok), 1):
            return math.exp(lp["logprob"]), lp.get("top_logprobs", [])
        off += len(tok)
    return None, None


def emu_router(model, item, think):
    msgs = [{"role": "system", "content": EMU_SYS},
            {"role": "user", "content": f"Request: {item['prompt']}" + (f"\nAttachments: {', '.join(item['attachments'])}" if item["attachments"] else "\nAttachments: none")}]
    r, ms = ollama.chat(model, msgs, fmt=EMU_SCHEMA, options={"temperature": 0, "num_ctx": 4096}, think=think, logprobs=True, top_logprobs=5)
    content = r["message"]["content"]
    obj = json.loads(content)
    lps = r.get("logprobs") or []
    out = {"task_type": obj["task_type"], "difficulty": obj["difficulty"], "ms": ms, "in_tok": r.get("prompt_eval_count")}
    p, _ = value_token_prob(lps, content, "task_type")
    out["tt_conf_first_token"] = p
    for k in NOULS:
        p, top = value_token_prob(lps, content, k)
        if p is None:
            out[k] = 1.0 if obj[k] else 0.0
        else:
            out[k] = p if obj[k] else 1 - p
    return out


def labels(item):
    return item["labels"]


def run_router(models_jev, models_emu, out, tag):
    items = load("router-200.jsonl")
    for model in models_jev + models_emu:
        is_jev = model in models_jev
        think = None
        if not is_jev:
            think = False if "thinking" in ollama.show(model).get("capabilities", []) else None
        # warm up so latency excludes the load
        (jev_router(model, items[0]) if is_jev else emu_router(model, items[0], think))
        memory_snapshot(f"{tag}:{model}:loaded")
        for it in items:
            try:
                pred = jev_router(model, it) if is_jev else emu_router(model, it, think)
                err = None
            except Exception as e:  # noqa: BLE001
                pred, err = {}, str(e)[:300]
            write(out, {"model": model, "mode": "jev" if is_jev else "emulated", "id": it["id"], "labels": labels(it), "pred": pred, "error": err})
        print(f"{tag}: {model} done", flush=True)
        if tag == "router":
            ollama.unload(model)


def cmd_noul(models):
    items = load("noul-40.jsonl")
    out = RES / "s3-noul.jsonl"
    for model in models:
        ollama.systemone(model, items[0]["state"], {"answer": {"type": "noul", "instructions": items[0]["question"]}})
        for it in items:
            r, ms = ollama.systemone(model, it["state"], {"answer": {"type": "noul", "instructions": it["question"]}})
            write(out, {"model": model, "id": it["id"], "expected": it["expected"], "noul": r["answers"]["answer"]["noul"], "ms": ms})
        print(f"noul: {model} done", flush=True)


def cmd_contention(models_jev, chat_model):
    stop = threading.Event()
    gen = {"n": 0, "tokens": 0}

    def churn():
        think = False if "thinking" in ollama.show(chat_model).get("capabilities", []) else None
        while not stop.is_set():
            r, _ = ollama.chat(chat_model, [{"role": "user", "content": "Write a long, detailed story about a lighthouse keeper. At least 1500 words."}],
                               options={"num_predict": 600, "num_ctx": 8192, "temperature": 0.8}, think=think)
            gen["n"] += 1
            gen["tokens"] += r.get("eval_count", 0)

    ollama.chat(chat_model, [{"role": "user", "content": "hi"}], options={"num_ctx": 8192})
    th = threading.Thread(target=churn, daemon=True)
    th.start()
    time.sleep(3)
    try:
        run_router(models_jev, [], RES / "s3-contention.jsonl", "contention")
    finally:
        stop.set()
        th.join(timeout=120)
    print(f"contention: chat model generated {gen['tokens']} tokens over {gen['n']} requests", flush=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("part", choices=["noul", "router", "contention"])
    ap.add_argument("--jev", default="tev1:0.8b,tev1")
    ap.add_argument("--emu", default="qwen3.5:4b,llama3.2:3b")
    ap.add_argument("--chat", default="qwen3.5:4b")
    a = ap.parse_args()
    jev = [m for m in a.jev.split(",") if m]
    if a.part == "noul":
        cmd_noul(jev)
    elif a.part == "router":
        run_router(jev, [m for m in a.emu.split(",") if m], RES / "s3-router.jsonl", "router")
    else:
        cmd_contention(jev, a.chat)


if __name__ == "__main__":
    main()
