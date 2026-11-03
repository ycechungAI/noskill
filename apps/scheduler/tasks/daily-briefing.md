---
id: daily-briefing
name: Comprehensive Daily Briefing
model: "granite4:3b"
schedule: "0 7 * * *"        # ASSUMED daily 07:00 — adjust to taste
enabled: true
tools: [web_search, fetch_page, current_datetime]
timeout_minutes: 20
catchup: true
output:
  dir: "~/dev/briefing/{year}/{month_name}"
  filename: "day{day}.md"
  raw: true                  # scheduler writes the report verbatim to the path above
---

Generate a comprehensive daily briefing for today covering the following
sections, in this order:

1. **AI Industry**
   - Search for the latest AI news from major AI companies (OpenAI, Google,
     Anthropic, Meta, etc.).
   - Include major model releases, funding, policy updates, corporate news.
   - Provide 4–6 key bullet points with links to sources.

2. **World Wars / Conflict**
   - Report on major active conflicts (Ukraine–Russia, Iran tensions,
     Middle East, Sudan, etc.).
   - Focus on developments in the past 24–48 hours.
   - Include 3–5 key bullet points with sources.

3. **US Stock Market**
   - Report the latest market performance (Dow, S&P 500, Nasdaq).
   - Include key drivers, major movers, sector performance.
   - Provide 3–4 key bullet points with financial sources.

4. **Wall Street Breakfast (Podcast)**
   - Find the most recent Wall Street Breakfast episode from Seeking Alpha.
   - Summarize 3–4 key stories and market insights from that episode.
   - Include episode title/date and source link.

5. **Cybersecurity Today (Podcast)**
   - Find the most recent Cybersecurity Today episode.
   - Summarize 3–4 key cybersecurity stories from that episode.
   - Include episode details and source link.

Format requirements:
- Start with: # Daily Briefing — <Month> <Day>, <Year>
- Use markdown with clear section headers (##).
- Use bullet points with bold headlines and explanatory text.
- Include inline [link text](URL) citations for sources.
- End with a ## Sources section listing all URLs used.
- Professional, concise, linked — consistent with the previous day files in
  the same directory.

Your final report IS the briefing file — the scheduler saves it to
~/dev/briefing/<year>/<Month>/day<N>.md automatically, so output only the
briefing content itself.
