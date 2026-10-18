"""Generate tools-50.jsonl and tools-world.json (PHASE0_TESTS.md, datasets).

The world is a mock: invented files, pages and search results, a fixed clock.
Expected answers that need computing are computed here, not typed by hand.
Run: python3 spikes/data/make_tools50.py
"""
import datetime as dt
import hashlib
import json
from pathlib import Path
from zoneinfo import ZoneInfo

HERE = Path(__file__).parent
NOW_UTC = "2026-10-15T08:30:00+00:00"
HOME = "/Users/me"
LOCAL_TZ = "Europe/London"

now = dt.datetime.fromisoformat(NOW_UTC)
def local(tz):
    return now.astimezone(ZoneInfo(tz))

WORLD = {
    "now_utc": NOW_UTC,
    "local_tz": LOCAL_TZ,
    "home": HOME,
    "files": [
        "~/Downloads/invoice-0412.pdf",
        "~/Downloads/report-q3.pdf",
        "~/Downloads/IMG_2041.jpg",
        "~/Downloads/IMG_2042.jpg",
        "~/Downloads/holiday.png",
        "~/Downloads/setup-ollama.dmg",
        "~/Downloads/notes.txt",
        "~/Downloads/[SubsPlease] Frieren - 03 (1080p).mkv",
        "~/Documents/budget-2026.xlsx",
        "~/Documents/PDFs/tax-2025.pdf",
        "~/Documents/Notes/ideas.md",
        "~/Pictures/Screenshots/shot-001.png",
        "~/Desktop/todo.md",
        "~/Desktop/screenshot-1.png",
        "~/Projects/noskill/README.md",
        "~/Projects/noskill/go.mod",
        "/Volumes/AnimeDrive/Anime/Frieren/Frieren - 01.mkv",
        "/Volumes/AnimeDrive/Anime/Frieren/Frieren - 02.mkv",
        "/Volumes/AnimeDrive/Anime/Dandadan/Dandadan - 01.mkv",
    ],
    "tools": [
        {"name": "search", "description": "Search the web. Returns a list of results with title, url and snippet.",
         "parameters": {"type": "object", "properties": {"query": {"type": "string", "description": "Search query"}}, "required": ["query"]}},
        {"name": "fetch", "description": "Download a web page or file by URL and return its text.",
         "parameters": {"type": "object", "properties": {"url": {"type": "string", "description": "Full URL starting with http(s)://"}}, "required": ["url"]}},
        {"name": "list_dir", "description": "List the files and folders in a folder on this computer. Folders end with '/'.",
         "parameters": {"type": "object", "properties": {"path": {"type": "string", "description": "Folder path, e.g. ~/Downloads or /Volumes/<drive>"}}, "required": ["path"]}},
        {"name": "move", "description": "Move or rename a file. If dst is an existing folder the file keeps its name. Never overwrites.",
         "parameters": {"type": "object", "properties": {"src": {"type": "string", "description": "Current file path"}, "dst": {"type": "string", "description": "New file path or destination folder"}}, "required": ["src", "dst"]}},
        {"name": "run_python", "description": "Run a short Python 3 program (standard library only, no network, no file access) and return what it prints. Use print() for the result.",
         "parameters": {"type": "object", "properties": {"code": {"type": "string", "description": "Python source code"}}, "required": ["code"]}},
        {"name": "get_time", "description": "Get the current date, time and weekday. Without a timezone it returns the user's local time.",
         "parameters": {"type": "object", "properties": {"timezone": {"type": "string", "description": "Optional IANA timezone, e.g. Asia/Tokyo"}}, "required": []}},
    ],
}

def hm(t):
    return t.strftime("%H:%M").lstrip("0") or "0:00"

def any_re(*xs):
    return {"answer_any": list(xs)}

def call(tool, **matchers):
    return {"tool": tool, "args": matchers}

T = []
def task(kind, prompt, expect_calls=(), checks=None, fixtures=None, note=None):
    T.append({"kind": kind, "prompt": prompt, "expect_calls": list(expect_calls),
              "checks": checks or {}, "fixtures": fixtures or {}, **({"note": note} if note else {})})

lon, tok, nyc, la = local(LOCAL_TZ), local("Asia/Tokyo"), local("America/New_York"), local("America/Los_Angeles")
weekday = lon.strftime("%A")
hours_to_midnight = (dt.datetime.combine(lon.date() + dt.timedelta(days=1), dt.time(), lon.tzinfo) - lon).total_seconds() / 3600
days_xmas = (dt.date(2026, 12, 25) - lon.date()).days
days_between = (dt.date(2026, 11, 2) - dt.date(2026, 3, 14)).days
sha12 = hashlib.sha256(b"noskill").hexdigest()[:12]

