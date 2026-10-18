# Packaging, install and updates

Goal G2: from nothing to a first run in under 2 minutes on any OS, without admin rights, and nothing that makes the OS warn "unidentified developer".

## Artifacts per release

| Artifact | Built on | Notes |
|---|---|---|
| `noskill` core: darwin (universal arm64+x64), windows amd64/arm64, linux amd64/arm64 | One Linux runner, `CGO_ENABLED=0` cross-compile | Static, < 40 MB (NFR-2). `-trimpath`, `-buildvcs`, fixed mod times → reproducible |
| `noskill-tray` macOS (`Noskill.app`, universal) | macOS runner | Needs CGO (Cocoa) and an app bundle (notifications need a bundle ID) |
| `noskill-tray` Windows / Linux | Linux runner | `fyne.io/systray` is pure Go on Windows (Win32 calls) and Linux (D-Bus StatusNotifierItem); confirm in [S8](PHASE0_TESTS.md#s8-tray-app-on-each-os) |
| `checksums.txt` + signatures, SBOM (syft, SPDX), build provenance | Release job | See [Supply chain](#supply-chain) |

Tool: **GoReleaser** (open-source edition) drives builds, archives, checksums, nfpm packages, Homebrew, Scoop, winget and AUR. The Windows MSI and the macOS `.pkg` are built in separate jobs (WiX and `pkgbuild`), because GoReleaser's MSI support is in its paid edition.

## Per-OS channels

### macOS
| Channel | What | Status |
|---|---|---|
| Homebrew tap | `brew install noskill/tap/noskill`; tray: `brew install --cask noskill/tap/noskill-tray` | v0.1. Apply to homebrew-core once the project is notable enough |
| Signed `.pkg` | Installs both into `/usr/local/bin` + `/Applications` | Needs admin; for people who don't use Homebrew |
| `install.sh` | Per-user to `~/.local/bin`, tray to `~/Applications` | No sudo |

Signing: **Developer ID Application** certificate (Apple Developer Program, $99/year), hardened runtime, then `notarytool`. A bare CLI binary can be notarised but not stapled, so Gatekeeper checks the ticket online the first time it runs. The `.app` and `.pkg` are stapled. Downloads via `curl` don't get the quarantine flag, so `install.sh` never triggers Gatekeeper; browser downloads do, and notarisation handles that.

### Windows
| Channel | What | Status |
|---|---|---|
| winget | `winget install noskill` (manifest PR to `microsoft/winget-pkgs` per release) | v0.1 |
| Scoop | `scoop bucket add noskill https://github.com/noskill/scoop-bucket` | v0.1 |
| MSI (WiX) | **Per-user** by default: `%LOCALAPPDATA%\Programs\noskill`, adds itself to the user `PATH`, no UAC prompt. A per-machine MSI (admin) can also register a Windows Service | v0.1 per-user, v0.3 per-machine |
| `install.ps1` | `irm https://noskill.dev/install.ps1 \| iex` (domain to be confirmed), per-user | v0.1 |

Signing: Authenticode on every `.exe` and the MSI. Options: **Azure Trusted Signing** (managed, about $10/month, but eligibility depends on country and organisation age), or an OV certificate (about $200–400/year; private keys must now live on a hardware token or cloud HSM). Neither gives instant SmartScreen reputation; it builds as downloads accumulate, and winget/Scoop installs aren't affected. Checked 2026-10-15 ([Name and signing checks](#name-and-signing-checks)): Azure Artifact Signing (renamed from Trusted Signing) is $9.99/month but its Public Trust profile is only for individuals in the US or Canada, or organisations in the US, Canada, EU, UK and a few other regions. **SignPath Foundation** signs OSI-licensed public repositories for free from an automated build, so noskill (MIT) qualifies and is the first choice. An OV certificate is the fallback.

### Linux
| Channel | What |
|---|---|
| `install.sh` | Static tarball to `~/.local/bin`, systemd user unit on `noskill daemon install` |
| `.deb` / `.rpm` | nfpm; GitHub release assets, apt/yum repo later |
| AUR | `noskill-bin` |
| Nix | Community flake; not maintained by us in v1 |
| Container image | `ghcr.io/noskill/noskill` for headless servers, P2. The script sandbox is weaker inside containers (Landlock works, bubblewrap usually doesn't) and `doctor` says so |

## Install scripts
`install.sh` and `install.ps1`:
1. Detect OS and architecture; download the archive + `checksums.txt` from GitHub Releases over HTTPS.
2. Verify the SHA-256. If `cosign` or `gh` is installed, also verify the signature/attestation; otherwise print the one-line command to do it by hand.
3. Install per-user (no sudo / no UAC); add to `PATH` if needed and say so.
4. Run `noskill init` unless `--no-init`.

They never install Ollama silently. `init` offers to open Ollama's official installer.

## `noskill update`
| Installed via | What `update` does |
|---|---|
| `install.sh` / `install.ps1` / per-user MSI | Self-update (below) |
| Homebrew, winget, Scoop, apt/rpm, AUR | Detects it (install path / package database) and prints the right command, e.g. `brew upgrade noskill`. Never fights the package manager |

Self-update steps:
1. Fetch the release metadata (GitHub Releases API). `[update] check = true` does only this, weekly, and notifies; it sends nothing about you.
2. Download to a temp file **next to** the current binary (same filesystem, so the swap is atomic).
3. Verify the signature of `checksums.txt` with the **release public key embedded in the binary** (ed25519), then the SHA-256. Any mismatch → abort and keep the old version.
4. Stop the daemon (waits for running jobs up to 5 min, or `--force`), then swap: rename on macOS/Linux; on Windows rename the running `.exe` to `.old`, move the new one in, and delete `.old` on the next start.
5. Start the daemon. It migrates `state.db` after backing it up (see [DATA_MODEL.md](DATA_MODEL.md#migrations)) and the tray reconnects.
6. On failure (daemon doesn't come up healthy within 30 s): put the old binary back and restore the DB backup.

`noskill update --to <version>` rolls back the same way. `--channel beta` follows pre-releases.

**Tray/core version skew:** the IPC protocol has a version number. A tray older than the core shows "Update the tray app" and keeps working in read-only mode.

## `noskill uninstall`
- Stops and unregisters the daemon (launchd / systemd / Task Scheduler) and the tray login item; removes binaries noskill installed itself (package-manager installs: prints the command instead).
- **Keeps** config, skills, traces and `state.db` by default.
- `--purge` moves the data folder to the **OS trash** (consistent with the no-hard-delete rule), after showing its size.
- Never touches Ollama, its models, qBittorrent, or files skills filed for you. Prints the models `init`/model-manager pulled, with the command to remove them if you want.

## CI pipelines (GitHub Actions)
| Workflow | Trigger | Does |
|---|---|---|
| `pr.yml` | Pull request | `go vet`, staticcheck, unit tests on the [CI matrix](PLATFORMS.md#ci-matrix), security test suite, `govulncheck`, link check of `docs/` |
| `nightly.yml` | Daily | Integration tests against real Ollama + `qwen3:0.6b` (Linux, macOS); eval smoke set; unsigned snapshot builds |
| `release.yml` | Tag `v*` | GoReleaser + tray jobs → sign (Apple, Authenticode) → notarise → checksums + signatures + SBOM + provenance → GitHub Release → package-manager PRs/pushes |

Secrets (signing keys, Apple API key, Azure credentials) live in a protected GitHub environment that only `release.yml` on tags can use, with required reviewer approval.

## Supply chain
- `checksums.txt` signed twice: **cosign keyless** (Sigstore transparency log, for people who want to audit) and the **ed25519 release key** (what `noskill update` checks offline).
- GitHub artifact attestations (SLSA build provenance) for every binary: `gh attestation verify noskill --repo noskill/noskill`.
- SBOM per artifact (syft); `govulncheck` blocks releases with reachable vulnerabilities.
- Dependencies pinned with `go.sum`; Renovate PRs; GitHub Actions pinned to commit SHAs.
- Community skills are pinned by commit hash ([SECURITY.md](SECURITY.md) T7).

## Versioning
SemVer. v0.1 ships when the [MVP acceptance criteria](../SPEC.md#6-acceptance-criteria-for-mvp-v01) pass; 1.0 comes once the config, skill and proxy formats are frozen. Config keys and skill front-matter are stable within a minor version; breaking changes come with an automatic migration or a clear `config check` error. The proxy's OpenAI/Ollama surface follows those APIs, not our version number.

## Costs (estimates)
| Item | Cost |
|---|---|
| Apple Developer Program | $99 / year |
| Windows signing | ~$120 / year (Trusted Signing) or ~$200–400 / year (OV certificate) |
| Domain for install scripts | ~$15 / year |
| CI, releases, container registry | Free for a public GitHub repo |

## Name and signing checks
Checked 2026-10-15 from public pages and registries. Nothing was registered or paid for.

| Item | Result |
|---|---|
| GitHub | The user `noskill` exists (a personal account since 2011, 97 repos), so the org name is taken. Free: `noskill-dev`, `noskill-ai`, `noskillhq`, `usenoskill`, `noskill-app`. `getnoskill` is taken. `noskill/noskill` doesn't exist |
| Similar projects | Search shows repos named `noskill` (game tools, a small skills repo), all with few or no stars. None is a local-model skills runtime |
| Homebrew, winget, Scoop, AUR | No `noskill` formula, cask, manifest, bucket entry or AUR package |
| Go, PyPI, npm | `noskill` is free on PyPI and npm. `pkg.go.dev` has nothing at `github.com/noskill/noskill` |
| Domains | `noskill.com`, `.dev`, `.io`, `.ai` and `.app` are all registered. DNS shows no records for `.sh`, `.run`, `.tools`, `.so`, `getnoskill.com` and `usenoskill.com`, but a registrar still has to confirm each is purchasable |
| Windows signing | SignPath Foundation first (free for OSI-licensed public repos with an automated build), then an OV certificate. Azure Artifact Signing ($9.99/month) only if the publisher is an individual in the US or Canada or a qualifying organisation. SmartScreen reputation is per file hash, so each release starts at zero whichever option is used |
| macOS signing | Apple Developer Program, $99/year for individuals, covers Developer ID and notarisation at no extra cost. Only the Account Holder can create Developer ID certificates. Fee waivers don't apply to individuals |

**Decision needed from you:** pick the public name for the GitHub org and domain (the plain `noskill` ones are taken), and whether you'll enrol in the Apple Developer Program. Neither is done.
