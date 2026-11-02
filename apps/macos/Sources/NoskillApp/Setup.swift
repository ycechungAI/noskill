import Foundation

// Setup (FR-I8, docs/APP.md "Setup"): what's on this Mac, what's missing or broken, and the fix
// for each. Everything in this file is plain logic over facts gathered elsewhere (Healer.swift),
// so the decisions are tested without touching the machine.

/// One row in Setup. Tasks covers both sched parts (the daemon and its page).
enum SetupPart: String, CaseIterable, Identifiable {
    case ollama, models, agent, research, tasks
    var id: String { rawValue }

    var name: String {
        switch self {
        case .ollama: "Ollama"
        case .models: "Models"
        case .agent: "Agent"
        case .research: "Research"
        case .tasks: "Tasks"
        }
    }

    /// The project behind the part, as people may know it.
    var project: String? {
        switch self {
        case .agent: "noskill"
        case .research: "orcastrator"
        case .tasks: "sched"
        case .ollama, .models: nil
        }
    }

    /// What the part does, in plain words, for someone who has never heard of it.
    var explainer: String {
        switch self {
        case .ollama:
            "Runs AI models on this Mac. The other parts all use it, and nothing you ask leaves the computer."
        case .models:
            "The AI models themselves: granite4:3b does most of the work and gemma3:1b is a small, quick backup."
        case .agent:
            "Your assistant. It answers questions, uses skills such as web search and tidying files, runs jobs on a schedule and asks before it changes anything. Chat apps can use your models through it."
        case .research:
            "Writes long research reports. It plans the questions, searches from several angles, checks the draft and writes it up."
        case .tasks:
            "Runs prompts on a schedule (for example a news summary every morning) and saves each result as a report."
        }
    }

    /// The settings key for the part's folder, when it has one.
    var folderKey: WritableKeyPath<AppConfig, String>? {
        switch self {
        case .agent: \.noskillDir
        case .research: \.orcastratorDir
        case .tasks: \.schedulerDir
        case .ollama, .models: nil
        }
    }
}

/// A file or folder as Setup sees it.
struct PathFact: Equatable {
    var exists = false
    var isDirectory = false
    var isExecutable = false
    var ownedByMe = true
    /// The owner's permission bits, 0...7 (r = 4, w = 2, x = 1), as in `ls -l`.
    var ownerBits = 7
    var readable = true
    var writable = true

    /// The owner's own access is switched off. A copy from another macOS user often keeps working
    /// through ACLs until a rewritten file loses them (docs/APP.md "Setup").
    var ownerAccessMissing: Bool { exists && ownedByMe && ownerBits & (isDirectory ? 7 : 6) != (isDirectory ? 7 : 6) }
}

/// A project's Python environment.
enum VenvState: Equatable {
    case missing
    case ok
    case broken(String)

    /// `home` is pyvenv.cfg's `home =` line, `pythonRuns` whether its Python started (nil when it
    /// wasn't tried), and `shebang` the first line of the program the app runs.
    static func of(
        exists: Bool, home: String?, pythonRuns: Bool?, program: String, programExists: Bool,
        shebang: String?, venvRealPath: String
    ) -> VenvState {
        guard exists else { return .missing }
        if let home, home.range(of: "homebrew|Homebrew|Cellar|linuxbrew", options: .regularExpression) != nil {
            return .broken("It was made with Homebrew's Python, which doesn't run reliably for this user.")
        }
        if pythonRuns == false {
            return .broken("Its Python doesn't start. It may come from another user or an install that's gone.")
        }
        guard programExists else { return .broken("The program \(program) is missing from it.") }
        if let shebang, !shebang.hasPrefix("#!" + venvRealPath + "/bin/python") {
            return .broken("It was made in another folder, so \(program) points to the wrong place.")
        }
        return .ok
    }
}

struct ProjectFacts: Equatable {
    var folder = PathFact()
    /// The project's own setup script is there (orcastrator's start.sh, sched's _venv.sh).
    var hasSetupScript = false
    var venv = VenvState.missing
}

