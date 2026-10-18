"""S5: a stand-in model server that records every request Hermes sends, and answers with a short canned reply.

  python3 spikes/s5/capture.py [--port 11600] [--out spikes/s5/out/capture.jsonl]
Serves GET /v1/models (one model, with a context window) and POST /v1/chat/completions (stream or not).
Each request is saved in full, so we can measure the system prompt and tool schemas Hermes sends before the user's message.
"""
import argparse
import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

MODEL = "capture-1"
CTX = 65536


class H(BaseHTTPRequestHandler):
    out = None

    def log_message(self, *a):
        pass

    def _json(self, obj, code=200):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path.startswith("/v1/models") or self.path.startswith("/models"):
            self._json({"object": "list", "data": [{"id": MODEL, "object": "model", "owned_by": "capture", "context_length": CTX}]})
        elif self.path == "/":
            self._json({"ok": True})
        else:
            self._json({"error": "not found"}, 404)

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n)
        try:
            body = json.loads(raw)
        except json.JSONDecodeError:
            body = {"_raw": raw[:200].decode(errors="replace")}
        with open(self.out, "a") as f:
            f.write(json.dumps({"t": time.time(), "path": self.path, "bytes": len(raw), "ua": self.headers.get("User-Agent"), "body": body}) + "\n")
        if not self.path.endswith("/chat/completions"):
            self._json({"error": "not supported"}, 404)
            return
        text = "Captured."
        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for chunk in ({"role": "assistant", "content": ""}, {"content": text}, {}):
                fin = "stop" if not chunk else None
                self.wfile.write(("data: " + json.dumps({"id": "c1", "object": "chat.completion.chunk", "model": MODEL,
                                                         "choices": [{"index": 0, "delta": chunk, "finish_reason": fin}]}) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")
        else:
            self._json({"id": "c1", "object": "chat.completion", "model": MODEL,
                        "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}],
                        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=11600)
    ap.add_argument("--out", default=str(Path(__file__).parent / "out" / "capture.jsonl"))
    a = ap.parse_args()
    H.out = a.out
    Path(a.out).parent.mkdir(parents=True, exist_ok=True)
    print("capture server on", a.port, flush=True)
    ThreadingHTTPServer(("127.0.0.1", a.port), H).serve_forever()
