"""S9 attack script: one check per run, printed as one JSON line {"check", "blocked", "detail"}.

  python attack.py <check>        (targets come from S9_TARGETS, a JSON object set by run.py)

Never reads the contents of real secrets: for ~/.ssh and the keychain it only tries to open the file.
"""
import json
import os
import socket
import struct
import subprocess
import sys
import time

T = json.loads(os.environ.get("S9_TARGETS", "{}"))
WORK = os.environ.get("NOSKILL_WORK", os.getcwd())


def out(check, blocked, detail=""):
    print(json.dumps({"check": check, "blocked": bool(blocked), "detail": str(detail)[:300]}), flush=True)


def try_open(path):
    try:
        with open(path, "rb"):
            pass
        return False, "opened"
    except OSError as e:
        return True, f"{type(e).__name__}: {e.strerror}"


def read_ssh():
    return try_open(T["ssh"])


def read_keychain():
    return try_open(T["keychain"])


def read_canary():
    try:
        with open(T["canary"]) as f:
            return False, f"read {len(f.read())} bytes"
    except OSError as e:
        return True, f"{type(e).__name__}: {e.strerror}"


def list_home():
    try:
        return False, f"{len(os.listdir(T['home']))} entries"
    except OSError as e:
        return True, f"{type(e).__name__}: {e.strerror}"


def read_symlink():
    link = os.path.join(WORK, "out", "link-to-canary")
    try:
        if not os.path.lexists(link):
            os.symlink(T["canary"], link)
    except OSError as e:
        return True, f"symlink: {e.strerror}"
    return try_open(link)


def read_dotdot():
    rel = os.path.relpath(T["canary"], WORK)  # ../../canary/secret.txt
    return try_open(os.path.join(WORK, rel))


def write_outside():
    try:
        with open(T["outside_write"], "w") as f:
            f.write("escaped\n")
        return False, "wrote " + T["outside_write"]
    except OSError as e:
        return True, f"{type(e).__name__}: {e.strerror}"


def raw_socket():
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP)
        s.close()
        return False, "raw socket opened"
    except OSError as e:
        return True, f"{type(e).__name__}: {e.strerror}"


def icmp_ping():
    # Unprivileged ICMP (SOCK_DGRAM) is allowed for normal users on macOS; the sandbox must stop it.
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM, socket.IPPROTO_ICMP)
        s.settimeout(2)
        pkt = struct.pack("!BBHHH", 8, 0, 0, 0x5339, 1) + b"noskill-s9"
        s.sendto(pkt, ("1.1.1.1", 0))  # sending is enough to leak data; a reply is a bonus
        try:
            s.recvfrom(1024)
            return False, "sent; echo reply from 1.1.1.1"
        except OSError:
            return False, "sent echo request (no reply)"
    except OSError as e:
        return True, f"{type(e).__name__}: {e}"


def tcp(host, port):
    try:
        c = socket.create_connection((host, port), timeout=3)
        c.close()
        return False, f"connected {host}:{port}"
    except OSError as e:
        return True, f"{type(e).__name__}: {e}"


def tcp_direct_ip():
    return tcp("1.1.1.1", 443)


def tcp_localhost_ollama():
    return tcp("127.0.0.1", 11434)


def dns_resolve():
    try:
        ips = {a[4][0] for a in socket.getaddrinfo(T["denied_host"], 443)}
        return False, f"resolved {sorted(ips)[:2]}"
    except OSError as e:
        return True, f"{type(e).__name__}: {e}"


def udp_dns():
    q = struct.pack("!HHHHHH", 0x5339, 0x0100, 1, 0, 0, 0) + b"".join(
        bytes([len(p)]) + p.encode() for p in T["denied_host"].split(".")) + b"\x00\x00\x01\x00\x01"
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.settimeout(2)
        s.sendto(q, ("8.8.8.8", 53))
        s.recvfrom(512)
        return False, "DNS answer from 8.8.8.8"
    except OSError as e:
        return True, f"{type(e).__name__}: {e}"


def _via_proxy(url):
    import urllib.request
    try:
        with urllib.request.urlopen(url, timeout=10) as r:
            return False, f"HTTP {r.status}"
    except Exception as e:  # urllib raises URLError/HTTPError/OSError
        return True, f"{type(e).__name__}: {e}"


def proxy_denied_domain():
    return _via_proxy(f"https://{T['denied_host']}/")


def proxy_rebind_to_local():
    # An allowlisted name that resolves to 127.0.0.1 must not reach local services through the proxy.
    return _via_proxy(f"https://{T['rebind_host']}:443/")


def proxy_ip_literal():
    return _via_proxy("https://1.1.1.1/")


