"""S4: memory slots and swap cost (PHASE0_TESTS.md#s4-memory-slots-and-swap-cost). macOS only (memory_pressure).

  python3 spikes/s4/run.py load                  # warm load times, and loads after the other models (cold-ish)
  python3 spikes/s4/run.py ctx --label f16       # memory at 8k and 16k context (label the server's KV setting)
  python3 spikes/s4/run.py reload                # does changing num_ctx reload the model?
  python3 spikes/s4/run.py cache --models qwen3:8b,...   # memory and latency over 200 varied requests, model alone
  python3 spikes/s4/run.py resident --primary qwen3:8b [--no-ceiling]   # tev1:0.8b resident + one primary, 200 requests
  python3 spikes/s4/run.py swap                  # swap time between primaries with tev1:0.8b resident
Talks to OLLAMA_HOST (default 127.0.0.1:11434). Rows go to results/s4-<part>.jsonl. Every long part stops itself
if free memory drops below --stop-below % (default 8) or macOS reports critical memory pressure.
"""
import argparse
import json
import os
import re
import statistics as st
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(Path(__file__).parent))
from common import ollama  # noqa: E402

RES = ROOT / "results"
PRIMARIES = ["qwen3:8b", "qwen2.5-coder:7b", "qwen3:4b", "qwen2.5vl:7b"]
RESIDENT = "tev1:0.8b"
RESIDENT_WEIGHTS_GB = 1.68  # S3: tev1:0.8b just loaded
TYPES = {"chat": "Casual talk, advice or general knowledge", "code": "Write, explain, fix or review code or commands",
         "reasoning": "Logic, trade-offs, diagnosis, multi-step thinking", "math": "Calculations, algebra, probability or proofs",
         "research": "Needs current information from the web", "summarise": "Condense given text, a page or a document",
         "extract": "Pull structured data out of given text or a file", "translate": "Translate between languages",
         "creative": "Stories, poems, names or other creative writing", "vision": "About an attached image",
         "plan": "A plan, schedule, itinerary or step-by-step strategy"}
DECISION = {"task_type": {"type": "choice", "instructions": "Which kind of task is this request?", "criteria": TYPES}}


def free_pct():
    out = subprocess.run(["memory_pressure", "-Q"], capture_output=True, text=True, timeout=10).stdout
    m = re.search(r"free percentage:\s*(\d+)%", out)
    return int(m.group(1)) if m else None


def pressure_level():
    """macOS memory pressure as the OS reports it: 1 normal, 2 warning, 4 critical."""
    out = subprocess.run(["sysctl", "-n", "kern.memorystatus_vm_pressure_level"], capture_output=True, text=True).stdout.strip()
    return int(out) if out.isdigit() else None


def swap_used_gb():
    out = subprocess.run(["sysctl", "-n", "vm.swapusage"], capture_output=True, text=True).stdout
    m = re.search(r"used = ([\d.]+)M", out)
    return round(float(m.group(1)) / 1024, 2) if m else None


def runners():
    """{model: runner} from `ollama ps` (the RUNNER column)."""
    out = subprocess.run(["ollama", "ps"], capture_output=True, text=True, env={**os.environ, "OLLAMA_HOST": ollama.BASE}).stdout.splitlines()
    res = {}
    for line in out[1:]:
        parts = re.split(r"\s{2,}", line.strip())
        if len(parts) >= 2:
            res[parts[0]] = parts[-2] if len(parts) >= 6 else None
    return res


def loaded():
    r = runners()
    return [{"name": m["name"], "gb": round(m["size"] / 1e9, 2), "vram_gb": round(m.get("size_vram", 0) / 1e9, 2),
             "ctx": m.get("context_length"), "runner": r.get(m["name"])} for m in ollama.ps()]


def snap(**extra):
    return {"t": time.time(), "free_pct": free_pct(), "pressure": pressure_level(), "swap_gb": swap_used_gb(), "loaded": loaded(), **extra}


