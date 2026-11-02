import AppKit
import SwiftUI
import WebKit

enum Pane: String, CaseIterable, Identifiable {
    case overview, setup, agent, research, tasks, ollama, logs
    var id: String { rawValue }

    var title: String {
        switch self {
        case .overview: "Overview"
        case .setup: "Setup"
        case .agent: "Agent"
        case .research: "Research"
        case .tasks: "Tasks"
        case .ollama: "Ollama"
        case .logs: "Logs"
        }
    }

    var icon: String {
        switch self {
        case .overview: "square.grid.2x2"
        case .setup: "wrench.and.screwdriver"
        case .agent: "brain"
        case .research: "doc.text.magnifyingglass"
        case .tasks: "calendar.badge.clock"
        case .ollama: "cpu"
        case .logs: "text.alignleft"
        }
    }

    /// The part whose page this pane shows.
    var service: ServiceID? {
        switch self {
        case .agent: .agent
        case .research: .research
        case .tasks: .tasksWeb
        default: nil
        }
    }

    static let pages: [Pane] = allCases.filter { $0.service != nil }

    /// The parts this pane's own Start and Stop act on; nothing else is touched, so stopping
    /// Research leaves the Agent running and the other way round. Tasks is both sched parts.
    var parts: [ServiceID] {
        switch self {
        case .agent: [.agent]
        case .research: [.research]
        case .tasks: [.tasksDaemon, .tasksWeb]
        case .ollama: [.ollama]
        default: []
        }
    }
}

struct ContentView: View {
    @EnvironmentObject var sup: Supervisor
    @EnvironmentObject var setup: SetupModel
    @EnvironmentObject var nav: Navigation
    @EnvironmentObject var ollama: OllamaModel
    @StateObject private var pages = PageStore()
    /// Page panes opened so far. Their web views stay in the window from then on, shown or
    /// hidden, so switching panes never moves a web view (which used to leave clicks stuck).
    @State private var opened: Set<Pane> = []

    var body: some View {
        // A click on empty sidebar space would clear the selection; keep the pane instead.
        let selection = Binding<Pane?>(get: { nav.pane }, set: { if let p = $0 { nav.pane = p } })
        NavigationSplitView {
            List(Pane.allCases, selection: selection) { p in
                HStack {
                    Label(p.title, systemImage: p.icon)
                    Spacer()
                    if let id = p.service ?? (p == .ollama ? .ollama : nil) { StatusDot(state: sup.state(id)) }
                }
                .badge(p == .setup ? setup.problemCount : 0)
                .tag(p)
            }
            .navigationSplitViewColumnWidth(min: 170, ideal: 190)
        } detail: {
            ZStack {
                ForEach(Pane.pages.filter(opened.contains)) { p in
                    PageView(service: p.service!, title: p.title, pages: pages, visible: nav.pane == p)
                }
                switch nav.pane {
                case .overview: OverviewView()
                case .setup: SetupView()
                case .ollama: OllamaPane(ollama: ollama)
                case .logs: LogsView()
                default: EmptyView()
                }
            }
            .navigationTitle(nav.pane.title)
        }
        .onAppear { if nav.pane.service != nil { opened.insert(nav.pane) } }
        .onChange(of: nav.pane) { _, p in if p.service != nil { opened.insert(p) } }
        .toolbar {
            if let id = nav.pane.service {
                ToolbarItemGroup(placement: .navigation) {
                    let page = sup.spec(id).page
                    Button { if let page { pages.reload(id, url: page) } } label: {
                        Label("Reload", systemImage: "arrow.clockwise")
                    }
                    .help("Reload this page")
                    .disabled(!sup.state(id).isRunning)
                    Button { if let page { NSWorkspace.shared.open(page) } } label: {
                        Label("Open in Browser", systemImage: "safari")
                    }
                    .help("Open this page in your browser")
                    .disabled(!sup.state(id).isRunning)
                }
            }
            if nav.pane.parts.isEmpty {
                ToolbarItemGroup {
                    Text("\(sup.runningCount) of \(sup.specs.count) running").foregroundStyle(.secondary)
                    Button { Task { await sup.startAll() } } label: { Label("Start All", systemImage: "play.fill") }
                        .help("Start every part that's installed and not running")
                        .disabled(sup.busy)
                    Button { sup.stopAll() } label: { Label("Stop All", systemImage: "stop.fill") }
                        .help("Stop everything this app started")
                        .disabled(!sup.ownsAny)
                }
            } else {
                PartControls(title: nav.pane.title, parts: nav.pane.parts)
            }
        }
    }
}

