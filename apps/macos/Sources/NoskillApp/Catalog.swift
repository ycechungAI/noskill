import Foundation

/// The parts the app runs, in the order Start All brings them up (docs/APP.md).
enum ServiceID: String, CaseIterable, Identifiable {
    case ollama, agent, research, tasksDaemon, tasksWeb
    var id: String { rawValue }
}

struct ServiceSpec: Identifiable {
    let id: ServiceID
    let name: String
    let detail: String
    /// Full path of the program to run; nil when it wasn't found.
    let executable: String?
    let arguments: [String]
    let workingDirectory: String?
    /// The part counts as running when this port answers, whoever started it.
    let port: Int?
    let healthPath: String
    /// For a part with no port (the sched daemon): a `pgrep -f` pattern that finds it running.
    let processPattern: String?
    let installHint: String
    /// The part's own page, shown in the window.
    let page: URL?

    var installed: Bool { executable != nil }
}

enum Catalog {
    /// Where Ollama's own installer, its app and noskill's start.sh put the `ollama` command.
    static func ollamaCandidates(home: String = NSHomeDirectory()) -> [String] {
        [
            "/usr/local/bin/ollama",
            "/opt/homebrew/bin/ollama",
            "/Applications/Ollama.app/Contents/Resources/ollama",
            home + "/Applications/Ollama.app/Contents/Resources/ollama",
            home + "/.local/bin/ollama",
        ]
    }

    static func specs(
        _ cfg: AppConfig, home: String = NSHomeDirectory(),
        isExecutable: (String) -> Bool = FileManager.default.isExecutableFile(atPath:)
    ) -> [ServiceSpec] {
        func found(_ path: String) -> String? { isExecutable(path) ? path : nil }
        func local(_ port: Int, _ path: String) -> URL? { URL(string: "http://127.0.0.1:\(port)\(path)") }
        let orc = expandTilde(cfg.orcastratorDir)
        let sched = expandTilde(cfg.schedulerDir)
        let schedBin = found(sched + "/.venv/bin/sched")
        let schedHint = "Open Setup to make its .venv in \(sched)."

        return [
            ServiceSpec(
                id: .ollama, name: "Ollama", detail: "Runs the local models",
                executable: ollamaCandidates(home: home).first(where: isExecutable), arguments: ["serve"],
                workingDirectory: nil, port: cfg.ollamaPort, healthPath: "/api/version",
                processPattern: nil, installHint: "Open Setup to install Ollama.", page: nil),
            ServiceSpec(
                id: .agent, name: "Agent", detail: "noskill: skills, routing, proxy, jobs, approvals",
                executable: found(expandTilde(cfg.noskillBinary)), arguments: ["serve"],
                workingDirectory: nil, port: cfg.agentPort, healthPath: "/noskill/v1/health",
                processPattern: nil, installHint: "Open Setup to install the noskill agent.",
                page: local(cfg.agentPort, "/ui")),
            ServiceSpec(
                id: .research, name: "Research", detail: "orcastrator: STORM research reports",
                executable: found(orc + "/.venv/bin/orcastrator-web"),
                arguments: ["--port", String(cfg.researchPort)], workingDirectory: orc,
                port: cfg.researchPort, healthPath: "/", processPattern: nil,
                installHint: "Open Setup to make its .venv in \(orc).",
                page: local(cfg.researchPort, "/")),
            ServiceSpec(
                id: .tasksDaemon, name: "Task runner", detail: "sched daemon: runs tasks when they're due",
                executable: schedBin, arguments: ["daemon"], workingDirectory: sched,
                port: nil, healthPath: "",
                // Matches this app's `sched daemon` and the login agent's `python -m schedlm.cli daemon`.
                processPattern: "(bin/sched|schedlm\\.cli) daemon( |$)",
                installHint: schedHint, page: nil),
            ServiceSpec(
                id: .tasksWeb, name: "Tasks", detail: "sched serve: the scheduled tasks page",
                executable: schedBin, arguments: ["serve", "--port", String(cfg.tasksPort)],
                workingDirectory: sched, port: cfg.tasksPort, healthPath: "/api/tasks",
                processPattern: nil, installHint: schedHint, page: local(cfg.tasksPort, "/")),
        ]
    }
}
