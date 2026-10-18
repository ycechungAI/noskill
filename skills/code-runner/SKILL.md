---
name: code-runner
description: Write and run short Python programs in a sandbox to calculate, analyse data files, convert formats or check an answer. Use for math, statistics, CSV/JSON/Excel work, unit conversions, or any time exact computation beats guessing.
license: MIT
noskill:
  version: 0.1.0
  origin: builtin
  status: active
  # Without an embedding model, these are what pick the skill: a question like "sum of the squares
  # of the primes below 1000" shares one word with the description. "compute " keeps "computer" out;
  # no "python", which would pull "write a python function" away from the `code` routing rule.
  triggers: ["calculate", "compute ", "sum of", "average of", "statistics", "csv", "spreadsheet", "excel"]
  task_types: [math, extract, code]
  min_model: { context: 4096, tools: any }
  permissions:
    network: []
    filesystem: []          # the user's files come from --allow (or the skills that bring it in)
    exec: false             # run_python itself is sandboxed: no network, no access outside its folder
  tools:
    - builtin: run_python     # { code, files?: [paths], timeout?: 30, save_to?: folder } → stdout, files
---

# Code runner

## Steps
1. Write a short Python script that prints the answer. Use only the standard library, `pandas`, `numpy`, `openpyxl` or `matplotlib`.
2. Call `run_python`. Pass any user files in `files`. The script reads them as `in/<file name>`.
3. If it errors, read the error, fix the script, and run again (max 3 tries).
4. Answer with the result. Show the script only if the user asked or the result needs explaining.

## Rules
- Print results; don't just compute them.
- Save any output files to `out/`. Pass `save_to` only when the user asked for the files somewhere; otherwise tell them the path from `files`.

<!--
=== Implementation spec ===
- Runtime: a `uv` venv with numpy, pandas, openpyxl and matplotlib (tools.CodeDeps), created by
  `noskill skills prepare code-runner`, never silently mid-run. Until then run_python has the
  standard library only and says how to install the rest when an import fails.
- Sandbox: no network; filesystem limited to a per-run temp dir + read-only copies of passed files;
  CPU/time/memory limits (30 s, 1 GB default). macOS sandbox-exec, Linux Landlock/seccomp (or bwrap),
  Windows Job object + restricted token (documented weaker isolation).
- WASM option later (Pyodide-style) for stronger isolation without OS sandboxes (FR-S5).
- Output files stay in the call's workspace (<data>/runs/<run>/<call>/work/out); `save_to` copies
  them through the broker (write scopes, no overwrite, undo log).
Tests: math tasks with exact answers; CSV aggregation; network access attempt fails; infinite loop killed.
-->