def proxy_no_credential():
    # Talk to the proxy without the run's credential (what a leftover process sees once it's revoked).
    import urllib.request
    h, _, hp = os.environ.get("HTTPS_PROXY", "http://127.0.0.1:18080").rpartition("@")
    hp = hp.replace("http://", "")
    op = urllib.request.build_opener(urllib.request.ProxyHandler({"https": "http://" + hp}))
    try:
        with op.open(os.environ.get("S9_ALLOWED_URL", "https://pypi.org/simple/requests/"), timeout=10) as r:
            return False, f"HTTP {r.status} without a credential"
    except Exception as e:
        return True, f"{type(e).__name__}: {e}"


def ssh_agent():
    p = T.get("ssh_agent")
    if not p:
        return None, "no SSH_AUTH_SOCK on this machine"
    try:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(p)
        s.close()
        return False, "connected to the ssh-agent socket"
    except OSError as e:
        return True, f"{type(e).__name__}: {e.strerror}"


def exec_shell():
    try:
        r = subprocess.run(["/bin/sh", "-c", "echo pwned"], capture_output=True, text=True, timeout=5)
        return (r.stdout.strip() != "pwned"), f"rc={r.returncode} out={r.stdout.strip()!r}"
    except OSError as e:
        return True, f"{type(e).__name__}: {e.strerror}"


def exec_curl_noproxy():
    try:
        r = subprocess.run(["/usr/bin/curl", "-sS", "--noproxy", "*", "-m", "5", "-o", "/dev/null", "-w", "%{http_code}",
                            f"https://{T['denied_host']}/"], capture_output=True, text=True, timeout=10)
        return not r.stdout.strip().startswith(("2", "3")), f"rc={r.returncode} code={r.stdout.strip()} {r.stderr.strip()[:120]}"
    except OSError as e:
        return True, f"{type(e).__name__}: {e.strerror}"


def signal_outside():
    try:
        os.kill(os.getppid(), 0)  # signal 0: permission check only, harmless
        return False, "may signal the parent (outside the sandbox)"
    except OSError as e:
        return True, f"{type(e).__name__}: {e.strerror}"


def fork_bomb():
    n, kids = 0, []
    for _ in range(1000):
        try:
            pid = os.fork()
        except OSError as e:
            err = e.strerror
            break
        if pid == 0:
            time.sleep(3)
            os._exit(0)
        kids.append(pid)
        n += 1
    else:
        err = ""
    for k in kids:
        try:
            os.waitpid(k, 0)
        except OSError:
            pass
    return n < 1000, f"forked {n} ({err})"


def memory():
    keep, mb = [], 0
    try:
        while mb < 4096:
            keep.append(b"\x01" * (128 << 20))  # written, so the pages are resident
            mb += 128
            print(json.dumps({"progress_mb": mb}), file=sys.stderr, flush=True)
        return False, f"allocated {mb} MB"
    except MemoryError:
        return True, f"MemoryError at {mb} MB"


def big_write():
    p = os.path.join(WORK, "out", "big.bin")
    chunk, mb = b"\x00" * (16 << 20), 0
    try:
        with open(p, "wb") as f:
            while mb < 1024:
                f.write(chunk)
                mb += 16
        return False, f"wrote {mb} MB"
    except OSError as e:
        return True, f"{type(e).__name__} at {mb} MB: {e.strerror}"


def daemonize():
    # Double fork + setsid: the grandchild leaves the process group and tries to outlive the run.
    pid = os.fork()
    if pid == 0:
        os.setsid()
        if os.fork() == 0:
            with open(os.path.join(WORK, "out", "daemon.pid"), "w") as f:
                f.write(str(os.getpid()))
            time.sleep(1.5)  # by now the run has ended; is the survivor still confined?
            with open(os.path.join(WORK, "out", "daemon.log"), "w") as f:
                f.write(json.dumps({"after_run": {"read_ssh": try_open(T["ssh"]), "tcp_direct_ip": tcp("1.1.1.1", 443),
                                                  "proxy_port": tcp(*os.environ.get("HTTPS_PROXY", "http://127.0.0.1:18080").rpartition("@")[2].replace("http://", "").split(":"))}}))
            time.sleep(20)
            os._exit(0)
        os._exit(0)
    os.waitpid(pid, 0)
    time.sleep(0.3)
    return None, "spawned; run.py checks whether it outlived the run"


CHECKS = {k: v for k, v in globals().items() if callable(v) and not k.startswith("_") and k not in
          ("out", "try_open", "tcp", "json", "os", "socket", "struct", "subprocess", "sys", "time")}

if __name__ == "__main__":
    name = sys.argv[1]
    try:
        blocked, detail = CHECKS[name]()
    except Exception as e:  # anything unexpected counts as blocked only if it's a permission-style error
        blocked, detail = isinstance(e, PermissionError), f"{type(e).__name__}: {e}"
    out(name, blocked, detail)
