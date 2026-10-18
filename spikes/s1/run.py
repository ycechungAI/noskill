"""S1: tool calling on small models (PHASE0_TESTS.md#s1-tool-calling-on-small-models).

Runs tools-50 × strategies × models × seeds against a mock world and appends one
JSON line per run to results/s1-raw.jsonl (resumable: finished runs are skipped).

  python3 spikes/s1/run.py --models llama3.2:3b,qwen3.5:4b --seeds 1,2,3
  python3 spikes/s1/report.py

Strategies:
  native       Ollama `tools`; the model's own tool-call format, parsed by Ollama
  constrained  `format` = JSON schema (anyOf one object per tool + "answer"); grammar-enforced
  prompted     tools described in the prompt; JSON parsed leniently from free text
run_python executes model-written code under sandbox-exec (no network, no writes, no reads in /Users).
"""
import argparse
import copy
import datetime as dt
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from common import ollama  # noqa: E402

DATA = ROOT / "data"
WORLD = json.loads((DATA / "tools-world.json").read_text())
HOME = WORLD["home"]
MAX_STEPS = 8
SYSTEM_BASE = (
    "You are an assistant running on the user's Mac. The user's home folder is /Users/me (also written ~). "
    "External drives are mounted under /Volumes/<drive name>. Use the tools when they help. "
    "When you have what you need, answer the user concisely."
)
CITY_TZ = {"tokyo": "Asia/Tokyo", "london": "Europe/London", "newyork": "America/New_York", "losangeles": "America/Los_Angeles",
           "la": "America/Los_Angeles", "paris": "Europe/Paris", "berlin": "Europe/Berlin", "sydney": "Australia/Sydney", "utc": "UTC"}
SANDBOX = '(version 1)(allow default)(deny network*)(deny file-write*)(allow file-write* (literal "/dev/null"))(deny file-read* (subpath "/Users"))'


def squash(s):
    return re.sub(r"[\s_]+", "", str(s).lower())