/// Start, Stop and Restart for one pane's parts only.
struct PartControls: ToolbarContent {
    @EnvironmentObject var sup: Supervisor
    let title: String
    let parts: [ServiceID]

    var body: some ToolbarContent {
        let startable = parts.filter { sup.spec($0).installed && !sup.state($0).isRunning && sup.state($0) != .starting }
        let owned = parts.filter(sup.owns)
        ToolbarItemGroup {
            Text(title).foregroundStyle(.secondary)
            Button { startable.forEach(sup.start) } label: { Label("Start \(title)", systemImage: "play.fill") }
                .help("Start \(title) only")
                .disabled(startable.isEmpty)
            Button { owned.forEach(sup.stop) } label: { Label("Stop \(title)", systemImage: "stop.fill") }
                .help(owned.isEmpty && parts.contains(where: { sup.state($0).isRunning })
                      ? "\(title) was started outside the app, so the app leaves it running"
                      : "Stop \(title) only; the other parts keep running")
                .disabled(owned.isEmpty)
            Button { owned.forEach(sup.restart) } label: { Label("Restart \(title)", systemImage: "arrow.triangle.2.circlepath") }
                .help("Restart \(title) only")
                .disabled(owned.isEmpty)
        }
    }
}

struct StatusDot: View {
    let state: ServiceState
    var body: some View {
        Circle().fill(color).frame(width: 8, height: 8).help(StatusText.of(state))
    }
    var color: Color {
        switch state {
        case .running: .green
        case .starting: .yellow
        case .failed: .red
        case .stopped, .notInstalled: .gray.opacity(0.5)
        }
    }
}

enum StatusText {
    static func of(_ s: ServiceState) -> String {
        switch s {
        case .notInstalled: "Not installed"
        case .stopped: "Stopped"
        case .starting: "Starting…"
        case .running(let external): external ? "Running (started outside the app)" : "Running"
        case .failed(let why): "Failed. \(why)"
        }
    }
}

// MARK: Overview

struct OverviewView: View {
    @EnvironmentObject var sup: Supervisor
    @EnvironmentObject var setup: SetupModel
    @EnvironmentObject var nav: Navigation

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                Text("noskill").font(.largeTitle.bold())
                Text("Local AI on this Mac: the noskill agent, orcastrator research reports and sched tasks, all on Ollama.")
                    .foregroundStyle(.secondary)
                if setup.problemCount > 0 {
                    HStack {
                        Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                        Text(setup.problemCount == 1 ? "1 part needs setting up." : "\(setup.problemCount) parts need setting up.")
                        Spacer()
                        Button("Open Setup") { nav.pane = .setup }.buttonStyle(.borderedProminent)
                    }
                    .padding(12)
                    .background(.orange.opacity(0.12), in: RoundedRectangle(cornerRadius: 8))
                }
                ForEach(sup.specs) { s in ServiceRow(spec: s) }
                Text("Settings: \(AppConfig.fileURL.path)").font(.caption).foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
            .padding(24)
            .frame(maxWidth: 820, alignment: .leading)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(Color(nsColor: .windowBackgroundColor))
    }
}

struct ServiceRow: View {
    @EnvironmentObject var sup: Supervisor
    @EnvironmentObject var nav: Navigation
    let spec: ServiceSpec

    var body: some View {
        let state = sup.state(spec.id)
        HStack(alignment: .top, spacing: 12) {
            StatusDot(state: state).padding(.top, 6)
            VStack(alignment: .leading, spacing: 3) {
                HStack {
                    Text(spec.name).font(.headline)
                    if let port = spec.port { Text(":\(String(port))").font(.caption.monospaced()).foregroundStyle(.secondary) }
                }
                Text(spec.detail).font(.callout).foregroundStyle(.secondary)
                Text(StatusText.of(state)).font(.callout)
                    .foregroundStyle(state == .notInstalled || { if case .failed = state { true } else { false } }() ? .red : .primary)
                    .textSelection(.enabled)
            }
            Spacer()
            buttons(state)
        }
        .padding(12)
        .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 8))
    }

    @ViewBuilder func buttons(_ state: ServiceState) -> some View {
        switch state {
        case .running(external: true):
            Text("Started outside the app").font(.caption).foregroundStyle(.secondary)
        case .running, .starting:
            HStack {
                Button("Restart") { sup.restart(spec.id) }
                Button("Stop") { sup.stop(spec.id) }
            }
        case .stopped, .failed:
            Button("Start") { sup.start(spec.id) }.buttonStyle(.borderedProminent)
        case .notInstalled:
            Button("Set Up…") { nav.pane = .setup }
        }
    }
}

