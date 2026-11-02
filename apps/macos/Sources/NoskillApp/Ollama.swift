import AppKit
import SwiftUI

// The Ollama pane (FR-I8, docs/APP.md "Ollama"): install Ollama, pull models, sign in to
// ollama.com and chat with a model, all from the app.

struct OllamaModelInfo: Identifiable, Equatable {
    let name: String
    let bytes: Int64
    let capabilities: [String]
    var id: String { name }
    /// Embedding-only models can't chat.
    var canChat: Bool { !capabilities.contains("embedding") || capabilities.contains("completion") }
    var size: String { ByteCountFormatter.string(fromByteCount: bytes, countStyle: .file) }
}

struct ChatMessage: Identifiable, Equatable {
    let id = UUID()
    let role: String
    var text: String
}

enum OllamaAPI {
    static func base(_ port: Int) -> String { "http://127.0.0.1:\(port)" }

    static func parseTags(_ data: Data) -> [OllamaModelInfo]? {
        guard let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let list = obj["models"] as? [[String: Any]] else { return nil }
        return list.compactMap { m in
            guard let name = m["name"] as? String else { return nil }
            return OllamaModelInfo(
                name: name, bytes: (m["size"] as? NSNumber)?.int64Value ?? 0,
                capabilities: m["capabilities"] as? [String] ?? [])
        }
        .sorted { $0.name < $1.name }
    }

    static func tags(port: Int) async -> [OllamaModelInfo]? {
        guard let url = URL(string: base(port) + "/api/tags") else { return nil }
        let req = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 2)
        guard let (data, _) = try? await URLSession.shared.data(for: req) else { return nil }
        return parseTags(data)
    }

    /// Pulls one model; `progress` gets each status line and the fraction done when known.
    static func pull(_ model: String, port: Int, progress: @MainActor (PullLine) -> Void) async throws {
        guard let url = URL(string: base(port) + "/api/pull") else { return }
        var req = URLRequest(url: url, timeoutInterval: 3600)
        req.httpMethod = "POST"
        req.httpBody = try JSONSerialization.data(withJSONObject: ["model": model, "stream": true])
        let (bytes, _) = try await URLSession.shared.bytes(for: req)
        for try await line in bytes.lines {
            try Task.checkCancellation()
            guard let p = PullLine.parse(line) else { continue }
            if let e = p.error { throw NSError(domain: "ollama", code: 1, userInfo: [NSLocalizedDescriptionKey: e]) }
            await progress(p)
        }
    }

    /// One streamed line of `/api/chat`: the next piece of the answer, and whether it's the last.
    static func parseChatLine(_ line: String) -> (text: String, done: Bool, error: String?)? {
        guard let obj = try? JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any] else { return nil }
        if let e = obj["error"] as? String { return ("", true, e) }
        let msg = obj["message"] as? [String: Any]
        return (msg?["content"] as? String ?? "", obj["done"] as? Bool ?? false, nil)
    }
}

