"""Live progress page for the Phase 0 spikes.

  python3 spikes/status/server.py            # http://127.0.0.1:8765

Reads results/*.jsonl and the running spike processes; the page polls /status.json every 2 s.
Nothing here changes any file.
"""
import json
import re
import shlex
import subprocess
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from common import ollama  # noqa: E402

DATA, RES = ROOT / "data", ROOT / "results"
PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8765
S3_JEV, S3_EMU = ["tev1:0.8b", "tev1"], ["qwen3.5:4b", "llama3.2:3b"]


def rows(name):
    p = RES / name
    if not p.exists():
        return []
    out = []
    for l in p.read_text().splitlines():
        try:
            out.append(json.loads(l))
        except json.JSONDecodeError:
            pass  # line being written
    return out


def count_lines(p):
    return sum(1 for l in p.read_text().splitlines() if l.strip()) if p.exists() else 0


def procs():
    """Running spike scripts: {script: (argv, elapsed)}."""
    try:
        out = subprocess.run(["ps", "-ax", "-o", "etime=,command="], capture_output=True, text=True, timeout=5).stdout
    except (OSError, subprocess.TimeoutExpired):
        return {}
    found = {}
    for line in out.splitlines():
        m = re.match(r"\s*(\S+)\s+(.*)", line)
        if not m or "python" not in m.group(2).lower():
            continue
        try:
            argv = shlex.split(m.group(2))
        except ValueError:
            continue
        for i, a in enumerate(argv):
            if a.endswith(("s1/run.py", "s3/run.py", "s3/latency_probe.py", "s3/lean.py", "s4/run.py", "s5/run_hermes.py", "s9/run.py", "data/review.py", "data/adjudicate.py")):
                found[a.split("spikes/")[-1]] = (argv[i + 1:], m.group(1))
    return found


def arg(argv, name, default):
    return argv[argv.index(name) + 1] if name in argv and argv.index(name) + 1 < len(argv) else default


def fmt_eta(sec):
    if sec is None:
        return None
    sec = int(sec)
    return f"{sec // 3600}h {sec % 3600 // 60:02d}m" if sec >= 3600 else f"{sec // 60}m {sec % 60:02d}s"


def s1_status(running):
    argv = running[0] if running else []
    models = arg(argv, "--models", "llama3.2:3b,qwen3.5:4b").split(",")
    strategies = arg(argv, "--strategies", "native,constrained,prompted").split(",")
    seeds = arg(argv, "--seeds", "1,2,3").split(",")
    tasks = [t for t in arg(argv, "--tasks", "").split(",") if t] or [json.loads(l)["id"] for l in (DATA / "tools-50.jsonl").read_text().splitlines() if l.strip()]
    per = len(seeds) * len(tasks)
    allr = rows("s1-raw.jsonl")
    # timed-out runs are finished (scored as failures by report.py); other errors are retried on resume
    rs = [r for r in allr if not r.get("error") or "timed out" in r["error"]]
    errors = len({(r["model"], r["strategy"], r["seed"], r["task"]) for r in allr if r.get("error") and "timed out" not in r["error"]})
    seen = {(r["model"], r["strategy"], r["seed"], r["task"]) for r in rs}
    groups, eta = [], 0.0
    all_ms = [r["ms"] for r in rs]
    for m in models:
        mms = [r["ms"] for r in rs if r["model"] == m]
        avg = (sum(mms) / len(mms)) if mms else (sum(all_ms) / len(all_ms) if all_ms else None)
        for s in strategies:
            g = [r for r in rs if r["model"] == m and r["strategy"] == s]
            done = len({(r["seed"], r["task"]) for r in g})
            ok = {(r["seed"], r["task"]): bool(r["success"]) and not r.get("error") for r in g}
            groups.append({"label": f"{m} · {s}", "done": done, "total": per,
                           "success": round(100 * sum(ok.values()) / len(ok)) if ok else None})
            if avg is not None:
                eta += (per - done) * avg / 1000
    total = len(models) * len(strategies) * per
    last = rs[-1] if rs else None
    log = RES / "s1-console.log"
    tail = log.read_text().splitlines()[-1] if log.exists() and log.read_text().strip() else ""
    done = min(len(seen), total)
    return {"name": "S1 · tool calling", "done": done, "total": total, "errors": errors,
            "state": "running" if running else ("done" if done >= total else ("stopped" if done else "waiting")),
            "elapsed": running[1] if running else None,
            "eta": fmt_eta(eta) if running and done < total else None,
            "now": f"{last['model']} · {last['strategy']} · seed {last['seed']} · {last['task']}" if last and running else None,
            "last_line": "" if running else tail, "groups": groups}  # console is block-buffered while running; tail shows crashes


