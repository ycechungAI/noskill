import Foundation
import Testing
@testable import NoskillApp

@Suite struct ConfigTests {
    @Test func missingKeysTakeTheDefaults() throws {
        let json = #"{"researchPort": 9000, "orcastratorDir": "/x/orc"}"#
        let cfg = try JSONDecoder().decode(AppConfig.self, from: Data(json.utf8))
        let d = AppConfig.defaults()
        #expect(cfg.researchPort == 9000)
        #expect(cfg.orcastratorDir == "/x/orc")
        #expect(cfg.agentPort == d.agentPort)
        #expect(cfg.schedulerDir == d.schedulerDir)
        #expect(cfg.startAllOnLaunch)
    }

    @Test func noFileIsTheFirstRunAndWritesNothing() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        let url = dir.appendingPathComponent("app.json")
        let (cfg, firstRun) = AppConfig.load(from: url)
        #expect(cfg == AppConfig.defaults())
        #expect(firstRun)
        #expect(!FileManager.default.fileExists(atPath: url.path))
        var changed = cfg
        changed.orcastratorDir = "/elsewhere/orc"
        try changed.save(to: url)
        #expect(AppConfig.load(from: url) == (changed, false))
    }

    @Test func unreadableFileGivesDefaultsAndIsKept() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let url = dir.appendingPathComponent("app.json")
        try Data("{not json".utf8).write(to: url)
        #expect(AppConfig.load(from: url) == (AppConfig.defaults(), false))
        #expect(try String(contentsOf: url, encoding: .utf8) == "{not json")
    }
}

@Suite struct CatalogTests {
    let cfg = AppConfig.defaults(home: "/h")

    @Test func startOrderPutsOllamaFirst() {
        #expect(Catalog.specs(cfg, isExecutable: { _ in true }).map(\.id) == ServiceID.allCases)
        #expect(ServiceID.allCases.first == .ollama)
    }

    @Test func missingProgramsAreNotInstalled() {
        let specs = Catalog.specs(cfg, isExecutable: { _ in false })
        #expect(specs.allSatisfy { !$0.installed })
        #expect(specs.first { $0.id == .research }!.installHint.contains("/h/dev/orcastrator"))
    }

    @Test func runsEachProjectsOwnProgram() {
        let specs = Catalog.specs(cfg, isExecutable: { _ in true })
        func s(_ id: ServiceID) -> ServiceSpec { specs.first { $0.id == id }! }
        #expect(s(.agent).executable == "/h/.local/bin/noskill")
        #expect(s(.agent).arguments == ["serve"])
        #expect(s(.research).executable == "/h/dev/orcastrator/.venv/bin/orcastrator-web")
        #expect(s(.research).arguments == ["--port", "8400"])
        #expect(s(.research).workingDirectory == "/h/dev/orcastrator")
        #expect(s(.tasksDaemon).arguments == ["daemon"])
        #expect(s(.tasksDaemon).port == nil)
        #expect(s(.tasksWeb).arguments == ["serve", "--port", "8787"])
        #expect(s(.tasksWeb).workingDirectory == "/h/dev/scheduler")
        #expect(s(.agent).page?.absoluteString == "http://127.0.0.1:11435/ui")
    }

    @Test func findsOllamaWhereNoskillsStartScriptPutsIt() {
        let specs = Catalog.specs(cfg, home: "/h", isExecutable: { $0 == "/h/Applications/Ollama.app/Contents/Resources/ollama" })
        #expect(specs.first { $0.id == .ollama }!.installed)
    }

    @Test func ollamaPrefersUsrLocal() {
        let specs = Catalog.specs(cfg, isExecutable: { $0.hasPrefix("/usr/local") || $0.hasPrefix("/Applications") })
        #expect(specs.first { $0.id == .ollama }!.executable == "/usr/local/bin/ollama")
    }

    @Test func daemonPatternMatchesBothWaysOfStartingIt() throws {
        let pattern = Catalog.specs(cfg, isExecutable: { _ in true }).first { $0.id == .tasksDaemon }!.processPattern!
        let re = try NSRegularExpression(pattern: pattern)
        func matches(_ s: String) -> Bool { re.firstMatch(in: s, range: NSRange(s.startIndex..., in: s)) != nil }
        #expect(matches("/h/dev/scheduler/.venv/bin/python /h/dev/scheduler/.venv/bin/sched daemon"))
        #expect(matches(".venv/bin/python -m schedlm.cli daemon"))
        #expect(!matches("/h/.local/bin/noskill daemon start"))
        #expect(!matches(".venv/bin/sched daemonize"))
    }
}

@Suite struct StateTests {
    let t0 = Date(timeIntervalSince1970: 1_000)

    @Test func answeringPortIsRunning() {
        #expect(ServiceState.next(installed: true, up: true, owned: true, startedAt: t0, now: t0, previous: .starting) == .running(external: false))
        #expect(ServiceState.next(installed: false, up: true, owned: false, startedAt: nil, now: t0, previous: .notInstalled) == .running(external: true))
    }

    @Test func startingUntilTheTimeout() {
        #expect(ServiceState.next(installed: true, up: false, owned: true, startedAt: t0, now: t0 + 5, previous: .starting) == .starting)
        if case .failed = ServiceState.next(installed: true, up: false, owned: true, startedAt: t0, now: t0 + 31, previous: .starting) {} else {
            Issue.record("expected failed after the start timeout")
        }
    }

    @Test func aFailureStaysUntilStartedAgain() {
        #expect(ServiceState.next(installed: true, up: false, owned: false, startedAt: nil, now: t0, previous: .failed("x")) == .failed("x"))
        #expect(ServiceState.next(installed: true, up: false, owned: false, startedAt: nil, now: t0, previous: .running(external: true)) == .stopped)
        #expect(ServiceState.next(installed: false, up: false, owned: false, startedAt: nil, now: t0, previous: .stopped) == .notInstalled)
    }
}

@Suite struct LogBufferTests {
    @Test func joinsLinesSplitAcrossChunks() {
        var b = LogBuffer()
        b.append("hel")
        b.append("lo\nwor")
        #expect(b.lines == ["hello"])
        #expect(b.lastLine == "wor")
        b.append("ld\n")
        #expect(b.lines == ["hello", "world"])
        #expect(b.text == "hello\nworld")
    }

    @Test func progressBarsKeepOnlyWhatTheyDrewLast() {
        var b = LogBuffer()
        b.append("10%\r50%")
        #expect(b.lastLine == "50%")
        b.append("\r100%\r\ndone\r\n")
        #expect(b.lines == ["100%", "done"])
    }

    @Test func keepsOnlyTheLastLines() {
        var b = LogBuffer()
        for i in 0..<(LogBuffer.maxLines + 20) { b.append("\(i)\n") }
        #expect(b.lines.count == LogBuffer.maxLines)
        #expect(b.lines.first == "20")
    }
}

@Suite struct EnvironmentTests {
    @Test func addsInstallFoldersToAFinderPath() {
        let env = Supervisor.childEnvironment(["PATH": "/usr/bin:/bin"])
        let path = env["PATH"]!.split(separator: ":").map(String.init)
        #expect(path.prefix(2) == ["/usr/bin", "/bin"])
        #expect(path.contains("/usr/local/bin"))
        #expect(path.contains(NSHomeDirectory() + "/.local/bin"))
        #expect(env["PYTHONUNBUFFERED"] == "1")
    }
}