/// Everything Setup needs to know about this Mac.
struct MachineFacts: Equatable {
    var ollamaExecutable: String?
    var ollamaUp = false
    /// nil when Ollama doesn't answer, so the models can't be listed.
    var installedModels: [String]?
    var memoryGB = 0
    var agentBinary = PathFact()
    /// ~/.noskill/config.toml exists (`noskill init` has run).
    var agentConfigured = false
    /// The noskill program shipped inside this app (build-app.sh puts it in Resources).
    var bundledAgent: String?
    var noskillSource = PathFact()
    var noskillHasStartScript = false
    var hasGo = false
    var research = ProjectFacts()
    var tasks = ProjectFacts()
    var hasUV = false
    var hasManagedPython = false
}

enum Models {
    static let sizes = ["granite4:3b": "2.1 GB", "gemma3:1b": "0.8 GB"]

    /// What noskill's start.sh suggests: both on 8 GB or more (or when the size is unknown),
    /// only the small one below that.
    static func recommended(memoryGB: Int) -> [String] {
        memoryGB >= 8 || memoryGB == 0 ? ["granite4:3b", "gemma3:1b"] : ["gemma3:1b"]
    }

    /// `name` and `name:latest` are the same model.
    static func missing(recommended: [String], installed: [String]) -> [String] {
        func norm(_ s: String) -> String { s.hasSuffix(":latest") ? String(s.dropLast(7)) : s }
        let have = Set(installed.map(norm))
        return recommended.filter { !have.contains(norm($0)) }
    }

    static func describe(_ models: [String]) -> String {
        models.map { m in sizes[m].map { "\(m) (about \($0))" } ?? m }.joined(separator: " and ")
    }
}

/// A one-click fix. `confirm` is the question asked first; nil means it runs straight away.
enum Fix: Equatable {
    case installOllama
    case startOllama
    case pullModels([String])
    case installBundledAgent(source: String, to: String)
    case installAgentFromSource(dir: String, needsGo: Bool)
    case initAgent(binary: String)
    case repairPermissions(String)
    case buildVenv(SetupPart, dir: String, needsUV: Bool, needsPython: Bool)
    case pickFolder(SetupPart)

    var title: String {
        switch self {
        case .installOllama: "Install Ollama"
        case .startOllama: "Start Ollama"
        case .pullModels(let m): m.count == 1 ? "Download \(m[0])" : "Download the models"
        case .installBundledAgent: "Install the agent"
        case .installAgentFromSource: "Build and install"
        case .initAgent: "Set it up"
        case .repairPermissions: "Repair access"
        case .buildVenv: "Set up Python"
        case .pickFolder: "Choose folder…"
        }
    }

    /// Asked before anything that downloads, takes long or changes files.
    var confirm: String? {
        switch self {
        case .startOllama, .pickFolder:
            return nil
        case .installOllama:
            return "Download Ollama (a few hundred MB, from Ollama's release on GitHub, checksum checked) into ~/Applications and open it? You can also get it yourself from ollama.com/download."
        case .pullModels(let m):
            return "Download \(Models.describe(m)) from ollama.com? It can take a while on a slow connection."
        case .installBundledAgent(_, let to):
            return "Copy the noskill agent that comes with this app to \(tilde(to)), then run “noskill init”, which finds Ollama and your models and writes a starting config in ~/.noskill? Nothing is downloaded."
        case .installAgentFromSource(let dir, let needsGo):
            let go = needsGo ? " Go isn't on this Mac, so it first downloads Go (about 70 MB, from go.dev, checksum checked) into ~/.local/go." : ""
            return "Build noskill from \(tilde(dir)) with its own “./start.sh install”? It takes a minute or two.\(go) If the recommended models are missing it downloads them too."
        case .initAgent:
            return "Run “noskill init”? It finds Ollama and your models and writes a starting config in ~/.noskill. Nothing is downloaded."
        case .repairPermissions(let path):
            return "Give yourself back read and write access to everything in \(tilde(path))? This only adds permissions for you (chmod -R u+rwX); no file is changed or removed."
        case .buildVenv(let part, let dir, let needsUV, let needsPython):
            var gets: [String] = []
            if needsUV { gets.append("uv, a Python installer (about 20 MB, from astral.sh), into ~/.local/bin") }
            if needsPython { gets.append("Python 3.12 (about 20 MB) through uv") }
            gets.append("\(part.project ?? part.name)'s Python packages from PyPI (tens of MB)")
            return "Set up \(part.name)'s Python environment in \(tilde(dir))/.venv? It downloads \(gets.joined(separator: ", then ")). It takes a few minutes. Homebrew's Python is never used."
        }
    }
}