// MARK: Setup

struct SetupView: View {
    @EnvironmentObject var sup: Supervisor
    @EnvironmentObject var setup: SetupModel
    @EnvironmentObject var nav: Navigation

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                if setup.firstRun {
                    Text("Welcome to noskill").font(.largeTitle.bold())
                    Text("noskill runs AI on this Mac, privately: what you ask stays on this computer. It brings four parts together, and this page checks each one and fixes what's missing. Nothing is downloaded or changed until you say so.")
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                } else {
                    Text("Setup").font(.largeTitle.bold())
                    Text("What's on this Mac, with a fix for anything missing or broken.").foregroundStyle(.secondary)
                }
                HStack(spacing: 10) {
                    summary
                    Spacer()
                    if setup.checking { ProgressView().controlSize(.small) }
                    Button("Check Again") { Task { await setup.check() } }
                        .disabled(setup.checking || setup.running != nil)
                }
                ForEach(SetupPart.allCases) { part in SetupRow(part: part) }
                if setup.firstRun || setup.problemCount == 0 { footer }
                Text("Settings: \(AppConfig.fileURL.path)").font(.caption).foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
            .padding(24)
            .frame(maxWidth: 820, alignment: .leading)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(Color(nsColor: .windowBackgroundColor))
    }

    @ViewBuilder var summary: some View {
        if !setup.checked {
            Text("Checking this Mac…").font(.headline)
        } else if setup.problemCount == 0 {
            Label("Everything is ready.", systemImage: "checkmark.circle.fill").font(.headline).foregroundStyle(.green)
        } else {
            Label(setup.problemCount == 1 ? "1 thing needs attention." : "\(setup.problemCount) things need attention.",
                  systemImage: "exclamationmark.triangle.fill")
                .font(.headline).foregroundStyle(.orange)
        }
    }

    var footer: some View {
        HStack {
            Button {
                setup.finish()
                nav.pane = .overview
                Task { await sup.startAll() }
            } label: { Text(setup.problemCount == 0 ? "Start noskill" : "Start What's Ready").frame(minWidth: 140) }
                .buttonStyle(.borderedProminent).controlSize(.large)
                .disabled(!setup.checked || setup.running != nil)
            if setup.problemCount > 0 {
                Text("You can come back to Setup from the sidebar at any time.").font(.callout).foregroundStyle(.secondary)
            }
        }
        .padding(.top, 6)
    }
}

struct SetupRow: View {
    @EnvironmentObject var sup: Supervisor
    @EnvironmentObject var setup: SetupModel
    let part: SetupPart
    @State private var asking: Fix?

