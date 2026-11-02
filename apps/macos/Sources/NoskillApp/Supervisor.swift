import Foundation

enum ServiceState: Equatable {
    case notInstalled
    case stopped
    case starting
    /// external: something other than this app started it, so the app won't stop it.
    case running(external: Bool)
    case failed(String)

    var isRunning: Bool { if case .running = self { return true } else { return false } }

    /// How long a part the app started may take before its port has to answer.
    static let startTimeout: TimeInterval = 30

    /// The state after a check. `up`: its port answered (or its process was found). `owned`: the
    /// app started it and the process is still alive.
    static func next(
        installed: Bool, up: Bool, owned: Bool, startedAt: Date?, now: Date, previous: ServiceState
    ) -> ServiceState {
        if up { return .running(external: !owned) }
        if owned {
            if let s = startedAt, now.timeIntervalSince(s) > startTimeout {
                return .failed("Started, but it didn't answer within \(Int(startTimeout)) s.")
            }
            return .starting
        }
        if !installed { return .notInstalled }
        if case .failed = previous { return previous }
        return .stopped
    }
}

/// The last lines a part printed. Output arrives in chunks, so a line without its newline yet is
/// kept in `partial` until the rest comes.
struct LogBuffer: Equatable {
    static let maxLines = 500
    private(set) var lines: [String] = []
    private var partial = ""

    mutating func append(_ text: String) {
        let parts = (partial + text).components(separatedBy: "\n")
        partial = parts.last ?? ""
        lines.append(contentsOf: parts.dropLast().map(Self.overwritten))
        if lines.count > Self.maxLines { lines.removeFirst(lines.count - Self.maxLines) }
    }

    var text: String { (lines + (partial.isEmpty ? [] : [Self.overwritten(partial)])).joined(separator: "\n") }
    var lastLine: String? {
        let p = Self.overwritten(partial)
        return p.isEmpty ? lines.last(where: { !$0.isEmpty }) : p
    }

    /// A progress bar (curl, pip, Ollama) redraws its line with a carriage return; keep only what
    /// it drew last, as a terminal shows it.
    static func overwritten(_ line: String) -> String {
        guard let r = line.lastIndex(of: "\r") else { return line }
        let after = line[line.index(after: r)...]
        return after.isEmpty ? overwritten(String(line[..<r])) : String(after)
    }
}

/// Starts, stops and watches the parts (ADR-0017). It starts only what isn't running and stops
/// only what it started.
@MainActor
final class Supervisor: ObservableObject {
    @Published private(set) var states: [ServiceID: ServiceState] = [:]
    @Published private(set) var busy = false
    @Published private(set) var config: AppConfig
    @Published private(set) var specs: [ServiceSpec]
    let logs: LogStore

    private var processes: [ServiceID: Process] = [:]
    private var startedAt: [ServiceID: Date] = [:]
    private var stopping: Set<ServiceID> = []
    private var timer: Timer?

    init(config: AppConfig, logs: LogStore) {
        self.config = config
        self.logs = logs
        self.specs = Catalog.specs(config)
        for s in specs { states[s.id] = s.installed ? .stopped : .notInstalled }
    }

    /// New folders or ports from Setup. What the app started keeps running.
    func apply(_ cfg: AppConfig) {
        config = cfg
        reloadSpecs()
    }

    /// Looks for the programs again, after Setup installed one.
    func reloadSpecs() {
        let fresh = Catalog.specs(config)
        guard fresh.map(\.executable) != specs.map(\.executable) || fresh.map(\.port) != specs.map(\.port) else { return }
        specs = fresh
        Task { await refresh() }
    }

    func spec(_ id: ServiceID) -> ServiceSpec { specs.first { $0.id == id }! }
    func state(_ id: ServiceID) -> ServiceState { states[id] ?? .stopped }
    var runningCount: Int { specs.filter { state($0.id).isRunning }.count }
    var ownsAny: Bool { !processes.isEmpty }
    func owns(_ id: ServiceID) -> Bool { processes[id] != nil }

