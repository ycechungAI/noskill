---
id: isthereanydeal-deals
name: Check Top 10 Deals Under $10 CAD
model: "gemma3:1b"
schedule: 0 7 * * *
enabled: false
tools:
- web_search
- fetch_page
timeout_minutes: 10
catchup: false
output:
  keep: 30
---

Visit https://isthereanydeal.com/deals/#filter:N4IgDgTglgxgpiAXKAtlAdk9BXANrgGhBQEMAPJARgAYBfIgEwhSQG0BmAVgMoDYDelALpEwuEgBcAZgHtmbAEwiQEgJ5gEiVsKJqNAORkTFBdgQDsBABzKAzhLgkUABTgQYSVBiS9uxclTUdEQocBIkAMLQErCexN6IviEBiDTBxGEkAKq2bnFomIl-pBSpQfQgMhroUVAxHsjxhUn-pWkVtgAWVbZsgkK0QA;sort:expiry and retrieve the current list of deals priced under $10 CAD and display the unique deals with their names, prices, and links. Ensure all prices are in Canadian dollars (CAD).