/// The percentage a progress bar (curl's, pip's) drew last, as 0...1.
func percentDone(_ line: String) -> Double? {
    guard let r = line.range(of: #"(\d{1,3}(\.\d+)?)%"#, options: [.regularExpression, .backwards]),
          let v = Double(line[r].dropLast()), v <= 100 else { return nil }
    return v / 100
}

@MainActor
final class OllamaModel: ObservableObject {
    @Published private(set) var models: [OllamaModelInfo] = []
    @Published private(set) var pulling: String?
    @Published private(set) var pullFraction: Double?
    @Published private(set) var pullStatus = ""
    @Published private(set) var pullResult: (ok: Bool, text: String)?
    @Published private(set) var account = ""
    @Published private(set) var signingIn = false
    @Published var chatModel = ""
    @Published private(set) var chat: [ChatMessage] = []
    @Published private(set) var answering = false
    @Published private(set) var favourites: [String] = UserDefaults.standard.stringArray(forKey: "favouriteModels") ?? []
    @Published private(set) var recent: [String] = UserDefaults.standard.stringArray(forKey: "recentModels") ?? []
    private let sup: Supervisor
    private let logs: LogStore
    private var pullTask: Task<Void, Never>?
    private var chatTask: Task<Void, Never>?

    init(sup: Supervisor, logs: LogStore) {
        self.sup = sup
        self.logs = logs
    }

    var port: Int { sup.config.ollamaPort }
    var chatModels: [OllamaModelInfo] { models.filter(\.canChat) }

    func isInstalled(_ name: String) -> Bool { models.contains { ModelCatalog.same($0.name, name) } }
    func isFavourite(_ name: String) -> Bool { favourites.contains { ModelCatalog.same($0, name) } }

    func toggleFavourite(_ name: String) {
        favourites = ModelLists.toggle(favourites, name)
        UserDefaults.standard.set(favourites, forKey: "favouriteModels")
    }

    func used(_ name: String) {
        recent = ModelLists.bump(recent, name)
        UserDefaults.standard.set(recent, forKey: "recentModels")
    }

    /// Picks an installed model for Chat.
    func useInChat(_ name: String) {
        if let m = models.first(where: { ModelCatalog.same($0.name, name) }) { chatModel = m.name }
    }

    func refresh() async {
        models = await OllamaAPI.tags(port: port) ?? []
        if !chatModels.contains(where: { $0.name == chatModel }) {
            chatModel = chatModels.first { $0.name.hasPrefix("granite4") }?.name ?? chatModels.first?.name ?? ""
        }
    }

    func pull(_ name: String) {
        let model = name.trimmingCharacters(in: .whitespaces)
        guard !model.isEmpty, pulling == nil else { return }
        pulling = model
        pullFraction = nil
        pullStatus = "Starting…"
        pullResult = nil
        logs.append("ollama", "$ pull \(model)\n")
        pullTask = Task {
            do {
                try await OllamaAPI.pull(model, port: port) { p in
                    self.pullStatus = p.status
                    if let f = p.fraction { self.pullFraction = f }
                }
                pullResult = (true, "Downloaded \(model).")
                used(model)
            } catch is CancellationError {
                pullResult = (false, "Stopped.")
            } catch {
                pullResult = (false, "Couldn't download \(model): \(error.localizedDescription)")
            }
            pulling = nil
            await refresh()
        }
    }

    func stopPull() { pullTask?.cancel() }

    /// `ollama signin` prints a link to open (or says you're signed in already); `signout` signs
    /// out. Both talk to ollama.com, so they only run when pressed.
    func account(_ verb: String) {
        guard !signingIn, let exe = sup.spec(.ollama).executable else { return }
        signingIn = true
        account = ""
        Task {
            let out = await Task.detached { Self.run(exe, [verb], seconds: 300) }.value
            account = out.trimmingCharacters(in: .whitespacesAndNewlines)
            logs.append("ollama", "$ ollama \(verb)\n\(out)\n")
            signingIn = false
        }
    }

    nonisolated static func run(_ exe: String, _ args: [String], seconds: Double) -> String {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        p.environment = Supervisor.childEnvironment()
        p.standardInput = FileHandle.nullDevice
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        final class Output: @unchecked Sendable {
            let lock = NSLock()
            var text = ""
            var opened = false
        }
        let out = Output()
        pipe.fileHandleForReading.readabilityHandler = { h in
            let data = h.availableData
            guard !data.isEmpty else { h.readabilityHandler = nil; return }
            out.lock.lock(); defer { out.lock.unlock() }
            out.text += String(decoding: data, as: UTF8.self)
            // The sign-in link: open it once, as the CLI asks you to.
            if !out.opened, let r = out.text.range(of: #"https://ollama\.com/connect\S+"#, options: .regularExpression),
               let url = URL(string: String(out.text[r])) {
                out.opened = true
                DispatchQueue.main.async { NSWorkspace.shared.open(url) }
            }
        }
        guard (try? p.run()) != nil else { return "Couldn't run \(exe)." }
        let deadline = Date().addingTimeInterval(seconds)
        while p.isRunning && Date() < deadline { usleep(100_000) }
        if p.isRunning { p.terminate() }
        usleep(100_000)
        out.lock.lock(); defer { out.lock.unlock() }
        return out.text
    }

    func send(_ prompt: String) {
        let text = prompt.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty, !answering, !chatModel.isEmpty else { return }
        chat.append(ChatMessage(role: "user", text: text))
        chat.append(ChatMessage(role: "assistant", text: ""))
        answering = true
        let history = chat.dropLast().map { ["role": $0.role, "content": $0.text] }
        let model = chatModel
        used(model)
        chatTask = Task {
            do {
                guard let url = URL(string: OllamaAPI.base(port) + "/api/chat") else { return }
                var req = URLRequest(url: url, timeoutInterval: 600)
                req.httpMethod = "POST"
                req.httpBody = try JSONSerialization.data(withJSONObject: ["model": model, "messages": history, "stream": true])
                let (bytes, _) = try await URLSession.shared.bytes(for: req)
                for try await line in bytes.lines {
                    try Task.checkCancellation()
                    guard let piece = OllamaAPI.parseChatLine(line) else { continue }
                    if let e = piece.error { chat[chat.count - 1].text += "\n[\(e)]"; break }
                    chat[chat.count - 1].text += piece.text
                    if piece.done { break }
                }
            } catch is CancellationError {
                chat[chat.count - 1].text += " [stopped]"
            } catch {
                chat[chat.count - 1].text += "\n[\(error.localizedDescription)]"
            }
            answering = false
        }
    }

    func stopChat() { chatTask?.cancel() }
    func clearChat() { stopChat(); chat = [] }
}

struct OllamaPane: View {
    @EnvironmentObject var sup: Supervisor
    @EnvironmentObject var setup: SetupModel
    @ObservedObject var ollama: OllamaModel
    @State private var newModel = ""
    @State private var prompt = ""
    @State private var askInstall = false
    @State private var list = "Popular"

    var body: some View {
        let state = sup.state(.ollama)
        HSplitView {
            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    status(state)
                    if state.isRunning {
                        modelsSection
                        accountSection
                        findSection
                    }
                }
                .padding(20)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .frame(minWidth: 340, idealWidth: 420)
            chatSection(running: state.isRunning).frame(minWidth: 360)
        }
        .background(Color(nsColor: .windowBackgroundColor))
        .task(id: state.isRunning) { if state.isRunning { await ollama.refresh() } }
        .alert("Install Ollama", isPresented: $askInstall) {
            Button("Download and Install") { Task { await setup.run(.installOllama, for: .ollama) } }
            Button("Cancel", role: .cancel) {}
        } message: { Text(Fix.installOllama.confirm ?? "") }
    }

    @ViewBuilder func status(_ state: ServiceState) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Ollama").font(.largeTitle.bold())
            Text(SetupPart.ollama.explainer).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            HStack(spacing: 8) {
                StatusDot(state: state)
                Text(StatusText.of(state)).textSelection(.enabled)
            }
            if setup.running == .ollama {
                ProgressRow(progress: setup.progress) { setup.cancel() }
            } else if let r = setup.results[.ollama], !r.text.isEmpty {
                Label(r.text, systemImage: r.ok ? "checkmark" : "xmark.octagon").foregroundStyle(r.ok ? .green : .red)
                    .fixedSize(horizontal: false, vertical: true)
            }
            HStack {
                switch state {
                case .notInstalled:
                    Button("Download and Install…") { askInstall = true }.buttonStyle(.borderedProminent)
                        .disabled(setup.running != nil)
                    Button("Get It from ollama.com") { NSWorkspace.shared.open(URL(string: "https://ollama.com/download")!) }
                        .buttonStyle(.link)
                case .stopped, .failed:
                    Button("Start Ollama") { sup.start(.ollama) }.buttonStyle(.borderedProminent)
                case .starting:
                    ProgressView().controlSize(.small)
                case .running(let external):
                    if !external { Button("Stop Ollama") { sup.stop(.ollama) } }
                }
            }
        }
    }

    var modelsSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Models").font(.title2.bold())
            let missing = Models.missing(recommended: Models.recommended(memoryGB: Int(ProcessInfo.processInfo.physicalMemory / 1_073_741_824)), installed: ollama.models.map(\.name))
            ForEach(missing, id: \.self) { m in
                HStack {
                    Image(systemName: "star").foregroundStyle(.orange)
                    Text(m).font(.body.monospaced())
                    Text("recommended, about \(Models.sizes[m] ?? "?")").foregroundStyle(.secondary)
                    Spacer()
                    Button("Download") { ollama.pull(m) }.disabled(ollama.pulling != nil)
                }
            }
            ForEach(ollama.models) { m in
                HStack {
                    Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
                    Text(m.name).font(.body.monospaced()).textSelection(.enabled)
                    Spacer()
                    Text(m.size).foregroundStyle(.secondary)
                }
            }
            if ollama.models.isEmpty { Text("No models yet.").foregroundStyle(.secondary) }
            HStack {
                TextField("Model name, for example qwen3:4b", text: $newModel)
                    .textFieldStyle(.roundedBorder)
                    .onSubmit { ollama.pull(newModel) }
                Button("Download") { ollama.pull(newModel) }.disabled(newModel.isEmpty || ollama.pulling != nil)
            }
            if let m = ollama.pulling {
                PullRow(model: m, fraction: ollama.pullFraction, status: ollama.pullStatus) { ollama.stopPull() }
            } else if let r = ollama.pullResult {
                Label(r.text, systemImage: r.ok ? "checkmark" : "xmark.octagon").foregroundStyle(r.ok ? .green : .red)
            }
        }
    }

    var accountSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("ollama.com account").font(.title2.bold())
            Text("Optional. Signing in lets you run Ollama's cloud models, which run on ollama.com instead of this Mac. Sign In opens ollama.com in your browser to connect this Mac.")
                .foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            HStack {
                Button("Sign In…") { ollama.account("signin") }.disabled(ollama.signingIn)
                Button("Sign Out") { ollama.account("signout") }.disabled(ollama.signingIn)
                if ollama.signingIn { ProgressView().controlSize(.small); Text("Waiting for ollama.com…").foregroundStyle(.secondary) }
            }
            if !ollama.account.isEmpty {
                Text(ollama.account).font(.caption.monospaced()).textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    /// Popular, favourite and recent models in plain words, each one a click to download or use.
    var findSection: some View {
        let mem = Int(ProcessInfo.processInfo.physicalMemory / 1_073_741_824)
        let rows: [CatalogModel] = switch list {
        case "Favourites": ollama.favourites.map(ModelCatalog.row)
        case "Recent": ollama.recent.map(ModelCatalog.row)
        default: ModelCatalog.popular
        }
        return VStack(alignment: .leading, spacing: 10) {
            Text("Find a model").font(.title2.bold())
            Text("Models are the “brains” Ollama runs. Bigger ones know more and write better but need more memory and answer slower. This Mac has about \(mem) GB of memory.")
                .foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            Picker("List", selection: $list) {
                ForEach(["Popular", "Favourites", "Recent"], id: \.self) { Text($0).tag($0) }
            }
            .pickerStyle(.segmented).labelsHidden()
            if rows.isEmpty {
                Text(list == "Favourites" ? "Star a model to keep it here." : "Models you download or chat with show up here.")
                    .foregroundStyle(.secondary)
            }
            ForEach(rows) { m in CatalogRow(model: m, ollama: ollama, memoryGB: mem) }
            Button("More models on ollama.com") { NSWorkspace.shared.open(URL(string: "https://ollama.com/library")!) }
                .buttonStyle(.link).font(.callout)
        }
    }

    func chatSection(running: Bool) -> some View {
        VStack(spacing: 0) {
            HStack {
                Text("Chat").font(.title2.bold())
                Spacer()
                Picker("Model", selection: $ollama.chatModel) {
                    ForEach(ollama.chatModels) { m in Text(m.name).tag(m.name) }
                }
                .frame(maxWidth: 240)
                .disabled(!running || ollama.chatModels.isEmpty)
                Button("Clear") { ollama.clearChat() }.disabled(ollama.chat.isEmpty)
            }
            .padding(12)
            Divider()
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 10) {
                        if ollama.chat.isEmpty {
                            Text(running ? "Ask the model anything. It runs on this Mac." : "Start Ollama to chat.")
                                .foregroundStyle(.secondary).padding(.top, 20)
                        }
                        ForEach(ollama.chat) { m in
                            Text(m.text.isEmpty ? "…" : m.text)
                                .textSelection(.enabled)
                                .padding(10)
                                .background(m.role == "user" ? AnyShapeStyle(Color.accentColor.opacity(0.15)) : AnyShapeStyle(.quaternary.opacity(0.5)),
                                            in: RoundedRectangle(cornerRadius: 8))
                                .frame(maxWidth: .infinity, alignment: m.role == "user" ? .trailing : .leading)
                                .id(m.id)
                        }
                    }
                    .padding(12)
                }
                .onChange(of: ollama.chat.last?.text) { _, _ in if let id = ollama.chat.last?.id { proxy.scrollTo(id, anchor: .bottom) } }
            }
            Divider()
            HStack {
                TextField("Message", text: $prompt, axis: .vertical)
                    .textFieldStyle(.roundedBorder)
                    .lineLimit(1...5)
                    .onSubmit(send)
                if ollama.answering {
                    Button("Stop") { ollama.stopChat() }
                } else {
                    Button("Send", action: send).keyboardShortcut(.return, modifiers: .command)
                        .disabled(!running || prompt.isEmpty || ollama.chatModel.isEmpty)
                }
            }
            .padding(12)
        }
    }

    func send() {
        guard !ollama.answering else { return }
        ollama.send(prompt)
        prompt = ""
    }
}

