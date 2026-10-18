"""S2 check against real Ollama: the proxy (port 11438) in front of Ollama with qwen3.5:4b.

  out/proxy -listen 127.0.0.1:11438 -backend http://127.0.0.1:11434 -model qwen3.5:4b &
  python3 spikes/s2/real.py
Checks real streaming formats (reasoning field names, tool-call chunks) and that cancelling reaches Ollama.
"""
import http.client
import json
import re
import subprocess
import time
from pathlib import Path

P = ("127.0.0.1", 11438)
LOG = Path.home() / ".ollama/logs/server.log"


def post(path, body, stream=True):
    c = http.client.HTTPConnection(*P, timeout=300)
    c.request("POST", path, json.dumps(body), {"Content-Type": "application/json"})
    return c, c.getresponse()


def sse(r):
    for line in r:
        line = line.decode().strip()
        if line.startswith("data: ") and line != "data: [DONE]":
            yield json.loads(line[6:])


def main():
    print("| Check | Result |\n|---|---|")
    # 1. OpenAI stream with a real tool call
    t0 = time.time()
    c, r = post("/v1/chat/completions", {"model": "noskill/auto", "stream": True, "reasoning_effort": "none", "messages": [{"role": "user", "content": "What time is it right now? Use your tool, then answer in one short sentence."}]})
    reasoning, content, first = "", "", None
    for ch in sse(r):
        d = ch["choices"][0]["delta"]
        reasoning += d.get("reasoning_content") or ""
        if d.get("content") and first is None:
            first = time.time() - t0
        content += d.get("content") or ""
    c.close()
    print(f"| OpenAI stream, real tool call | status {r.status}; progress shows get_time: {'Calling get_time' in reasoning}; answer: {content.strip()[:90]!r}; {time.time() - t0:.1f} s |")
    # 2. Ollama chat
    c, r = post("/api/chat", {"model": "noskill/auto", "think": False, "messages": [{"role": "user", "content": "What time is it right now? Use your tool, then answer in one short sentence."}]})
    lines = [json.loads(l) for l in r if l.strip()]
    c.close()
    th = "".join(l["message"].get("thinking", "") for l in lines)
    ans = "".join(l["message"]["content"] for l in lines)
    print(f"| Ollama /api/chat stream | last line done={lines[-1]['done']}; thinking {len(th)} chars; answer {ans.strip()[:90]!r} |")
    # 2b. real thinking is forwarded (capped at 150 tokens so the check stays short)
    c, r = post("/v1/chat/completions", {"model": "noskill/auto", "stream": True, "max_tokens": 150, "messages": [{"role": "user", "content": "What is 17 times 23?"}]})
    th2 = "".join(ch["choices"][0]["delta"].get("reasoning_content") or "" for ch in sse(r))
    c.close()
    print(f"| Real thinking tokens forwarded as `reasoning_content` (OpenAI) | {len(th2)} chars, e.g. {th2[:40]!r} |")
    # 3. cancel a long story and see when Ollama's own log closes the request
    before = LOG.read_text().count("\n")
    c, r = post("/v1/chat/completions", {"model": "noskill/auto", "stream": True, "messages": [{"role": "user", "content": "Write a very long story about a lighthouse keeper, at least 3000 words. Do not use tools."}], "reasoning_effort": "none"})
    n = 0
    for ch in sse(r):
        if ch["choices"][0]["delta"].get("content"):
            n += 1
        if n >= 15:
            break
    closed = time.time()
    c.sock.shutdown(2)
    c.close()
    end = None
    for _ in range(100):
        time.sleep(0.1)
        new = LOG.read_text().splitlines()[before:]
        done = [l for l in new if "/v1/chat/completions" in l and "POST" in l]
        if done:
            end = time.time()
            m = re.search(r"\|\s+(\d+)\s*\|\s+([\d.]+)(ms|s|m[\d.]+s)", done[-1])
            line = done[-1]
            break
    print(f"| Cancel after 15 content chunks of a 3000-word story | Ollama finished the request {end - closed:.1f} s after the client closed (log: `{line.split('|', 1)[1][:60].strip()}…`) |" if end else "| Cancel | Ollama never closed the request within 10 s |")
    # 4. plain passthrough of a real model, Ollama format, with `ollama ps` showing the model still loaded
    c, r = post("/api/chat", {"model": "qwen3.5:4b", "stream": False, "think": False, "messages": [{"role": "user", "content": "Say hi in three words."}]})
    d = json.loads(r.read())
    c.close()
    print(f"| Plain name /api/chat, non-stream | {d['message']['content'].strip()!r}, done_reason {d.get('done_reason')} |")


main()
