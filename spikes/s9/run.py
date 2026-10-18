"""S9 (macOS part): sandbox feasibility. Attacks, legitimate work and cost, each through cmd/sbx.

  python3 spikes/s9/run.py [--parts attacks,legit,cost,copyin] [--out spikes/results/s9-macos.jsonl]

Modes: "none" = the same launcher without the OS sandbox (proves each attack is real), "sandbox" = SBPL
profile without fork, "sandbox+fork" = the same plus process-fork (for scripts that need subprocesses).
The egress proxy (cmd/egress) runs on 127.0.0.1:18080 with the allowlist below and is stopped by PID.
Fork-bomb and 4 GB checks only run sandboxed (the launcher's watchdog and rlimits apply in both modes).
"""
import argparse
import json
import os
import shutil
import signal
import statistics
import subprocess
import sys
import time
from pathlib import Path

S9 = Path(__file__).resolve().parent
ROOT = S9.parent
OUT = S9 / "out"
BIN = OUT / "bin"
RES = ROOT / "results"
PROGRESS = RES / "s9-progress.json"
HOME = Path.home()
PY = HOME / ".local/share/uv/python/cpython-3.12.13-macos-aarch64-none"
PROXY = "127.0.0.1:18080"
TOKEN = os.urandom(12).hex()  # per-run proxy credential (one for the whole spike here)
ALLOW = "pypi.org,files.pythonhosted.org,localtest.me"
ALLOWED_URL = "https://pypi.org/simple/requests/"

ATTACKS = ["read_ssh", "read_keychain", "read_canary", "list_home", "read_symlink", "read_dotdot", "write_outside",
           "raw_socket", "icmp_ping", "tcp_direct_ip", "tcp_localhost_ollama", "dns_resolve", "udp_dns",
           "proxy_denied_domain", "proxy_rebind_to_local", "proxy_ip_literal", "proxy_no_credential", "ssh_agent", "signal_outside",
           "exec_shell", "exec_curl_noproxy", "big_write", "daemonize", "fork_bomb", "memory"]
NEEDS_FORK = {"exec_shell", "exec_curl_noproxy", "daemonize", "fork_bomb"}
SANDBOX_ONLY = {"fork_bomb", "memory"}

sys.path.insert(0, str(S9))
from deps import closure  # noqa: E402

NODE = Path(os.path.realpath("/opt/homebrew/bin/node"))
NODE_RO = closure(str(NODE)) + ["/opt/homebrew/etc/openssl@4", "/opt/homebrew/etc/ca-certificates"]  # Homebrew node reads its CA file there
UV = Path(os.path.realpath("/opt/homebrew/bin/uv"))
UV_RO = closure(str(UV))


def progress(**kw):
    cur = json.loads(PROGRESS.read_text()) if PROGRESS.exists() else {}
    cur.update(kw)
    PROGRESS.write_text(json.dumps(cur))


def fresh_run(name):
    d = OUT / "runs" / name
    if d.exists():  # our own scratch run from a previous attempt: move aside, never delete
        (OUT / "runs-old").mkdir(parents=True, exist_ok=True)
        d.rename(OUT / "runs-old" / f"{name}-{time.time_ns()}")
    (d / "work" / "in").mkdir(parents=True)
    for f in ("attack.py", "legit.py", "legit.mjs"):
        shutil.copy(S9 / f, d / "work" / f)
    return d