    func begin() {
        Task { await refresh() }
        timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.refresh() }
        }
    }

    func refresh() async {
        let checks = specs.map { ($0.id, $0.port, $0.healthPath, $0.processPattern) }
        var up: [ServiceID: Bool] = [:]
        await withTaskGroup(of: (ServiceID, Bool).self) { group in
            for (id, port, path, pattern) in checks {
                group.addTask { (id, await Probe.isUp(port: port, path: path, pattern: pattern)) }
            }
            for await (id, ok) in group { up[id] = ok }
        }
        let now = Date()
        var next = states
        for s in specs {
            next[s.id] = ServiceState.next(
                installed: s.installed, up: up[s.id] ?? false, owned: processes[s.id] != nil,
                startedAt: startedAt[s.id], now: now, previous: state(s.id))
        }
        // One change at most per check, and none when nothing moved, so the window isn't redrawn
        // for nothing (a redraw mid-click is what made the sidebar feel stuck).
        if next != states { states = next }
    }

    // MARK: Start and stop

    func start(_ id: ServiceID) {
        let s = spec(id)
        guard let exe = s.executable, processes[id] == nil, !state(id).isRunning else { return }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = s.arguments
        if let dir = s.workingDirectory { p.currentDirectoryURL = URL(fileURLWithPath: dir) }
        p.environment = Self.childEnvironment()
        p.standardInput = FileHandle.nullDevice
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        pipe.fileHandleForReading.readabilityHandler = { [weak self] h in
            let data = h.availableData
            guard !data.isEmpty else { h.readabilityHandler = nil; return }
            let text = String(decoding: data, as: UTF8.self)
            Task { @MainActor in self?.logs.append(id.rawValue, text) }
        }
        p.terminationHandler = { [weak self] proc in
            let code = proc.terminationStatus
            Task { @MainActor in self?.exited(id, code: code) }
        }
        logs.append(id.rawValue, "$ \(([exe] + s.arguments).joined(separator: " "))\n")
        do {
            try p.run()
            processes[id] = p
            startedAt[id] = Date()
            states[id] = .starting
        } catch {
            states[id] = .failed("Couldn't start: \(error.localizedDescription)")
        }
    }

    private func exited(_ id: ServiceID, code: Int32) {
        processes[id] = nil
        startedAt[id] = nil
        if stopping.remove(id) != nil {
            states[id] = .stopped
        } else {
            let last = logs.lastLine(id.rawValue).map { ": \($0)" } ?? ""
            states[id] = .failed("Exited with code \(code)\(last)")
        }
    }

    func stop(_ id: ServiceID) {
        guard let p = processes[id] else { return }
        stopping.insert(id)
        p.terminate()
        let pid = p.processIdentifier
        DispatchQueue.main.asyncAfter(deadline: .now() + 5) { [weak self] in
            if self?.processes[id] === p { kill(pid, SIGKILL) }
        }
    }

    func restart(_ id: ServiceID) {
        guard processes[id] != nil else { start(id); return }
        stop(id)
        Task {
            for _ in 0..<60 where processes[id] != nil { try? await Task.sleep(for: .milliseconds(100)) }
            start(id)
        }
    }

    /// Ollama first, since everything else needs it; then the rest once it answers.
    func startAll() async {
        busy = true
        defer { busy = false }
        await refresh()
        if !state(.ollama).isRunning {
            start(.ollama)
            for _ in 0..<30 {
                try? await Task.sleep(for: .milliseconds(500))
                await refresh()
                if state(.ollama).isRunning { break }
            }
        }
        for s in specs where s.id != .ollama && !state(s.id).isRunning { start(s.id) }
        await refresh()
    }

    func stopAll() {
        for s in specs.reversed() { stop(s.id) }
    }

    /// At quit: stop everything the app started and wait a little for it to go.
    func shutdown() {
        let running = Array(processes.values)
        for p in running { p.terminate() }
        let deadline = Date().addingTimeInterval(3)
        while running.contains(where: { $0.isRunning }) && Date() < deadline { usleep(50_000) }
        for p in running where p.isRunning { kill(p.processIdentifier, SIGKILL) }
    }

    /// An app opened from Finder gets PATH=/usr/bin:/bin:/usr/sbin:/sbin, which has neither
    /// ollama nor uv's Python, so add the usual install folders (as scheduler's _venv.sh does).
    nonisolated static func childEnvironment(_ base: [String: String] = ProcessInfo.processInfo.environment) -> [String: String] {
        var env = base
        let home = NSHomeDirectory()
        let extra = ["\(home)/.local/bin", "/usr/local/bin", "/opt/homebrew/bin"]
        var path = (env["PATH"] ?? "/usr/bin:/bin:/usr/sbin:/sbin").split(separator: ":").map(String.init)
        for dir in extra where !path.contains(dir) { path.append(dir) }
        env["PATH"] = path.joined(separator: ":")
        env["PYTHONUNBUFFERED"] = "1"  // so Python's output reaches Logs as it happens
        return env
    }
}

enum Probe {
    static func isUp(port: Int?, path: String, pattern: String?) async -> Bool {
        if let port, let url = URL(string: "http://127.0.0.1:\(port)\(path)") {
            var req = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 1.5)
            req.httpMethod = "GET"
            // Any HTTP answer, even an error, means something is listening there.
            return (try? await URLSession.shared.data(for: req).1 as? HTTPURLResponse) != nil
        }
        if let pattern { return processExists(pattern) }
        return false
    }

    static func processExists(_ pattern: String) -> Bool {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/pgrep")
        p.arguments = ["-f", "-u", String(getuid()), pattern]
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return false }
        p.waitUntilExit()
        return p.terminationStatus == 0
    }
}