# ---------------- mock world ----------------
class World:
    def __init__(self, fixtures):
        self.files = {self.norm(f) for f in WORLD["files"]}
        self.fx = fixtures

    @staticmethod
    def norm(p):
        p = str(p).strip().strip("'\"`").replace("$HOME", "~")
        if p.startswith("~"):
            p = HOME + p[1:]
        elif not p.startswith("/"):
            p = HOME + "/" + p
        p = os.path.normpath(p)
        return p

    def dirs(self):
        d = set()
        for f in self.files:
            parent = os.path.dirname(f)
            while parent not in ("/", ""):
                d.add(parent)
                parent = os.path.dirname(parent)
        return d

    def find(self, p, kind):
        """Case-insensitive lookup of an existing file ('f') or dir ('d')."""
        n = self.norm(p).lower()
        pool = self.files if kind == "f" else self.dirs()
        return next((x for x in pool if x.lower() == n), None)

    # tools
    def get_time(self, timezone=None):
        tzname = WORLD["local_tz"]
        if timezone and squash(timezone) not in ("", "local"):
            try:
                ZoneInfo(timezone.strip())
                tzname = timezone.strip()
            except (ZoneInfoNotFoundError, ValueError):
                key = squash(timezone.split("/")[-1])
                if key not in CITY_TZ:
                    return f"Error: unknown timezone '{timezone}'. Use an IANA name like Asia/Tokyo."
                tzname = CITY_TZ[key]
        t = dt.datetime.fromisoformat(WORLD["now_utc"]).astimezone(ZoneInfo(tzname))
        return f"{t.strftime('%Y-%m-%d %H:%M %A')} ({tzname}, UTC{t.strftime('%z')[:3]}:{t.strftime('%z')[3:]})"

    def search(self, query):
        res = self.fx.get("search", [])
        return json.dumps(res, ensure_ascii=False) if res else "No results found."

    @staticmethod
    def _norm(url):
        u = re.sub(r"^https?://(www\.)?", "", str(url).strip().lower()).rstrip("/")
        u = re.sub(r"^raw\.githubusercontent\.com/([^/]+/[^/]+)", r"github.com/\1", u)
        return re.sub(r"^api\.github\.com/repos/", "github.com/", u)

    def fetch(self, url):
        pages = self.fx.get("pages", {})
        u = self._norm(url)
        best = None
        for k, v in pages.items():
            nk = self._norm(k)
            if u == nk or u.startswith(nk + "/"):
                if best is None or len(nk) > best[0]:
                    best = (len(nk), v)
        return best[1] if best else "Error 404: page not found."

    def list_dir(self, path):
        d = self.find(path, "d")
        if not d:
            return f"Error: no such folder: {path}"
        kids = sorted({x[len(d) + 1:].split("/")[0] + ("/" if "/" in x[len(d) + 1:] else "")
                       for x in self.files if x.startswith(d + "/")})
        return "\n".join(kids) if kids else "(empty)"

    def move(self, src, dst):
        s = self.find(src, "f")
        if not s:
            return f"Error: no such file: {src}"
        dd = self.find(dst, "d")
        if dd:
            target = dd + "/" + os.path.basename(s)
        else:
            parent = self.find(os.path.dirname(self.norm(dst)), "d")
            if not parent:
                return f"Error: destination folder does not exist: {os.path.dirname(self.norm(dst))}"
            target = parent + "/" + os.path.basename(self.norm(dst))
        if self.find(target, "f"):
            return f"Error: {target} already exists (never overwrites)."
        self.files.remove(s)
        self.files.add(target)
        return f"Moved {s} -> {target}"

    def run_python(self, code):
        try:
            p = subprocess.run(["sandbox-exec", "-p", SANDBOX, sys.executable, "-I", "-S", "-B", "-c", str(code)],
                               capture_output=True, text=True, timeout=10, cwd="/tmp")
            out = (p.stdout + (("\n" + p.stderr) if p.returncode else "")).strip()
            return out[:2000] or "(no output; use print())"
        except subprocess.TimeoutExpired:
            return "Error: timed out after 10 s"


TOOLS = {t["name"]: t for t in WORLD["tools"]}


def validate(name, args):
    if name not in TOOLS:
        return f"unknown tool '{name}'"
    if not isinstance(args, dict):
        return "args must be an object"
    spec = TOOLS[name]["parameters"]
    for r in spec.get("required", []):
        if r not in args or args[r] in (None, ""):
            return f"missing required argument '{r}'"
    for k, v in args.items():
        if k in spec["properties"] and v is not None and not isinstance(v, str):
            return f"argument '{k}' must be a string"
    return None


def execute(world, name, args):
    err = validate(name, args)
    if err:
        return False, f"Error: {err}. Available tools: {', '.join(TOOLS)}."
    kwargs = {k: v for k, v in args.items() if k in TOOLS[name]["parameters"]["properties"]}
    return True, getattr(world, name)(**kwargs)


# ---------------- strategies ----------------
def tool_list_text():
    lines = []
    for t in WORLD["tools"]:
        params = ", ".join(f"{k} ({'required' if k in t['parameters'].get('required', []) else 'optional'}): {v.get('description', v['type'])}"
                           for k, v in t["parameters"]["properties"].items())
        lines.append(f"- {t['name']}: {t['description']} Arguments: {params or 'none'}")
    return "\n".join(lines)


NATIVE_TOOLS = [{"type": "function", "function": {"name": t["name"], "description": t["description"], "parameters": t["parameters"]}} for t in WORLD["tools"]]

CONSTRAINED_SCHEMA = {"anyOf": [
    {"type": "object", "properties": {"tool": {"type": "string", "enum": [t["name"]]}, "args": t["parameters"]}, "required": ["tool", "args"]}
    for t in WORLD["tools"]
] + [{"type": "object", "properties": {"tool": {"type": "string", "enum": ["answer"]}, "text": {"type": "string"}}, "required": ["tool", "text"]}]}

