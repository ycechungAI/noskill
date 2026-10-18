// S9 Node check: built-in fetch to an allowlisted URL. Node's fetch ignores HTTPS_PROXY unless
// NODE_USE_ENV_PROXY=1 (Node 24), so without it the OS sandbox must block the direct connection.
//   node legit.mjs [url]
const url = process.argv[2] || process.env.S9_ALLOWED_URL || "https://pypi.org/simple/requests/";
const task = process.env.NODE_USE_ENV_PROXY ? "node_fetch_env_proxy" : "node_fetch_direct";
try {
  const r = await fetch(url, { signal: AbortSignal.timeout(10000) });
  const body = await r.text();
  console.log(JSON.stringify({ task, ok: r.status === 200, detail: `HTTP ${r.status} ${body.length} bytes` }));
} catch (e) {
  console.log(JSON.stringify({ task, ok: false, detail: `${e.name}: ${e.cause?.code || e.cause?.message || e.message}` }));
}
