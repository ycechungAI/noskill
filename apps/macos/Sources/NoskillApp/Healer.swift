import AppKit
import Foundation

/// Output from the parts and from Setup's fixes, for Logs. It's its own object so that output
/// arriving many times a second redraws only the Logs pane, not the whole window.
@MainActor
final class LogStore: ObservableObject {
    static let setupKey = "setup"
    @Published private(set) var buffers: [String: LogBuffer] = [:]

    func append(_ key: String, _ text: String) { buffers[key, default: LogBuffer()].append(text) }
    func lastLine(_ key: String) -> String? { buffers[key]?.lastLine }
}

/// The running fix's progress, apart from SetupModel for the same reason as LogStore.
@MainActor
final class RunProgress: ObservableObject {
    @Published var fraction: Double?
    @Published var line = ""
}

/// Gathers MachineFacts. Runs off the main thread: it reads the disk and starts small programs.
enum MachineProbe {
    static func gather(_ cfg: AppConfig, home: String = NSHomeDirectory()) async -> MachineFacts {
        var f = MachineFacts()
        f.ollamaExecutable = Catalog.ollamaCandidates(home: home).first(where: FileManager.default.isExecutableFile(atPath:))
        f.installedModels = await models(port: cfg.ollamaPort)
        f.ollamaUp = f.installedModels != nil
        f.memoryGB = Int(ProcessInfo.processInfo.physicalMemory / 1_073_741_824)

        f.agentBinary = path(expandTilde(cfg.noskillBinary))
        let noskillHome = ProcessInfo.processInfo.environment["NOSKILL_HOME"] ?? home + "/.noskill"
        f.agentConfigured = FileManager.default.fileExists(atPath: noskillHome + "/config.toml")
        if let b = Bundle.main.resourceURL?.appendingPathComponent("noskill").path,
           FileManager.default.isExecutableFile(atPath: b) { f.bundledAgent = b }
        let src = expandTilde(cfg.noskillDir)
        f.noskillSource = path(src)
        f.noskillHasStartScript = FileManager.default.fileExists(atPath: src + "/start.sh")
        f.hasGo = ["/usr/local/go/bin/go", "/usr/local/bin/go", "/opt/homebrew/bin/go", home + "/.local/go/bin/go"]
            .contains(where: FileManager.default.isExecutableFile(atPath:))

        f.research = project(expandTilde(cfg.orcastratorDir), script: "start.sh", program: "orcastrator-web")
        f.tasks = project(expandTilde(cfg.schedulerDir), script: "_venv.sh", program: "sched")
        f.hasUV = [home + "/.local/bin/uv", home + "/.cargo/bin/uv"].contains(where: FileManager.default.isExecutableFile(atPath:))
        let pythons = (try? FileManager.default.contentsOfDirectory(atPath: home + "/.local/share/uv/python")) ?? []
        f.hasManagedPython = pythons.contains { $0.hasPrefix("cpython-3.12") }
        return f
    }

    static func path(_ p: String) -> PathFact {
        var st = stat()
        guard stat(p, &st) == 0 else { return PathFact() }
        let fm = FileManager.default
        return PathFact(
            exists: true, isDirectory: st.st_mode & S_IFMT == S_IFDIR, isExecutable: fm.isExecutableFile(atPath: p),
            ownedByMe: st.st_uid == getuid(), ownerBits: Int(st.st_mode >> 6) & 7,
            readable: fm.isReadableFile(atPath: p), writable: fm.isWritableFile(atPath: p))
    }

    static func project(_ dir: String, script: String, program: String) -> ProjectFacts {
        var p = ProjectFacts(folder: path(dir))
        guard p.folder.exists, p.folder.readable else { return p }
        p.hasSetupScript = FileManager.default.fileExists(atPath: dir + "/" + script)
        let venv = dir + "/.venv"
        let cfgText = try? String(contentsOfFile: venv + "/pyvenv.cfg", encoding: .utf8)
        let home = cfgText?.split(separator: "\n").first { $0.hasPrefix("home") }.map(String.init)
        let exists = FileManager.default.fileExists(atPath: venv)
        let brew = home?.range(of: "homebrew|Homebrew|Cellar|linuxbrew", options: .regularExpression) != nil
        // Homebrew's Python can hang for this user, so it isn't even tried.
        let runs = exists && !brew ? runsWithin(10, venv + "/bin/python", ["-c", "import sys"]) : nil
        let prog = venv + "/bin/" + program
        let shebang = (try? String(contentsOfFile: prog, encoding: .utf8))?.split(separator: "\n").first.map(String.init)
        let real = (try? FileManager.default.destinationOfSymbolicLink(atPath: venv)) ?? venv
        p.venv = VenvState.of(
            exists: exists, home: home, pythonRuns: runs, program: program,
            programExists: FileManager.default.isExecutableFile(atPath: prog), shebang: shebang,
            venvRealPath: (real as NSString).resolvingSymlinksInPath)
        return p
    }