# ---------------- single-call (20) ----------------
task("single", "What time is it right now?", [call("get_time")], any_re(rf"\b0?{hm(lon)}\b"))
task("single", "What's the time in Tokyo right now?", [call("get_time", timezone={"contains": "tokyo"})],
     any_re(rf"\b{hm(tok)}\b", rf"\b{int(tok.strftime('%I'))}:{tok.strftime('%M')}\s*pm\b"))
task("single", "What day of the week is it today?", [call("get_time")], any_re(weekday))
task("single", "How many hours are left until midnight here?", [call("get_time")],
     any_re(r"14\.5", r"14\s*(hours?|h)\s*(and\s*)?30", r"14:30", r"fourteen and a half"),
     note=f"local time {hm(lon)} → {hours_to_midnight} h")
task("single", "What's in my Downloads folder?", [call("list_dir", path={"path": "~/Downloads"})],
     {"answer_all": [r"invoice-0412", r"setup-ollama"]})
task("single", "How many PDF files are in my Downloads folder?", [call("list_dir", path={"path": "~/Downloads"})],
     any_re(r"\b2\b", r"\btwo\b"))
task("single", "Which Frieren episodes do I already have on my external drive AnimeDrive?",
     [call("list_dir", path={"contains": "animedrive"})], {"answer_all": [r"0?1\b", r"0?2\b"]})
task("single", "Is there a todo file on my Desktop?", [call("list_dir", path={"path": "~/Desktop"})], any_re(r"todo\.md", r"\byes\b"))
task("single", "Move invoice-0412.pdf from my Downloads into Documents/PDFs.",
     [call("move", src={"path": "~/Downloads/invoice-0412.pdf"}, dst={"contains": "documents/pdfs"})],
     {"fs_exists": ["~/Documents/PDFs/invoice-0412.pdf"], "fs_missing": ["~/Downloads/invoice-0412.pdf"]})
task("single", "Rename ~/Desktop/screenshot-1.png to error-dialog.png and keep it on the Desktop.",
     [call("move", src={"path": "~/Desktop/screenshot-1.png"}, dst={"path": "~/Desktop/error-dialog.png"})],
     {"fs_exists": ["~/Desktop/error-dialog.png"], "fs_missing": ["~/Desktop/screenshot-1.png"]})
task("single", "Put notes.txt from Downloads into my Documents/Notes folder.",
     [call("move", src={"path": "~/Downloads/notes.txt"}, dst={"contains": "documents/notes"})],
     {"fs_exists": ["~/Documents/Notes/notes.txt"], "fs_missing": ["~/Downloads/notes.txt"]})
task("single", "What is 17.5% of 2,340? Use Python so it's exact.", [call("run_python", code={"contains": "2340"})], any_re(r"409\.5\b"))
task("single", "How many days are there between 2026-03-14 and 2026-11-02? Calculate it with Python.",
     [call("run_python", code={"contains": "2026"})], any_re(rf"\b{days_between}\b"))
task("single", "Compute the SHA-256 hash of the string 'noskill' and give me the first 12 hex characters.",
     [call("run_python", code={"contains": "sha256"})], any_re(sha12))
task("single", "Convert 72 °F to Celsius, rounded to one decimal place. Use Python.",
     [call("run_python", code={"contains": "72"})], any_re(r"22\.2\b"))
task("single", "Search the web for the latest Ollama release version.",
     [call("search", query={"contains": "ollama"})], any_re(r"0\.40\.3"),
     {"search": [{"title": "Releases · ollama/ollama", "url": "https://github.com/ollama/ollama/releases", "snippet": "v0.40.3 (latest) · released Oct 9, 2026 · Fixes for decision models on Apple Silicon."},
                 {"title": "Ollama blog", "url": "https://ollama.com/blog", "snippet": "News and release notes from the Ollama team."}]})
task("single", "Find the official website of the Jellyfin project.",
     [call("search", query={"contains": "jellyfin"})], any_re(r"jellyfin\.org"),
     {"search": [{"title": "Jellyfin: The Free Software Media System", "url": "https://jellyfin.org/", "snippet": "Jellyfin is the volunteer-built media solution that puts you in control."},
                 {"title": "jellyfin/jellyfin - GitHub", "url": "https://github.com/jellyfin/jellyfin", "snippet": "The Free Software Media System - Server Backend & API."}]})
