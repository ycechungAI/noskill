import Foundation
import Testing
@testable import NoskillApp

/// A Mac with everything installed and working; each test breaks one thing.
private func healthy() -> MachineFacts {
    var f = MachineFacts()
    f.ollamaExecutable = "/Applications/Ollama.app/Contents/Resources/ollama"
    f.ollamaUp = true
    f.installedModels = ["granite4:3b", "gemma3:1b", "nomic-embed-text:latest"]
    f.memoryGB = 16
    f.agentBinary = PathFact(exists: true, isExecutable: true)
    f.agentConfigured = true
    f.noskillSource = PathFact(exists: true, isDirectory: true)
    f.noskillHasStartScript = true
    f.hasGo = true
    let project = ProjectFacts(folder: PathFact(exists: true, isDirectory: true), hasSetupScript: true, venv: .ok)
    f.research = project
    f.tasks = project
    f.hasUV = true
    f.hasManagedPython = true
    return f
}

private let cfg = AppConfig.defaults(home: "/h")

private func finding(_ part: SetupPart, _ f: MachineFacts) -> Finding {
    Diagnosis.check(cfg, f).first { $0.part == part }!
}

@Suite struct DiagnosisTests {
    @Test func aHealthyMacHasNothingToFix() {
        let all = Diagnosis.check(cfg, healthy())
        #expect(all.map(\.part) == SetupPart.allCases)
        #expect(all.allSatisfy { $0.level == .ok && $0.fix == nil })
        #expect(!Onboarding.showSetup(firstRun: false, findings: all))
        #expect(Onboarding.showSetup(firstRun: true, findings: all))
    }

    @Test func noOllamaOffersToInstallItAndTheModelsWait() {
        var f = healthy()
        f.ollamaExecutable = nil
        f.ollamaUp = false
        f.installedModels = nil
        #expect(finding(.ollama, f).fix == .installOllama)
        #expect(finding(.ollama, f).level == .problem)
        #expect(finding(.models, f).level == .note)
        #expect(finding(.models, f).fix == nil)
    }

    @Test func ollamaInstalledButStoppedIsOnlyANote() {
        var f = healthy()
        f.ollamaUp = false
        f.installedModels = nil
        #expect(finding(.ollama, f).level == .note)
        #expect(finding(.ollama, f).fix == .startOllama)
        #expect(finding(.models, f).fix == .startOllama)
        #expect(!Onboarding.showSetup(firstRun: false, findings: Diagnosis.check(cfg, f)))
    }

    @Test func missingModelsArePulledAndSmallMacsGetTheSmallOne() {
        var f = healthy()
        f.installedModels = ["gemma3:1b"]
        #expect(finding(.models, f).fix == .pullModels(["granite4:3b"]))
        f.installedModels = []
        f.memoryGB = 4
        #expect(finding(.models, f).fix == .pullModels(["gemma3:1b"]))
        #expect(finding(.models, f).why!.contains("4 GB"))
    }

    @Test func theAgentIsInstalledFromTheAppFirst() {
        var f = healthy()
        f.agentBinary = PathFact()
        f.bundledAgent = "/Applications/noskill.app/Contents/Resources/noskill"
        #expect(finding(.agent, f).fix == .installBundledAgent(source: f.bundledAgent!, to: "/h/.local/bin/noskill"))
        f.bundledAgent = nil
        #expect(finding(.agent, f).fix == .installAgentFromSource(dir: "/h/dev/noskill", needsGo: false))
        f.hasGo = false
        #expect(finding(.agent, f).fix == .installAgentFromSource(dir: "/h/dev/noskill", needsGo: true))
        f.noskillHasStartScript = false
        #expect(finding(.agent, f).fix == .pickFolder(.agent))
        f.noskillSource = PathFact()
        #expect(finding(.agent, f).fix == .pickFolder(.agent))
    }

    @Test func anInstalledAgentWithNoConfigRunsInit() {
        var f = healthy()
        f.agentConfigured = false
        #expect(finding(.agent, f).fix == .initAgent(binary: "/h/.local/bin/noskill"))
    }

    @Test func aCopiedFolderWithNoOwnerAccessIsRepaired() {
        var f = healthy()
        // d---r-x--- as left by a copy from another user (docs/APP.md "Setup").
        f.research.folder = PathFact(exists: true, isDirectory: true, ownerBits: 0)
        #expect(finding(.research, f).fix == .repairPermissions("/h/dev/orcastrator"))
        #expect(finding(.research, f).level == .problem)
        // The noskill source only matters once the agent is installed: a note, not a problem.
        f = healthy()
        f.noskillSource = PathFact(exists: true, isDirectory: true, ownerBits: 0)
        #expect(finding(.agent, f).level == .note)
        #expect(finding(.agent, f).fix == .repairPermissions("/h/dev/noskill"))
    }

