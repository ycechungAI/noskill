"""Sample Ollama model memory and macOS free memory every 10 s while a spike script runs.

  python3 spikes/s3/memwatch.py s3/lean.py results/s3-lean.jsonl [--stop-below 12]
Writes results/s3-memwatch.jsonl. If free memory drops below --stop-below %, it stops the
watched script (SIGTERM) so the machine doesn't swap hard. macOS only (memory_pressure).
"""
import argparse
import json
import os
import re
import signal
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from common import ollama  # noqa: E402


def pids(script):
    out = subprocess.run(["pgrep", "-f", script], capture_output=True, text=True).stdout
    found = [int(p) for p in out.split() if int(p) != os.getpid()]

    def is_python(p):  # shells whose command line mentions the script would match too
        comm = subprocess.run(["ps", "-o", "comm=", "-p", str(p)], capture_output=True, text=True).stdout
        return "ython" in comm

    return [p for p in found if is_python(p)]


def free_pct():
    mp = subprocess.run(["memory_pressure", "-Q"], capture_output=True, text=True, timeout=10).stdout
    m = re.search(r"free percentage:\s*(\d+)%", mp)
    return int(m.group(1)) if m else None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("script")
    ap.add_argument("rows")
    ap.add_argument("--stop-below", type=int, default=12)
    a = ap.parse_args()
    rows_path = ROOT / a.rows
    for _ in range(30):  # the watched script may start a moment after this one
        if pids(a.script):
            break
        time.sleep(1)
    with (ROOT / "results" / "s3-memwatch.jsonl").open("a") as out:
        while pids(a.script):
            lines = rows_path.read_text().splitlines() if rows_path.exists() else []
            last = json.loads(lines[-1]) if lines else {}
            rec = {"t": time.time(), "free_pct": free_pct(), "rows": len(lines),
                   "last_ms": (last.get("pred") or {}).get("ms"), "set": last.get("set"), "load": last.get("load"),
                   "loaded": [{"name": m["name"], "gb": round(m["size"] / 1e9, 2)} for m in ollama.ps()]}
            if rec["free_pct"] is not None and rec["free_pct"] < a.stop_below:
                for p in pids(a.script):
                    os.kill(p, signal.SIGTERM)
                rec["stopped"] = f"free memory {rec['free_pct']}% < {a.stop_below}%"
            out.write(json.dumps(rec) + "\n")
            out.flush()
            if "stopped" in rec:
                print(rec["stopped"], flush=True)
                break
            time.sleep(10)


if __name__ == "__main__":
    main()
