# Current state

_Last updated: 2026-10-27 by Claude Code_

## In progress

- 2026-10-27: Repo was copied from another macOS user (`neolinux`). Fixed for user `ycechung`:
  file modes were `---xr-----` (owner couldn't read) — reset to 644/755 matching git, which also
  cleared the "every file modified" churn (it was mode-only). `.venv` rebuilt on a uv-managed
  Python 3.12 (old one kept at `.venv.old-neolinux`, gitignored via `.venv.*/`).
- `start.sh` now self-heals the venv and never uses Homebrew (uncommitted, along with `.gitignore`).

## Next steps

- Commit `start.sh` + `.gitignore`.
- Check `Plan.md` and `Todo.md` for the next planned task.

## Gotchas

- `/opt/homebrew` on this machine belongs to another user (uid 501); its `python3.15` hangs for
  `ycechung`. Don't build venvs on Homebrew Python — `start.sh` uses uv-managed Python instead.
- The Model dropdown can default to `nomic-embed-text` (embedding-only); pick a chat model.

## Update 2026-11-01

- orcastrator is now the **Research** part of the noskill app (`~/dev/noskill/docs/APP.md`); README
  says so. The app runs `.venv/bin/orcastrator-web --port 8400` from this folder. Uncommitted: README.md.

## Update 2026-11-03 (retired: moved into noskill repo)

- Working tree retired; this checkout keeps only `.agent-memory/`, `.gitignore` and the `MOVED.md` tombstone.
- Code moved to `apps/orcastrator/` in the noskill repo; branch `feature/setup-flag` archived there.
- Full context: noskill repo `docs/APP.md`, `.agent-memory/STATE.md`, ADR-0017.
