"""Local agent monitor: checks that Ollama, the sched daemon and anything else
under `monitor.services` in config.yaml are up, restarts whatever isn't, and
serves a status page. See Architecture.md §8.

./start.sh installs two login agents for it next to the daemon's:
  local.agent-monitor        `sched monitor serve` -- status page, kept alive
  local.agent-monitor.heal   `sched monitor heal` -- at login and every 10 min

A service is checked by any of:
  check.launchd   label of a LaunchAgent that must be loaded and running
  check.http      URL that must answer with an HTTP status below 500
and healed by one of:
  heal.launchd    restart that LaunchAgent, or load its plist if it isn't loaded
  heal.command    argv to run instead, e.g. ["open", "-g", "-a", "Ollama"]

Checks and heals never raise; a failure comes back as text.
"""

from __future__ import annotations

import json
import logging
import os
import re
import subprocess
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from dataclasses import asdict, dataclass
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Callable
from urllib.parse import parse_qs, urlsplit

from schedlm.config import Config, ConfigWatcher

logger = logging.getLogger(__name__)

SCHED_LABEL = "com.local.sched"
MONITOR_LABEL = "local.agent-monitor"
HEAL_LABEL = "local.agent-monitor.heal"
LAUNCH_AGENTS = Path.home() / "Library" / "LaunchAgents"
STATUS_PAGE = Path(__file__).parent / "web" / "static" / "monitor.html"

HTTP_TIMEOUT_SECONDS = 3.0
HEAL_SETTLE_SECONDS = 5  # wait after a heal before checking again
LOG_MAX_BYTES = 256 * 1024
LOG_KEEP_LINES = 1000

Runner = Callable[[list], subprocess.CompletedProcess]

# Never send localhost checks through a proxy from the environment.
_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def run(argv: list) -> subprocess.CompletedProcess:
    return subprocess.run(argv, capture_output=True, text=True, timeout=30)


def _now() -> str:
    return datetime.now().strftime("%Y-%m-%d %H:%M:%S")


@dataclass
class Service:
    name: str
    label: str
    check: dict
    heal: dict | None = None


@dataclass
class Status:
    name: str
    label: str
    ok: bool
    detail: str
    can_heal: bool
    checked_at: str


def default_services(cfg: Config) -> list[Service]:
    return [
        Service(
            "ollama",
            "Ollama",
            {"http": cfg.ollama.host.rstrip("/") + "/api/version"},
            {"command": ["open", "-g", "-a", "Ollama"]},
        ),
        Service("sched", "sched daemon", {"launchd": SCHED_LABEL}, {"launchd": SCHED_LABEL}),
        Service(
            "monitor",
            "agent monitor",
            {"launchd": MONITOR_LABEL, "http": f"http://127.0.0.1:{cfg.monitor.port}/api/health"},
            {"launchd": MONITOR_LABEL},
        ),
    ]


def services_for(cfg: Config) -> list[Service]:
    """The defaults, merged by name with config.yaml's `monitor.services`: an
    entry replaces the default of the same name, `enabled: false` drops it,
    and new names are added after the defaults."""
    services = {s.name: s for s in default_services(cfg)}
    for entry in cfg.monitor.services:
        if not isinstance(entry, dict) or not isinstance(entry.get("name"), str):
            logger.warning("config.yaml: monitor.services entry without a name, ignoring: %r", entry)
            continue
        name = entry["name"]
        if entry.get("enabled", True) is False:
            services.pop(name, None)
            continue
        base = services.get(name)
        services[name] = Service(
            name=name,
            label=entry.get("label", base.label if base else name),
            check=entry.get("check", base.check if base else {}),
            heal=entry.get("heal", base.heal if base else None),
        )
    return list(services.values())


def data_dir(cfg: Config) -> Path:
    return Path(cfg.paths.data) / "monitor"


