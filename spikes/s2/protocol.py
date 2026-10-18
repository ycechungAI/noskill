"""S2 protocol checks against the proxy: scripted OpenAI-SDK, Ollama-format and ollama-CLI clients.

  uv run --with openai python spikes/s2/protocol.py [--runs 100] [--cli-runs 10]
Needs the mock (11436), the proxy (11435) and a proxy with -keepalive 1s on 11437 (see run.sh).
A "run" is one pass through every scenario for that client; any failed assertion counts as a protocol error.
"""
import argparse
import http.client
import json
import os
import socket
import subprocess
import time
import uuid
from collections import Counter

P = ("127.0.0.1", 11435)
MOCK = ("127.0.0.1", 11436)
KA = ("127.0.0.1", 11437)


def req(addr, method, path, body=None, headers=None):
    c = http.client.HTTPConnection(*addr, timeout=60)
    h = {"Content-Type": "application/json", **(headers or {})}
    c.request(method, path, json.dumps(body) if body is not None else None, h)
    return c, c.getresponse()


def mock_cancel(rid):
    for _ in range(100):
        c, r = req(MOCK, "GET", f"/_cancel/{rid}")
        d = json.loads(r.read())
        c.close()
        if d["cancelled_at"]:
            return d
        time.sleep(0.02)
    return None


def msg(text):
    return [{"role": "user", "content": text}]


# ---------------- OpenAI SDK ----------------
def openai_scenarios():
    from openai import OpenAI, BadRequestError
    cl = OpenAI(base_url="http://127.0.0.1:11435/v1", api_key="spike", max_retries=0)

    def models():
        ids = [m.id for m in cl.models.list().data]
        assert "noskill/auto" in ids and "mock-1" in ids, ids

    def nonstream():
        r = cl.chat.completions.create(model="noskill/auto", messages=msg("hi"))
        assert r.choices[0].message.content.count("tok") == 40 and r.choices[0].finish_reason == "stop"

    def stream():
        chunks = list(cl.chat.completions.create(model="noskill/auto", messages=msg("hi"), stream=True))
        assert chunks[0].choices[0].delta.role == "assistant", "first chunk has no role"
        assert chunks[-1].choices[0].finish_reason == "stop", "last chunk has no finish_reason"
        n = sum(1 for c in chunks if c.choices and c.choices[0].delta.content)
        assert n == 40, n

    def thinking():
        chunks = list(cl.chat.completions.create(model="noskill/auto", messages=msg("think hard"), stream=True))
        r = "".join(getattr(c.choices[0].delta, "reasoning_content", None) or "" for c in chunks if c.choices)
        assert "hmm" in r, "no reasoning_content"

    def long():
        n = sum(1 for c in cl.chat.completions.create(model="noskill/auto", messages=msg("long answer"), stream=True)
                if c.choices and c.choices[0].delta.content)
        assert n == 5000, n

    def tool():
        chunks = list(cl.chat.completions.create(model="noskill/auto", messages=msg("what time is it"), stream=True))
        r = "".join(getattr(c.choices[0].delta, "reasoning_content", None) or "" for c in chunks if c.choices)
        t = "".join(c.choices[0].delta.content or "" for c in chunks if c.choices)
        assert "Calling get_time" in r and t.count("tok") == 12, (r, t)

    def stop():
        rid = uuid.uuid4().hex
        t0 = time.time()
        s = cl.chat.completions.create(model="noskill/auto", messages=msg("slow long answer"), stream=True, extra_headers={"X-Req-Id": rid})
        for i, _ in enumerate(s):
            if i == 10:
                break
        s.close()
        closed = time.time()
        d = mock_cancel(rid)
        assert d and d["cancelled_at"] / 1e9 - closed < 1.0, f"backend not cancelled within 1 s: {d}"

    def plain():
        n = sum(1 for c in cl.chat.completions.create(model="mock-1", messages=msg("hi"), stream=True) if c.choices and c.choices[0].delta.content)
        assert n == 40, n

    def bad():
        try:
            cl.chat.completions.create(model="noskill/auto", messages=[])
        except BadRequestError as e:
            assert "messages" in str(e)
        else:
            raise AssertionError("no error for empty messages")

    return {"models": models, "non-stream": nonstream, "stream": stream, "thinking": thinking, "long answer": long,
            "tool step": tool, "stop mid-stream": stop, "plain name": plain, "error format": bad}


