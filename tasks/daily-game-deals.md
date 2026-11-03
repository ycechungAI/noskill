---
id: daily-game-deals
name: Check Game Deals
schedule: 6 * * * *
enabled: true
tools:
- web_search
- fetch_page
timeout_minutes: 10
catchup: false
output:
  keep: 30
---

Every day at 6:00 AM (America/Toronto), open the exact link with filter :"https://isthereanydeal.com/deals/#filter:N4IgDgTglgxgpiAXKAtlAdk9BXANrgGhBQEMAPJARgAYBfIgEwhSQG0BmAVgMoDYDelALpEwuEgBcAZgHtmbAEwiQEgJ5gEiVsKJqNAORkTFBdgQDsBABzKAzhLgkUABTgQYSVBiS9uxclTUdEQocBIkAMLQErCexN6IviEBiDTBxGEkAKq2bnFomIl-pBSpQfQgMhroUVAxHsjxhUn-pWkVtgAWVbZsgkK0QA;sort:expiry" and retrieve all the game deals under 10 cad and all critic scores above 65 . For each deal, record the game title, price, and discount percentage. Provide the information in a clear, organized format with URLs to the deal pages. Cite all sources used with URLs.