func tilde(_ path: String, home: String = NSHomeDirectory()) -> String {
    path == home ? "~" : path.hasPrefix(home + "/") ? "~" + path.dropFirst(home.count) : path
}

struct Finding: Identifiable, Equatable {
    enum Level: Equatable { case ok, note, problem }
    let part: SetupPart
    let level: Level
    /// One line: what was found.
    let summary: String
    /// Why it matters or what went wrong, in plain words.
    var why: String? = nil
    var fix: Fix? = nil
    var id: String { part.rawValue }
}

enum Diagnosis {
    /// One finding per part, in the order Setup shows and fixes them: each one's fix only needs
    /// the parts above it.
    static func check(_ cfg: AppConfig, _ f: MachineFacts) -> [Finding] {
        [ollama(f), models(f), agent(cfg, f),
         project(.research, dir: expandTilde(cfg.orcastratorDir), f.research, f, script: "start.sh"),
         project(.tasks, dir: expandTilde(cfg.schedulerDir), f.tasks, f, script: "_venv.sh")]
    }

    static func ollama(_ f: MachineFacts) -> Finding {
        if f.ollamaUp { return Finding(part: .ollama, level: .ok, summary: "Running.") }
        guard f.ollamaExecutable != nil else {
            return Finding(
                part: .ollama, level: .problem, summary: "Not installed.",
                why: "The app can download and install it for you, or you can get it from ollama.com.",
                fix: .installOllama)
        }
        return Finding(
            part: .ollama, level: .note, summary: "Installed, not running yet. Start All starts it.",
            fix: .startOllama)
    }

    static func models(_ f: MachineFacts) -> Finding {
        guard let installed = f.installedModels else {
            if f.ollamaExecutable == nil {
                return Finding(part: .models, level: .note, summary: "Needs Ollama first.")
            }
            return Finding(
                part: .models, level: .note, summary: "Start Ollama to see which models are here.",
                fix: .startOllama)
        }
        let want = Models.recommended(memoryGB: f.memoryGB)
        let missing = Models.missing(recommended: want, installed: installed)
        if missing.isEmpty {
            let others = installed.count - want.count
            return Finding(
                part: .models, level: .ok,
                summary: "\(want.joined(separator: " and ")) are here" + (others > 0 ? ", with \(others) more." : "."))
        }
        let small = f.memoryGB > 0 && f.memoryGB < 8 ? " This Mac has about \(f.memoryGB) GB of memory, so only the small model is suggested." : ""
        return Finding(
            part: .models, level: .problem, summary: "Missing \(missing.joined(separator: " and ")).",
            why: "The agent, research and tasks need a model to think with.\(small)",
            fix: .pullModels(missing))
    }

    static func agent(_ cfg: AppConfig, _ f: MachineFacts) -> Finding {
        let bin = expandTilde(cfg.noskillBinary)
        let src = expandTilde(cfg.noskillDir)
        if f.agentBinary.exists {
            if f.agentBinary.ownerAccessMissing || !f.agentBinary.isExecutable {
                return Finding(
                    part: .agent, level: .problem, summary: "\(tilde(bin)) is there but can't be run.",
                    why: "Its permissions don't let you run it, which often happens after a copy from another user.",
                    fix: .repairPermissions(bin))
            }
            if !f.agentConfigured {
                return Finding(
                    part: .agent, level: .problem, summary: "Installed, but not set up yet.",
                    why: "noskill needs a starting config that names your models.", fix: .initAgent(binary: bin))
            }
            if f.noskillSource.ownerAccessMissing {
                return Finding(
                    part: .agent, level: .note, summary: "Installed at \(tilde(bin)).",
                    why: "Your own access to the noskill folder \(tilde(src)) is switched off, so editing or rebuilding it may fail.",
                    fix: .repairPermissions(src))
            }
            return Finding(part: .agent, level: .ok, summary: "Installed at \(tilde(bin)).")
        }
        if let bundled = f.bundledAgent {
            return Finding(
                part: .agent, level: .problem, summary: "Not installed.",
                why: "This app comes with the agent; installing it takes a few seconds.",
                fix: .installBundledAgent(source: bundled, to: bin))
        }
        guard f.noskillSource.exists else {
            return Finding(
                part: .agent, level: .problem, summary: "Not installed, and the noskill folder isn't at \(tilde(src)).",
                why: "Copy the noskill project to this Mac (github.com/ycechungAI/noskill), then choose its folder.",
                fix: .pickFolder(.agent))
        }
        if let fix = folderAccessFix(.agent, src, f.noskillSource) {
            return Finding(part: .agent, level: .problem, summary: "Not installed, and the noskill folder can't be used.", why: fix.why, fix: fix.fix)
        }
        guard f.noskillHasStartScript else {
            return Finding(
                part: .agent, level: .problem, summary: "Not installed.",
                why: "\(tilde(src)) doesn't look like the noskill project (it has no start.sh). Choose the right folder.",
                fix: .pickFolder(.agent))
        }
        return Finding(
            part: .agent, level: .problem, summary: "Not installed.",
            why: "It can be built from \(tilde(src)).", fix: .installAgentFromSource(dir: src, needsGo: !f.hasGo))
    }