# ---------------- Ollama format ----------------
def nd_lines(r):
    out = []
    for line in r:
        line = line.strip()
        if line:
            out.append(json.loads(line))
    return out


def ollama_scenarios():
    def tags():
        c, r = req(P, "GET", "/api/tags")
        names = [m["name"] for m in json.loads(r.read())["models"]]
        assert "noskill/auto" in names and "mock-1" in names, names
        c.close()
        for path in ("/api/version", "/api/ps"):
            c, r = req(P, "GET", path)
            assert r.status == 200 and json.loads(r.read()), path
            c.close()

    def show():
        c, r = req(P, "POST", "/api/show", {"model": "noskill/auto"})
        d = json.loads(r.read())
        assert r.status == 200 and "tools" in d["capabilities"], d
        c.close()
        c, r = req(P, "POST", "/api/show", {"model": "mock-1"})
        assert r.status == 200
        c.close()

    def stream():
        c, r = req(P, "POST", "/api/chat", {"model": "noskill/auto", "messages": msg("hi")})  # stream defaults to true
        assert r.getheader("Content-Type", "").startswith("application/x-ndjson")
        ls = nd_lines(r)
        c.close()
        assert ls[-1]["done"] is True and ls[-1]["done_reason"] == "stop", ls[-1]
        assert all(not l["done"] for l in ls[:-1])
        assert sum(l["message"]["content"].count("tok") for l in ls) == 40

    def nonstream():
        c, r = req(P, "POST", "/api/chat", {"model": "noskill/auto", "messages": msg("hi"), "stream": False})
        d = json.loads(r.read())
        c.close()
        assert d["done"] and d["message"]["content"].count("tok") == 40

    def thinking():
        c, r = req(P, "POST", "/api/chat", {"model": "noskill/auto", "messages": msg("think hard")})
        ls = nd_lines(r)
        c.close()
        assert "hmm" in "".join(l["message"].get("thinking", "") for l in ls)

    def long():
        c, r = req(P, "POST", "/api/chat", {"model": "noskill/auto", "messages": msg("long answer")})
        ls = nd_lines(r)
        c.close()
        n = sum(l["message"]["content"].count("tok") for l in ls)
        assert n == 5000, n

    def tool():
        c, r = req(P, "POST", "/api/chat", {"model": "noskill/auto", "messages": msg("what time is it")})
        ls = nd_lines(r)
        c.close()
        assert "Calling get_time" in "".join(l["message"].get("thinking", "") for l in ls)
        assert sum(l["message"]["content"].count("tok") for l in ls) == 12

    def stop():
        rid = uuid.uuid4().hex
        c, r = req(P, "POST", "/api/chat", {"model": "noskill/auto", "messages": msg("slow long answer")}, {"X-Req-Id": rid})
        for _ in range(10):
            r.readline()
        c.sock.shutdown(socket.SHUT_RDWR)  # a plain close() would keep the fd alive through the response's file object
        c.sock.close()
        closed = time.time()
        d = mock_cancel(rid)
        assert d and d["cancelled_at"] / 1e9 - closed < 1.0, f"backend not cancelled within 1 s: {d}"

    def plain():
        c, r = req(P, "POST", "/api/chat", {"model": "mock-1", "messages": msg("hi")})
        ls = nd_lines(r)
        c.close()
        assert ls[-1]["done"] and sum(l["message"]["content"].count("tok") for l in ls) == 40

    def generate():
        c, r = req(P, "POST", "/api/generate", {"model": "noskill/auto", "prompt": "what time is it"})
        ls = nd_lines(r)
        c.close()
        assert ls[-1]["done"] and "Calling get_time" in "".join(l.get("thinking", "") for l in ls)
        assert sum(l["response"].count("tok") for l in ls) == 12
        c, r = req(P, "POST", "/api/generate", {"model": "noskill/auto", "prompt": "hi", "stream": False})
        d = json.loads(r.read())
        c.close()
        assert d["done"] and d["response"].count("tok") == 40

    def bad():
        c, r = req(P, "POST", "/api/chat", {"model": "noskill/auto", "messages": []})
        d = json.loads(r.read())
        c.close()
        assert r.status == 400 and isinstance(d["error"], str), d

    return {"tags/version/ps": tags, "show": show, "stream (default)": stream, "non-stream": nonstream, "thinking": thinking,
            "long answer": long, "tool step": tool, "stop mid-stream": stop, "plain name": plain, "generate": generate, "error format": bad}


