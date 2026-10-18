"""Summarise results/s7-macos.jsonl: drift per scheduler, wake events, skipped/catch-up runs, notification events.
  python3 spikes/s7/report.py [--after HH:MM]   (only rows at or after that local time)"""
import json, statistics as st, sys
from pathlib import Path

rows = [json.loads(l) for l in (Path(__file__).resolve().parents[1] / "results" / "s7-macos.jsonl").read_text().splitlines() if l.strip()]
after = sys.argv[sys.argv.index("--after") + 1] if "--after" in sys.argv else ""
rows = [r for r in rows if r["ts"][11:16] >= after]
by = {}
for r in rows:
    if r["event"] == "run":
        by.setdefault((r["mode"], r["variant"], r["kind"]), []).append(r["drift_ms"] / 1000)
print("scheduler / variant / kind: runs, median, max drift (s)")
for k, v in sorted(by.items()):
    print(" ", *k, "|", len(v), round(st.median(v), 2), round(max(v), 2))
for r in rows:
    if r["event"] in ("wake", "skipped", "start", "stop") or r["event"].startswith("notify_") or (r["event"] == "run" and r["kind"] == "catchup"):
        print(r["ts"][11:19], r["event"], r.get("variant", ""), {k: v for k, v in r.items() if k not in ("ts", "event", "variant", "pid", "boot", "launchd_label", "ppid")})