    static func models(port: Int) async -> [String]? {
        guard let url = URL(string: "http://127.0.0.1:\(port)/api/tags") else { return nil }
        let req = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 2)
        guard let (data, _) = try? await URLSession.shared.data(for: req),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let list = obj["models"] as? [[String: Any]] else { return nil }
        return list.compactMap { $0["name"] as? String }
    }

    /// Runs a program and says whether it exited 0 within `seconds`.
    static func runsWithin(_ seconds: Double, _ exe: String, _ args: [String]) -> Bool {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        p.standardInput = FileHandle.nullDevice
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return false }
        let deadline = Date().addingTimeInterval(seconds)
        while p.isRunning && Date() < deadline { usleep(20_000) }
        if p.isRunning { p.terminate(); return false }
        return p.terminationStatus == 0
    }
}

/// Setup: checks this Mac and runs the fixes, one at a time (docs/APP.md "Setup").
@MainActor
final class SetupModel: ObservableObject {
    @Published private(set) var findings: [Finding] = []
    @Published private(set) var checked = false
    @Published private(set) var checking = false
    /// The part whose fix is running.
    @Published private(set) var running: SetupPart?
    /// What the last fix of each part ended with.
    @Published private(set) var results: [SetupPart: (ok: Bool, text: String)] = [:]
    @Published var firstRun: Bool
    let progress = RunProgress()
    private let sup: Supervisor
    private let logs: LogStore
    private var process: Process?
    private var pullTask: Task<Void, Error>?
    private var cancelled = false

    init(sup: Supervisor, logs: LogStore, firstRun: Bool) {
        self.sup = sup
        self.logs = logs
        self.firstRun = firstRun
    }

    var problemCount: Int { findings.filter { $0.level == .problem }.count }
    func finding(_ part: SetupPart) -> Finding? { findings.first { $0.part == part } }

    func check() async {
        checking = true
        let cfg = sup.config
        let facts = await Task.detached { await MachineProbe.gather(cfg) }.value
        findings = Diagnosis.check(cfg, facts)
        checked = true
        checking = false
        sup.reloadSpecs()
    }

    /// Saves app.json, which also ends the first run.
    func finish() {
        try? sup.config.save()
        firstRun = false
    }

    func setFolder(_ part: SetupPart, _ url: URL) async {
        guard let key = part.folderKey else { return }
        var cfg = sup.config
        cfg[keyPath: key] = url.path
        sup.apply(cfg)
        try? cfg.save()
        firstRun = false
        results[part] = nil
        await check()
    }

    /// Stops the running fix, with whatever its script started (pip, curl, uv).
    func cancel() {
        cancelled = true
        pullTask?.cancel()
        guard let p = process else { return }
        for pid in Self.descendants(of: p.processIdentifier) { kill(pid, SIGTERM) }
        p.terminate()
    }

