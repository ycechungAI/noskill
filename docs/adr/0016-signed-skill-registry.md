# ADR-0016: A skill registry is a signed folder; skills carry their own signature

Status: **accepted** · Date: 2026-10-18 · Implements FR-S10

## Context
FR-S10 asks for an optional signed skill registry or index for sharing. `noskill skills install <folder|git-url>` already exists:
- It shows the permissions, asks, and copies the folder.
- From a URL, it records the commit it reviewed (SECURITY.md T7: "skills pinned by commit hash").

What install doesn't show is who wrote the skill, or whether it changed after they published it. This had to be settled:

1. **Hosting.** The project has no server, domain or GitHub org yet. Release signing is deferred on purpose (ROADMAP Phase 1).
2. **Keys.** The release key is for binaries and is still to be made. ROADMAP block 5c says a registry is signed with a key you make locally.
3. **Compatibility.** A signed skill must still load as a plain agentskills.io folder.

## Decision
**Signed skill.** `noskill skills sign <folder>` writes `.noskill-sig.json` into the skill folder.
- The signature covers a manifest:
  - first, `noskill-skill-v1`, the skill's name and version;
  - then one line per regular file, `sha256  path`. Paths use `/` and are sorted, and `.git` and the signature file are left out.
  - Symlinks and special files make signing fail: installing would skip them, so the signature wouldn't describe what gets installed.
- The signature is ed25519. The file holds the manifest's digest and its file list (so a check can name the file that changed), the key ID (the first 16 hex digits of the public key's sha256), the public key and the signature.
- A loader that doesn't know noskill ignores a dotfile, so plain loading is unchanged.

**Keys.** `noskill registry key new` makes an ed25519 key pair.
- The private seed goes into the OS keychain (`internal/secrets`, name `registry-signing-key`). It's never printed or written to a file.
- `registry key show` prints the public key and its ID to share.
- Your own key is always trusted on this machine. A skill signed with it shows as "signed by you", even when it came from your own registry.
- There's no export: a lost key means a new key, and everyone re-adds your registry with it.

**Registry.** A registry is a folder, local or a git repository, with:
- `index.json`: entries with name, version, description, the subfolder's path, its manifest digest, and the permissions in short form. It also has a `made` time and the registry name.
- `index.json.sig`: an ed25519 signature over the exact bytes of `index.json`.

`noskill registry publish <folder> [--name n]` signs every skill in the folder's subfolders and writes both files. You then share the folder, for example by pushing it to a git host.

**Trust.** `noskill registry add <name> <path|git-url> --key <public key>` saves the source in `<config>/registries.json`.
- The key must be passed. Trusting whatever key the registry itself holds would trust anyone who can write to it.
- `registry list`, `registry remove <name>` and `registry update [name]` manage the sources.
- `update` fetches each index (a shallow clone for git), checks its signature, and keeps the exact bytes it checked in `<data>/registry/<name>.json` (re-checked on every read). An index whose `made` is older than the one kept is refused, so a stale copy can't roll a skill back.

**Search and install.**
- `noskill registry search <words>` searches the kept indexes.
- `noskill skills install <registry>/<skill>` installs from a registry:
  1. Fetch the registry and check `index.json` against the key you added.
  2. Find the entry, then check the skill folder's manifest digest against it and the folder's own signature against the same key.
  3. Show the permissions as now, plus "signed by <registry> (key <id>)", and ask.
  4. Record `registry:<name>/<skill>@<version> sha256:<digest>` (and the commit, for git) as where it came from.
- Any mismatch refuses the install and says which file changed.

**Plain installs.** `skills install <folder|git-url>` checks `.noskill-sig.json` when there is one:
- **Valid, by a key you trust** (your own or a registry's): "signed by …".
- **Valid, by a key you don't know:** "signed by an unknown key <id>". It's treated as unsigned.
- **Invalid:** refused. The folder changed after it was signed.

**Checking later.** `noskill skills verify <name>` recomputes an installed skill's manifest and compares it with its signature. It finds files edited after installing (FR-S9 still keeps versions).

**No network code beyond git.** Registries are fetched with the git that install already uses (`app.GitFetch`, shared with `skills install <git-url>`).

**As built (block 5c):** `internal/registry` (manifests, signatures, the index; no I/O beyond reading the folder), `internal/app/registry.go` (keys, sources, publish, fetch and checks), `noskill registry …` and `skills sign|verify`. A local folder registry is read in place; a git one is cloned for each fetch and install, and the commit goes into `installed_from`.

## Consequences
- **Signing says who published a skill and that it hasn't changed since. It doesn't say the skill is safe.** The permission screen and the sandbox stay the defence (SECURITY.md T2), and the install screen still shows every permission.
- **Revoking a key means removing the registry.** There's no revocation list.
- **One key per registry.** A registry that gathers other people's skills re-signs them with its own key: `publish` rewrites each skill's `.noskill-sig.json`. The registry vouches for the copy it holds, not for where that copy came from.
- **Timestamps come from the publisher's clock.** The rollback check only compares one registry's indexes with each other.