S3_ABOUT = ("Can a small decision model (tev1, 4.2B, or tev1:0.8b) be the always-loaded router next to the chat model? "
            "It answers through Ollama's /v1/systemone: one call returns a choice with probabilities, a yes/no probability "
            "(noul) or a score, without generating text. Pass marks: task-type accuracy ≥ 85% and median ≤ 150 ms while "
            "another model is generating.")
S3_PARTS = {
    "noul": "Part 1 of 3 · noul check. Each decision model answers 40 yes/no questions about short texts (20 should be "
            "yes, 20 no). If noul means P(yes), it should be ≥ 0.9 on yes items and ≤ 0.1 on no items.",
    "router": "Part 2 of 3 · routing accuracy. Each of the 200 labelled requests is classified: task type (11 kinds), "
              "difficulty 1–5, and whether it needs the web, an image or tools. First by the decision models, one "
              "/v1/systemone call per request asking all five questions (about 1,000 prompt tokens), then by qwen3.5:4b and llama3.2:3b doing the same job the ordinary way (JSON answer + "
              "token probabilities), to see whether a separate decision model is worth it. Nothing else is running.",
    "contention": "Part 3 of 3 · under load. The same 200 requests on the decision models while qwen3.5:4b keeps writing "
                  "600-token stories in the background, like a chat in progress. This is the realistic case: the "
                  "150 ms target applies here, and memory pressure is recorded.",
}
NOULS = ["needs_web", "needs_vision", "needs_tools"]


def s3_gold():
    """id → labels to score against: settled (final), else a and b where they agree, else a. Same as s3/report.py."""
    out = {}
    try:
        items = [json.loads(l) for l in (DATA / "router-200.jsonl").read_text().splitlines() if l.strip()]
    except (OSError, json.JSONDecodeError):
        return out
    for i in items:
        L = i["labels"]
        if "final" in L:
            out[i["id"]] = L["final"]
        elif "b" in L:
            a, b = L["a"], L["b"]
            out[i["id"]] = {"task_type": a["task_type"] if a["task_type"] == b["task_type"] else None,
                            **{k: a[k] for k in NOULS if a[k] == b[k]}}
        else:
            out[i["id"]] = L["a"]
    return out