    nonisolated static func descendants(of pid: pid_t) -> [pid_t] {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/pgrep")
        p.arguments = ["-P", String(pid)]
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return [] }
        p.waitUntilExit()
        let kids = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
            .split(separator: "\n").compactMap { pid_t($0) }
        return kids.flatMap { descendants(of: $0) + [$0] }
    }

    func run(_ fix: Fix, for part: SetupPart) async {
        guard running == nil else { return }
        running = part
        cancelled = false
        results[part] = nil
        progress.fraction = nil
        progress.line = "Starting…"
        logs.append(LogStore.setupKey, "\n== \(part.name): \(fix.title)\n")
        let outcome: (Bool, String)
        switch fix {
        case .installOllama:
            outcome = await script(HealScript.installOllama, [], done: "Ollama is installed and opening.")
            if outcome.0 { _ = await waitForOllama(seconds: 30) }
        case .startOllama:
            outcome = await startOllama()
        case .pullModels(let models):
            outcome = await pull(models)
        case .installBundledAgent(let source, let to):
            outcome = await script(HealScript.installBundledAgent, [source, to], done: "The agent is installed.")
        case .installAgentFromSource(let dir, _):
            outcome = await script(HealScript.installAgentFromSource, [dir, expandTilde(sup.config.noskillBinary)], done: "The agent is built and installed.")
        case .initAgent(let binary):
            outcome = await script(HealScript.initAgent, [binary], done: "The agent is set up.")
        case .repairPermissions(let path):
            outcome = await script(HealScript.repairPermissions, [path], done: "Access repaired.")
        case .buildVenv(let p, let dir, _, _):
            outcome = await script(HealScript.buildVenv(p), [dir], done: "Python environment ready.")
        case .pickFolder:
            outcome = (true, "")
        }
        running = nil
        results[part] = cancelled ? (false, "Stopped.") : outcome
        await check()
        // Say so when a fix "worked" but the check still finds the same problem.
        if outcome.0, !cancelled, let f = finding(part), f.level == .problem, f.fix == fix {
            results[part] = (false, "That ran, but the problem is still there. See Logs › Setup for what it printed.")
        }
    }

    /// Waits for an Ollama that's starting by itself (its app, just installed).
    private func waitForOllama(seconds: Int) async -> Bool {
        for _ in 0..<(seconds * 2) {
            sup.reloadSpecs()
            await sup.refresh()
            if sup.state(.ollama).isRunning { return true }
            try? await Task.sleep(for: .milliseconds(500))
        }
        return false
    }

    private func startOllama() async -> (Bool, String) {
        sup.start(.ollama)
        for _ in 0..<40 {
            try? await Task.sleep(for: .milliseconds(500))
            await sup.refresh()
            if sup.state(.ollama).isRunning { return (true, "Ollama is running.") }
            if case .failed(let why) = sup.state(.ollama) { return (false, why) }
        }
        return (false, "Ollama didn't answer within 20 s. See Logs › Ollama.")
    }

    /// Pulls through Ollama's API, which reports bytes done, so the bar can show real progress.
    private func pull(_ models: [String]) async -> (Bool, String) {
        if !sup.state(.ollama).isRunning {
            let (ok, why) = await startOllama()
            if !ok { return (false, why) }
        }
        for (i, m) in models.enumerated() {
            var last = ""
            let task = Task {
                try await OllamaAPI.pull(m, port: sup.config.ollamaPort) { p in
                    if p.status != last { self.logs.append(LogStore.setupKey, "\(m): \(p.status)\n"); last = p.status }
                    self.progress.line = "\(m): \(p.status)"
                    if let fr = p.fraction { self.progress.fraction = (Double(i) + fr) / Double(models.count) }
                }
            }
            pullTask = task
            do { try await task.value } catch {
                if cancelled { return (false, "Stopped.") }
                logs.append(LogStore.setupKey, "error: \(error.localizedDescription)\n")
                return (false, "Couldn't download \(m): \(error.localizedDescription)")
            }
        }
        pullTask = nil
        return (true, "Downloaded \(models.joined(separator: " and ")).")
    }

    /// Runs one of HealScript's scripts with bash, its output going to Logs › Setup and the last
    /// line to the progress row.
    private func script(_ text: String, _ args: [String], done: String) async -> (Bool, String) {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/bash")
        p.arguments = ["-c", text, "bash"] + args
        p.environment = Supervisor.childEnvironment()
        p.standardInput = FileHandle.nullDevice
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        let logs = self.logs
        let progress = self.progress
        pipe.fileHandleForReading.readabilityHandler = { h in
            let data = h.availableData
            guard !data.isEmpty else { h.readabilityHandler = nil; return }
            let text = String(decoding: data, as: UTF8.self)
            Task { @MainActor in
                logs.append(LogStore.setupKey, text)
                if let l = logs.lastLine(LogStore.setupKey) {
                    progress.line = l
                    if let f = percentDone(l) { progress.fraction = f }
                }
            }
        }
        let code: Int32 = await withCheckedContinuation { cont in
            p.terminationHandler = { cont.resume(returning: $0.terminationStatus) }
            do { try p.run() } catch {
                p.terminationHandler = nil
                logs.append(LogStore.setupKey, "couldn't start bash: \(error.localizedDescription)\n")
                cont.resume(returning: -1)
                return
            }
            process = p
        }
        process = nil
        try? await Task.sleep(for: .milliseconds(100))  // let the last output arrive
        if code == 0 { return (true, done) }
        let last = logs.lastLine(LogStore.setupKey).map { ": \($0)" } ?? ""
        return (false, "It stopped with an error\(last)")
    }
}