    static func project(_ part: SetupPart, dir: String, _ p: ProjectFacts, _ f: MachineFacts, script: String) -> Finding {
        let name = part.project ?? part.name
        guard p.folder.exists else {
            return Finding(
                part: part, level: .problem, summary: "The \(name) folder isn't at \(tilde(dir)).",
                why: "If it's somewhere else on this Mac, choose it. If it isn't on this Mac yet, copy the \(name) project here first.",
                fix: .pickFolder(part))
        }
        if let fix = folderAccessFix(part, dir, p.folder) {
            return Finding(part: part, level: .problem, summary: "The \(name) folder can't be used.", why: fix.why, fix: fix.fix)
        }
        guard p.hasSetupScript else {
            return Finding(
                part: part, level: .problem, summary: "\(tilde(dir)) doesn't look like \(name).",
                why: "It has no \(script). Choose the \(name) project folder.", fix: .pickFolder(part))
        }
        let build = Fix.buildVenv(part, dir: dir, needsUV: !f.hasUV, needsPython: !f.hasManagedPython)
        switch p.venv {
        case .ok:
            return Finding(part: part, level: .ok, summary: "Ready in \(tilde(dir)).")
        case .missing:
            return Finding(
                part: part, level: .problem, summary: "Its Python environment isn't set up yet.",
                why: "\(name) is written in Python and runs from its own .venv folder.", fix: build)
        case .broken(let why):
            return Finding(part: part, level: .problem, summary: "Its Python environment is broken.", why: why + " Setting it up again fixes it.", fix: build)
        }
    }

    /// A folder that's there but the app can't read or write.
    static func folderAccessFix(_ part: SetupPart, _ dir: String, _ p: PathFact) -> (why: String, fix: Fix)? {
        if p.ownerAccessMissing {
            return ("Your own access to \(tilde(dir)) is switched off, which often happens after a copy from another user.", .repairPermissions(dir))
        }
        if !p.ownedByMe && !(p.readable && p.writable) {
            return ("\(tilde(dir)) belongs to another user, so you can't change it. Copy it into your home folder and choose the copy.", .pickFolder(part))
        }
        return nil
    }
}

enum Onboarding {
    /// Setup opens by itself the first time, and whenever something needs fixing.
    static func showSetup(firstRun: Bool, findings: [Finding]) -> Bool {
        firstRun || findings.contains { $0.level == .problem }
    }

    /// The first time, nothing starts until the person has seen Setup.
    static func startAllOnLaunch(_ cfg: AppConfig, firstRun: Bool) -> Bool { cfg.startAllOnLaunch && !firstRun }
}

/// One line of Ollama's streamed `/api/pull` answer.
struct PullLine: Equatable {
    var status: String
    var fraction: Double?
    var error: String?
    var done: Bool { status == "success" }

    static func parse(_ line: String) -> PullLine? {
        guard let obj = try? JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any] else { return nil }
        if let e = obj["error"] as? String { return PullLine(status: "error", error: e) }
        let status = obj["status"] as? String ?? ""
        let total = (obj["total"] as? NSNumber)?.doubleValue ?? 0
        let done = (obj["completed"] as? NSNumber)?.doubleValue ?? 0
        return PullLine(status: status, fraction: total > 0 ? min(done / total, 1) : nil)
    }
}