CONSTRAINED_SYS = SYSTEM_BASE + "\n\nTools:\n" + tool_list_text() + (
    '\n\nAlways reply with one JSON object. To call a tool: {"tool": "<name>", "args": {...}}. '
    'To reply to the user: {"tool": "answer", "text": "<your reply>"}. Call one tool at a time; you will get its result next.')

PROMPTED_SYS = SYSTEM_BASE + "\n\nTools:\n" + tool_list_text() + (
    '\n\nTo call a tool, reply with ONLY a JSON object like {"tool": "<name>", "args": {...}} and nothing else. '
    "You will get the result in the next message. Call one tool at a time. "
    "When you are done, reply to the user in plain text, without JSON.")


def parse_loose_json(text):
    t = re.sub(r"```(?:json)?", "", text).strip()
    i, j = t.find("{"), t.rfind("}")
    if i < 0 or j <= i:
        return None
    try:
        return json.loads(t[i:j + 1])
    except json.JSONDecodeError:
        return "invalid"


def run_task(model, strategy, seed, task, think):
    world = World(task["fixtures"])
    opts = {"temperature": 0.2, "seed": seed, "num_ctx": 8192}
    calls, final, steps, err = [], None, 0, None
    sys_prompt = {"native": SYSTEM_BASE, "constrained": CONSTRAINED_SYS, "prompted": PROMPTED_SYS}[strategy]
    msgs = [{"role": "system", "content": sys_prompt}, {"role": "user", "content": task["prompt"]}]
    t0 = time.perf_counter()
    try:
        while steps < MAX_STEPS:
            steps += 1
            if strategy == "native":
                r, _ = ollama.chat(model, msgs, tools=NATIVE_TOOLS, options=opts, think=think)
                m = r["message"]
                tcs = m.get("tool_calls") or []
                if not tcs:
                    final = m.get("content", "")
                    break
                msgs.append({"role": "assistant", "content": m.get("content", ""), "tool_calls": tcs})
                for tc in tcs:
                    fn = tc.get("function", {})
                    args = fn.get("arguments", {})
                    if isinstance(args, str):
                        try:
                            args = json.loads(args)
                        except json.JSONDecodeError:
                            pass
                    ok, out = execute(world, fn.get("name"), args)
                    calls.append({"tool": fn.get("name"), "args": args, "valid": ok})
                    msgs.append({"role": "tool", "tool_name": fn.get("name"), "content": out})
            else:
                fmt = CONSTRAINED_SCHEMA if strategy == "constrained" else None
                r, _ = ollama.chat(model, msgs, fmt=fmt, options=opts, think=think)
                content = r["message"].get("content", "")
                msgs.append({"role": "assistant", "content": content})
                obj = parse_loose_json(content) if strategy == "prompted" else (json.loads(content) if content.strip() else "invalid")
                if obj == "invalid" or (strategy == "constrained" and not isinstance(obj, dict)):
                    if strategy == "prompted" and '"tool"' not in content:
                        final = content
                        break
                    calls.append({"tool": None, "args": content[:300], "valid": False})
                    msgs.append({"role": "user", "content": "Error: that was not valid JSON. Reply with one JSON object."})
                    continue
                if not isinstance(obj, dict) or "tool" not in obj:
                    final = content  # prompted: plain answer that happens to contain braces
                    break
                if obj["tool"] == "answer":
                    final = obj.get("text", "") if strategy == "constrained" else content
                    break
                ok, out = execute(world, obj.get("tool"), obj.get("args", {}))
                calls.append({"tool": obj.get("tool"), "args": obj.get("args"), "valid": ok})
                msgs.append({"role": "user", "content": f"Result of {obj.get('tool')}:\n{out}"})
    except Exception as e:  # noqa: BLE001
        err = str(e)[:300]
    ms = (time.perf_counter() - t0) * 1000
    return world, calls, final, steps, ms, err