def sbx(run, mode, cmd, ro=(), fork=False, execs=(), env=None, timeout="30s", mem_mb=512, nproc=64, proxy=True):
    sbmode = "none" if mode == "none" else "sandbox"
    a = [str(BIN / "sbx"), "-run", str(run), "-mode", sbmode, "-ro", ",".join(map(str, ro)), "-timeout", timeout,
         "-mem-mb", str(mem_mb), "-nproc", str(nproc)]
    if proxy:
        a += ["-proxy", PROXY, "-proxy-cred", "run:" + TOKEN]
    if fork or mode == "sandbox+fork":
        a += ["-fork"]
    if execs:
        a += ["-exec", ",".join(execs)]
    if env:
        for k, v in env.items():
            a += ["-env", f"{k}={v}"]
    # stdout/stderr go to files, not pipes: a detached grandchild holding a pipe would block us
    so, se = Path(run) / "stdout.txt", Path(run) / "stderr.txt"
    t0 = time.perf_counter()
    with open(so, "w") as fo, open(se, "w") as fe:
        subprocess.run(a + ["--"] + [str(c) for c in cmd], stdout=fo, stderr=fe, timeout=180)
    wall = (time.perf_counter() - t0) * 1000
    out, err = so.read_text(errors="replace"), se.read_text(errors="replace")
    res = {}
    for line in err.splitlines():
        if line.startswith("SBX_RESULT "):
            res = json.loads(line[11:])
    lines = [json.loads(l) for l in out.splitlines() if l.startswith("{")]
    return (lines[-1] if lines else None), res, wall, err[-400:]


def targets():
    canary = OUT / "canary" / "secret.txt"
    canary.parent.mkdir(parents=True, exist_ok=True)
    canary.write_text("S9-CANARY-not-a-real-secret\n")
    (OUT / "outside").mkdir(exist_ok=True)
    return {"ssh": str(HOME / ".ssh/id_ed25519"), "keychain": str(HOME / "Library/Keychains/login.keychain-db"),
            "canary": str(canary), "home": str(HOME), "outside_write": str(OUT / "outside" / "escape.txt"),
            "denied_host": "example.org", "rebind_host": "localtest.me", "ssh_agent": os.environ.get("SSH_AUTH_SOCK", "")}


def write(f, rec):
    f.write(json.dumps(rec) + "\n")
    f.flush()
    print(json.dumps(rec)[:200], flush=True)


def attacks(f):
    T = json.dumps(targets())
    plan = [(c, m) for c in ATTACKS for m in ("none", "sandbox", "sandbox+fork")
            if not (m == "none" and c in SANDBOX_ONLY) and not (m == "sandbox+fork" and c not in NEEDS_FORK)]
    progress(part="attacks", part_total=len(plan), part_done=0)
    for i, (check, mode) in enumerate(plan):
        run = fresh_run(f"atk-{mode}-{check}")
        r, res, wall, err = sbx(run, mode, [PY / "bin/python3", "attack.py", check], ro=[PY], env={"S9_TARGETS": T},
                                timeout="20s", mem_mb=512, nproc=64)
        blocked = r["blocked"] if r else None
        detail = r["detail"] if r else (err.strip().splitlines() or [""])[-1]
        if res.get("killed") in ("memory", "procs", "timeout"):
            blocked, detail = True, f"killed by watchdog ({res['killed']}); peak {res.get('peak_rss_mb')} MB, {res.get('peak_procs')} procs. " + detail
        if check == "daemonize":
            pidf = run / "work/out/daemon.pid"
            if pidf.exists():
                pid = int(pidf.read_text())
                alive = subprocess.run(["ps", "-p", str(pid)], capture_output=True).returncode == 0
                blocked = not alive
                detail = f"grandchild {pid} {'outlived the run' if alive else 'was gone after the run'}"
                if alive:
                    time.sleep(2)
                    log = run / "work/out/daemon.log"
                    if log.exists():
                        detail += "; after the run it got " + log.read_text()[:220]
                    try:
                        os.kill(pid, signal.SIGKILL)  # our own test process, by PID
                    except ProcessLookupError:
                        detail += " (exited by itself before the kill)"
            else:
                blocked, detail = True, "could not start a detached process. " + detail
        if check == "write_outside" and Path(json.loads(T)["outside_write"]).exists():
            p = Path(json.loads(T)["outside_write"])
            p.rename(p.with_name(f"escape-{mode}-{int(time.time())}.txt"))  # keep as evidence; next run starts clean
        write(f, {"part": "attacks", "check": check, "mode": mode, "blocked": blocked, "detail": detail,
                  "exit": res.get("exit"), "signal": res.get("signal"), "killed": res.get("killed"), "wall_ms": round(wall, 1)})
        progress(part_done=i + 1)