struct CatalogRow: View {
    let model: CatalogModel
    @ObservedObject var ollama: OllamaModel
    let memoryGB: Int

    var body: some View {
        let installed = ollama.isInstalled(model.name)
        let fits = model.fits(memoryGB: memoryGB)
        HStack(alignment: .top, spacing: 10) {
            Button { ollama.toggleFavourite(model.name) } label: {
                Image(systemName: ollama.isFavourite(model.name) ? "star.fill" : "star")
                    .foregroundStyle(ollama.isFavourite(model.name) ? .yellow : .secondary)
            }
            .buttonStyle(.plain)
            .help(ollama.isFavourite(model.name) ? "Remove from favourites" : "Add to favourites")
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline) {
                    Text(model.name).font(.body.monospaced()).textSelection(.enabled)
                    if !model.blurb.isEmpty {
                        Text(model.kind.rawValue).font(.caption).padding(.horizontal, 6).padding(.vertical, 1)
                            .background(.quaternary, in: Capsule())
                    }
                }
                if !model.blurb.isEmpty {
                    Text(model.blurb).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                }
                HStack(spacing: 6) {
                    if model.sizeGB > 0 || model.isCloud { Text(model.size).font(.caption).foregroundStyle(.secondary) }
                    if !fits { Label("Needs more memory than this Mac has", systemImage: "exclamationmark.triangle").font(.caption).foregroundStyle(.orange) }
                }
            }
            Spacer(minLength: 4)
            if installed {
                Button("Use in Chat") { ollama.useInChat(model.name) }.disabled(!model.blurb.isEmpty && model.kind == .search)
            } else {
                Button("Download") { ollama.pull(model.name) }.disabled(ollama.pulling != nil)
            }
        }
        .padding(.vertical, 4)
    }
}

struct PullRow: View {
    let model: String
    let fraction: Double?
    let status: String
    let stop: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                if let fraction { ProgressView(value: fraction) } else { ProgressView().progressViewStyle(.linear) }
                Button("Stop", action: stop).controlSize(.small)
            }
            Text("\(model): \(status)").font(.caption.monospaced()).foregroundStyle(.secondary).lineLimit(1)
        }
    }
}