    @Test func anAgentThatCantRunIsRepaired() {
        var f = healthy()
        f.agentBinary = PathFact(exists: true, isExecutable: false, ownerBits: 4)
        #expect(finding(.agent, f).fix == .repairPermissions("/h/.local/bin/noskill"))
    }

    @Test func someoneElsesFolderCantBeRepairedSoPickACopy() {
        var f = healthy()
        f.tasks.folder = PathFact(exists: true, isDirectory: true, ownedByMe: false, ownerBits: 0, readable: true, writable: false)
        #expect(finding(.tasks, f).fix == .pickFolder(.tasks))
        #expect(finding(.tasks, f).why!.contains("another user"))
    }

    @Test func projectsNeedTheirFolderThenTheirScriptThenAVenv() {
        var f = healthy()
        f.research = ProjectFacts()
        #expect(finding(.research, f).fix == .pickFolder(.research))
        f.research = ProjectFacts(folder: PathFact(exists: true, isDirectory: true), hasSetupScript: false)
        #expect(finding(.research, f).fix == .pickFolder(.research))
        f.research.hasSetupScript = true
        f.research.venv = .missing
        f.hasUV = false
        #expect(finding(.research, f).fix == .buildVenv(.research, dir: "/h/dev/orcastrator", needsUV: true, needsPython: false))
        f.tasks.venv = .broken("Its Python doesn't start.")
        #expect(finding(.tasks, f).fix == .buildVenv(.tasks, dir: "/h/dev/scheduler", needsUV: true, needsPython: false))
        #expect(finding(.tasks, f).why!.hasPrefix("Its Python doesn't start."))
    }

    @Test func aChosenFolderIsTheOneChecked() {
        var c = cfg
        c[keyPath: SetupPart.research.folderKey!] = "/Volumes/Work/orcastrator"
        var f = healthy()
        f.research.venv = .missing
        let r = Diagnosis.check(c, f).first { $0.part == .research }!
        #expect(r.fix == .buildVenv(.research, dir: "/Volumes/Work/orcastrator", needsUV: false, needsPython: false))
        #expect(SetupPart.ollama.folderKey == nil)
    }

    @Test func theFirstRunWaitsBeforeStartingAnything() {
        #expect(!Onboarding.startAllOnLaunch(cfg, firstRun: true))
        #expect(Onboarding.startAllOnLaunch(cfg, firstRun: false))
        var off = cfg
        off.startAllOnLaunch = false
        #expect(!Onboarding.startAllOnLaunch(off, firstRun: false))
    }
}

@Suite struct VenvTests {
    let real = "/h/dev/scheduler/.venv"
    func state(home: String? = "home = /h/.local/bin", runs: Bool? = true, prog: Bool = true,
               shebang: String? = "#!/h/dev/scheduler/.venv/bin/python3.12") -> VenvState {
        VenvState.of(exists: true, home: home, pythonRuns: runs, program: "sched", programExists: prog,
                     shebang: shebang, venvRealPath: real)
    }

    @Test func aWorkingVenvIsOk() {
        #expect(state() == .ok)
        #expect(VenvState.of(exists: false, home: nil, pythonRuns: nil, program: "sched", programExists: false, shebang: nil, venvRealPath: real) == .missing)
    }

    @Test func homebrewPythonIsBrokenWithoutRunningIt() {
        guard case .broken(let why) = state(home: "home = /opt/homebrew/opt/python@3.12/bin", runs: nil) else {
            Issue.record("expected broken"); return
        }
        #expect(why.contains("Homebrew"))
    }

    @Test func aPythonThatDoesntStartOrAMissingProgramIsBroken() {
        #expect(state(runs: false) != .ok)
        #expect(state(prog: false) != .ok)
    }

    @Test func aMovedFolderLeavesTheProgramPointingElsewhere() {
        #expect(state(shebang: "#!/Users/someone/dev/scheduler/.venv/bin/python3.12") != .ok)
    }
}

@Suite struct ModelsTests {
    @Test func latestIsTheSameModel() {
        #expect(Models.missing(recommended: ["granite4:3b", "gemma3:1b"], installed: ["gemma3:1b:latest", "granite4:3b"]) == [])
        #expect(Models.missing(recommended: ["gemma3:1b"], installed: ["gemma3:4b"]) == ["gemma3:1b"])
    }

