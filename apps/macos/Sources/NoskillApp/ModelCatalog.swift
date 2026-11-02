import Foundation

// The Ollama pane's "Find a model" lists (docs/APP.md "Ollama"): a short, built-in guide to
// popular models in plain words, so nobody has to know LLM names or browse ollama.com, plus the
// person's favourites and recent models. Nothing here touches the network.

struct CatalogModel: Identifiable, Equatable {
    enum Kind: String, CaseIterable {
        case chat = "Everyday chat"
        case reasoning = "Careful reasoning"
        case code = "Code"
        case images = "Reads images"
        case search = "Search and documents"
        case cloud = "Cloud (needs sign-in)"
    }

    let name: String
    let kind: Kind
    /// Download size in GB (about); 0 for a cloud model, which downloads nothing.
    let sizeGB: Double
    /// What it's good for, for someone who doesn't know models.
    let blurb: String
    var id: String { name }

    var isCloud: Bool { kind == .cloud }
    var size: String { isCloud ? "runs on ollama.com" : sizeGB < 1 ? "\(Int((sizeGB * 1000).rounded())) MB" : String(format: "%.1f GB", sizeGB) }

    /// A model runs well when this Mac has about 1.5× its size in memory, leaving room for the
    /// rest of the system. Unknown memory (0) assumes it fits.
    func fits(memoryGB: Int) -> Bool { isCloud || memoryGB == 0 || sizeGB * 1.5 <= Double(memoryGB) }
}

enum ModelCatalog {
    static let popular: [CatalogModel] = [
        CatalogModel(name: "granite4:3b", kind: .chat, sizeGB: 2.1,
                     blurb: "noskill's main model. Quick, follows instructions well and can use tools."),
        CatalogModel(name: "gemma3:1b", kind: .chat, sizeGB: 0.8,
                     blurb: "Tiny and very fast. Good for short answers on any Mac; noskill's backup model."),
        CatalogModel(name: "llama3.2:3b", kind: .chat, sizeGB: 2.0,
                     blurb: "Meta's small all-rounder. Friendly answers and summaries."),
        CatalogModel(name: "qwen3:4b", kind: .reasoning, sizeGB: 2.5,
                     blurb: "Thinks step by step before it answers: slower, but better at tricky questions."),
        CatalogModel(name: "phi4-mini:3.8b", kind: .reasoning, sizeGB: 2.5,
                     blurb: "Microsoft's small model, strong at maths and logic for its size."),
        CatalogModel(name: "llama3.1:8b", kind: .chat, sizeGB: 4.9,
                     blurb: "A bigger, more knowledgeable all-rounder for Macs with 8 GB or more to spare."),
        CatalogModel(name: "qwen3:8b", kind: .reasoning, sizeGB: 5.2,
                     blurb: "A larger step-by-step thinker: better answers to hard questions, takes longer."),
        CatalogModel(name: "deepseek-r1:8b", kind: .reasoning, sizeGB: 5.2,
                     blurb: "Shows its reasoning as it works through a problem."),
        CatalogModel(name: "gemma3:4b", kind: .images, sizeGB: 3.3,
                     blurb: "Google's model that can also look at pictures and describe them."),
        CatalogModel(name: "gemma3:12b", kind: .images, sizeGB: 8.1,
                     blurb: "A bigger Gemma: better writing and image understanding; needs plenty of memory."),
        CatalogModel(name: "qwen2.5-coder:7b", kind: .code, sizeGB: 4.7,
                     blurb: "Writes, explains and fixes code."),
        CatalogModel(name: "mistral:7b", kind: .chat, sizeGB: 4.1,
                     blurb: "A well-known general model, good at writing and summarising."),
        CatalogModel(name: "gpt-oss:20b", kind: .reasoning, sizeGB: 14,
                     blurb: "OpenAI's open model. Strong reasoning, but needs a Mac with lots of memory."),
        CatalogModel(name: "nomic-embed-text", kind: .search, sizeGB: 0.27,
                     blurb: "Doesn't chat. Lets noskill search your documents by meaning."),
        CatalogModel(name: "gpt-oss:120b-cloud", kind: .cloud, sizeGB: 0,
                     blurb: "A very large model that runs on ollama.com, not this Mac, so what you ask leaves the computer. Needs sign-in."),
    ]

    static func entry(_ name: String) -> CatalogModel? { popular.first { same($0.name, name) } }

    /// `name` and `name:latest` are the same model.
    static func same(_ a: String, _ b: String) -> Bool {
        func norm(_ s: String) -> String { s.hasSuffix(":latest") ? String(s.dropLast(7)) : s }
        return norm(a) == norm(b)
    }

    /// A list row for any name: the guide's entry when there is one, else a plain one.
    static func row(_ name: String) -> CatalogModel {
        entry(name) ?? CatalogModel(name: name, kind: .chat, sizeGB: 0, blurb: "")
    }
}

/// Favourites and recent models, kept in the app's own preferences (not app.json: they're
/// per person, not settings).
enum ModelLists {
    static let maxRecent = 8

    /// Puts `name` first, once, keeping at most `max`.
    static func bump(_ list: [String], _ name: String, max: Int = maxRecent) -> [String] {
        Array(([name] + list.filter { !ModelCatalog.same($0, name) }).prefix(max))
    }

    static func toggle(_ list: [String], _ name: String) -> [String] {
        list.contains { ModelCatalog.same($0, name) } ? list.filter { !ModelCatalog.same($0, name) } : list + [name]
    }
}
