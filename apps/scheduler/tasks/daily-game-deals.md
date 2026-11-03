---
id: daily-game-deals
name: Check Game Deals
schedule: 6 * * * *
enabled: true
tools:
- web_search
- fetch_page
sources:
- https://www.cheapshark.com/api/1.0/deals?onSale=1&upperPrice=10&sortBy=Price&pageSize=60
- https://www.cheapshark.com/api/1.0/stores
checks:
  require_cited_sources: true
  min_report_links: 3
timeout_minutes: 10
catchup: false
output:
  keep: 30
---

Find current game deals that would cost under 10 CAD.

Fetch the required sources first. The first returns JSON deal records with fields
including `title`, `salePrice` (USD), `normalPrice`, `savings` (percent off),
`metacriticScore` (0 when unknown), `steamAppID`, and `storeID`. The second maps
`storeID` values to store names.

Then:
1. Keep deals with a numeric `metacriticScore` above 65 and a non-zero `salePrice`.
2. Prices on CheapShark are in US dollars. To show CAD, fetch a USD→CAD rate and
   try `{"https://api.frankfurter.dev/v1/latest?base=USD&symbols=CAD"}`-style JSON
   endpoints (also `https://open.er-api.com/v6/latest/USD`). If no rate source is
   reachable, say so in one line and list the USD prices as-is -- never invent or
   estimate a conversion rate. A deal under 10 CAD qualifies today if its USD
   price times the fetched rate is under 10.
3. Build a Markdown table: Game | Price (USD) | Price (CAD, or "n/a") | Discount % |
   Critic score | Store | Deal link (`https://www.cheapshark.com/redirect?dealID=<dealID>`).
4. Sort by price and list at most 15 rows.
Cite CheapShark and any rate source with their URLs.
The `#filter:...;sort:...` links from isthereanydeal.com cannot be honored by
fetch_page (the filter is browser-only JavaScript state); do not try to search
for or reconstruct them.