def p50(xs):
    xs = sorted(xs)
    return xs[len(xs) // 2] if xs else None


def s3_numbers(part, rs, models, gold):
    """Interim results per model for the table under the S3 card."""
    out = []
    for m in models:
        g = [r for r in rs if r["model"] == m]
        if not g:
            continue
        if part == "noul":
            yes = [r["noul"] for r in g if r["expected"]]
            no = [r["noul"] for r in g if not r["expected"]]
            right = sum((r["noul"] > 0.5) == r["expected"] for r in g)
            out.append([m, len(g), f"yes items avg {sum(yes) / len(yes):.2f}" if yes else "–",
                        f"no items avg {sum(no) / len(no):.2f}" if no else "–",
                        f"{round(100 * right / len(g))}% right", f"{p50([r['ms'] for r in g]):.0f} ms"])
            continue
        ok = [r for r in g if not r.get("error") and r.get("pred")]
        tt = [(r["pred"]["task_type"] == gold[r["id"]]["task_type"]) for r in ok
              if gold.get(r["id"], {}).get("task_type")]
        tl = [((r["pred"].get("needs_tools") or 0) > 0.5) == gold[r["id"]]["needs_tools"] for r in ok
              if "needs_tools" in gold.get(r["id"], {})]
        ms = p50([r["pred"]["ms"] for r in ok])
        err = len(g) - len(ok)
        out.append([m, len(g), f"type {round(100 * sum(tt) / len(tt))}% right" if tt else "–",
                    f"tools {round(100 * sum(tl) / len(tl))}% right" if tl else "–",
                    f"median {ms:.0f} ms" if ms is not None else "–", f"{err} errors" if err else ""])
    return out


def s3_status(running):
    argv = running[0] if running else []
    part_running = argv[0] if argv else None
    jev = arg(argv, "--jev", ",".join(S3_JEV)).split(",")
    emu = arg(argv, "--emu", ",".join(S3_EMU)).split(",")
    parts = [("noul", "s3-noul.jsonl", jev, 40), ("router", "s3-router.jsonl", jev + emu, 200), ("contention", "s3-contention.jsonl", jev, 200)]
    groups, done, total, tables, current = [], 0, 0, [], None
    gold = s3_gold()
    for part, f, models, n in parts:
        rs = rows(f)
        for m in models:
            d = min(n, sum(1 for r in rs if r["model"] == m))
            groups.append({"label": f"{part} · {m}", "done": d, "total": n, "success": None})
            done += d
            total += n
        if rs:
            tables.append({"title": {"noul": "noul check", "router": "Routing, nothing else running",
                                     "contention": "Routing while qwen3.5:4b generates"}[part],
                           "rows": s3_numbers(part, rs, models, gold)})
        if part == part_running and rs:
            m = rs[-1]["model"]
            current = (m, rs[-1]["id"], max(0, n - sum(1 for r in rs if r["model"] == m)),
                       models[models.index(m) + 1:] if m in models else [])
    notes = [S3_ABOUT]
    if part_running in S3_PARTS:
        notes.append(S3_PARTS[part_running])
    if current:
        m, rid, left, after = current
        nxt = f"; then {', '.join(after)}" if after else ("; then S3 is finished" if part_running == "contention" else "")
        notes.append(f"Right now: {m}, request {rid} ({left} left for this model{nxt}).")
    mem = rows("s3-memory.jsonl")
    if mem:
        x = mem[-1]
        notes.append(f"Last memory check ({x['tag'].replace(':loaded', '')}): {x['free_pct']}% free; loaded "
                     + ", ".join(f"{l['name']} {l['gb']} GB" for l in x["loaded"]) + ".")
    if tables:
        notes.append("The numbers below are interim. Accuracy is against your labels (the settled ones where you've "
                     "settled them), so it moves as you settle disagreements. Tools = the needs-tools probability is "
                     "above 0.5 and the label says tools are needed, or both say not. Median = time for one request: all five "
                     "questions in one call for the decision models, one JSON answer for the chat models.")
    log = RES / "s3-console.log"
    tail = [l for l in log.read_text().splitlines() if l.strip()][-4:] if log.exists() else []
    return {"name": "S3 · decision models", "done": done, "total": total, "errors": 0,
            "state": f"running ({part_running})" if running else ("done" if done >= total else ("stopped" if done else "waiting for S1")),
            "elapsed": running[1] if running else None, "eta": None, "now": None, "last_line": "",
            "notes": notes, "tables": tables, "log": tail, "groups": groups}


def probe_status(running):
    rs = rows("s3-latency.jsonl")
    if not rs and not running:
        return None
    argv = running[0] if running else []
    models = arg(argv, "--models", "tev1:0.8b,tev1").split(",")
    n = int(arg(argv, "--n", "15"))
    variants = list(dict.fromkeys(r["variant"] for r in rs)) or ["all five (as in S3)"]
    nv = 6
    groups = []
    for m in models:
        g = [r for r in rs if r["model"] == m]
        groups.append({"label": m, "done": len(g), "total": nv * n, "success": None})
    table = []
    for m in models:
        for v in variants:
            g = [r for r in rs if r["model"] == m and r["variant"] == v]
            if g:
                table.append([m, len(g), v, f"{p50([r['in_tok'] or 0 for r in g]):.0f} tokens",
                              f"new request {p50([r['ms'] for r in g]):.0f} ms", f"same again {p50([r['again_ms'] for r in g]):.0f} ms"])
    log = RES / "s3-latency-console.log"
    return {"name": "S3 follow-up · why are decisions slow?", "done": len(rs), "total": len(models) * nv * n, "errors": 0,
            "state": "running" if running else "done",
            "elapsed": running[1] if running else None, "eta": None, "now": None, "last_line": "",
            "notes": ["Each request is timed with different question sets: all five as in S3, each question alone, and "
                      "task type with names only (no descriptions). Then the identical call is sent again right away: "
                      "if that is fast, reading the prompt is the cost and Ollama can reuse it when it repeats."],
            "tables": [{"title": "Median so far", "rows": table}] if table else [],
            "log": [], "groups": groups}


LAYA_SETS = ["type, names only", "type, short descriptions", "all five, short descriptions"]


def lean_status(running):
    rs = rows("s3-lean.jsonl")
    if not rs and not running:
        return None
    gold = s3_gold()
    planned = [("laya", s) for s in LAYA_SETS] + [("tev1:0.8b", s) for s in ["type, short descriptions", "all five, short descriptions"]]
    combos = list(dict.fromkeys([(r["model"], r["set"]) for r in rs] + planned))
    plan = [("idle", 200), ("under load", 50)]
    groups, table = [], []
    for model, name in combos:
        for load_tag, n in plan:
            g = [r for r in rs if r["model"] == model and r["load"] == load_tag and r["set"] == name]
            groups.append({"label": f"{model} · {load_tag} · {name}", "done": len(g), "total": n, "success": None})
            ok = [r for r in g if not r.get("error") and r.get("pred")]
            if ok:
                tt = [r["pred"]["task_type"] == gold[r["id"]]["task_type"] for r in ok if gold.get(r["id"], {}).get("task_type")]
                table.append([f"{model} · {load_tag} · {name}", len(g), f"type {round(100 * sum(tt) / len(tt))}% right" if tt else "–",
                              f"median {p50([r['pred']['ms'] for r in ok]):.0f} ms", f"{p50([r['pred']['in_tok'] or 0 for r in ok]):.0f} tokens"])
    cur = rs[-1] if rs else None
    return {"name": "S3 follow-up · fewer questions, and laya", "done": len(rs), "total": sum(n for _, n in plan) * len(combos),
            "errors": sum(1 for r in rs if r.get("error")),
            "state": "running" if running else "done", "elapsed": running[1] if running else None, "eta": None, "now": None,
            "last_line": "",
            "notes": ["Each question is read as its own copy of request + question, so fewer and shorter questions should be faster. "
                      "Each question set runs on all 200 requests with the decision model alone in memory (unloaded between sets), "
                      "then on 50 requests while qwen3.5:4b writes stories.",
                      "Now testing laya (421M, ModernBERT, no text generation) against tev1:0.8b. laya allows at most 176 tokens of options, "
                      "so the task-type options use short descriptions; tev1:0.8b gets the same short version for a fair comparison.",
                      *([f"Right now: {cur['model']} · {cur['load']} · {cur['set']}, request {cur['id']}."] if cur and running else [])],
            "tables": [{"title": "So far", "rows": table}] if table else [], "log": [], "groups": groups}


def labels_status(running):
    items = [json.loads(l) for l in (DATA / "router-200.jsonl").read_text().splitlines() if l.strip()]
    b = sum(1 for i in items if "b" in i["labels"])
    return {"name": "Second labeller (you)", "done": b, "total": len(items), "errors": 0,
            "state": "running" if running else ("done" if b >= len(items) else ("not started" if not b else "paused")),
            "elapsed": running[1] if running else None, "eta": None, "now": None,
            "last_line": "", "groups": []}


def settle_status(running):
    pc = DATA / "router-200.labels-c.jsonl"
    if not pc.exists():
        return None
    ai = {json.loads(l)["id"]: json.loads(l)["labels"] for l in pc.read_text().splitlines() if l.strip()}
    try:
        items = [json.loads(l) for l in (DATA / "router-200.jsonl").read_text().splitlines() if l.strip()]
    except json.JSONDecodeError:
        return None
    fields = ["task_type", "needs_web", "needs_vision", "needs_tools"]
    todo = [i for i in items if "b" in i["labels"] and i["id"] in ai and
            (any(i["labels"]["b"][k] != ai[i["id"]][k] for k in fields) or abs(i["labels"]["b"]["difficulty"] - ai[i["id"]]["difficulty"]) >= 2)]
    done = sum(1 for i in todo if "final" in i["labels"])
    return {"name": "Settle disagreements with the AI (you)", "done": done, "total": len(todo), "errors": 0,
            "state": "running" if running else ("done" if todo and done >= len(todo) else ("not started" if not done else "paused")),
            "elapsed": running[1] if running else None, "eta": None, "now": None,
            "last_line": "" if running or done else "python3 spikes/data/adjudicate.py", "groups": []}


_mem = {"t": 0, "v": None}


def machine():
    if time.time() - _mem["t"] > 10:
        try:
            mp = subprocess.run(["memory_pressure", "-Q"], capture_output=True, text=True, timeout=5).stdout
            m = re.search(r"free percentage:\s*(\d+)%", mp)
            _mem["v"] = int(m.group(1)) if m else None
        except (OSError, subprocess.TimeoutExpired):
            _mem["v"] = None
        _mem["t"] = time.time()
    try:
        loaded = [{"name": m["name"], "gb": round(m["size"] / 1e9, 2), "ctx": m.get("context_length")} for m in ollama.ps()]
    except Exception:  # noqa: BLE001
        loaded = None
    return {"free_pct": _mem["v"], "loaded": loaded}


_hist = {}  # experiment name → [(time, done)] seen by this server, for a rate-based estimate


def rate_eta(e):
    """Time left from the progress rate over the last 10 minutes (when the experiment gives none)."""
    h = _hist.setdefault(e["name"], [])
    now = time.time()
    if not h or h[-1][1] != e["done"]:
        h.append((now, e["done"]))
    while len(h) > 2 and now - h[0][0] > 600:
        h.pop(0)
    if len(h) < 2 or h[-1][1] <= h[0][1]:
        return None
    rate = (h[-1][1] - h[0][1]) / (h[-1][0] - h[0][0])
    return fmt_eta((e["total"] - e["done"]) / rate) if rate > 0 else None


S4_PARTS = [("load", "load times", 40), ("reload", "context change reloads", 20), ("ctx", "memory at 8k and 16k", 8),
            ("cache", "prompt-cache growth, model alone", 800), ("resident", "tev1:0.8b resident + one primary", 800), ("swap", "swaps", 12)]


def s4_status(running):
    """S4 memory and swap spike: one card, progress per part, a few headline numbers."""
    data = {k: rows(f"s4-{k}.jsonl") for k, _, _ in S4_PARTS}
    q8 = rows("s4-s1-q8.jsonl")
    if not any(data.values()) and not q8 and not running:
        return None
    groups, tables = [], []
    for k, label, total in S4_PARTS:
        rs = data[k]
        if k in ("cache", "resident"):  # rows carry i = requests done so far, per model (and mode)
            best = {}
            for r in rs:
                key = (r.get("model") or r.get("primary"), r.get("mode"))
                best[key] = max(best.get(key, 0), r.get("i") or 0)
            done = sum(best.values())
        else:
            done = len(rs)
        groups.append({"label": label, "done": done, "total": total, "success": None})
    groups.append({"label": "S1 subset with q8_0 KV cache (llama3.2:3b native)", "done": len(q8), "total": 150, "success": None})
    load = data["load"]
    if load:
        t = []
        for kind in ("warm", "after others"):
            for m in dict.fromkeys(r["model"] for r in load):
                xs = [r["load_ms"] for r in load if r["kind"] == kind and r["model"] == m]
                if xs:
                    t.append([f"{kind} · {m}", len(xs), f"median load {p50(xs):.0f} ms", f"max {max(xs):.0f} ms", ""])
        tables.append({"title": "Load times", "rows": t})
    cache = [r for r in data["cache"] if r.get("i")]
    if cache:
        t = []
        for m in dict.fromkeys(r["model"] for r in cache):
            xs = [r for r in cache if r["model"] == m]
            peak = max((l["gb"] for r in xs for l in r["loaded"] if l["name"] == m), default=0)
            t.append([m, f"{max(r['i'] for r in xs)} requests", f"size now/peak {xs[-1]['loaded'][0]['gb'] if xs[-1]['loaded'] else '–'}/{peak} GB",
                      f"lowest free {min(r['free_pct'] for r in xs if r['free_pct'] is not None)}%", ""])
        tables.append({"title": "Prompt-cache growth (model alone)", "rows": t})
    res = [r for r in data["resident"] if r.get("i")]
    if res:
        t = []
        for key in dict.fromkeys((r["primary"], r["mode"]) for r in res):
            xs = [r for r in res if (r["primary"], r["mode"]) == key]
            t.append([f"{key[0]} · {key[1]}", f"{max(r['i'] for r in xs)} requests", f"lowest free {min(r['free_pct'] for r in xs if r['free_pct'] is not None)}%",
                      f"reloads {xs[-1].get('reloads', 0)}", f"swap used {xs[-1].get('swap_gb')} GB"])
        tables.append({"title": "Resident + primary", "rows": t})
    cur = None  # the part whose results file changed last
    files = [(( RES / f"s4-{k}.jsonl").stat().st_mtime, k) for k, _, _ in S4_PARTS if data[k]]
    if files:
        k = max(files)[1]
        cur = (k, data[k][-1])
    done = sum(g["done"] for g in groups)
    total = sum(g["total"] for g in groups)
    return {"name": "S4 · memory slots and swap cost", "done": done, "total": total, "errors": sum(1 for k in data for r in data[k] if r.get("stopped")),
            "state": "running" if running else "done", "elapsed": running[1] if running else None, "eta": None, "now": None, "last_line": "",
            "notes": ["How long models take to load, how memory grows with context size and with Ollama's prompt cache, and whether tev1:0.8b plus one 7-8B model "
                      "stays out of memory pressure after 200 varied requests. A run stops itself if free memory drops below 8% or macOS reports critical memory pressure.",
                      *([f"Right now: {cur[0]}, {cur[1].get('model') or cur[1].get('primary') or cur[1].get('to') or ''}."] if cur and running else [])],
            "tables": tables, "log": [], "groups": groups}


def s5_status(running):
    """S5 Hermes teardown: the 10 tools-50 tasks run through Hermes, with a table of results."""
    rs = rows("s5-hermes.jsonl")
    if not rs and not running:
        return None
    ok = sum(1 for r in rs if r["success"])
    table = [[r["task"], "OK" if r["success"] else "failed", f"{r['seconds']:.0f} s", f"free {r['mem_after']['free_pct']}%",
              (r["final"] or r["err"] or "")[:70].replace("\n", " ")] for r in rs]
    cur = f"Right now: waiting for task {len(rs) + 1} of 10." if running and len(rs) < 10 else None
    return {"name": "S5 · Hermes Agent with qwen3.5:4b (64k context)", "done": len(rs), "total": 10, "errors": len(rs) - ok,
            "state": "running" if running else "done", "elapsed": running[1] if running else None, "eta": None, "now": None, "last_line": "",
            "notes": ["Ten tools-50 tasks run through Hermes Agent with real tools (files, shell, clock) in a sandbox, on a 4B local model. "
                      "Hermes needs at least 64,000 tokens of context, so this uses a separate Ollama with a 64k window. Each task is a fresh `hermes chat --oneshot` run.",
                      f"{ok} of {len(rs)} tasks succeeded so far.", *([cur] if cur else [])],
            "tables": [{"title": "Tasks", "rows": table}] if table else [], "log": [], "groups": [{"label": "tasks", "done": len(rs), "total": 10, "success": None}]}


def s9_status(running):
    """S9 sandbox feasibility (macOS part): attack matrix, legitimate work, cost."""
    rs = rows("s9-macos.jsonl")
    pg = json.loads((RES / "s9-progress.json").read_text()) if (RES / "s9-progress.json").exists() else {}
    if not rs and not running:
        return None
    parts = pg.get("parts", ["attacks", "legit", "cost", "copyin"])
    order = {"attacks": 0, "legit": 1, "cost": 2, "copyin": 3}
    part = pg.get("part", "")
    done_parts = sum(1 for x in parts if order.get(x, 9) < order.get(part, 9)) if part != "done" else len(parts)
    frac = (pg.get("part_done", 0) / pg["part_total"]) if pg.get("part_total") and part in order else 0
    tables = []
    atk = [r for r in rs if r["part"] == "attacks"]
    if atk:
        modes = ["none", "sandbox", "sandbox+fork"]
        by = {}
        for r in atk:
            by.setdefault(r["check"], {})[r["mode"]] = r
        cell = lambda r: "" if r is None else ("blocked" if r["blocked"] else "OPEN" if r["blocked"] is False else "n/a")
        tables.append({"title": "Attacks (none = no OS sandbox, proves the attack is real)", "rows":
                       [[c, *[cell(v.get(m)) for m in modes], (v.get("sandbox") or v.get("sandbox+fork") or {}).get("detail", "")[:70]] for c, v in by.items()]})
    lg = [r for r in rs if r["part"] == "legit"]
    if lg:
        tables.append({"title": "Legitimate work", "rows": [[r["task"], r["mode"], "OK" if r["ok"] else "failed", f"{r['wall_ms']:.0f} ms", r["detail"][:70]] for r in lg]})
    co = [r for r in rs if r["part"] == "cost"]
    if co:
        tables.append({"title": "Start-up cost (ms)", "rows": [[r["case"], r["mode"], r["p50_ms"], r["p90_ms"]] for r in co]})
    ci = [r for r in rs if r["part"] == "copyin"]
    if ci:
        tables.append({"title": "Copy-in of 100 MB (ms)", "rows": [[r["shape"], r["how"], r["p50_ms"], r["max_ms"]] for r in ci]})
    total = len(parts) * 100
    return {"name": "S9 · Sandbox feasibility (macOS)", "done": int((done_parts + frac) * 100), "total": total, "errors": 0,
            "state": "running" if running else "done", "elapsed": running[1] if running else None, "eta": None, "now": None, "last_line": "",
            "notes": ["Each attack runs in its own throwaway folder through the noskill launcher, once without the OS sandbox (to prove the attack works) and once inside it. "
                      "Secrets are only opened, never read.", *([f"Right now: {part} ({pg.get('part_done', 0)} of {pg.get('part_total', '?')})."] if running else [])],
            "tables": tables, "log": [], "groups": [{"label": x, "done": 100 if order[x] < order.get(part, 9) or part == "done" else int(frac * 100) if x == part else 0, "total": 100, "success": None} for x in parts]}


def s7_status():
    """S7 daemon, scheduler and notifications (macOS part): installed launchd agents, run drift, wake events, notification clicks."""
    rs = rows("s7-macos.jsonl")
    pg = json.loads((RES / "s7-progress.json").read_text()) if (RES / "s7-progress.json").exists() else {}
    if not rs and not pg:
        return None
    steps = pg.get("steps", [])
    done = sum(1 for x in steps if x.get("state") == "done")
    agents = []
    try:
        out = subprocess.run(["launchctl", "list"], capture_output=True, text=True, timeout=5).stdout
        agents = [l.split()[-1].replace("dev.noskill.s7.", "") for l in out.splitlines() if "dev.noskill.s7." in l]
    except (OSError, subprocess.TimeoutExpired):
        pass
    runs = [r for r in rs if r.get("event") == "run"]
    tables = []
    if steps:
        tables.append({"title": "Steps", "rows": [[x["name"], x.get("state", "todo"), x.get("note", "")] for x in steps]})
    by = {}
    for r in runs:
        by.setdefault((r.get("mode"), r.get("variant")), []).append(abs(r.get("drift_ms", 0)))
    if by:
        tables.append({"title": "Runs by scheduler (drift in s)", "rows": [[m, v, len(d), round(sorted(d)[len(d) // 2] / 1000, 2), round(max(d) / 1000, 2)] for (m, v), d in by.items()]})
    if runs:
        tables.append({"title": "Latest runs", "rows": [[r["ts"][11:19], r.get("mode"), r.get("variant"), r.get("kind"), r.get("scheduled"), round(r.get("drift_ms", 0) / 1000, 2), r.get("collapsed", r.get("slots_since_last_run", ""))] for r in runs[-10:]][::-1]})
    ev = [r for r in rs if r.get("event") in ("wake", "skipped", "start", "stop") or str(r.get("event", "")).startswith("notify_")]
    if ev:
        tables.append({"title": "Events (start, wake, skipped, notifications)", "rows": [[r["ts"][11:19], r["event"], r.get("variant", r.get("id", "")), json.dumps({k: v for k, v in r.items() if k not in ("ts", "event", "variant", "pid", "id")})[:90]] for r in ev[-12:]][::-1]})
    return {"name": "S7 · Daemon, scheduler and notifications (macOS)", "done": done, "total": max(len(steps), 1), "errors": 0,
            "state": "running" if steps and done < len(steps) else "done", "elapsed": None, "eta": None, "now": None, "last_line": "",
            "notes": ["Three throwaway launchd user agents run a 5-minute job and log to results/s7-macos.jsonl: `once` (catch up once after a missed run), `skip`, and `calendar` (launchd's own StartCalendarInterval).",
                      f"Loaded now: {', '.join(agents) or 'none'}.", *([pg["now"]] if pg.get("now") else [])],
            "tables": tables, "log": [], "groups": [{"label": x["name"][:24], "done": 1 if x.get("state") == "done" else 0, "total": 1, "success": None} for x in steps]}


def job_cards():
    """noskill's own long jobs (eval simulate, probes) write results/jobs/<name>.json as they go."""
    out = []
    for f in sorted((RES / "jobs").glob("*.json"), key=lambda f: f.stat().st_mtime, reverse=True):
        try:
            j = json.loads(f.read_text())
        except (OSError, json.JSONDecodeError):
            continue
        state = j.get("state", "?")
        if state == "running" and time.time() - f.stat().st_mtime > 900:
            state = "stale (no update for 15 min)"
        started = j.get("started")
        out.append({"name": j.get("name", f.stem), "done": j.get("done", 0), "total": max(j.get("total", 0), 1), "errors": 0,
                    "state": state, "elapsed": round(time.time() - started) if started and state == "running" else None,
                    "eta": None, "now": j.get("now"), "last_line": "", "notes": j.get("notes", [])[-12:],
                    "tables": [], "log": [], "groups": []})
    return out


def status():
    p = procs()
    exps = [s1_status(p.get("s1/run.py")), s3_status(p.get("s3/run.py")), labels_status(p.get("data/review.py"))]
    pr = probe_status(p.get("s3/latency_probe.py"))
    if pr:
        exps.insert(2, pr)
    ln = lean_status(p.get("s3/lean.py"))
    if ln:
        exps.insert(2, ln)
    s5 = s5_status(p.get("s5/run_hermes.py"))
    if s5:
        exps.insert(0 if s5["state"] == "running" else 2, s5)
    s9 = s9_status(p.get("s9/run.py"))
    if s9:
        exps.insert(0 if s9["state"] == "running" else 2, s9)
    s7 = s7_status()
    if s7:
        exps.insert(0 if s7["state"] == "running" else 2, s7)
    st = settle_status(p.get("data/adjudicate.py"))
    if st:
        exps.append(st)
    for e in exps:
        if e["state"].startswith("running") and not e["eta"] and e["done"] < e["total"]:
            e["eta"] = rate_eta(e)
    runs = exps[:2]
    jobs = job_cards()
    for j in jobs:
        if j["state"] == "running":
            j["eta"] = rate_eta(j)
            if not j["eta"] and j["elapsed"] and j["done"]:
                j["eta"] = fmt_eta((j["total"] - j["done"]) * j["elapsed"] / j["done"])
    live = [j for j in jobs if j["state"] == "running"]
    if live:  # a running noskill job is what's happening now: it alone fills the overall bar
        runs = live
    exps = live + exps + [j for j in jobs if j["state"] != "running"][:3]
    s4 = s4_status(p.get("s4/run.py"))
    if s4:  # a running job goes first and counts in the overall bar; a finished one goes below the other cards
        if s4["state"] == "running" and not s4["eta"]:
            s4["eta"] = rate_eta(s4)
        exps.insert(0 if s4["state"] == "running" else 2, s4)
        if s4["state"] == "running":
            runs = runs + [s4]
    if s9 and s9["state"] == "running":
        runs = runs + [s9]
    if s7 and s7["state"] == "running":
        runs = runs + [s7]
    return {"time": time.strftime("%H:%M:%S"), "experiments": exps,
            "overall": {"done": sum(e["done"] for e in runs), "total": sum(e["total"] for e in runs)},
            "machine": machine()}


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/status.json"):
            body, ctype = json.dumps(status()).encode(), "application/json"
        elif self.path in ("/", "/index.html"):
            body, ctype = (Path(__file__).parent / "index.html").read_bytes(), "text/html; charset=utf-8"
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
    print(f"Spike status on http://127.0.0.1:{PORT}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