/// The shell scripts behind the fixes. Paths are passed as arguments ($1, $2), never pasted into
/// the script, so a folder name with spaces or quotes can't break it.
enum HealScript {
    /// Finds or fetches uv and a uv-managed Python 3.12, puts it first on PATH so the project's own
    /// script picks it (Homebrew's Python is never used), then runs that script's setup step.
    static func buildVenv(_ part: SetupPart) -> String {
        let common = #"""
        set -euo pipefail
        dir=$1
        is_brew() { case "$1|$(readlink "$1" 2>/dev/null || true)" in */homebrew/*|*/Homebrew/*|*/Cellar/*|*/linuxbrew/*) return 0 ;; esac; return 1; }
        uv=""
        for c in "$HOME/.local/bin/uv" "$HOME/.cargo/bin/uv"; do
          if [ -x "$c" ] && ! is_brew "$c"; then uv=$c; break; fi
        done
        if [ -z "$uv" ]; then
          echo "==> Installing uv into ~/.local/bin (from astral.sh)…"
          curl -LsSf https://astral.sh/uv/install.sh | env UV_NO_MODIFY_PATH=1 sh
          uv=$HOME/.local/bin/uv
        fi
        export UV_PYTHON_PREFERENCE=only-managed
        if ! py=$("$uv" python find 3.12 2>/dev/null); then
          echo "==> Fetching Python 3.12 with uv…"
          "$uv" python install 3.12
          py=$("$uv" python find 3.12)
        fi
        echo "==> Using $py"
        export PATH="$(dirname "$py"):$PATH"
        cd "$dir"

        """#
        switch part {
        case .tasks:
            return common + #"""
            SCHED_PYTHON="$py" sh -c '. ./_venv.sh'
            echo "==> .venv is ready"
            """#
        default:
            return common + #"""
            if grep -q -- '--setup' start.sh; then exec bash ./start.sh --setup; fi
            # An older start.sh without --setup: its same steps by hand.
            if [ -e .venv ]; then mv .venv ".venv.stale-$(date +%Y%m%d%H%M%S)"; fi
            "$py" -m venv .venv
            .venv/bin/pip install --quiet --upgrade pip
            .venv/bin/pip install -e .
            echo "==> .venv is ready"
            """#
        }
    }

    /// The way noskill's start.sh installs Ollama: the official release, checksum checked, into
    /// ~/Applications (no admin needed), then opened so it can finish setting itself up.
    static let installOllama = #"""
    set -euo pipefail
    rel=https://github.com/ollama/ollama/releases/latest/download
    dest="$HOME/Applications"
    if [ -e "$dest/Ollama.app" ]; then echo "==> $dest/Ollama.app is already there; opening it"; exec open -a "$dest/Ollama.app"; fi
    t=$(mktemp -d "${TMPDIR:-/tmp}/noskill-ollama.XXXXXX")
    trap 'rm -rf "$t"' EXIT
    echo "==> Reading the checksums of Ollama's latest release…"
    want=$(curl -fsSL "$rel/sha256sum.txt" | awk '$2 == "./Ollama-darwin.zip" {print $1}')
    [ -n "$want" ] || { echo "couldn't read Ollama's checksums; get it from https://ollama.com/download"; exit 1; }
    echo "==> Downloading Ollama-darwin.zip…"
    curl -fL --progress-bar -o "$t/Ollama-darwin.zip" "$rel/Ollama-darwin.zip"
    got=$(shasum -a 256 "$t/Ollama-darwin.zip" | awk '{print $1}')
    [ "$got" = "$want" ] || { echo "the download doesn't match its checksum; nothing was installed"; exit 1; }
    mkdir -p "$dest"
    ditto -x -k "$t/Ollama-darwin.zip" "$dest"
    echo "==> Installed $dest/Ollama.app; opening it"
    open -a "$dest/Ollama.app"
    """#

    static let repairPermissions = #"""
    set -eu
    if [ -d "$1" ]; then chmod -R u+rwX "$1"; else chmod u+rwx "$1"; fi
    echo "==> Repaired access to $1"
    """#

    /// cp -X leaves out extended attributes, so the copy doesn't carry the DMG's download flag.
    static let installBundledAgent = #"""
    set -eu
    mkdir -p "$(dirname "$2")"
    cp -X "$1" "$2.new"
    chmod 755 "$2.new"
    mv -f "$2.new" "$2"
    echo "==> Installed $2"
    exec "$2" init
    """#

    static let installAgentFromSource = #"""
    set -eu
    cd "$1"
    NOSKILL_INSTALL_DIR=$(dirname "$2") exec sh ./start.sh install -y
    """#

    static let initAgent = #"exec "$1" init"#
}
