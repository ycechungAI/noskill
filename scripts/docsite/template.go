package main

// pageHTML takes: title, stylesheet URL, home URL, nav, body, source path.
const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<link rel="stylesheet" href="%s">
</head>
<body>
<header class="top"><a class="brand" href="%s">noskill</a><span class="tag">Your local model starts with no skills. It learns them.</span><a class="jump" href="#pages">Pages ↓</a></header>
<div class="wrap">
<nav class="side" id="pages" aria-label="Pages">
%s</nav>
<main>
<article>
%s</article>
<footer>Source: <code>%s</code></footer>
</main>
</div>
</body>
</html>
`

const css = `:root {
  --bg: #fdfdfc; --fg: #1d1d1b; --muted: #6a6a64; --line: #e3e2dc; --accent: #2f5fd0;
  --code-bg: #f3f2ee; --side-bg: #f8f7f3;
}
@media (prefers-color-scheme: dark) {
  :root { --bg: #161615; --fg: #e8e7e2; --muted: #9b9a92; --line: #2e2e2b; --accent: #8fb0ff; --code-bg: #212120; --side-bg: #1b1b1a; }
}
* { box-sizing: border-box; }
html { -webkit-text-size-adjust: 100%; }
body { margin: 0; background: var(--bg); color: var(--fg); font: 16px/1.6 system-ui, -apple-system, "Segoe UI", sans-serif; }
a { color: var(--accent); }
a.src { color: inherit; text-decoration: underline dotted; cursor: help; }
.top { display: flex; gap: 16px; align-items: baseline; flex-wrap: wrap; padding: 12px 16px; border-bottom: 1px solid var(--line); }
.brand { font-weight: 700; font-size: 1.15rem; color: var(--fg); text-decoration: none; }
.tag { color: var(--muted); font-size: .9rem; }
.jump { display: none; margin-left: auto; font-size: .9rem; }
.wrap { display: flex; align-items: flex-start; }
.side { flex: 0 0 260px; position: sticky; top: 0; max-height: 100vh; overflow-y: auto; padding: 16px; background: var(--side-bg); border-right: 1px solid var(--line); font-size: .9rem; }
.side h2 { font-size: .75rem; letter-spacing: .06em; text-transform: uppercase; color: var(--muted); margin: 16px 0 4px; }
.side h2:first-child { margin-top: 0; }
.side ul { list-style: none; margin: 0; padding: 0; }
.side li a { display: block; padding: 3px 8px; border-radius: 4px; color: var(--fg); text-decoration: none; }
.side li a:hover { background: var(--code-bg); }
.side li a.here { background: var(--code-bg); font-weight: 600; }
main { flex: 1; min-width: 0; padding: 8px 16px 48px; }
article { max-width: 52rem; margin: 0 auto; overflow-wrap: anywhere; }
article h1 { font-size: 2rem; line-height: 1.2; }
article h2 { margin-top: 2rem; padding-bottom: .2rem; border-bottom: 1px solid var(--line); }
article table { border-collapse: collapse; display: block; overflow-x: auto; max-width: 100%; }
article th, article td { border: 1px solid var(--line); padding: 6px 10px; text-align: left; vertical-align: top; }
article th { background: var(--code-bg); }
code { background: var(--code-bg); padding: .1em .3em; border-radius: 3px; font: .9em ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
pre { background: var(--code-bg); padding: 12px 14px; border-radius: 6px; overflow-x: auto; line-height: 1.45; }
pre code { background: none; padding: 0; }
blockquote { margin: 1rem 0; padding: .1rem 1rem; border-left: 3px solid var(--line); color: var(--muted); }
footer { max-width: 52rem; margin: 3rem auto 0; padding-top: 1rem; border-top: 1px solid var(--line); color: var(--muted); font-size: .85rem; }
@media (max-width: 800px) {
  .wrap { display: flex; flex-direction: column; }
  main { order: 1; width: 100%; }
  .side { order: 2; position: static; max-height: none; width: 100%; border-right: 0; border-top: 1px solid var(--line); }
  .jump { display: inline; }
}
`
