# ADR-0001: Core language is Go

Status: **accepted** · Date: 2026-10-15

## Context
Requirements G1–G3: one small binary for 6 OS/arch targets, < 150 ms cold start, < 50 MB idle RSS, an always-on daemon, easy contributions. Skills themselves can be any language (script/WASM/MCP), so the core language only affects the runtime.

## Options
| | Go | Rust | Python | TypeScript (Bun/Node) |
|---|---|---|---|---|
| Single static binary, easy cross-compile | ✅ trivial (no CGO) | ✅ but cross-compile toolchains per target | ❌ (PyInstaller is fragile) | ⚠️ `bun build --compile`, large binaries |
| Startup / memory | ✅ | ✅✅ | ❌ | ⚠️ |
| Pure-language SQLite, WASM, MCP SDK | ✅ modernc sqlite, wazero, official Go MCP SDK | ✅ rusqlite (C), wasmtime, rmcp | ✅ | ✅ |
| ML ecosystem (for LoRA, future non-GGUF models) | ❌ (use sidecar) | ❌ (use sidecar) | ✅✅ | ❌ |
| Contributor friendliness | ✅ | ⚠️ steeper | ✅✅ | ✅ |
| Prior art reuse | Ollama itself is Go; llmrouter is Go | — | Hermes, most agent frameworks | Goose UI |

## Decision
**Go** for the core. ML-heavy pieces (LoRA training, any future non-GGUF models) run as on-demand Python sidecars via managed `uv`, which keeps the core binary small and still gets the Python ML ecosystem when needed.

## Consequences
- Can't import Python agent frameworks directly, so we interop through MCP and the skill format instead.
- Same language as Ollama, which helps with contributions and reading its API code.
- Revisit if: WASM sandboxing performance in wazero is inadequate, or we need in-process GPU inference.
