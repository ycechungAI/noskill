"""Build rss-100.jsonl from YOUR qBittorrent RSS feeds (S6). Stays local (.gitignore).

1. Export:  QBT_URL=http://127.0.0.1:8080 python3 spikes/data/export_rss.py export
   Uses the Web API. If "Bypass authentication for clients on localhost" is off,
   set QBT_USER and QBT_PASS in your shell first (they are only sent to QBT_URL).
2. Label:   python3 spikes/data/export_rss.py label
   For each title, type the series it belongs to (Enter = same as suggested,
   '-' = not a series you'd track). Progress is saved after each item.
"""
import http.cookiejar
import json
import os
import random
import re
import sys
import urllib.parse
import urllib.request
from pathlib import Path

OUT = Path(__file__).parent / "rss-100.jsonl"
URL = os.environ.get("QBT_URL", "http://127.0.0.1:8080").rstrip("/")


def export():
    jar = http.cookiejar.CookieJar()
    op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    op.addheaders = [("Referer", URL)]
    if os.environ.get("QBT_USER"):
        body = urllib.parse.urlencode({"username": os.environ["QBT_USER"], "password": os.environ.get("QBT_PASS", "")}).encode()
        r = op.open(URL + "/api/v2/auth/login", body).read().decode()
        if r.strip() != "Ok.":
            sys.exit(f"login failed: {r!r}")
    print("Web API version:", op.open(URL + "/api/v2/app/webapiVersion").read().decode())
    tree = json.loads(op.open(URL + "/api/v2/rss/items?withData=true").read())

    titles = []
    def walk(node, path):
        for name, v in node.items():
            if isinstance(v, dict) and "articles" in v:
                titles.extend((a.get("title", ""), name) for a in v.get("articles", []))
            elif isinstance(v, dict):
                walk(v, path + [name])
    walk(tree, [])
    titles = list({t: f for t, f in titles if t}.items())
    print(f"{len(titles)} unique article titles in {URL}")
    random.Random(42).shuffle(titles)
    with open(OUT, "w") as f:
        for i, (t, feed) in enumerate(titles[:100], 1):
            f.write(json.dumps({"id": f"q{i:03d}", "title": t, "feed": feed, "series": None}, ensure_ascii=False) + "\n")
    print(f"wrote {min(100, len(titles))} items to {OUT}. Next: label them.")


def guess(title):
    t = re.sub(r"^\s*(\[[^\]]*\]\s*)+", "", title)
    t = re.split(r"\s+-\s+\d|\s+S\d+E\d+|\s+\(|\s+\[|\.mkv|\.mp4", t)[0]
    return t.strip()


def label():
    items = [json.loads(l) for l in OUT.read_text().splitlines() if l.strip()]
    for it in items:
        if it["series"] is not None:
            continue
        g = guess(it["title"])
        s = input(f"{it['id']} {it['title']}\n   series [{g}]> ").strip()
        if s == "q":
            break
        it["series"] = "" if s == "-" else (s or g)
        OUT.write_text("".join(json.dumps(i, ensure_ascii=False) + "\n" for i in items))
    print(f"labelled {sum(i['series'] is not None for i in items)}/{len(items)}")


if __name__ == "__main__":
    {"export": export, "label": label}.get(sys.argv[1] if len(sys.argv) > 1 else "", lambda: sys.exit(__doc__))()
