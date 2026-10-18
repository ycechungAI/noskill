# S9 results: Sandbox feasibility (macOS part)
2026-10-15 · Mac mini M4, 16 GB, macOS 27.0.1, normal user (no admin rights used) · harness [spikes/s9](../../spikes/s9/): `cmd/sbx` (launcher), `cmd/egress` (proxy), `cmd/copyin`, `attack.py`, `legit.py`, `legit.mjs`, `run.py` · uv-managed CPython 3.12.13, Homebrew Node 24.21, uv 0.13.0, `/usr/bin/curl` · raw rows: `spikes/results/s9-macos.jsonl`

**Deviations from the plan:**
- **Only macOS was run.** Linux (L1, the 22.04/5.15 VM and the Docker container) and Windows (W1) were not run. A Colima VM for Linux was set up, but the Linux launcher backend wasn't finished in this session, and the VM was stopped without running anything. Linux and Windows stay open (see "Still open").
- **Every attack also ran without the OS sandbox** (`none`: the same launcher, rlimits and watchdog, but no `sandbox-exec`) to prove that each attack really works on this machine. The fork-bomb and 4 GB checks only ran sandboxed.
- **Secrets were never read.** For `~/.ssh/id_ed25519` and the login keychain the script only tries to open the file. A canary file stands in for "a secret it can read".
- **Two harness bugs were fixed before the final run:** the detached-process check misparsed the proxy URL once it carried a credential, and Homebrew Node's CA file wasn't on the sandbox's read list. Earlier attempts are kept as `s9-macos-attempt1/2.jsonl`.

