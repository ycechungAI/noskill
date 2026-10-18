# Phase 0 spikes

Throwaway experiments for [docs/PHASE0_TESTS.md](../docs/PHASE0_TESTS.md). Nothing here is product code. Results that matter are written up in [docs/spikes/](../docs/spikes/) and any decision they change goes into SPEC/ADRs.

S1 and S3 are Python 3 stdlib only (no installs) and need Ollama on `127.0.0.1:11434` (or `OLLAMA_HOST`). S2 is Go (stdlib only) plus `uv run --with openai` for the SDK client.

## Layout
```
common/ollama.py        tiny Ollama client (chat, /v1/systemone, ps, show, unload)
data/
  make_tools50.py       → tools-50.jsonl + tools-world.json (S1, S5)
  make_decisions.py     → router-200.jsonl + noul-40.jsonl (S3)
  review.py             your labelling pass over router-200 (labeller b)
  adjudicate.py         settle requests where you and the AI disagree
  export_rss.py         → rss-100.jsonl from YOUR qBittorrent (S6; stays local)
s1/run.py, s1/report.py tool calling: native vs constrained vs prompted
s3/run.py, s3/report.py JEV decision models: noul, router, contention, memory
s5/                     Hermes Agent teardown: sandboxed install, capture server, 10-task runner, the skill it wrote (the Hermes clone and install live in s5/ and s5/out, git-ignored)
s9/                     sandbox feasibility (macOS): Go launcher (cmd/sbx) + egress proxy (cmd/egress) + copy-in timer, attack/legit scripts, run.py (out/ is git-ignored)
s7/                     daemon + scheduler + notifications (macOS): Go `s7` (daemon, install/uninstall as a launchd user agent, notify IPC), Swift helper `notifyapp/` (`build.sh`), `report.py`; out/ is git-ignored
s2/                     proxy compatibility: mock backend, Go proxy, bench, protocol tests, Open WebUI launcher (run.sh)
status/server.py        live progress page: http://127.0.0.1:8765 (reads results/, changes nothing)
results/                raw JSONL (git-ignored)
```

## Run
```bash
python3 spikes/data/make_tools50.py && python3 spikes/data/make_decisions.py
python3 spikes/s1/run.py --models llama3.2:3b,qwen3.5:4b --seeds 1,2,3   # resumable
python3 spikes/s1/report.py
python3 spikes/s3/run.py noul && python3 spikes/s3/run.py router && python3 spikes/s3/run.py contention
python3 spikes/s3/report.py
spikes/s2/run.sh   # S2: mock + proxy, protocol tests (100 runs per client), overhead, cancel
python3 spikes/status/server.py   # progress bars for all of the above
```
Don't run S1 and S3 at the same time unless you want contention: both time model calls.

## Notes on the mock world
- Files, pages and search results in `tools-world.json` / fixtures are invented. A fixed clock (2026-10-15 09:30 Europe/London, a Thursday) keeps answers checkable.
- Task success means the end state matches: the files moved where they should, and/or the final answer matches the expected pattern. Using a tool isn't required for success; calling one on a no-tool task counts as a false call.
- `run_python` runs the model's code with `sandbox-exec` (no network, no writes, no reads under `/Users`), 10 s timeout.

## Labels
`router-200`: AI draft (labeller "a", `make_decisions.py`) → your pass ("b", `review.py`) → settle the disagreements (`adjudicate.py`, stored as `labels.final`). `router-200.labels-c.jsonl` is a blind second pass by the same AI. It matched the draft 200/200, which shows a same-model relabel adds nothing, and it supplies the AI side in `adjudicate.py`. S3's report scores against settled labels, then a+b agreement. See [PHASE0_TESTS.md](../docs/PHASE0_TESTS.md#assisted-labelling).