def legit(f):
    steps = []
    data = "category,amount\n" + "".join(f"{c},{a}\n" for c, a in zip(
        ["food", "rent", "food", "travel", "fun", "food", "travel", "fun", "rent", "food"] * 2, range(10, 30)))

    def csv_run(mode):
        run = fresh_run(f"legit-{mode}-csv")
        (run / "work/in/data.csv").write_text(data)
        return sbx(run, mode, [PY / "bin/python3", "legit.py", "csv"], ro=[PY], proxy=False)

    def py_net(mode, task):
        run = fresh_run(f"legit-{mode}-{task}")
        return sbx(run, mode, [PY / "bin/python3", "legit.py", task], ro=[PY], env={"S9_ALLOWED_URL": ALLOWED_URL})

    def node(mode, env_proxy):
        run = fresh_run(f"legit-{mode}-node-{'env' if env_proxy else 'direct'}")
        env = {"NODE_USE_ENV_PROXY": "1"} if env_proxy else {}
        return sbx(run, mode, [NODE, "legit.mjs", ALLOWED_URL], ro=NODE_RO, env=env)

    def curl(mode, noproxy):
        run = fresh_run(f"legit-{mode}-curl{'-noproxy' if noproxy else ''}")
        url = "https://example.org/" if noproxy else ALLOWED_URL
        cmd = ["/usr/bin/curl", "-sS", "-m", "10", "-o", "/dev/null", "-w", "%{http_code}"] + (["--noproxy", "*"] if noproxy else []) + [url]
        a = [str(BIN / "sbx"), "-run", str(run), "-mode", "none" if mode == "none" else "sandbox", "-proxy", PROXY, "-proxy-cred", "run:" + TOKEN,
             "-exec", "/usr/bin/curl", "-timeout", "20s", "--"] + cmd
        t0 = time.perf_counter()
        p = subprocess.run(a, capture_output=True, text=True)
        code = p.stdout.strip()
        ok = code.startswith("2")
        task = "curl_noproxy_example_org" if noproxy else "curl_proxy"
        return {"task": task, "ok": ok, "detail": f"code={code} {p.stderr.splitlines()[0] if p.stderr else ''}"[:200]}, {}, (time.perf_counter() - t0) * 1000, ""

    def uv_install(mode):
        run = fresh_run(f"legit-{mode}-uv")
        w = run / "work"
        env = {"UV_CACHE_DIR": w / "uvcache", "UV_PYTHON_DOWNLOADS": "never", "UV_NO_CONFIG": "1"}
        r1 = sbx(run, mode, [UV, "venv", w / "venv", "--python", PY / "bin/python3"], ro=UV_RO + [str(PY)], fork=True, env=env, timeout="60s", mem_mb=1024)
        r2 = sbx(run, mode, [UV, "pip", "install", "--python", w / "venv/bin/python", "requests", "httpx"],
                 ro=UV_RO + [str(PY)], fork=True, env=env, timeout="120s", mem_mb=1024)
        ok = r2[1].get("exit") == 0 and (w / "venv").exists()
        tail = (r2[3] or r1[3]).strip().splitlines()
        rec = {"task": "uv_pip_install", "ok": ok, "detail": f"venv exit {r1[1].get('exit')}, install exit {r2[1].get('exit')}: {(tail or [''])[-1][:160]}"}
        out = [(rec, r2[1], r1[2] + r2[2])]
        for task in ("requests", "httpx"):
            r = sbx(run, mode, [w / "venv/bin/python", "legit.py", task], ro=[PY], env={"S9_ALLOWED_URL": ALLOWED_URL})
            out.append(r[:3])
        return out

    for mode in ("none", "sandbox"):
        steps += [("csv", lambda m=mode: [csv_run(m)[:3]], mode), ("urllib", lambda m=mode: [py_net(m, "urllib")[:3]], mode),
                  ("uv+requests+httpx", lambda m=mode: uv_install(m), mode),
                  ("node direct", lambda m=mode: [node(m, False)[:3]], mode), ("node env proxy", lambda m=mode: [node(m, True)[:3]], mode),
                  ("curl proxy", lambda m=mode: [curl(m, False)[:3]], mode), ("curl --noproxy", lambda m=mode: [curl(m, True)[:3]], mode)]
    progress(part="legit", part_total=len(steps), part_done=0)
    for i, (name, fn, mode) in enumerate(steps):
        for r, res, wall in fn():
            r = r or {"task": name, "ok": False, "detail": "no output"}
            write(f, {"part": "legit", "task": r["task"], "mode": mode, "ok": r["ok"], "detail": r["detail"],
                      "exit": res.get("exit") if res else None, "wall_ms": round(wall, 1)})
        progress(part_done=i + 1)