def unload_all():
    for m in ollama.ps():
        ollama.unload(m["name"])
    for _ in range(20):
        if not ollama.ps():
            return
        time.sleep(0.25)


def gen(model, ctx=8192, prompt="hi", n=1, keep="10m"):
    """One tiny /api/generate: returns (response, wall_ms). load_duration (ns) in the response says how long loading took."""
    return ollama._post("/api/generate", {"model": model, "prompt": prompt, "stream": False, "keep_alive": keep,
                                          "options": {"num_predict": n, "num_ctx": ctx, "temperature": 0}, "think": False}, timeout=600)


def prompts():
    items = [json.loads(l) for l in (ROOT / "data" / "router-200.jsonl").read_text().splitlines() if l.strip()]
    return [i["prompt"] for i in items]


def out_file(name, a):
    return (RES / f"s4-{name}.jsonl").open("a" if getattr(a, "append", False) else "w")


def emit(f, rec):
    f.write(json.dumps(rec) + "\n")
    f.flush()


# ---------------- load ----------------
def cmd_load(a):
    f = out_file("load", a)
    models = a.models.split(",")
    unload_all()
    for m in models:  # warm: the files were just read, so they're in the page cache
        gen(m)
        for i in range(a.n):
            ollama.unload(m)
            time.sleep(0.5)
            r, wall = gen(m)
            emit(f, {"kind": "warm", "model": m, "i": i, "load_ms": r.get("load_duration", 0) / 1e6, "wall_ms": wall, **snap()})
        print("warm", m, "done", flush=True)
        ollama.unload(m)
    for i in range(a.n):  # after the others: their weights have pushed this model's files out of the page cache (as far as we can tell)
        for m in models:
            unload_all()
            r, wall = gen(m)
            emit(f, {"kind": "after others", "model": m, "i": i, "load_ms": r.get("load_duration", 0) / 1e6, "wall_ms": wall, **snap()})
        print("after others round", i, "done", flush=True)
    unload_all()


# ---------------- ctx ----------------
def cmd_ctx(a):
    f = out_file("ctx", a)
    for m in a.models.split(","):
        for ctx in (8192, 16384):
            unload_all()
            time.sleep(1)
            r, wall = gen(m, ctx=ctx, prompt="Write one sentence about the sea.", n=16)
            time.sleep(1)
            emit(f, {"label": a.label, "model": m, "ctx": ctx, "load_ms": r.get("load_duration", 0) / 1e6, **snap()})
            print(a.label, m, ctx, loaded(), flush=True)
    unload_all()


# ---------------- reload ----------------
def cmd_reload(a):
    f = out_file("reload", a)
    for m in a.models.split(","):
        unload_all()
        for step, ctx in (("first load at 4096", 4096), ("same 4096", 4096), ("change to 8192", 8192), ("same 8192", 8192), ("back to 4096", 4096)):
            r, wall = gen(m, ctx=ctx)
            emit(f, {"model": m, "step": step, "ctx_asked": ctx, "load_ms": r.get("load_duration", 0) / 1e6, "wall_ms": wall, "loaded": loaded()})
            print(m, step, round(r.get("load_duration", 0) / 1e6), "ms load", flush=True)
    unload_all()


# ---------------- cache ----------------
def chat_short(m, text, ctx=8192, n=8):
    think = False if "thinking" in ollama.show(m).get("capabilities", []) else None
    return ollama.chat(m, [{"role": "user", "content": text}], options={"num_predict": n, "num_ctx": ctx, "temperature": 0}, think=think)


def guard(a, f, label):
    fp, lvl = free_pct(), pressure_level()
    if (fp is not None and fp < a.stop_below) or lvl == 4:
        emit(f, {"stopped": f"free memory {fp}% (limit {a.stop_below}%), pressure level {lvl}", "part": label})
        print("STOPPED:", label, fp, lvl, flush=True)
        return True
    return False