    @Test func recommendedFollowsMemory() {
        #expect(Models.recommended(memoryGB: 16) == ["granite4:3b", "gemma3:1b"])
        #expect(Models.recommended(memoryGB: 0) == ["granite4:3b", "gemma3:1b"])
        #expect(Models.recommended(memoryGB: 6) == ["gemma3:1b"])
    }
}

@Suite struct FixTests {
    @Test func everyDownloadOrChangeAsksFirst() {
        let asks: [Fix] = [
            .installOllama, .pullModels(["granite4:3b"]), .installBundledAgent(source: "/a", to: "/h/.local/bin/noskill"),
            .installAgentFromSource(dir: "/d", needsGo: true), .initAgent(binary: "/b"),
            .repairPermissions("/d"), .buildVenv(.tasks, dir: "/d", needsUV: true, needsPython: true),
        ]
        #expect(asks.allSatisfy { $0.confirm != nil })
        #expect([Fix.startOllama, .pickFolder(.research)].allSatisfy { $0.confirm == nil })
        #expect(Fix.installOllama.confirm!.contains("checksum"))
    }

    @Test func theQuestionSaysWhatIsDownloaded() {
        #expect(Fix.pullModels(["granite4:3b", "gemma3:1b"]).confirm!.contains("about 2.1 GB"))
        #expect(Fix.installAgentFromSource(dir: "/d", needsGo: true).confirm!.contains("Go"))
        #expect(!Fix.installAgentFromSource(dir: "/d", needsGo: false).confirm!.contains("go.dev"))
        let both = Fix.buildVenv(.research, dir: "/d", needsUV: true, needsPython: true).confirm!
        #expect(both.contains("uv") && both.contains("Python 3.12") && both.contains("Homebrew's Python is never used"))
        let neither = Fix.buildVenv(.research, dir: "/d", needsUV: false, needsPython: false).confirm!
        #expect(!neither.contains("astral.sh") && !neither.contains("Python 3.12"))
    }

    @Test func pathsUnderHomeAreShownWithATilde() {
        #expect(tilde("/h/dev/x", home: "/h") == "~/dev/x")
        #expect(tilde("/hx/dev", home: "/h") == "/hx/dev")
        #expect(tilde("/h", home: "/h") == "~")
    }
}

@Suite struct HealScriptTests {
    @Test func scriptsAreValidBash() throws {
        for s in [HealScript.buildVenv(.research), HealScript.buildVenv(.tasks), HealScript.repairPermissions, HealScript.installOllama,
                  HealScript.installBundledAgent, HealScript.installAgentFromSource, HealScript.initAgent] {
            let p = Process()
            p.executableURL = URL(fileURLWithPath: "/bin/bash")
            p.arguments = ["-n", "-c", s]
            try p.run()
            p.waitUntilExit()
            #expect(p.terminationStatus == 0, "\(s.prefix(80))")
        }
    }

    @Test func venvScriptsUseTheProjectsOwnSetupAndUvPython() {
        let orc = HealScript.buildVenv(.research)
        #expect(orc.contains("./start.sh --setup"))
        #expect(orc.contains("UV_PYTHON_PREFERENCE=only-managed"))
        #expect(orc.contains("is_brew"))
        #expect(HealScript.buildVenv(.tasks).contains(#"SCHED_PYTHON="$py" sh -c '. ./_venv.sh'"#))
    }

    @Test func repairOnlyAddsOwnerBits() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let sub = dir.appendingPathComponent("sub")
        try FileManager.default.createDirectory(at: sub, withIntermediateDirectories: true)
        try Data("x".utf8).write(to: sub.appendingPathComponent("f"))
        defer { try? FileManager.default.removeItem(at: dir) }
        chmod(sub.appendingPathComponent("f").path, 0o044)
        chmod(sub.path, 0o050)
        chmod(dir.path, 0o050)
        #expect(MachineProbe.path(dir.path).ownerAccessMissing)
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/bash")
        p.arguments = ["-c", HealScript.repairPermissions, "bash", dir.path]
        p.standardOutput = FileHandle.nullDevice
        try p.run()
        p.waitUntilExit()
        #expect(p.terminationStatus == 0)
        #expect(!MachineProbe.path(dir.path).ownerAccessMissing)
        let mode = { (path: String) -> Int in
            Int((try! FileManager.default.attributesOfItem(atPath: path)[.posixPermissions] as! NSNumber).intValue)
        }
        #expect(mode(sub.path) == 0o750)
        #expect(mode(sub.appendingPathComponent("f").path) == 0o644)
    }
}

