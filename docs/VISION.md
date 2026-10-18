# Vision

## Problem

Local models are now good enough for real work, but out of the box they:

1. **Can't do things.** No web access, no files, no schedule. Each front end (Open WebUI, LM Studio, Continue, Jan) has its own plugin system, and none of them share skills.
2. **Don't get better.** Every session starts from zero. A workflow that took 15 tool calls to figure out yesterday takes 15 again today.
3. **Use one model for everything.** Users have 5–20 models pulled but pick one by hand. A 70B model answers "what's 2+2", and a 3B model gets asked to refactor a codebase.
4. **Are painful to set up across OSes.** Most agent frameworks assume Linux + Python + Docker. Windows users and non-developers fall off early.

Projects like Hermes Agent and OpenSpace (see [PRIOR_ART.md](PRIOR_ART.md)) prove self-improving skill libraries work. noskill's focus is making that **fast, universal and local-first**.

## Purpose

Give any local model a growing, verified library of skills that it can learn, share and run on any desktop OS, through one small binary that plugs into tools people already use.

## Goals

| # | Goal | Measure |
|---|---|---|
| G1 | **Universal:** one binary for macOS (arm64/x64), Windows (x64/arm64), Linux (x64/arm64) | Same feature set on all 6 targets; CI green on each |
| G2 | **Easy:** under 2 minutes from download to first skill run | `noskill init` auto-detects backends and models, with zero config files to edit |
| G3 | **Fast:** the runtime adds negligible overhead to model latency | < 20 ms p50 added per turn; < 50 MB RSS idle; cold start < 150 ms |
| G4 | **Model-agnostic:** works with models that have native tool calling, models that don't, and JEV decision models (Ollama `/v1/systemone`) | Built-in skills pass their eval suite on a 3B–4B model with no native tool support |
| G5 | **Self-improving:** learns new skills and improves existing ones, measurably | Learned skills raise task success and lower steps/tokens on a held-out eval set over time |
| G6 | **Smart orchestration:** picks the right local model per task, considering hardware | Routed setup beats the best single model on the quality/latency trade-off in the local eval |
| G7 | **Interoperable:** skills use the open agentskills.io `SKILL.md` format; exposed over MCP and an OpenAI/Ollama-compatible API | Skills from other compatible runtimes load unmodified; Open WebUI works by changing one URL |
| G8 | **Safe by default:** every skill declares permissions; learned skills are sandboxed and verified before use | No skill can touch network/files/exec beyond its manifest without a prompt |

## Non-goals (for v1)

- Training or pretraining base models. Weight fine-tuning (LoRA from traces) is an optional later phase, not the core loop.
- Building a chat UI to rival Open WebUI. We ship a CLI and a minimal TUI/web status page; real chat UIs connect through the proxy.
- Cloud-first operation. Cloud models may be added as optional backends, but every feature must work fully offline (except skills that need the internet by nature).
- Hosting or distributing copyrighted content. Media skills (like the anime tracker) automate the user's own configured tools and sources. See [SECURITY.md](SECURITY.md#legal-use).

## Users

| Persona | Wants |
|---|---|
| **Home-lab tinkerer** | Ollama on a Mac mini or a gaming PC; wants automations (media, backups, news digests) running unattended |
| **Privacy-focused professional** | Research and writing help without sending data to the cloud |
| **Developer** | Plug skills into their editor or agent through MCP or the OpenAI-compatible API; write and share skills |
| **Researcher / hobbyist ML** | Try new model types (JEV decision models, small reasoning models) inside a real agent loop |

## Guiding principles

1. **Skills are files.** Plain `SKILL.md` + optional scripts, readable and diffable, in git if you want.
2. **Learning is verified, not hoped for.** Nothing learned is used until it passes tests in a sandbox.
3. **Weak models are first-class.** Design for a 4B model on a laptop; big models are a bonus.
4. **Don't replace, plug in.** Wrap Ollama/LM Studio/llama.cpp; don't fork them.
5. **One binary, no runtime install.** Dependencies like Python are fetched on demand only for skills that need them.
