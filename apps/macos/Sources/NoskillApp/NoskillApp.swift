import AppKit
import SwiftUI

// The noskill app (FR-I8, ADR-0017): one window and a menu-bar icon over Ollama, the noskill
// agent, orcastrator and sched. See docs/APP.md.

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let logs = LogStore()
    let supervisor: Supervisor
    let setup: SetupModel
    let nav = Navigation()
    let ollama: OllamaModel
    private let firstRun: Bool

    override init() {
        let (cfg, firstRun) = AppConfig.load()
        self.firstRun = firstRun
        supervisor = Supervisor(config: cfg, logs: logs)
        setup = SetupModel(sup: supervisor, logs: logs, firstRun: firstRun)
        ollama = OllamaModel(sup: supervisor, logs: logs)
        nav.pane = firstRun ? .setup : .overview
        super.init()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        if CommandLine.arguments.contains("--self-test") { selfTest(); return }
        if let i = CommandLine.arguments.firstIndex(of: "--snapshot"), i + 1 < CommandLine.arguments.count {
            snapshot(to: CommandLine.arguments[i + 1]); return
        }
        supervisor.begin()
        Task {
            await setup.check()
            if Onboarding.showSetup(firstRun: firstRun, findings: setup.findings) { nav.pane = .setup }
        }
        if Onboarding.startAllOnLaunch(supervisor.config, firstRun: firstRun) { Task { await supervisor.startAll() } }
    }

    /// `NoskillApp --self-test` (make-dmg.sh): start up, check this Mac without changing it, print
    /// what Setup would show, and quit. Exits 1 if the bundled agent is missing or doesn't run.
    private func selfTest() {
        Task {
            let cfg = supervisor.config
            let facts = await Task.detached { await MachineProbe.gather(cfg) }.value
            let version = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "?"
            print("noskill app \(version) started")
            for f in Diagnosis.check(cfg, facts) { print("\(f.part.name): \(f.level) — \(f.summary)") }
            var ok = true
            if let b = facts.bundledAgent {
                let runs = MachineProbe.runsWithin(10, b, ["version"])
                print("bundled agent: \(b) \(runs ? "runs" : "DOESN'T RUN")")
                ok = runs
            } else {
                print("bundled agent: missing")
                ok = false
            }
            fflush(stdout)
            exit(ok ? 0 : 1)
        }
    }

    /// `NoskillApp --snapshot DIR`: check this Mac, then save a picture of each pane the app draws
    /// itself (Setup, Overview, Ollama, Logs) to DIR and quit. Starts nothing and changes nothing;
    /// for checking the window where screen recording isn't allowed (docs/APP.md "Checking it").
    private func snapshot(to dir: String) {
        Task {
            await supervisor.refresh()
            await setup.check()
            try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
            for pane in [Pane.setup, .overview, .ollama, .logs] {
                nav.pane = pane
                try? await Task.sleep(for: .milliseconds(1500))
                guard let view = NSApp.windows.first(where: { $0.isVisible && $0.frame.width > 400 })?.contentView,
                      let rep = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { continue }
                view.cacheDisplay(in: view.bounds, to: rep)
                try? rep.representation(using: .png, properties: [:])?.write(to: URL(fileURLWithPath: "\(dir)/\(pane.rawValue).png"))
                print("saved \(dir)/\(pane.rawValue).png")
            }
            fflush(stdout)
            exit(0)
        }
    }

    // Closing the window leaves everything running; the menu-bar icon brings it back.
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func applicationWillTerminate(_ notification: Notification) {
        setup.cancel()
        supervisor.shutdown()
    }
}

/// Which pane the window shows; Setup and the menu-bar menu can change it.
@MainActor
final class Navigation: ObservableObject {
    @Published var pane: Pane = .overview
}

@main
struct NoskillApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        Window("noskill", id: "main") {
            ContentView()
                .environmentObject(delegate.supervisor)
                .environmentObject(delegate.setup)
                .environmentObject(delegate.logs)
                .environmentObject(delegate.nav)
                .environmentObject(delegate.ollama)
        }
        .defaultSize(width: 1100, height: 740)

        MenuBarExtra {
            MenuContent()
                .environmentObject(delegate.supervisor)
                .environmentObject(delegate.setup)
                .environmentObject(delegate.nav)
        } label: {
            MenuLabel(sup: delegate.supervisor)
        }
    }
}