    var body: some View {
        let finding = setup.finding(part)
        let isRunning = setup.running == part
        HStack(alignment: .top, spacing: 12) {
            icon(finding, isRunning).frame(width: 22).padding(.top, 2)
            VStack(alignment: .leading, spacing: 5) {
                HStack(alignment: .firstTextBaseline) {
                    Text(part.name).font(.headline)
                    if let p = part.project { Text(p).font(.callout).foregroundStyle(.secondary) }
                }
                Text(part.explainer).font(.callout).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                if let finding {
                    Text(finding.summary).font(.callout.weight(.medium)).textSelection(.enabled)
                        .padding(.top, 2)
                    if let why = finding.why {
                        Text(why).font(.callout).foregroundStyle(.secondary).textSelection(.enabled)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                if let r = setup.results[part], !r.text.isEmpty {
                    Label(r.text, systemImage: r.ok ? "checkmark" : "xmark.octagon")
                        .font(.callout).foregroundStyle(r.ok ? .green : .red).textSelection(.enabled)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if isRunning { ProgressRow(progress: setup.progress) { setup.cancel() } }
                buttons(finding, isRunning)
            }
            Spacer(minLength: 0)
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(background(finding), in: RoundedRectangle(cornerRadius: 10))
        .alert(asking?.title ?? "", isPresented: Binding(get: { asking != nil }, set: { if !$0 { asking = nil } }), presenting: asking) { fix in
            Button("Continue") { Task { await setup.run(fix, for: part) } }
            Button("Cancel", role: .cancel) {}
        } message: { fix in
            Text(fix.confirm ?? "")
        }
    }

    @ViewBuilder func icon(_ f: Finding?, _ isRunning: Bool) -> some View {
        if isRunning || f == nil {
            ProgressView().controlSize(.small)
        } else {
            switch f!.level {
            case .ok: Image(systemName: "checkmark.circle.fill").foregroundStyle(.green).font(.title3)
            case .note: Image(systemName: "info.circle.fill").foregroundStyle(.blue).font(.title3)
            case .problem: Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange).font(.title3)
            }
        }
    }

    func background(_ f: Finding?) -> some ShapeStyle {
        f?.level == .problem ? AnyShapeStyle(Color.orange.opacity(0.10)) : AnyShapeStyle(.quaternary.opacity(0.4))
    }

    @ViewBuilder func buttons(_ f: Finding?, _ isRunning: Bool) -> some View {
        let busy = setup.running != nil || setup.checking
        HStack(spacing: 10) {
            if let fix = f?.fix, !isRunning {
                Button(fix.title) { tap(fix) }
                    .buttonStyle(f?.level == .problem ? AnyPrimitiveButtonStyle(.borderedProminent) : AnyPrimitiveButtonStyle(.bordered))
                    .disabled(busy)
            }
            if f?.fix == .installOllama, !isRunning {
                Button("Get It from ollama.com Instead") { NSWorkspace.shared.open(URL(string: "https://ollama.com/download")!) }
                    .buttonStyle(.link)
            }
            if part.folderKey != nil, f?.fix != .pickFolder(part), !isRunning, part != .agent || f?.level != .ok {
                Button("Choose Another Folder…") { pickFolder() }
                    .buttonStyle(.link).disabled(busy)
            }
        }
        .padding(.top, 4)
    }

    func tap(_ fix: Fix) {
        if case .pickFolder = fix { pickFolder(); return }
        if fix.confirm != nil { asking = fix } else { Task { await setup.run(fix, for: part) } }
    }

    func pickFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.prompt = "Use This Folder"
        panel.message = "Choose the \(part.project ?? part.name) project folder."
        if let key = part.folderKey {
            panel.directoryURL = URL(fileURLWithPath: expandTilde(sup.config[keyPath: key])).deletingLastPathComponent()
        }
        guard panel.runModal() == .OK, let url = panel.url else { return }
        Task { await setup.setFolder(part, url) }
    }
}

/// Type-erased button style, so a row can pick prominent or plain at run time.
struct AnyPrimitiveButtonStyle: PrimitiveButtonStyle {
    private let make: (Configuration) -> AnyView
    init<S: PrimitiveButtonStyle>(_ s: S) { make = { AnyView(s.makeBody(configuration: $0)) } }
    func makeBody(configuration: Configuration) -> some View { make(configuration) }
}

struct ProgressRow: View {
    @ObservedObject var progress: RunProgress
    let stop: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                if let f = progress.fraction { ProgressView(value: f) } else { ProgressView().progressViewStyle(.linear) }
                Button("Stop", action: stop).controlSize(.small)
            }
            Text(progress.line).font(.caption.monospaced()).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
        }
        .padding(.top, 4)
    }
}

// MARK: Pages

/// Keeps each page's web view alive while you switch panes, so a page keeps its place.
@MainActor
final class PageStore: ObservableObject {
    private var views: [ServiceID: WKWebView] = [:]

    func view(for id: ServiceID) -> WKWebView {
        if let v = views[id] { return v }
        let v = WKWebView(frame: .zero, configuration: WKWebViewConfiguration())
        views[id] = v
        return v
    }

    /// Loads the page the first time, and reloads it after its part restarted.
    func load(_ id: ServiceID, url: URL) {
        let v = view(for: id)
        if v.url == nil { v.load(URLRequest(url: url)) } else { v.reload() }
    }

    func reload(_ id: ServiceID, url: URL) { load(id, url: url) }
}

struct PageView: View {
    @EnvironmentObject var sup: Supervisor
    @EnvironmentObject var nav: Navigation
    let service: ServiceID
    let title: String
    let pages: PageStore
    let visible: Bool

