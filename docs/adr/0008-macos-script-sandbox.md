# ADR-0008: macOS script sandbox and egress proxy rules

Status: **accepted for macOS**; Linux and Windows pending S9 · Date: 2026-10-15 · Evidence: [S9 results](../spikes/S9-results.md)

## Context
Script tools (Python/Node/shell, learned scripts, `code-runner`) run in an OS sandbox with a copy-in workspace and an egress proxy ([SECURITY.md](../SECURITY.md#enforcement-layers)). For macOS the plan named `sandbox-exec` with a generated profile, and `ulimit` limits. Phase 0 spike S9 measured this on macOS 27.0.1 as a normal user. The results:
- With a deny-default profile, every attack in the security suite was blocked: reading `~/.ssh` and the keychain, symlink and `..` escapes, writes outside `work/`, direct TCP, ICMP and UDP, DNS, Ollama on localhost, the ssh-agent socket, signalling outside processes, running a shell, a fork bomb and a 1 GB file. Each attack succeeded without the sandbox.
- Legitimate work still ran:
  - Python (stdlib, requests, httpx, `uv pip install`), through the proxy;
  - Node `fetch` with `NODE_USE_ENV_PROXY=1`;
  - `curl`.
- It costs +41–45 ms per call, of which `sandbox-exec` is 11–15 ms. Copy-in of 100 MB takes 84–92 ms copied and 13–32 ms cloned.
- macOS refuses memory rlimits below the current virtual size, so memory can only be watched (overshoot at 50 ms polling: 512 → 869 MB).
- A normal user can't keep a process tree together on macOS. With fork allowed, a double-forked process outlives the run. It stays sandboxed but can still reach the proxy port.
- An allowlisted name can resolve to 127.0.0.1 (`localtest.me`).

## Decision
**Profile** (generated per run, written outside `work/`):
- `(deny default)`.
- Read access:
  - system libraries;
  - the dyld cache and its Cryptex;
  - a short list of `/etc` and `/dev` files;
  - the literal `/`;
  - the runtime's folders;
  - `work/`.
- `file-map-executable` on system and runtime paths.
- `file-read-metadata` everywhere; path lookups need it.
- Writes only to `work/`.
- `process-exec` only for the runtime and the executables the skill declares.
- `process-fork` **only when the skill declares `subprocess: true`**.
- Signals and process info only within the same sandbox.
- Network outbound only to the egress proxy's loopback port: no DNS, no other localhost ports.

**Process:**
- A clean environment: `HOME`, `TMPDIR` and cwd in `work/`, the proxy variables, `NODE_USE_ENV_PROXY=1`, and the skill's declared secrets.
- rlimits:
  - `RLIMIT_NPROC` = the user's current count + `max_procs` (default 64);
  - `RLIMIT_FSIZE` (default 256 MB);
  - `RLIMIT_CPU` = the timeout;
  - no core dumps.
- Memory is enforced by a watchdog in the daemon that kills the process group. It's a soft limit, so the memory budget keeps headroom for the overshoot.
  - **As built (block 1a):** the watchdog samples `ps -g <pgid>` every **50 ms**, not `proc_pidinfo` at ≤ 20 ms. `proc_pidinfo` needs CGO, and the CLI and daemon build without it (CGO is allowed only in the tray app, ADR-0004). 50 ms is the interval S9 measured (512 MB limit → 869 MB peak). A CGO-free `proc_pidinfo` via raw syscalls can replace it if the overshoot matters.
  - The rlimits are set by a re-exec of the noskill binary itself (`__noskill_sandbox_child`), which then execs `sandbox-exec`. Every binary that runs scripts must call `platform.MaybeRunSandboxChild()` first in `main`.

**Egress proxy:**
- Each run gets its own credential (`HTTPS_PROXY=http://run:<token>@127.0.0.1:<port>`), revoked when the run ends, so a process that outlives its run has no network.
- The proxy resolves names itself and refuses IP literals and any name that resolves to a loopback, private, link-local or unspecified address.
- CONNECT is allowed only to port 443 unless the skill declares another.
- Every host goes into the trace.

**Copy-in:** clone where the filesystem supports it (APFS `clonefile`, Btrfs/XFS reflink, ReFS block clone) and copy bytes otherwise. Inputs are read-only.

**After a run that was allowed to fork:**
- The daemon sweeps for leftover processes whose executable is in the run's runtime and that have open files under the run's `work/`, and kills them.
- `work/` goes to the trash.

**`doctor` self-test:** runs a known script under a generated profile. macOS 27 doesn't show sandbox denials for unsigned CLI processes, so a broken profile would otherwise only show up as a crash.

## Consequences
- Skills that need subprocesses (uv installs, build tools) must declare `subprocess: true`. Install review shows it. Their runs can leave a confined detached process until the sweep.
- A noskill-managed Node (the official self-contained build) is preferred to Homebrew's: Homebrew Node needs ~14 library folders plus its OpenSSL and CA folders on the read list.
- `sandbox-exec` is deprecated. If Apple removes it, learned code falls back to WASM only and other scripts need approval per run (the S9 fail rule).
- Linux (Landlock/bubblewrap/cgroups) and Windows (AppContainer/Job object) still need their S9 runs before their parts of the sandbox are built.
