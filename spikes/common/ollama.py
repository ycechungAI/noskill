"""Tiny Ollama client for Phase 0 spikes. Stdlib only, throwaway."""
import json
import os
import time
import urllib.error
import urllib.request

BASE = os.environ.get("OLLAMA_HOST", "http://127.0.0.1:11434")
if not BASE.startswith("http"):
    BASE = "http://" + BASE


class OllamaError(Exception):
    pass


def _post(path, body, timeout=300):
    req = urllib.request.Request(
        BASE + path, data=json.dumps(body).encode(), headers={"content-type": "application/json"}
    )
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            data = json.loads(r.read())
    except urllib.error.HTTPError as e:
        raise OllamaError(f"{e.code}: {e.read().decode(errors='replace')[:500]}") from None
    return data, (time.perf_counter() - t0) * 1000


def _get(path, timeout=30):
    with urllib.request.urlopen(BASE + path, timeout=timeout) as r:
        return json.loads(r.read())


def chat(model, messages, tools=None, fmt=None, options=None, think=None, logprobs=False, top_logprobs=0, keep_alive="10m"):
    """Non-streaming /api/chat. Returns (response_json, wall_ms)."""
    body = {"model": model, "messages": messages, "stream": False, "keep_alive": keep_alive}
    if tools:
        body["tools"] = tools
    if fmt is not None:
        body["format"] = fmt
    if options:
        body["options"] = options
    if think is not None:
        body["think"] = think
    if logprobs:
        body["logprobs"] = True
        if top_logprobs:
            body["top_logprobs"] = top_logprobs
    return _post("/api/chat", body)


def systemone(model, state, questions, keep_alive="10m"):
    """POST /v1/systemone (JEV-style decision models). Returns (response_json, wall_ms)."""
    return _post("/v1/systemone", {"model": model, "state": state, "questions": questions, "keep_alive": keep_alive})


def ps():
    return _get("/api/ps").get("models", [])


def tags():
    return [m["name"] for m in _get("/api/tags").get("models", [])]


def show(model):
    data, _ = _post("/api/show", {"model": model})
    return data


def unload(model):
    try:
        _post("/api/generate", {"model": model, "keep_alive": 0}, timeout=60)
    except OllamaError:
        pass


def version():
    return _get("/api/version").get("version")