def cost(f, n=30):
    cases = [("python -c pass", [PY / "bin/python3", "-c", "pass"], [PY]), ("node -e 0", [NODE, "-e", "0"], NODE_RO)]
    progress(part="cost", part_total=len(cases) * 3, part_done=0)
    k = 0
    for label, cmd, ro in cases:
        run = fresh_run("cost-" + label.split()[0])
        for mode in ("direct", "none", "sandbox"):
            ts = []
            for _ in range(n + 2):
                t0 = time.perf_counter()
                if mode == "direct":
                    subprocess.run([str(c) for c in cmd], capture_output=True, cwd=run / "work")
                else:
                    sbx(run, mode, cmd, ro=ro, proxy=False)
                ts.append((time.perf_counter() - t0) * 1000)
            ts = sorted(ts[2:])  # first two are warm-up
            write(f, {"part": "cost", "case": label, "mode": mode, "n": n, "p50_ms": round(statistics.median(ts), 1),
                      "p90_ms": round(ts[int(0.9 * n) - 1], 1), "min_ms": round(ts[0], 1)})
            k += 1
            progress(part_done=k)


def copyin(f):
    src = OUT / "copyin-src"
    if not src.exists():
        (src / "many").mkdir(parents=True)
        for i in range(100):
            (src / "many" / f"f{i:03d}.bin").write_bytes(os.urandom(1 << 20))
        (src / "one").mkdir()
        (src / "one" / "big.bin").write_bytes(os.urandom(100 << 20))
    progress(part="copyin", part_total=4, part_done=0)
    k = 0
    for shape in ("many", "one"):
        for how in ("copy", "clone"):
            ts = []
            for i in range(5):
                dst = OUT / "copyin-dst" / f"{shape}-{how}-{time.time_ns()}"
                t0 = time.perf_counter()
                subprocess.run([str(BIN / "copyin"), "-mode", how, str(src / shape), str(dst)], check=True)
                ts.append((time.perf_counter() - t0) * 1000)
            write(f, {"part": "copyin", "shape": "100 x 1 MB" if shape == "many" else "1 x 100 MB", "how": how,
                      "p50_ms": round(statistics.median(ts), 1), "max_ms": round(max(ts), 1)})
            k += 1
            progress(part_done=k)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--parts", default="attacks,legit,cost,copyin")
    ap.add_argument("--out", default=str(RES / "s9-macos.jsonl"))
    a = ap.parse_args()
    subprocess.run(["go", "build", "-o", str(BIN) + "/", "./cmd/..."], cwd=S9, check=True)
    PROGRESS.write_text(json.dumps({"parts": a.parts.split(","), "started": time.time()}))
    eg = subprocess.Popen([str(BIN / "egress"), "-listen", PROXY, "-allow", ALLOW, "-token", TOKEN, "-log", str(OUT / "egress.jsonl")],
                          stderr=subprocess.DEVNULL)
    time.sleep(0.5)
    try:
        with open(a.out, "a") as f:
            for part in a.parts.split(","):
                globals()[part](f)
    finally:
        os.kill(eg.pid, signal.SIGTERM)  # by PID
        progress(part="done")


if __name__ == "__main__":
    main()