# ---------------- scoring ----------------
def match_args(args, matchers):
    if not isinstance(args, dict):
        return False
    for k, m in matchers.items():
        v = args.get(k)
        if v is None:
            return False
        if "path" in m and World.norm(v).lower() != World.norm(m["path"]).lower():
            return False
        if "contains" in m and squash(m["contains"]) not in squash(v):
            return False
        if "eq" in m and str(v).strip().rstrip("/") != m["eq"].rstrip("/"):
            return False
    return True


def score(task, world, calls, final):
    c = task["checks"]
    ok = final is not None
    text = final or ""
    if ok and c.get("answer_any"):
        ok = any(re.search(p, text, re.I) for p in c["answer_any"])
    if ok and c.get("answer_all"):
        ok = all(re.search(p, text, re.I) for p in c["answer_all"])
    if ok and c.get("min_len"):
        ok = len(text.strip()) >= c["min_len"]
    if ok and c.get("max_words"):
        ok = len(re.findall(r"[\w'-]+", text)) <= c["max_words"]
    for f in c.get("fs_exists", []):
        ok = ok and world.find(f, "f") is not None
    for f in c.get("fs_missing", []):
        ok = ok and world.find(f, "f") is None
    for d, pat in c.get("fs_dir_match", {}).items():
        dd = world.find(d, "d")
        ok = ok and dd is not None and any(re.search(pat, os.path.basename(x)) for x in world.files if os.path.dirname(x) == dd)
    exp = task["expect_calls"]
    first = calls[0] if calls else None
    right_tool = bool(exp and first and first["tool"] == exp[0]["tool"])
    right_args = bool(right_tool and match_args(first["args"], exp[0]["args"]))
    return {"success": bool(ok), "right_tool": right_tool, "right_args": right_args,
            "false_call": task["kind"] == "none" and bool(calls)}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--models", default="llama3.2:3b,qwen3.5:4b")
    ap.add_argument("--strategies", default="native,constrained,prompted")
    ap.add_argument("--seeds", default="1,2,3")
    ap.add_argument("--tasks", default="")
    ap.add_argument("--out", default=str(ROOT / "results" / "s1-raw.jsonl"))
    a = ap.parse_args()
    tasks = [json.loads(l) for l in (DATA / "tools-50.jsonl").read_text().splitlines() if l.strip()]
    if a.tasks:
        keep = set(a.tasks.split(","))
        tasks = [t for t in tasks if t["id"] in keep]
    out = Path(a.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    done = set()
    if out.exists():
        for l in out.read_text().splitlines():
            r = json.loads(l)
            if not r.get("error"):
                done.add((r["model"], r["strategy"], r["seed"], r["task"]))
    models = a.models.split(",")
    for model in models:
        for other in models:
            if other != model:
                ollama.unload(other)
        caps = ollama.show(model).get("capabilities", [])
        think = False if "thinking" in caps else None
        ollama.chat(model, [{"role": "user", "content": "hi"}], options={"num_ctx": 8192}, think=think)  # load before timing
        for strategy in a.strategies.split(","):
            for seed in [int(s) for s in a.seeds.split(",")]:
                for task in tasks:
                    key = (model, strategy, seed, task["id"])
                    if key in done:
                        continue
                    world, calls, final, steps, ms, err = run_task(model, strategy, seed, task, think)
                    rec = {"model": model, "strategy": strategy, "seed": seed, "task": task["id"], "kind": task["kind"],
                           "steps": steps, "ms": round(ms), "calls": calls, "final": (final or "")[:600], "error": err,
                           **score(task, world, calls, final)}
                    with open(out, "a") as f:
                        f.write(json.dumps(rec) + "\n")  # ASCII escapes: model output can hold lone surrogates
                    print(f"{model:14} {strategy:11} s{seed} {task['id']} {'OK ' if rec['success'] else 'bad'} "
                          f"calls={len(calls)} {rec['ms']:>6} ms" + (f" ERR {err}" if err else ""), flush=True)


if __name__ == "__main__":
    main()