## How the macOS sandbox is built
The launcher writes a generated SBPL profile next to `work/` (outside the script's reach) and runs `sandbox-exec -f profile.sb <runtime> <script>`:
- `(deny default)`. Reads are allowed only for system libraries (`/usr/lib`, `/System`, the dyld cache and its Cryptex at `/private/preboot/Cryptexes`), the runtime's own folders, a few `/etc` files, `/dev/null|zero|random|urandom`, the literal `/` and `work/`. `file-map-executable` is granted on the same system and runtime paths.
- **Writes** are allowed only under `work/` (and `/dev/null`, `/dev/tty`).
- **Exec** is allowed only for the runtime folders plus executables the skill declares.
- **Fork** is off by default (`process-fork` not allowed). It's added only for scripts that need subprocesses, such as `uv`.
- **Network** is outbound only to the egress proxy's loopback port. There is no DNS: the proxy resolves names.
- **Signals and process info** only reach the same sandbox.
- `file-read-metadata` is allowed everywhere because path lookups need it. A script can learn that `~/.ssh/id_ed25519` exists and how big it is, not what's in it.
- **Environment:** the script gets a clean environment: `HOME`, `TMPDIR` and cwd inside `work/`, `HTTPS_PROXY` with a per-run credential, nothing else.
- **Limits:**
  - rlimits: `RLIMIT_NPROC` = the user's current process count + 64, `RLIMIT_FSIZE` 256 MB, `RLIMIT_CPU` = the timeout, no core dumps.
  - A watchdog polls the process group's RSS and process count every 50 ms and kills the group at the limit or the timeout.

## Numbers

### Attacks (24 checks)
| Attack | No sandbox | Sandbox | Sandbox + fork | How it was stopped |
|---|---|---|---|---|
| Open `~/.ssh/id_ed25519` | open | **blocked** | | SBPL file read |
| Open the login keychain | open | **blocked** | | SBPL file read |
| Read a canary file outside `work/` | read | **blocked** | | SBPL file read |
| List the home folder | listed | **blocked** | | SBPL file read |
| Symlink in `work/` → canary | open | **blocked** | | SBPL checks the resolved path |
| `work/../../canary` | open | **blocked** | | same |
| Write outside `work/` | wrote | **blocked** | | SBPL file write |
| Raw socket | blocked (needs root) | **blocked** | | OS + SBPL |
| ICMP echo (unprivileged `SOCK_DGRAM`) | sent | **blocked** | | SBPL network |
| TCP to 1.1.1.1:443, ignoring proxy variables | connected | **blocked** | | SBPL network |
| TCP to Ollama on 127.0.0.1:11434 | connected | **blocked** | | SBPL: only the proxy port |
| DNS lookup of a non-allowlisted name | resolved | **blocked** | | no mDNSResponder access |
| UDP DNS query straight to 8.8.8.8 | answered | **blocked** | | SBPL network |
| Proxy: non-allowlisted domain | 403 | **403** | | egress allowlist |
| Proxy: allowlisted name that resolves to 127.0.0.1 (`localtest.me`) | refused | **refused** | | proxy rejects local/private addresses |
| Proxy: IP literal | 403 | **403** | | proxy rejects IPs |
| Proxy without the run's credential | 407 | **407** | | per-run credential |
| Connect to the ssh-agent socket | connected | **blocked** | | SBPL network (Unix socket) |
| Signal a process outside the sandbox | allowed | **blocked** | | SBPL `signal (target same-sandbox)` |
| Run `/bin/sh` | ran | **blocked** | **blocked** | no fork / exec allowlist |
| Run `curl --noproxy '*'` | ran | **blocked** | **blocked** | no fork / exec allowlist |
| Write a 1 GB file | stopped at 256 MB | **stopped at 256 MB** | | `RLIMIT_FSIZE` |
| Fork 1000 processes | | **0** forked | **68** forked | no fork / `RLIMIT_NPROC` |
| Allocate 4 GB (512 MB limit) | | **killed at 869 MB** | | watchdog (no memory rlimit on macOS) |
| Double-fork + `setsid` to outlive the run | survived | **couldn't fork** | **survived, still confined** | see below |

With fork allowed, a detached grandchild **outlived the run**, as expected: macOS has no way for a normal user to keep a process tree together. After the run it was still sandboxed. It couldn't open `~/.ssh` and couldn't connect out directly. It could still connect to the proxy port, which is why the proxy needs a per-run credential that is revoked when the run ends.

### Legitimate work
| Task | No sandbox | Sandbox | Notes |
|---|---|---|---|
| Python: 20-line CSV transform `work/in` → `work/out` | OK 69 ms | **OK** 84 ms | |
| Python `urllib` HTTPS GET via proxy (pypi.org) | OK | **OK** 319 ms | |
| `uv venv` + `uv pip install requests httpx` via proxy | OK | **OK** 1.3 s | needs fork; `UV_CACHE_DIR` inside `work/` |
| Python `requests` via proxy | OK | **OK** 666 ms | |
| Python `httpx` via proxy | OK | **OK** 443 ms | |
| Node `fetch` with `NODE_USE_ENV_PROXY=1` | OK | **OK** 328 ms | |
| `curl` via proxy | OK | **OK** 242 ms | `/usr/bin/curl` declared as an executable |
| Node `fetch` *without* the env-proxy flag (ignores `HTTPS_PROXY`) | reached pypi.org directly | **blocked** (ENOTFOUND) | correct: a client that ignores the proxy can't get out |
| `curl --noproxy '*'` to example.org | reached it | **blocked** (no DNS) | correct |

Egress log over the run: 36 tunnels to pypi.org / files.pythonhosted.org. Refused: 6 to example.org, 6 to the IP literal, 6 without a credential and 6 to `localtest.me` (it resolves to loopback).

### Cost (30 runs each, p50 / p90)
| | Direct | Launcher, no sandbox | Launcher + `sandbox-exec` | Total overhead |
|---|---|---|---|---|
| `python -c pass` | 24.3 / 25.4 ms | 54.0 / 56.7 ms | 65.3 / 69.5 ms | **+41 ms** |
| `node -e 0` | 63.4 / 66.6 ms | 93.2 / 97.0 ms | 108.1 / 110.5 ms | **+45 ms** |

`sandbox-exec` itself adds 11–15 ms. The other ~30 ms is the spike launcher: it re-executes itself to set rlimits and runs `ps` once to count your processes. The daemon will do both in-process.

### Copy-in of 100 MB (5 runs each, p50 / max)
| Shape | Byte copy | APFS clone (`clonefile`) |
|---|---|---|
| 100 × 1 MB | 92 / 288 ms | **32** / 33 ms |
| 1 × 100 MB | 84 / 111 ms | **13** / 36 ms |

## Pass/fail
| Criterion | Result |
|---|---|
| All attacks blocked on macOS with no admin | **Pass**, with one documented gap: with fork allowed, a detached process can outlive the run. It stays sandboxed, and its proxy credential is revoked at the end of the run |
| All attacks blocked on Linux | **Not run** |
| Windows: file isolation without admin; network on/off; domain-filtering answer | **Not run** |
| Legitimate work succeeds (Python and Node) | **Pass** on macOS (Python stdlib, requests, httpx, uv install, Node fetch, curl) |
| Start-up overhead ≤ 150 ms p50 | **Pass**: +41 ms (Python), +45 ms (Node); `sandbox-exec` itself 11–15 ms |
| Copy-in of 100 MB ≤ 1 s | **Pass**: 84–92 ms copied, 13–32 ms cloned |
| A script ignoring proxy variables can't reach a non-allowlisted domain | **Pass** on macOS (direct TCP, DNS, UDP, `curl --noproxy`, Node fetch without the flag) |

## Surprises
1. **The deny-default profile needs three non-obvious allowances:**
   - Reading the literal `/`: without it dyld aborts before `main`.
   - `file-map-executable` on system and runtime paths.
   - The dyld cache's Cryptex path, `/private/preboot/Cryptexes`.
   macOS 27 doesn't log sandbox denials for unsigned command-line processes anywhere a normal user can see, so the profile was found by bisection. `noskill doctor` needs a self-test that runs a known script under the profile.
2. **macOS refuses memory rlimits.** `RLIMIT_AS`, `RLIMIT_DATA` and `RLIMIT_RSS` all fail below the process's current virtual size, which is about 466 GB on arm64 Python. Memory can only be watched. At a 50 ms poll and 128 MB per step, the 512 MB limit was overshot to 869 MB. Tighter polling via `proc_pidinfo`, plus headroom in the memory budget, is needed.
3. **Fork can be denied outright.** Most data-transform scripts don't need subprocesses, so "no fork" by default also stops fork bombs, shell-outs and detached processes. Tools that need it (uv, build tools) declare it.
4. **DNS is denied for free.** With network limited to the proxy port, the script can't reach `mDNSResponder`, so DNS exfiltration is blocked too and the proxy does all name resolution.
5. **The proxy must refuse names that resolve to local addresses.** `localtest.me` is a public name for 127.0.0.1. An allowlist entry like that would otherwise let a script reach Ollama or noskill's own API through the proxy.
6. **Node's `fetch` ignores `HTTPS_PROXY`** unless `NODE_USE_ENV_PROXY=1` (Node 24+). noskill must set it, and the sandbox makes sure nothing leaks if a client ignores it.
7. **Homebrew's Node needs about 14 Homebrew library folders plus its OpenSSL and CA folders.** uv-managed Python is self-contained. A noskill-managed Node (the official self-contained build) will keep the read list short.
8. **APFS clones make copy-in almost free**: 13–32 ms for 100 MB.

## Decisions
- **macOS sandbox as designed ([ADR-0008](../adr/0008-macos-script-sandbox.md)):**
  - `sandbox-exec` with a generated deny-default profile.
  - Fork off unless the skill declares `subprocess`.
  - Exec limited to the runtime plus declared executables.
  - Network only to the proxy port.
  - A clean environment.
  - rlimits for processes, file size and CPU, plus a watchdog for memory.
- **The egress proxy:**
  - Gives each run its own credential, revoked when the run ends.
  - Refuses IP literals and names that resolve to loopback, private or link-local addresses.
  - Resolves names itself.
- **Copy-in** clones where the filesystem supports it (APFS, Btrfs/XFS reflink, ReFS) and copies bytes otherwise. Inputs are read-only.
- **Node runs get `NODE_USE_ENV_PROXY=1`.**
- **Linux and Windows are still open.** The Phase 0 exit needs S9 on Linux ([PHASE0_GO_NO_GO.md](PHASE0_GO_NO_GO.md)).

## Still open
- **Linux:**
  - Landlock ABI detection and file rules.
  - TCP connect rules on kernels ≥ 6.7.
  - Blocking UDP and other socket types, which Landlock doesn't cover.
  - bubblewrap with a network namespace and the proxy through a Unix socket.
  - cgroup limits via `systemd-run --user`.
  - Docker and kernel 5.15.
- **Windows:** AppContainer + Job object, uv's Python inside it, and the loopback/domain-filtering question.
- **macOS follow-ups:**
  - Polling memory with `proc_pidinfo` and measuring the overshoot again.
  - A cleanup sweep for detached processes after runs that were allowed to fork.
  - Testing the profile against a noskill-managed Node.
