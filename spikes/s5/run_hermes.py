"""S5: run tools-50 tasks through Hermes Agent (real tools, a real local model), sandboxed. macOS only.

  python3 spikes/s5/run_hermes.py [--tasks t01,t05,...] [--timeout 420] [--out spikes/results/s5-hermes.jsonl]
Needs: Hermes installed by install_hermes.sh, and an Ollama on 127.0.0.1:11500 started with OLLAMA_CONTEXT_LENGTH=65536
(Hermes refuses models under 64k tokens of context). Each task gets a fresh fixture folder (the sandbox's HOME),
runs `hermes chat --oneshot -Q -q <prompt>` inside the sandbox, and is scored on the end state as in S1.
"""
import argparse
import datetime as dt
import json
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path

S5 = Path(__file__).resolve().parent
ROOT = S5.parent
HOME = S5 / "out" / "home"
TASKS = [json.loads(l) for l in (ROOT / "data" / "tools-50.jsonl").read_text().splitlines() if l.strip()]
WORLD = json.loads((ROOT / "data" / "tools-world.json").read_text())
DEFAULT = "t01,t05,t06,t09,t12,t14,t23,t25,t38,t39"
FIXTURE_DIRS = ["Downloads", "Documents", "Pictures", "Desktop", "Projects"]
sys.path.insert(0, str(ROOT))
from common import ollama  # noqa: E402,F401  (talks to OLLAMA_HOST, set to the 11500 server by the caller)


def reset_fixture():
    for d in FIXTURE_DIRS:
        shutil.rmtree(HOME / d, ignore_errors=True)
    for f in WORLD["files"]:
        if f.startswith("~/"):
            p = HOME / f[2:]
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text("fixture\n")


def mem():
    out = subprocess.run(["memory_pressure", "-Q"], capture_output=True, text=True).stdout
    m = re.search(r"free percentage:\s*(\d+)%", out)
    lvl = subprocess.run(["sysctl", "-n", "kern.memorystatus_vm_pressure_level"], capture_output=True, text=True).stdout.strip()
    return {"free_pct": int(m.group(1)) if m else None, "pressure": int(lvl) if lvl.isdigit() else None}


def time_pattern():
    now = dt.datetime.now()
    pats = []
    for d in (-1, 0, 1):
        t = now + dt.timedelta(minutes=d)
        pats.append(rf"\b0?{t.hour}:{t.minute:02d}\b")
        h12 = t.hour % 12 or 12
        pats.append(rf"\b0?{h12}:{t.minute:02d}\b")
    return pats


def score(task, final):
    ch = dict(task["checks"])
    if task["id"] == "t01":
        ch = {"answer_any": time_pattern()}
    ok, why = True, []
    for pat in ch.get("answer_any", []):
        pass
    if "answer_any" in ch:
        hit = any(re.search(p, final, re.I) for p in ch["answer_any"])
        ok &= hit
        if not hit:
            why.append("answer missing")
    if "answer_all" in ch:
        hit = all(re.search(p, final, re.I) for p in ch["answer_all"])
        ok &= hit
        if not hit:
            why.append("answer incomplete")
    for key, want in (("fs_exists", True), ("fs_missing", False)):
        for f in ch.get(key, []):
            if (HOME / f[2:]).exists() != want:
                ok = False
                why.append(f"{key}: {f}")
    return ok, why


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tasks", default=DEFAULT)
    ap.add_argument("--timeout", type=int, default=420)
    ap.add_argument("--out", default=str(ROOT / "results" / "s5-hermes.jsonl"))
    ap.add_argument("--stop-below", type=int, default=8)
    a = ap.parse_args()
    by = {t["id"]: t for t in TASKS}
    with open(a.out, "a") as f:
        for tid in a.tasks.split(","):
            m = mem()
            if (m["free_pct"] is not None and m["free_pct"] < a.stop_below) or m["pressure"] == 4:
                print("STOPPED: memory", m, flush=True)
                break
            t = by[tid]
            reset_fixture()
            t0 = time.time()
            try:
                p = subprocess.run([str(S5 / "hx.sh"), "hermes", "chat", "--oneshot", "-Q", "-q", t["prompt"]], capture_output=True, text=True, timeout=a.timeout, cwd=str(HOME))
                final, err, rc = p.stdout.strip(), p.stderr.strip()[-600:], p.returncode
            except subprocess.TimeoutExpired as e:
                final, err, rc = (e.stdout or b"").decode(errors="replace") if isinstance(e.stdout, bytes) else (e.stdout or ""), "timeout", -1
            secs = time.time() - t0
            ok, why = score(t, final)
            rec = {"task": tid, "kind": t["kind"], "prompt": t["prompt"], "rc": rc, "seconds": round(secs, 1), "success": ok, "why": why,
                   "final": final[:800], "err": err, "mem_after": mem(), "loaded": [{"name": x["name"], "gb": round(x["size"] / 1e9, 2), "ctx": x.get("context_length")} for x in ollama.ps()]}
            f.write(json.dumps(rec) + "\n")
            f.flush()
            print(f"{tid} {'OK ' if ok else 'bad'} {secs:6.1f}s rc={rc} {why} | {final[:90]!r}", flush=True)


if __name__ == "__main__":
    main()