task("single", "Who directed the anime Frieren: Beyond Journey's End? Search to confirm.",
     [call("search", query={"contains": "frieren"})], any_re(r"sait[oō]"),
     {"search": [{"title": "Frieren: Beyond Journey's End (TV series)", "url": "https://en.wikipedia.org/wiki/Frieren", "snippet": "The anime television series adaptation produced by Madhouse is directed by Keiichirō Saitō."}]})
task("single", "Fetch https://status.example.com/api.json and tell me whether the service is up.",
     [call("fetch", url={"eq": "https://status.example.com/api.json"})], any_re(r"degraded"),
     {"pages": {"https://status.example.com/api.json": "{\"service\": \"api\", \"status\": \"degraded\", \"open_incidents\": 1, \"updated\": \"2026-10-15T08:12:00Z\"}"}})
task("single", "What's the title of the page at https://blog.example.org/post/42?",
     [call("fetch", url={"eq": "https://blog.example.org/post/42"})], any_re(r"shipping noskill 0\.1"),
     {"pages": {"https://blog.example.org/post/42": "<html><head><title>Shipping noskill 0.1</title></head><body><h1>Shipping noskill 0.1</h1><p>Today we released the first preview.</p></body></html>"}})

# ---------------- two-step (20) ----------------
task("two_step", "Find the qBittorrent Web API documentation and tell me which endpoint adds or edits an RSS auto-download rule.",
     [call("search", query={"contains": "qbittorrent"}), call("fetch", url={"contains": "wiki"})], any_re(r"setRule"),
     {"search": [{"title": "WebUI API (qBittorrent 5.0) · qbittorrent/qBittorrent Wiki", "url": "https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-(qBittorrent-5.0)", "snippet": "Documentation of the qBittorrent Web API: authentication, application, log, sync, transfer, torrents, RSS, search."}],
      "pages": {"https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-(qBittorrent-5.0)": "RSS (experimental)\nAdd folder: /api/v2/rss/addFolder\nAdd feed: /api/v2/rss/addFeed\nSet auto-downloading rule: /api/v2/rss/setRule (ruleName, ruleDef)\nRemove auto-downloading rule: /api/v2/rss/removeRule\nGet all items: /api/v2/rss/items"}})
task("two_step", "Look up the release notes for Ollama 0.40.3 and tell me one change it made.",
     [call("search", query={"contains": "0.40.3"}), call("fetch", url={"contains": "ollama"})], any_re(r"mlx", r"keep_alive", r"systemone"),
     {"search": [{"title": "Release v0.40.3 · ollama/ollama", "url": "https://github.com/ollama/ollama/releases/tag/v0.40.3", "snippet": "What's changed in v0.40.3"}],
      "pages": {"https://github.com/ollama/ollama/releases/tag/v0.40.3": "v0.40.3\nWhat's Changed\n- Decision models (/v1/systemone) now run on the MLX backend on Apple Silicon\n- Fixed keep_alive being ignored for embedding models\n- Faster model loading from external drives"}})
task("two_step", "Move all the PDFs in my Downloads folder into Documents/PDFs.",
     [call("list_dir", path={"path": "~/Downloads"}), call("move", src={"contains": ".pdf"})],
     {"fs_exists": ["~/Documents/PDFs/invoice-0412.pdf", "~/Documents/PDFs/report-q3.pdf"], "fs_missing": ["~/Downloads/invoice-0412.pdf", "~/Downloads/report-q3.pdf"]})
task("two_step", "There's a Frieren episode in my Downloads. Move it into the Frieren folder on my AnimeDrive (Anime/Frieren).",
     [call("list_dir", path={"path": "~/Downloads"}), call("move", src={"contains": "frieren"}, dst={"contains": "/volumes/animedrive/anime/frieren"})],
     {"fs_dir_match": {"/Volumes/AnimeDrive/Anime/Frieren": r"03"}, "fs_missing": ["~/Downloads/[SubsPlease] Frieren - 03 (1080p).mkv"]})
task("two_step", "Move the images (jpg and png) from my Downloads into Pictures.",
     [call("list_dir", path={"path": "~/Downloads"}), call("move", src={"contains": "downloads"}, dst={"contains": "pictures"})],
     {"fs_exists": ["~/Pictures/IMG_2041.jpg", "~/Pictures/IMG_2042.jpg", "~/Pictures/holiday.png"], "fs_missing": ["~/Downloads/IMG_2041.jpg", "~/Downloads/IMG_2042.jpg", "~/Downloads/holiday.png"]})