# ---------------- ollama CLI ----------------
def cli_scenarios():
    env = {**os.environ, "OLLAMA_HOST": "http://127.0.0.1:11435"}

    def run(*args):
        r = subprocess.run(["ollama", *args], capture_output=True, text=True, env=env, timeout=60, stdin=subprocess.DEVNULL)
        assert r.returncode == 0, r.stderr
        return r.stdout

    return {"ollama list": lambda: (lambda o: None if "noskill/auto" in o and "mock-1" in o else (_ for _ in ()).throw(AssertionError(o)))(run("list")),
            "ollama show": lambda: run("show", "noskill/auto"),
            "ollama run": lambda: (lambda o: None if o.count("tok") >= 40 else (_ for _ in ()).throw(AssertionError(o)))(run("run", "noskill/auto", "hi", "--nowordwrap")),
            "ollama run (plain)": lambda: run("run", "mock-1", "hi", "--nowordwrap")}


# ---------------- keepalives ----------------
def keepalive():
    # proxy on 11437 pings every 1 s; the mock goes quiet for 3 s
    c, r = req(KA, "POST", "/v1/chat/completions", {"model": "noskill/auto", "stream": True, "messages": msg("quiet")})
    raw = r.read().decode()
    c.close()
    assert raw.count(": keepalive") >= 2, raw[:300]
    assert "data: [DONE]" in raw
    c, r = req(KA, "POST", "/api/chat", {"model": "noskill/auto", "messages": msg("quiet")})
    ls = nd_lines(r)
    c.close()
    empties = [l for l in ls if not l["done"] and l["message"]["content"] == "" and not l["message"].get("thinking")]
    assert len(empties) >= 2, len(empties)


def run_set(name, scen, runs):
    fails, first = Counter(), {}
    t0 = time.time()
    for _ in range(runs):
        for k, fn in scen.items():
            try:
                fn()
            except Exception as e:  # noqa: BLE001
                fails[k] += 1
                first.setdefault(k, f"{type(e).__name__}: {str(e)[:160]}")
    total = sum(fails.values())
    print(f"| {name} | {runs} | {len(scen)} | {total} | {', '.join(f'{k} ×{v}' for k, v in fails.items()) or '–'} | {time.time() - t0:.0f} s |")
    for k, v in first.items():
        print(f"|  ↳ first error, {k} | | | | {v} | |")
    return total


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--runs", type=int, default=100)
    ap.add_argument("--cli-runs", type=int, default=10)
    a = ap.parse_args()
    print("| Client and connection | Runs | Scenarios per run | Errors | Which | Time |\n|---|---|---|---|---|---|")
    run_set("OpenAI Python SDK, OpenAI connection (`/v1`)", openai_scenarios(), a.runs)
    run_set("Raw HTTP, Ollama connection (`/api`)", ollama_scenarios(), a.runs)
    run_set("`ollama` CLI, Ollama connection", cli_scenarios(), a.cli_runs)
    run_set("Keepalives (proxy ping every 1 s, backend silent 3 s)", {"SSE and NDJSON": keepalive}, 5)