@Suite struct PullLineTests {
    @Test func readsOllamasProgress() {
        #expect(PullLine.parse(#"{"status":"pulling manifest"}"#) == PullLine(status: "pulling manifest"))
        let p = PullLine.parse(#"{"status":"pulling 6a0746a1ec1a","digest":"sha256:6a07","total":2000,"completed":500}"#)!
        #expect(p.fraction == 0.25)
        #expect(PullLine.parse(#"{"status":"success"}"#)!.done)
        #expect(PullLine.parse(#"{"error":"pull model manifest: file does not exist"}"#)!.error != nil)
        #expect(PullLine.parse("not json") == nil)
    }
}

@Suite struct OllamaPaneTests {
    @Test func tagsKeepEmbeddingModelsOutOfChat() {
        let json = #"{"models":[{"name":"nomic-embed-text:latest","size":274302450,"capabilities":["embedding"]},{"name":"granite4:3b","size":2100000000,"capabilities":["completion","tools"]},{"name":"old:1b","size":1}]}"#
        let m = OllamaAPI.parseTags(Data(json.utf8))!
        #expect(m.map(\.name) == ["granite4:3b", "nomic-embed-text:latest", "old:1b"])
        #expect(m.filter(\.canChat).map(\.name) == ["granite4:3b", "old:1b"])
        #expect(OllamaAPI.parseTags(Data("{}".utf8)) == nil)
    }

    @Test func chatLinesStreamTheAnswer() {
        #expect(OllamaAPI.parseChatLine(#"{"message":{"role":"assistant","content":"Hel"},"done":false}"#)! == ("Hel", false, nil))
        #expect(OllamaAPI.parseChatLine(#"{"message":{"role":"assistant","content":""},"done":true}"#)!.done)
        #expect(OllamaAPI.parseChatLine(#"{"error":"model not found"}"#)!.error == "model not found")
    }

    @Test func readsCurlsProgressBar() {
        #expect(abs(percentDone("######################                       45.3%")! - 0.453) < 1e-9)
        #expect(percentDone("100.0%") == 1)
        #expect(percentDone("==> Downloading") == nil)
    }
}

@Suite struct PaneTests {
    @Test func eachTabStartsAndStopsOnlyItsOwnParts() {
        #expect(Pane.agent.parts == [.agent])
        #expect(Pane.research.parts == [.research])
        #expect(Pane.tasks.parts == [.tasksDaemon, .tasksWeb])
        #expect(Pane.ollama.parts == [.ollama])
        let tabs = Pane.allCases.filter { !$0.parts.isEmpty }
        for a in tabs { for b in tabs where a != b { #expect(Set(a.parts).isDisjoint(with: b.parts)) } }
        #expect(Pane.overview.parts.isEmpty && Pane.setup.parts.isEmpty && Pane.logs.parts.isEmpty)
    }
}

@Suite struct ModelCatalogTests {
    @Test func everyPopularModelIsExplainedOnceAndRecommendedOnesAreThere() {
        let names = ModelCatalog.popular.map(\.name)
        #expect(Set(names).count == names.count)
        #expect(ModelCatalog.popular.allSatisfy { !$0.blurb.isEmpty && ($0.sizeGB > 0 || $0.isCloud) })
        for m in Models.recommended(memoryGB: 16) { #expect(ModelCatalog.entry(m) != nil) }
        #expect(ModelCatalog.entry("nomic-embed-text:latest")?.kind == .search)
    }

    @Test func fitsLeavesRoomForTheSystem() {
        let big = ModelCatalog.entry("gpt-oss:20b")!
        #expect(!big.fits(memoryGB: 16))
        #expect(big.fits(memoryGB: 32))
        #expect(ModelCatalog.entry("gemma3:1b")!.fits(memoryGB: 4))
        #expect(ModelCatalog.entry("gpt-oss:120b-cloud")!.fits(memoryGB: 4))
    }

    @Test func recentKeepsTheNewestFirstAndOnlyOnce() {
        var r: [String] = []
        for m in ["a", "b", "a:latest", "c"] { r = ModelLists.bump(r, m, max: 2) }
        #expect(r == ["c", "a:latest"])
        #expect(ModelLists.toggle(["a"], "a:latest") == [])
        #expect(ModelLists.toggle(["a"], "b") == ["a", "b"])
        #expect(ModelCatalog.row("unknown:7b").blurb.isEmpty)
    }
}
