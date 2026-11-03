import json
import subprocess
import threading
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

from schedlm import monitor
from schedlm.config import Config, MonitorConfig, PathsConfig

RUNNING = """com.local.sched = {
\tactive count = 1
\tstate = running
\tpid = 822
\tendpoints = {
\t\t"x" = {
\t\t\tstate = active
\t\t}
\t}
}
"""
NOT_RUNNING = "local.agent-monitor.heal = {\n\tstate = not running\n}\n"


class FakeLaunchctl:
    """Answers `launchctl print` from a {label: stdout or None} map and
    records every other call."""

    def __init__(self, jobs: dict):
        self.jobs = jobs
        self.calls: list[list[str]] = []

    def __call__(self, argv):
        self.calls.append(argv)
        if argv[:2] == ["launchctl", "print"]:
            out = self.jobs.get(argv[2].split("/", 2)[2])
            if out is None:
                return subprocess.CompletedProcess(argv, 113, "", "Could not find service")
            return subprocess.CompletedProcess(argv, 0, out, "")
        return subprocess.CompletedProcess(argv, 0, "", "")

    def actions(self):
        return [c for c in self.calls if c[:2] != ["launchctl", "print"]]


@pytest.fixture
def http_server():
    """A local server whose /<code> path answers with that status."""

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            self.send_response(int(self.path.strip("/") or 200))
            self.end_headers()

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{server.server_address[1]}"
    server.shutdown()


def closed_port_url() -> str:
    server = ThreadingHTTPServer(("127.0.0.1", 0), BaseHTTPRequestHandler)
    port = server.server_address[1]
    server.server_close()
    return f"http://127.0.0.1:{port}/"


def make_cfg(tmp_path: Path, services: list | None = None, **monitor_kw) -> Config:
    return Config(
        paths=PathsConfig(data=str(tmp_path / "data")),
        monitor=MonitorConfig(services=services or [], **monitor_kw),
    )


def test_services_merge_defaults_with_config(tmp_path):
    cfg = make_cfg(
        tmp_path,
        [
            {"name": "ollama", "heal": {"command": ["true"]}},
            {"name": "monitor", "enabled": False},
            {"name": "noskill", "check": {"launchd": "dev.noskill.daemon"}},
            {"check": {"http": "http://x"}},  # no name: ignored
        ],
    )
    services = {s.name: s for s in monitor.services_for(cfg)}
    assert list(services) == ["ollama", "sched", "noskill"]
    assert services["ollama"].heal == {"command": ["true"]}
    assert services["ollama"].check == {"http": "http://localhost:11434/api/version"}
    assert services["noskill"].label == "noskill"
    assert services["noskill"].heal is None


def test_launchd_state_parses_top_level_state_and_pid():
    fake = FakeLaunchctl({"com.local.sched": RUNNING, "local.agent-monitor.heal": NOT_RUNNING})
    assert monitor.launchd_state("com.local.sched", fake) == (True, 822, "running")
    assert monitor.launchd_state("local.agent-monitor.heal", fake) == (True, None, "not running")
    assert monitor.launchd_state("missing", fake) == (False, None, "not loaded")


def test_check_service_launchd(tmp_path):
    svc = monitor.Service("sched", "sched daemon", {"launchd": "com.local.sched"}, {"launchd": "com.local.sched"})
    up = monitor.check_service(svc, FakeLaunchctl({"com.local.sched": RUNNING}))
    assert (up.ok, up.detail, up.can_heal) == (True, "pid 822", True)
    down = monitor.check_service(svc, FakeLaunchctl({}))
    assert (down.ok, down.detail) == (False, "com.local.sched not loaded")


def test_http_check(http_server):
    assert monitor.http_up(f"{http_server}/200")[0] is True
    assert monitor.http_up(f"{http_server}/404")[0] is True  # it answered
    ok, detail = monitor.http_up(f"{http_server}/503")
    assert not ok and "HTTP 503" in detail
    ok, detail = monitor.http_up(closed_port_url())
    assert not ok and detail.startswith("no answer")


def test_heal_launchd_kickstarts_a_loaded_job(tmp_path):
    fake = FakeLaunchctl({"com.local.sched": NOT_RUNNING})
    svc = monitor.Service("sched", "s", {}, {"launchd": "com.local.sched"})
    ok, action = monitor.heal_service(svc, fake, launch_agents=tmp_path)
    assert ok
    assert fake.actions()[0][:3] == ["launchctl", "kickstart", "-k"]
    assert fake.actions()[0][3].endswith("/com.local.sched")


def test_heal_launchd_bootstraps_an_unloaded_job(tmp_path):
    (tmp_path / "com.local.sched.plist").write_text("<plist/>")
    fake = FakeLaunchctl({})
    svc = monitor.Service("sched", "s", {}, {"launchd": "com.local.sched"})
    ok, _ = monitor.heal_service(svc, fake, launch_agents=tmp_path)
    assert ok
    assert fake.actions()[0][:2] == ["launchctl", "bootstrap"]
    assert fake.actions()[0][3] == str(tmp_path / "com.local.sched.plist")


def test_heal_launchd_without_plist_fails_without_running_anything(tmp_path):
    fake = FakeLaunchctl({})
    svc = monitor.Service("sched", "s", {}, {"launchd": "com.local.sched"})
    ok, why = monitor.heal_service(svc, fake, launch_agents=tmp_path)
    assert not ok and "doesn't exist" in why
    assert fake.actions() == []


def test_heal_command_failure_is_reported():
    def runner(argv):
        return subprocess.CompletedProcess(argv, 1, "", "Unable to find application named 'Ollama'")

    svc = monitor.Service("ollama", "Ollama", {}, {"command": ["open", "-a", "Ollama"]})
    ok, why = monitor.heal_service(svc, runner)
    assert not ok and "exited 1" in why and "Unable to find" in why


