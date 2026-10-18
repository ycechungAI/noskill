"""S9 legitimate work: must succeed under every mechanism. One JSON line per task.

  python legit.py csv | urllib | requests | httpx
"""
import csv
import json
import os
import sys

WORK = os.environ.get("NOSKILL_WORK", os.getcwd())
URL = os.environ.get("S9_ALLOWED_URL", "https://pypi.org/simple/requests/")


def out(task, ok, detail=""):
    print(json.dumps({"task": task, "ok": bool(ok), "detail": str(detail)[:300]}), flush=True)


def do_csv():
    # 20-line transform: total spend per category, sorted.
    totals = {}
    with open(os.path.join(WORK, "in", "data.csv"), newline="") as f:
        for row in csv.DictReader(f):
            totals[row["category"]] = totals.get(row["category"], 0.0) + float(row["amount"])
    with open(os.path.join(WORK, "out", "summary.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["category", "total"])
        for k, v in sorted(totals.items(), key=lambda kv: -kv[1]):
            w.writerow([k, f"{v:.2f}"])
    return True, f"{len(totals)} categories"


def do_urllib():
    import urllib.request
    with urllib.request.urlopen(URL, timeout=15) as r:
        return r.status == 200, f"HTTP {r.status} {len(r.read())} bytes"


def do_requests():
    import requests
    r = requests.get(URL, timeout=15)
    return r.status_code == 200, f"HTTP {r.status_code} {len(r.content)} bytes"


def do_httpx():
    import httpx
    r = httpx.get(URL, timeout=15)  # httpx reads HTTPS_PROXY from the environment by default
    return r.status_code == 200, f"HTTP {r.status_code} {len(r.content)} bytes"


if __name__ == "__main__":
    task = sys.argv[1]
    try:
        ok, detail = globals()["do_" + task]()
    except Exception as e:
        ok, detail = False, f"{type(e).__name__}: {e}"
    out(task, ok, detail)
