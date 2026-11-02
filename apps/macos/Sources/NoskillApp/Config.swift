import Foundation

/// The app's only state of its own: where the three projects live and which ports they use
/// (docs/APP.md "Settings", ADR-0017). Everything else stays in each project.
struct AppConfig: Codable, Equatable {
    var noskillBinary: String
    /// The noskill source folder; Setup runs its `./start.sh install` when there's no binary.
    var noskillDir: String
    var orcastratorDir: String
    var schedulerDir: String
    var ollamaPort: Int
    var agentPort: Int
    var researchPort: Int
    var tasksPort: Int
    var startAllOnLaunch: Bool

    static func defaults(home: String = NSHomeDirectory()) -> AppConfig {
        AppConfig(
            noskillBinary: home + "/.local/bin/noskill",
            noskillDir: home + "/dev/noskill",
            orcastratorDir: home + "/dev/orcastrator",
            schedulerDir: home + "/dev/scheduler",
            ollamaPort: 11434, agentPort: 11435, researchPort: 8400, tasksPort: 8787,
            startAllOnLaunch: true)
    }

    static var fileURL: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/noskill/app.json")
    }

    /// Reads app.json. `firstRun` is true when there's no file yet, so the app shows its welcome;
    /// nothing is written until Setup saves. A file that doesn't parse gives the defaults and is
    /// left as it is, so it can be fixed by hand.
    static func load(from url: URL = fileURL) -> (config: AppConfig, firstRun: Bool) {
        guard let data = try? Data(contentsOf: url) else { return (defaults(), true) }
        return ((try? JSONDecoder().decode(AppConfig.self, from: data)) ?? defaults(), false)
    }

    func save(to url: URL = fileURL) throws {
        try FileManager.default.createDirectory(
            at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys]
        try enc.encode(self).write(to: url, options: .atomic)
    }
}

extension AppConfig {
    // A key that's missing takes its default, so an app.json from an older build still loads.
    init(from decoder: Decoder) throws {
        let d = AppConfig.defaults()
        let c = try decoder.container(keyedBy: CodingKeys.self)
        noskillBinary = try c.decodeIfPresent(String.self, forKey: .noskillBinary) ?? d.noskillBinary
        noskillDir = try c.decodeIfPresent(String.self, forKey: .noskillDir) ?? d.noskillDir
        orcastratorDir = try c.decodeIfPresent(String.self, forKey: .orcastratorDir) ?? d.orcastratorDir
        schedulerDir = try c.decodeIfPresent(String.self, forKey: .schedulerDir) ?? d.schedulerDir
        ollamaPort = try c.decodeIfPresent(Int.self, forKey: .ollamaPort) ?? d.ollamaPort
        agentPort = try c.decodeIfPresent(Int.self, forKey: .agentPort) ?? d.agentPort
        researchPort = try c.decodeIfPresent(Int.self, forKey: .researchPort) ?? d.researchPort
        tasksPort = try c.decodeIfPresent(Int.self, forKey: .tasksPort) ?? d.tasksPort
        startAllOnLaunch = try c.decodeIfPresent(Bool.self, forKey: .startAllOnLaunch) ?? d.startAllOnLaunch
    }
}

func expandTilde(_ path: String) -> String { (path as NSString).expandingTildeInPath }
