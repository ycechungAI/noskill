---
id: weekly-market-summary
name: Weekly Stock Market Summary
model: "granite4:3b"
schedule: "0 8 * * 6"        # ASSUMED Saturday 08:00, after Friday close — adjust to taste
enabled: true
tools: [web_search, fetch_page, current_datetime]
timeout_minutes: 15
catchup: true
output:
  keep: 12
---

Produce a weekly stock market summary covering the past week. Use the
web_search and fetch_page tools to gather current market data — do not rely
on training data for prices or performance figures.

Research and report:
1. **Top 5 value stocks** currently trading near their 52-week lows that have
   strong fundamentals (e.g., solid earnings, healthy balance sheets,
   reasonable P/E). For each: ticker, current price, distance from 52-week
   low, and a one-line fundamentals rationale.
2. **Top 5 momentum stocks** showing strong recent performance (price strength
   over the past weeks/month, positive catalysts). For each: ticker, recent
   performance, and the driver behind the move.
3. **Key market trends and risks** to watch: major index moves this week
   (S&P 500, Nasdaq, Dow), notable sector rotation, macro events (Fed,
   inflation data, earnings season), and upcoming risks for the week ahead.

Format as a concise, scannable summary with the three sections above. Cite
sources for data points where available. End with a brief disclaimer that
this is informational research, not financial advice, and figures should be
verified before making investment decisions.

Keep the summary concise and direct.