def append_log(path: Path, line: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a") as f:
        f.write(f"{_now()} {line}\n")
    if path.stat().st_size > LOG_MAX_BYTES:
        lines = path.read_text().splitlines(keepends=True)
        path.write_text("".join(lines[-LOG_KEEP_LINES:]))


def tail(path: Path, n: int = 20) -> list[str]:
    try:
        return path.read_text().splitlines()[-n:]
    except OSError:
        return []


def _domain() -> str:
    return f"gui/{os.getuid()}"


def launchd_state(label: str, runner: Runner = run) -> tuple[bool, int | None, str]:
    """(loaded, pid or None, state) of a LaunchAgent in the user's GUI domain."""
    try:
        res = runner(["launchctl", "print", f"{_domain()}/{label}"])
    except (OSError, subprocess.SubprocessError) as e:
        return False, None, f"launchctl failed: {e}"
    if res.returncode != 0:
        return False, None, "not loaded"
    # The job's own keys come first; nested blocks repeat "state = ".
    state = re.search(r"^\s*state = (.+?)\s*$", res.stdout, re.M)  # e.g. "not running"
    pid = re.search(r"^\s*pid = (\d+)", res.stdout, re.M)
    return True, int(pid.group(1)) if pid else None, state.group(1) if state else "unknown"


def http_up(url: str, timeout: float = HTTP_TIMEOUT_SECONDS) -> tuple[bool, str]:
    started = time.monotonic()
    try:
        with _opener.open(url, timeout=timeout) as resp:
            code = resp.status
    except urllib.error.HTTPError as e:
        code = e.code
    except (urllib.error.URLError, OSError) as e:
        return False, f"no answer at {url} ({getattr(e, 'reason', e)})"
    if code >= 500:
        return False, f"HTTP {code} from {url}"
    return True, f"HTTP {code} in {round((time.monotonic() - started) * 1000)} ms"


def check_service(svc: Service, runner: Runner = run) -> Status:
    problems, details = [], []
    label = svc.check.get("launchd")
    if label:
        loaded, pid, state = launchd_state(label, runner)
        if not loaded:
            problems.append(f"{label} {state}")
        elif state != "running" or pid is None:
            problems.append(f"{label} is {state}")
        else:
            details.append(f"pid {pid}")
    url = svc.check.get("http")
    if url:
        ok, msg = http_up(url)
        (details if ok else problems).append(msg)
    return Status(
        name=svc.name,
        label=svc.label,
        ok=not problems,
        detail="; ".join(problems or details) or "no checks configured",
        can_heal=bool(svc.heal),
        checked_at=_now(),
    )


def check_all(services: list[Service], runner: Runner = run) -> list[Status]:
    if not services:
        return []
    with ThreadPoolExecutor(max_workers=len(services)) as pool:
        return list(pool.map(lambda s: check_service(s, runner), services))


def heal_service(
    svc: Service, runner: Runner = run, launch_agents: Path = LAUNCH_AGENTS
) -> tuple[bool, str]:
    """(ok, what was run -- or why nothing could be)."""
    heal = svc.heal or {}
    if "launchd" in heal:
        label = heal["launchd"]
        loaded, _, _ = launchd_state(label, runner)
        if loaded:
            argv = ["launchctl", "kickstart", "-k", f"{_domain()}/{label}"]
        else:
            plist = launch_agents / f"{label}.plist"
            if not plist.exists():
                return False, f"{label} isn't loaded and {plist} doesn't exist"
            argv = ["launchctl", "bootstrap", _domain(), str(plist)]
    elif "command" in heal:
        argv = [str(a) for a in heal["command"]]
    else:
        return False, "no heal action configured"
    shown = " ".join(argv)
    try:
        res = runner(argv)
    except (OSError, subprocess.SubprocessError) as e:
        return False, f"{shown} failed: {e}"
    if res.returncode != 0:
        output = (res.stderr or res.stdout or "").strip()[:200]
        return False, f"{shown} exited {res.returncode}: {output}"
    return True, shown


def _heal_and_recheck(
    svc: Service, runner: Runner, sleep: Callable[[float], None], launch_agents: Path
) -> tuple[str, Status]:
    healed, action = heal_service(svc, runner, launch_agents)
    if not healed:
        action = f"heal failed: {action}"
    sleep(HEAL_SETTLE_SECONDS)
    return action, check_service(svc, runner)


def heal_all(
    cfg: Config,
    runner: Runner = run,
    sleep: Callable[[float], None] = time.sleep,
    launch_agents: Path = LAUNCH_AGENTS,
) -> dict:
    """Check everything and heal what is still down after heal_grace_seconds,
    so a service that is still starting at login isn't restarted. Each heal
    is logged to data/monitor/heal.log; the run is saved as last-heal.json."""
    services = services_for(cfg)
    by_name = {s.name: s for s in services}
    statuses = check_all(services, runner)
    down = [st for st in statuses if not st.ok]
    if down and cfg.monitor.heal_grace_seconds > 0:
        sleep(cfg.monitor.heal_grace_seconds)
        again = {st.name: st for st in check_all([by_name[d.name] for d in down], runner)}
        statuses = [again.get(st.name, st) for st in statuses]

    out_dir = data_dir(cfg)
    results = []
    for st in statuses:
        entry = {"name": st.name, "label": st.label, "ok": st.ok, "detail": st.detail}
        if not st.ok:
            if by_name[st.name].heal:
                action, after = _heal_and_recheck(by_name[st.name], runner, sleep, launch_agents)
                entry.update(action=action, ok_after=after.ok, detail_after=after.detail)
                outcome = "up" if after.ok else "still down"
                append_log(
                    out_dir / "heal.log",
                    f"{st.name}: down ({st.detail}) -> {action} -> {outcome} ({after.detail})",
                )
            else:
                entry.update(action=None, ok_after=False, detail_after=st.detail)
                append_log(out_dir / "heal.log", f"{st.name}: down ({st.detail}); no heal configured")
        results.append(entry)

    summary = {
        "ran_at": _now(),
        "all_ok": all(r.get("ok_after", r["ok"]) for r in results),
        "results": results,
    }
    out_dir.mkdir(parents=True, exist_ok=True)
    tmp = out_dir / "last-heal.json.tmp"
    tmp.write_text(json.dumps(summary, indent=2))
    tmp.replace(out_dir / "last-heal.json")
    return summary


class Monitor:
    """The latest check results (refreshed in the background by serve()) and
    the heal-now action behind the status page."""

    def __init__(
        self,
        get_cfg: Callable[[], Config],
        runner: Runner = run,
        sleep: Callable[[float], None] = time.sleep,
    ):
        self.get_cfg = get_cfg
        self.runner = runner
        self.sleep = sleep
        self._lock = threading.Lock()
        self._statuses: list[Status] = []

    def refresh(self) -> list[Status]:
        cfg = self.get_cfg()
        statuses = check_all(services_for(cfg), self.runner)
        with self._lock:
            before = {s.name: s.ok for s in self._statuses}
            self._statuses = statuses
        for st in statuses:
            if st.name in before and before[st.name] != st.ok:
                state = "up" if st.ok else "down"
                append_log(data_dir(cfg) / "events.log", f"{st.name}: {state} ({st.detail})")
        return statuses

    def snapshot(self) -> dict:
        cfg = self.get_cfg()
        with self._lock:
            statuses = list(self._statuses)
        out_dir = data_dir(cfg)
        try:
            last_heal = json.loads((out_dir / "last-heal.json").read_text())
        except (OSError, ValueError):
            last_heal = None
        return {
            "services": [asdict(s) for s in statuses],
            "last_heal": last_heal,
            "heal_log": tail(out_dir / "heal.log"),
            "events": tail(out_dir / "events.log"),
        }

    def heal(self, name: str, launch_agents: Path = LAUNCH_AGENTS) -> dict | None:
        cfg = self.get_cfg()
        svc = next((s for s in services_for(cfg) if s.name == name), None)
        if svc is None:
            return None
        action, after = _heal_and_recheck(svc, self.runner, self.sleep, launch_agents)
        outcome = "up" if after.ok else "still down"
        append_log(
            data_dir(cfg) / "heal.log",
            f"{name}: healed from the status page -> {action} -> {outcome} ({after.detail})",
        )
        self.refresh()
        return {"ok": after.ok, "action": action, "status": asdict(after)}


def make_server(monitor: Monitor, host: str, port: int) -> ThreadingHTTPServer:
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, format, *args):  # the page polls; keep logs quiet
            pass

        def _send(self, code: int, body, content_type: str = "application/json") -> None:
            data = body.encode() if isinstance(body, str) else json.dumps(body).encode()
            self.send_response(code)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self) -> None:
            url = urlsplit(self.path)
            if url.path == "/":
                self._send(200, STATUS_PAGE.read_text(), "text/html; charset=utf-8")
            elif url.path == "/api/health":
                self._send(200, {"ok": True})
            elif url.path == "/api/status":
                if parse_qs(url.query).get("refresh") == ["1"]:
                    monitor.refresh()
                self._send(200, monitor.snapshot())
            else:
                self._send(404, {"detail": "not found"})

        def do_POST(self) -> None:
            match = re.fullmatch(r"/api/services/([\w.-]+)/heal", urlsplit(self.path).path)
            if match is None:
                self._send(404, {"detail": "not found"})
            # Browsers can't send a custom header cross-origin without a CORS
            # preflight, which this server never approves -- so other web
            # pages can't make it restart things.
            elif self.headers.get("X-Sched-Monitor") != "1":
                self._send(403, {"detail": "missing X-Sched-Monitor header"})
            else:
                result = monitor.heal(match.group(1))
                if result is None:
                    self._send(404, {"detail": f"no service '{match.group(1)}'"})
                else:
                    self._send(200, result)

    server = ThreadingHTTPServer((host, port), Handler)
    server.daemon_threads = True
    return server


def serve(config_path: Path, host: str = "127.0.0.1", port: int | None = None) -> None:
    watcher = ConfigWatcher(config_path)
    monitor = Monitor(watcher.current)
    server = make_server(monitor, host, port or watcher.current().monitor.port)

    def check_loop() -> None:
        while True:
            try:
                monitor.refresh()
            except Exception:  # noqa: BLE001 - keep the page up whatever a check does
                logger.exception("monitor check failed")
            time.sleep(max(5, watcher.current().monitor.check_interval_seconds))

    threading.Thread(target=check_loop, daemon=True).start()
    logger.info("agent monitor serving on http://%s:%s", host, server.server_address[1])
    server.serve_forever()
