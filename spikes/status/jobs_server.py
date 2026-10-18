"""Live progress page for build and check jobs (not the Phase 0 spikes).

  python3 spikes/status/jobs_server.py        # http://127.0.0.1:8766

A job is a JSON file in spikes/results/jobs/<name>.json:
  {"name": "...", "state": "running|done|failed", "done": 3, "total": 12, "now": "what just finished",
   "notes": ["..."], "steps": [{"name": "...", "state": "done|running|waiting|failed"}], "started": <unix seconds>}
It reuses spikes/status/index.html. Nothing here changes any file.
"""
import json
import sys
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
JOBS = HERE.parent / "results" / "jobs"
PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8766


def fmt(sec):
    sec = int(sec)
    return f"{sec // 60}m {sec % 60:02d}s"


# When each job's done count last changed, seen by this server: (time, done), newest last.
_seen = {}


def pace_eta(name, done, total, running):
    """Time left from the pace of the last few finished steps, not from the whole run.

    Average-since-start rises while a slow step is running (nothing finishes, elapsed grows). This
    uses only the recent steps, says nothing until it has three, and says "waiting" instead of
    growing when the current step has taken much longer than the recent ones.
    """
    now = time.time()
    seen = _seen.setdefault(name, [])
    if not seen or seen[-1][1] != done:
        seen.append((now, done))
        del seen[:-9]
    if not running or len(seen) < 3:
        return None, False
    (t0, d0), (t1, d1) = seen[0], seen[-1]
    if d1 <= d0:
        return None, False
    pace = (t1 - t0) / (d1 - d0)
    if now - t1 > max(3 * pace, 120):
        return None, True
    return fmt(pace * max(total - done, 0)), False


def jobs():
    out = []
    for p in sorted(JOBS.glob("*.json"), key=lambda p: p.stat().st_mtime, reverse=True):
        try:
            j = json.loads(p.read_text())
        except (OSError, json.JSONDecodeError):
            continue  # being written
        steps = j.get("steps", [])
        done = j.get("done", sum(1 for s in steps if s.get("state") == "done"))
        total = j.get("total", len(steps))
        started = j.get("started")
        running = j.get("state") == "running"
        elapsed = fmt(time.time() - started) if started and running else None
        eta, waiting = pace_eta(p.stem, done, total, running)
        if waiting:
            j["now"] = (j.get("now") or "") + " (this step is taking much longer than the last ones; waiting on the model)"
        out.append({"name": j.get("name", p.stem), "state": j.get("state", "running"), "done": done, "total": total,
                    "eta": eta, "elapsed": elapsed, "now": j.get("now"), "last_line": j.get("last_line", ""),
                    "notes": j.get("notes", []), "tables": [], "log": [],
                    "groups": [{"label": s["name"][:28], "done": 1 if s.get("state") == "done" else 0, "total": 1, "success": None}
                               for s in steps]})
    return out


def loaded():
    try:
        with urllib.request.urlopen("http://127.0.0.1:11434/api/ps", timeout=1.5) as r:
            return [{"name": m["name"], "gb": round(m["size"] / 1e9, 1), "ctx": m.get("context_length")} for m in json.load(r)["models"]]
    except Exception:
        return None


def status():
    exps = jobs()
    run = [e for e in exps if e["state"] == "running"]
    return {"time": time.strftime("%H:%M:%S"), "experiments": exps,
            "overall": {"done": sum(e["done"] for e in run), "total": sum(e["total"] for e in run)},
            "machine": {"free_pct": None, "loaded": loaded()}}


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/status.json"):
            body, ctype = json.dumps(status()).encode(), "application/json"
        elif self.path in ("/", "/index.html"):
            body, ctype = (HERE / "index.html").read_bytes(), "text/html; charset=utf-8"
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    print(f"Job status on http://127.0.0.1:{PORT}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