task("two_step", "How many days are left until Christmas this year? Check today's date first.",
     [call("get_time"), call("run_python")], any_re(rf"\b{days_xmas}\b"))
task("two_step", "Fetch https://data.example.com/prices.csv and compute the average price.",
     [call("fetch", url={"eq": "https://data.example.com/prices.csv"}), call("run_python")], any_re(r"\b4\.9\b", r"\b4\.90\b"),
     {"pages": {"https://data.example.com/prices.csv": "item,price\nA,4.20\nB,5.80\nC,3.50\nD,6.10\n"}})
task("two_step", "Get https://data.example.com/visits.json and tell me the total number of visits.",
     [call("fetch", url={"eq": "https://data.example.com/visits.json"}), call("run_python")], any_re(r"\b656\b"),
     {"pages": {"https://data.example.com/visits.json": "{\"days\": [{\"d\": \"mon\", \"v\": 120}, {\"d\": \"tue\", \"v\": 95}, {\"d\": \"wed\", \"v\": 143}, {\"d\": \"thu\", \"v\": 88}, {\"d\": \"fri\", \"v\": 210}]}"}})
task("two_step", "What's the current stable version of Go? Search for it, then check the official downloads page.",
     [call("search", query={"contains": "go"}), call("fetch", url={"contains": "go.dev"})], any_re(r"1\.27\.2"),
     {"search": [{"title": "All releases - The Go Programming Language", "url": "https://go.dev/dl/", "snippet": "Download Go binary distributions for Windows, macOS, Linux and source."}],
      "pages": {"https://go.dev/dl/": "Downloads\nStable versions\ngo1.27.2 ▾ (released 2026-10-07)\ngo1.26.9 ▾\nArchived versions"}})
task("two_step", "Which anime series are on my AnimeDrive (in the Anime folder), and how many episodes of each do I have?",
     [call("list_dir", path={"contains": "animedrive"}), call("list_dir", path={"contains": "animedrive"})],
     {"answer_all": [r"frieren[^.\n]*\b(2|two)\b|\b(2|two)\b[^.\n]*frieren", r"dandadan[^.\n]*\b(1|one)\b|\b(1|one)\b[^.\n]*dandadan"]})
task("two_step", "Find the AniList page for Dandadan and tell me how many episodes the first season has.",
     [call("search", query={"contains": "dandadan"}), call("fetch", url={"contains": "anilist"})], any_re(r"\b12\b", r"\btwelve\b"),
     {"search": [{"title": "Dandadan · AniList", "url": "https://anilist.co/anime/171018/Dandadan/", "snippet": "Dandadan · TV · Fall 2024 · Science Publishing"}],
      "pages": {"https://anilist.co/anime/171018/Dandadan/": "Dandadan\nFormat: TV\nEpisodes: 12\nEpisode Duration: 24 mins\nStatus: Finished\nSeason: Fall 2024\nStudios: Science SARU"}})
task("two_step", "What time is it in London and in Los Angeles right now?",
     [call("get_time", timezone={"contains": "london"}), call("get_time", timezone={"contains": "los_angeles"})],
     {"answer_all": [rf"\b0?{hm(lon)}\b", rf"\b0?{hm(la)}\b"]})
task("two_step", "Move setup-ollama.dmg from Downloads to my Desktop, then tell me which PDFs are still in Downloads.",
     [call("move", src={"path": "~/Downloads/setup-ollama.dmg"}), call("list_dir", path={"path": "~/Downloads"})],
     {"fs_exists": ["~/Desktop/setup-ollama.dmg"], "answer_all": [r"invoice-0412", r"report-q3"]})
task("two_step", "Search for the populations of Tokyo and Osaka (city proper), then use Python to compute how many times bigger Tokyo is.",
     [call("search", query={"contains": "tokyo"}), call("run_python")], any_re(r"\b5\.1\d*", r"\b5\.2\b"),
     {"search": [{"title": "Tokyo population 2026", "url": "https://stats.example.org/tokyo", "snippet": "Tokyo (23 special wards + Tama) population: 14,190,000 (2026 estimate)."},
                 {"title": "Osaka population 2026", "url": "https://stats.example.org/osaka", "snippet": "Osaka city population: 2,750,000 (2026 estimate)."}]})
task("two_step", "Fetch https://api.example.com/latest. It points to the newest report: fetch that too and give me the report's headline.",
     [call("fetch", url={"eq": "https://api.example.com/latest"}), call("fetch", url={"contains": "reports/2026-10-14"})], any_re(r"12\s?%"),
     {"pages": {"https://api.example.com/latest": "{\"latest\": \"https://api.example.com/reports/2026-10-14\"}",
                "https://api.example.com/reports/2026-10-14": "Weekly ops report 2026-10-14\nHeadline: Disk usage up 12% week over week\nDetails: ..."}})