    var body: some View {
        let s = sup.spec(service)
        let state = sup.state(service)
        ZStack {
            WebView(webView: pages.view(for: service), visible: visible && state.isRunning)
            if !state.isRunning {
                VStack(spacing: 12) {
                    Image(systemName: "power").font(.largeTitle).foregroundStyle(.secondary)
                    Text("\(title) isn't running").font(.title2)
                    Text(StatusText.of(state)).foregroundStyle(.secondary).textSelection(.enabled)
                        .multilineTextAlignment(.center).frame(maxWidth: 520)
                    if state == .notInstalled {
                        Button("Open Setup") { nav.pane = .setup }.controlSize(.large).buttonStyle(.borderedProminent)
                    } else if state == .starting {
                        ProgressView()
                    } else {
                        Button("Start \(title)") { sup.start(service) }.controlSize(.large).buttonStyle(.borderedProminent)
                    }
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(Color(nsColor: .windowBackgroundColor))
            }
        }
        .opacity(visible ? 1 : 0)
        .allowsHitTesting(visible)
        .accessibilityHidden(!visible)
        .onAppear { if state.isRunning, let url = s.page { pages.load(service, url: url) } }
        .onChange(of: state.isRunning) { _, running in
            if running, let url = s.page { pages.load(service, url: url) }
        }
    }
}

/// Hosts a long-lived WKWebView in a plain container. The container is made once per page and
/// never moves; hiding it (rather than removing it) keeps AppKit from sending clicks to a page
/// you can't see.
struct WebView: NSViewRepresentable {
    let webView: WKWebView
    let visible: Bool

    func makeNSView(context: Context) -> NSView {
        let box = NSView()
        webView.translatesAutoresizingMaskIntoConstraints = true
        webView.autoresizingMask = [.width, .height]
        webView.removeFromSuperview()
        webView.frame = box.bounds
        box.addSubview(webView)
        return box
    }

    func updateNSView(_ box: NSView, context: Context) {
        box.isHidden = !visible
    }
}

// MARK: Logs

struct LogsView: View {
    @EnvironmentObject var sup: Supervisor
    @EnvironmentObject var logs: LogStore
    @State private var selected = ServiceID.agent.rawValue

    var body: some View {
        let text = logs.buffers[selected]?.text ?? ""
        VStack(alignment: .leading, spacing: 0) {
            Picker("Part", selection: $selected) {
                ForEach(sup.specs) { s in Text(s.name).tag(s.id.rawValue) }
                Text("Setup").tag(LogStore.setupKey)
                Text("Ollama pulls").tag("ollama")
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .padding(12)
            ScrollViewReader { proxy in
                ScrollView {
                    Text(text.isEmpty ? "No output yet. Only parts this app started, and Setup's fixes, show their output here." : text)
                        .font(.system(.caption, design: .monospaced))
                        .foregroundStyle(text.isEmpty ? .secondary : .primary)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .textSelection(.enabled)
                        .padding(12)
                    Color.clear.frame(height: 1).id("end")
                }
                .onChange(of: text) { _, _ in proxy.scrollTo("end", anchor: .bottom) }
            }
        }
        .background(Color(nsColor: .windowBackgroundColor))
    }
}

// MARK: Menu bar

struct MenuContent: View {
    @EnvironmentObject var sup: Supervisor
    @EnvironmentObject var setup: SetupModel
    @EnvironmentObject var nav: Navigation
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        ForEach(sup.specs) { s in Text("\(s.name): \(StatusText.of(sup.state(s.id)))") }
        Divider()
        Button("Open noskill") { open(nil) }
        Button(setup.problemCount > 0 ? "Setup (\(setup.problemCount) to fix)…" : "Setup…") { open(.setup) }
        Button("Start All") { Task { await sup.startAll() } }.disabled(sup.busy)
        Button("Stop All") { sup.stopAll() }.disabled(!sup.ownsAny)
        Divider()
        Button("Quit noskill") { NSApp.terminate(nil) }.keyboardShortcut("q")
    }

    func open(_ pane: Pane?) {
        if let pane { nav.pane = pane }
        openWindow(id: "main")
        NSApp.activate()
    }
}

struct MenuLabel: View {
    @ObservedObject var sup: Supervisor
    var body: some View {
        let allUp = sup.runningCount == sup.specs.count
        Image(systemName: allUp ? "brain.filled.head.profile" : "brain.head.profile")
    }
}
