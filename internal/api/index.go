package api

// indexHTML is the minimal landing page served at "/". The dashboard is a separate React
// app, so this is a no-framework pointer showing the API is live and how to reach it.
const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>OCR API · OnChain Radar (Mantle)</title>
<style>
  :root {
    --bg:#0c0d10; --panel:#141519; --border:#24262d; --fg:#e6e7ea;
    --fg-dim:#9aa0aa; --fg-faint:#6b7079; --accent:#4f9cf9; --ok:#3fb950;
    --s2:8px; --s3:12px; --s4:16px; --s5:24px; --s6:32px;
  }
  *{box-sizing:border-box} html,body{margin:0;padding:0}
  body{background:var(--bg);color:var(--fg);font-size:14px;line-height:1.5;
    font-family:ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;
    -webkit-font-smoothing:antialiased}
  .mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,"Liberation Mono",monospace}
  .wrap{max-width:880px;margin:0 auto;padding:var(--s6) var(--s4)}
  h1{font-size:18px;font-weight:600;margin:0 0 var(--s2)}
  .sub{color:var(--fg-faint);font-size:13px;margin-bottom:var(--s5)}
  .live{display:inline-flex;align-items:center;gap:var(--s2);color:var(--ok);font-size:12px}
  .dot{width:8px;height:8px;border-radius:50%;background:var(--ok)}
  h2{font-size:12px;text-transform:uppercase;letter-spacing:.6px;color:var(--fg-faint);
    margin:var(--s5) 0 var(--s3);font-weight:600}
  ul{list-style:none;margin:0;padding:0;border:1px solid var(--border);border-radius:6px;
    background:var(--panel);overflow:hidden}
  li{padding:var(--s3) var(--s4);border-bottom:1px solid var(--border);display:flex;
    gap:var(--s4);align-items:baseline}
  li:last-child{border-bottom:none}
  li a{color:var(--accent);text-decoration:none;white-space:nowrap}
  li a:hover{text-decoration:underline}
  li .desc{color:var(--fg-dim);font-size:13px}
  code{color:var(--fg-dim)}
  footer{margin-top:var(--s6);color:var(--fg-faint);font-size:12px}
</style>
</head>
<body>
<div class="wrap">
  <h1>OCR API <span class="live"><span class="dot"></span>read-only · live</span></h1>
  <div class="sub mono">OnChain Radar · autonomous Mantle anomaly agent · JSON + SSE data layer</div>

  <h2>Endpoints</h2>
  <ul>
    <li><a href="/api/stats">/api/stats</a><span class="desc">headline rollup (counts, grades, 24h window)</span></li>
    <li><a href="/api/signals">/api/signals</a><span class="desc">signal feed (filter: type, pool, actor; paginated)</span></li>
    <li><a href="/api/signals/1">/api/signals/{id}</a><span class="desc">one signal + payload</span></li>
    <li><a href="/api/actors">/api/actors</a><span class="desc">smart-money actor leaderboard</span></li>
    <li><a href="/api/actors/0x">/api/actors/{addr}</a><span class="desc">one actor + recent signals</span></li>
    <li><a href="/api/pools">/api/pools</a><span class="desc">tracked pools: our activity + market stats</span></li>
    <li><a href="/api/pools/0x">/api/pools/{addr}</a><span class="desc">one pool + series + signals</span></li>
    <li><a href="/api/series?pool=0x&metric=swap_count">/api/series</a><span class="desc">one metric time series</span></li>
    <li><a href="/api/live">/api/live</a><span class="desc">SSE stream of new signals (text/event-stream)</span></li>
  </ul>

  <footer class="mono">Every signal is attested on Mantle. This API serves the verifiable track record.</footer>
</div>
</body>
</html>
`
