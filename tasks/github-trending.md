---
id: github-trending
name: GitHub Trending Daily Digest
model: "granite4:3b"
schedule: "0 8 * * *"        # ASSUMED daily 08:00 — adjust to taste
enabled: true
tools: [web_search, fetch_page, current_datetime]
timeout_minutes: 10
catchup: true
output:
  keep: 30
---

Search for and summarize today's trending GitHub projects. Start by fetching
https://github.com/trending for today's data; use web search only to fill in
context about specific projects if needed.

Cover:
1. **Top 5 trending projects** — name, language, recent activity/updates.
2. **Key themes** — what categories or tech are trending (AI, web frameworks,
   dev tools, etc.).
3. **Notable new or resurgent projects** — anything gaining sudden traction.
4. **Why it matters** — brief insight into why these are trending now.

Keep the summary concise and scannable: a daily digest someone interested in
tech trends can read in 2–3 minutes. Include links to the top projects for
easy reference.