task("two_step", "Check today's date, then fetch the canteen menu at https://canteen.example.com/menu/<date> (date as YYYY-MM-DD) and tell me today's soup.",
     [call("get_time"), call("fetch", url={"eq": f"https://canteen.example.com/menu/{lon.date().isoformat()}"})], any_re(r"tomato"),
     {"pages": {f"https://canteen.example.com/menu/{lon.date().isoformat()}": f"Menu for {lon.date().isoformat()}\nSoup: roasted tomato\nMain: mushroom risotto\nDessert: apple crumble"}})
task("two_step", "Find the GitHub repository for SearXNG and tell me its license.",
     [call("search", query={"contains": "searxng"}), call("fetch", url={"contains": "github.com/searxng"})], any_re(r"agpl"),
     {"search": [{"title": "searxng/searxng: SearXNG is a free internet metasearch engine", "url": "https://github.com/searxng/searxng", "snippet": "SearXNG is a free internet metasearch engine which aggregates results from various search services."}],
      "pages": {"https://github.com/searxng/searxng": "searxng/searxng\nSearXNG is a free internet metasearch engine.\nLicense: AGPL-3.0\nStars: 21k"}})
task("two_step", "There's a Frieren episode in my Downloads. Rename it to the simple format 'Frieren - 03.mkv' and keep it in Downloads.",
     [call("list_dir", path={"path": "~/Downloads"}), call("move", src={"contains": "frieren"}, dst={"path": "~/Downloads/Frieren - 03.mkv"})],
     {"fs_exists": ["~/Downloads/Frieren - 03.mkv"], "fs_missing": ["~/Downloads/[SubsPlease] Frieren - 03 (1080p).mkv"]})
task("two_step", "Is there a file called tax-2025.pdf somewhere in my Documents folder? Tell me which subfolder.",
     [call("list_dir", path={"path": "~/Documents"}), call("list_dir", path={"path": "~/Documents/PDFs"})], any_re(r"pdfs"))
task("two_step", "https://data.example.com/temps.json has today's hourly temperatures. What's the difference between the highest and lowest?",
     [call("fetch", url={"eq": "https://data.example.com/temps.json"}), call("run_python")], any_re(r"\b5\.5\b", r"\b5\.50\b"),
     {"pages": {"https://data.example.com/temps.json": "{\"unit\": \"C\", \"hourly\": [12.5, 14.0, 17.25, 16.0, 11.75]}"}})

# ---------------- no tool needed (10) ----------------
task("none", "What's the capital of Australia?", checks=any_re(r"canberra"))
task("none", "Translate 'good morning' into Spanish.", checks=any_re(r"buen[oa]s d[ií]as"))
task("none", "Write a haiku about autumn leaves.", checks={"min_len": 20})
task("none", "Explain in one sentence what a hash function is.", checks=any_re(r"hash"))
task("none", "What is 12 times 12?", checks=any_re(r"\b144\b"))
task("none", "Hi! How are you today?", checks={"min_len": 5})
task("none", "Give me a regular expression that matches a four-digit year.", checks=any_re(r"\\d\{4\}", r"\[0-9\]\{4\}"))
task("none", "Summarise in five words or fewer: 'The meeting moved from Tuesday to Thursday because the projector broke.'",
     checks={**any_re(r"thursday", r"resched", r"moved", r"postpon", r"delay", r"projector"), "max_words": 5})
task("none", "Is Python a dynamically typed language? Answer yes or no.", checks=any_re(r"\byes\b"))
task("none", "Convert this text to uppercase: hello world", checks=any_re(r"HELLO WORLD"))

assert len(T) == 50, len(T)
counts = {k: sum(t["kind"] == k for t in T) for k in ("single", "two_step", "none")}
assert counts == {"single": 20, "two_step": 20, "none": 10}, counts

with open(HERE / "tools-50.jsonl", "w") as f:
    for i, t in enumerate(T, 1):
        f.write(json.dumps({"id": f"t{i:02d}", **t}, ensure_ascii=False) + "\n")
(HERE / "tools-world.json").write_text(json.dumps(WORLD, indent=2, ensure_ascii=False) + "\n")
print(f"wrote {len(T)} tasks {counts}; local time {lon.isoformat()} ({weekday}); xmas {days_xmas}d; between {days_between}d; sha {sha12}")