def cmd_cache(a):
    f = out_file("cache", a)
    ps_ = prompts()[: a.n]
    for m in a.models.split(","):
        unload_all()
        chat_short(m, "hello there")
        emit(f, {"model": m, "i": 0, **snap()})
        ms = []
        for i, p in enumerate(ps_, 1):
            _, wall = chat_short(m, p)
            ms.append(wall)
            if i % 10 == 0:
                emit(f, {"model": m, "i": i, "median_ms_last10": st.median(ms[-10:]), **snap()})
                if guard(a, f, m):
                    break
        print("cache", m, "done", flush=True)
        unload_all()


# ---------------- resident + primary ----------------
def cmd_resident(a):
    f = out_file("resident", a)
    m = a.primary
    mode = "no ceiling" if a.no_ceiling else "ceiling"
    unload_all()
    ollama.systemone(RESIDENT, {"request": "hello there"}, DECISION)
    chat_short(m, "hello there")
    emit(f, {"primary": m, "mode": mode, "i": 0, **snap()})
    reloads, dec_ms, pri_ms = 0, [], []
    for i, p in enumerate(prompts()[: a.n], 1):
        r, ms = ollama.systemone(RESIDENT, {"request": p}, DECISION)
        dec_ms.append(ms)
        _, wall = chat_short(m, p, n=a.tokens)
        pri_ms.append(wall)
        if not a.no_ceiling:  # ADR-0006: reload the decision model after the reply when its cache has grown
            for x in ollama.ps():
                if x["name"] == RESIDENT and x["size"] / 1e9 > RESIDENT_WEIGHTS_GB + 1.0:
                    ollama.unload(RESIDENT)
                    reloads += 1
        if i % 10 == 0:
            emit(f, {"primary": m, "mode": mode, "i": i, "decision_median_ms": st.median(dec_ms[-10:]), "primary_median_ms": st.median(pri_ms[-10:]),
                     "reloads": reloads, **snap()})
            if guard(a, f, f"{m} {mode}"):
                break
    print("resident", m, mode, "done; reloads", reloads, flush=True)
    unload_all()


# ---------------- swap ----------------
def cmd_swap(a):
    f = out_file("swap", a)
    models = a.models.split(",")
    unload_all()
    ollama.systemone(RESIDENT, {"request": "hello there"}, DECISION)
    cur = None
    for rnd in range(a.n):
        for m in models:
            t0 = time.perf_counter()
            if cur and cur != m:
                ollama.unload(cur)
                for _ in range(40):
                    if not any(x["name"] == cur for x in ollama.ps()):
                        break
                    time.sleep(0.05)
            un_ms = (time.perf_counter() - t0) * 1000
            r, wall = gen(m)
            emit(f, {"from": cur, "to": m, "round": rnd, "unload_ms": un_ms, "load_ms": r.get("load_duration", 0) / 1e6, "first_token_wall_ms": wall,
                     "total_ms": un_ms + wall, **snap()})
            cur = m
        print("swap round", rnd, "done", flush=True)
    unload_all()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("part", choices=["load", "ctx", "reload", "cache", "resident", "swap"])
    ap.add_argument("--models", default=",".join(PRIMARIES))
    ap.add_argument("--primary", default="qwen3:8b")
    ap.add_argument("--label", default="f16", help="KV cache setting of the server under test (f16 or q8_0)")
    ap.add_argument("--n", type=int, default=None)
    ap.add_argument("--tokens", type=int, default=24, help="primary reply length in the resident test")
    ap.add_argument("--no-ceiling", action="store_true")
    ap.add_argument("--stop-below", type=int, default=8, help="stop when free memory %% drops below this, or the OS reports critical pressure")
    ap.add_argument("--append", action="store_true")
    a = ap.parse_args()
    if a.n is None:
        a.n = {"load": 5, "cache": 200, "resident": 200, "swap": 3}.get(a.part, 1)
    {"load": cmd_load, "ctx": cmd_ctx, "reload": cmd_reload, "cache": cmd_cache, "resident": cmd_resident, "swap": cmd_swap}[a.part](a)


if __name__ == "__main__":
    main()