def test_heal_all_leaves_a_service_that_comes_up_during_the_grace_period(tmp_path):
    fake = FakeLaunchctl({})
    sleeps = []

    def sleep(seconds):
        sleeps.append(seconds)
        fake.jobs["com.local.sched"] = RUNNING  # finished starting

    cfg = make_cfg(tmp_path, [{"name": "ollama", "enabled": False}, {"name": "monitor", "enabled": False}])
    summary = monitor.heal_all(cfg, runner=fake, sleep=sleep, launch_agents=tmp_path)
    assert sleeps == [15]
    assert fake.actions() == []
    assert summary["all_ok"] and summary["results"][0]["ok"]
    assert not (tmp_path / "data" / "monitor" / "heal.log").exists()


def test_heal_all_restarts_what_stays_down_and_logs_it(tmp_path):
    (tmp_path / "com.local.sched.plist").write_text("<plist/>")
    fake = FakeLaunchctl({})

    def runner(argv):
        result = fake(argv)
        if argv[:2] == ["launchctl", "bootstrap"]:
            fake.jobs["com.local.sched"] = RUNNING
        return result

    cfg = make_cfg(tmp_path, [{"name": "ollama", "enabled": False}, {"name": "monitor", "enabled": False}])
    summary = monitor.heal_all(cfg, runner=runner, sleep=lambda s: None, launch_agents=tmp_path)
    result = summary["results"][0]
    assert summary["all_ok"]
    assert (result["ok"], result["ok_after"], result["detail_after"]) == (False, True, "pid 822")
    assert result["action"].startswith("launchctl bootstrap")
    log = (tmp_path / "data" / "monitor" / "heal.log").read_text()
    assert "sched: down (com.local.sched not loaded) -> launchctl bootstrap" in log
    saved = json.loads((tmp_path / "data" / "monitor" / "last-heal.json").read_text())
    assert saved == summary


def test_heal_all_reports_a_failed_heal(tmp_path):
    cfg = make_cfg(
        tmp_path,
        [
            {"name": "sched", "enabled": False},
            {"name": "monitor", "enabled": False},
            {"name": "ollama", "check": {"http": closed_port_url()}, "heal": {"command": ["false"]}},
        ],
        heal_grace_seconds=0,
    )

    def runner(argv):
        return subprocess.CompletedProcess(argv, 1, "", "nope")

    summary = monitor.heal_all(cfg, runner=runner, sleep=lambda s: None)
    assert not summary["all_ok"]
    assert summary["results"][0]["action"] == "heal failed: false exited 1: nope"


def test_append_log_trims_when_large(tmp_path, monkeypatch):
    monkeypatch.setattr(monitor, "LOG_MAX_BYTES", 200)
    monkeypatch.setattr(monitor, "LOG_KEEP_LINES", 3)
    log = tmp_path / "x.log"
    for i in range(20):
        monitor.append_log(log, f"line {i}")
    lines = log.read_text().splitlines()
    assert len(lines) <= 6 and lines[-1].endswith("line 19")


@pytest.fixture
def status_server(tmp_path, http_server):
    cfg = make_cfg(
        tmp_path,
        [
            {"name": "sched", "enabled": False},
            {"name": "monitor", "enabled": False},
            {"name": "ollama", "check": {"http": f"{http_server}/200"}, "heal": {"command": ["true"]}},
        ],
    )
    calls = []

    def runner(argv):
        calls.append(argv)
        return subprocess.CompletedProcess(argv, 0, "", "")

    mon = monitor.Monitor(lambda: cfg, runner=runner, sleep=lambda s: None)
    server = monitor.make_server(mon, "127.0.0.1", 0)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{server.server_address[1]}", calls, tmp_path
    server.shutdown()


def _get(url):
    with urllib.request.urlopen(url, timeout=5) as resp:
        return resp.status, resp.read().decode()


def _post(url, headers=None):
    req = urllib.request.Request(url, method="POST", headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read())


def test_status_page_and_api(status_server):
    base, _, _ = status_server
    status, html = _get(base + "/")
    assert status == 200 and "Agent monitor" in html
    assert json.loads(_get(base + "/api/health")[1]) == {"ok": True}
    snap = json.loads(_get(base + "/api/status?refresh=1")[1])
    assert [s["name"] for s in snap["services"]] == ["ollama"]
    assert snap["services"][0]["ok"] is True
    assert snap["last_heal"] is None


def test_heal_endpoint_needs_header(status_server):
    base, calls, tmp_path = status_server
    status, body = _post(base + "/api/services/ollama/heal")
    assert status == 403 and calls == []

    status, body = _post(base + "/api/services/ollama/heal", {"X-Sched-Monitor": "1"})
    assert status == 200 and body["ok"] is True and body["action"] == "true"
    assert calls == [["true"]]
    assert "healed from the status page" in (tmp_path / "data" / "monitor" / "heal.log").read_text()

    status, _ = _post(base + "/api/services/nope/heal", {"X-Sched-Monitor": "1"})
    assert status == 404


def test_refresh_logs_status_changes(tmp_path, http_server):
    url = {"value": f"{http_server}/200"}

    def cfg():
        return make_cfg(
            tmp_path,
            [
                {"name": "sched", "enabled": False},
                {"name": "monitor", "enabled": False},
                {"name": "ollama", "check": {"http": url["value"]}},
            ],
        )

    mon = monitor.Monitor(cfg)
    mon.refresh()
    url["value"] = f"{http_server}/500"
    mon.refresh()
    mon.refresh()
    events = (tmp_path / "data" / "monitor" / "events.log").read_text().splitlines()
    assert len(events) == 1 and "ollama: down (HTTP 500" in events[0]
