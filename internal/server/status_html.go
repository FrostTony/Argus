package server

// statusHTML is the whole status page: one file, no external assets.
const statusHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>argus · {{.State.Node}}</title>
<link rel="icon" href="/logo.png" type="image/png">
<style>
:root{
  --bg:#f9f9f7; --panel:#fff; --sunk:#f4f4f1; --line:#e6e4e0; --line-soft:#efeeea;
  --ink:#1a1a18; --muted:#6b6862; --faint:#93908a;
  --up:#0ca30c; --down:#d03b3b; --down-bg:#fbeceb; --down-ink:#a52d2d;
  --s1:#2a78d6; --s2:#eb6834; --s3:#1baf7a; --s4:#eda100; --s5:#e87ba4;
  --grid:#e6e4de; --accent:#2a78d6; --hover:rgba(11,11,11,.035); --focus:rgba(42,120,214,.10);
}
@media (prefers-color-scheme:dark){
  :root:not([data-theme="light"]){
    --bg:#111114; --panel:#1a1a1f; --sunk:#15151a; --line:#2b2b33; --line-soft:#232329;
    --ink:#ecebf0; --muted:#9a97a3; --faint:#7d7a86;
    --up:#4ec94e; --down:#ff7b72; --down-bg:#2c1717; --down-ink:#ff9b94;
    --s1:#3987e5; --s2:#d95926; --s3:#199e70; --s4:#c98500; --s5:#d55181;
    --grid:#2a2a31; --accent:#8fa3ff; --hover:rgba(255,255,255,.04); --focus:rgba(143,163,255,.10);
  }
}
:root[data-theme="dark"]{
  --bg:#111114; --panel:#1a1a1f; --sunk:#15151a; --line:#2b2b33; --line-soft:#232329;
  --ink:#ecebf0; --muted:#9a97a3; --faint:#7d7a86;
  --up:#4ec94e; --down:#ff7b72; --down-bg:#2c1717; --down-ink:#ff9b94;
  --s1:#3987e5; --s2:#d95926; --s3:#199e70; --s4:#c98500; --s5:#d55181;
  --grid:#2a2a31; --accent:#8fa3ff; --hover:rgba(255,255,255,.04); --focus:rgba(143,163,255,.10);
}
*{box-sizing:border-box}
body{margin:0;padding:20px 16px 48px;background:var(--bg);color:var(--ink);
  font:14px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
.wrap{max-width:1180px;margin:0 auto}
a{color:var(--accent)}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}

header{display:flex;flex-wrap:wrap;align-items:center;gap:10px;margin-bottom:14px}
.logo{width:26px;height:26px;border-radius:7px;display:block;flex:none}
h1{font-size:18px;margin:0;font-weight:650;letter-spacing:-.01em}
h1 span{color:var(--muted);font-weight:400}
.meta{color:var(--muted);font-size:12.5px;margin-left:auto;display:flex;gap:12px;
  align-items:center;flex-wrap:wrap}
.dotlive{display:inline-block;width:7px;height:7px;border-radius:50%;background:var(--up)}
.dotlive.off{background:var(--faint)}
.meta button{padding:3px;border:0;background:none;color:var(--muted);border-radius:6px;
  display:inline-flex;align-items:center;line-height:0}
.meta button:hover{color:var(--ink);background:var(--hover)}
.meta svg{width:15px;height:15px;fill:currentColor}

.controls{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-bottom:14px}
input,select,button{font:inherit;font-size:12.5px;color:var(--ink);background:var(--panel);
  border:1px solid var(--line);border-radius:8px;padding:5px 9px}
input{min-width:200px}
input::placeholder{color:var(--faint)}
select,button{cursor:pointer}
button:hover,select:hover,input:hover{border-color:var(--faint)}
button.on{background:var(--focus);border-color:var(--accent);color:var(--accent)}
:focus-visible{outline:2px solid var(--accent);outline-offset:1px}

.tiles{display:flex;gap:8px;flex-wrap:wrap;margin-bottom:12px}
.tile{padding:7px 14px;border:1px solid var(--line);border-radius:10px;background:var(--panel);
  display:flex;align-items:baseline;gap:8px;min-width:108px}
.tile b{font-size:19px;font-weight:650;letter-spacing:-.02em;font-variant-numeric:tabular-nums}
.tile span{color:var(--muted);font-size:12px}
.tile.ok b{color:var(--up)} .tile.bad b{color:var(--down)}

.probe{border:1px solid var(--line);border-radius:12px;background:var(--panel);
  margin-bottom:10px;overflow:hidden}
.phead{display:flex;align-items:center;gap:9px;padding:9px 14px;flex-wrap:wrap;
  background:var(--sunk);border-bottom:1px solid var(--line)}
.phead .name{font-weight:600}
.tag{font:11.5px ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--muted);
  border:1px solid var(--line);border-radius:5px;padding:0 6px;background:var(--panel)}
.tag.warn{border-style:dashed}
.tag.bad{color:var(--down);border-color:var(--down);background:var(--down-bg)}
.phead .every{color:var(--faint);font-size:12px;margin-left:auto}

.target{border-top:1px solid var(--line-soft)}
.target:first-child{border-top:none}
.thead{display:flex;align-items:center;gap:12px;padding:9px 14px 6px}
.tname{font-weight:500;word-break:break-all;flex:1;min-width:0}
.tspark{width:110px;height:24px;flex:none;opacity:.9}
.tlast{color:var(--muted);font:12px ui-monospace,SFMono-Regular,Menlo,monospace;
  font-variant-numeric:tabular-nums;width:66px;text-align:right}
.tally{display:flex;align-items:center;gap:7px;white-space:nowrap;
  font:12px ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--muted)}
.seg{display:flex;gap:2px}
.seg i{width:9px;height:6px;border-radius:2px;background:var(--up)}
.seg i.no{background:var(--down)}

.backends{padding:0 8px 8px;display:flex;flex-direction:column;gap:2px}
.be{display:flex;align-items:center;gap:9px;width:100%;text-align:left;padding:5px 8px;
  border:1px solid transparent;border-radius:8px;background:none;cursor:pointer;
  font:12.5px ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--ink)}
.be:hover{background:var(--hover)}
.be[aria-expanded="true"]{background:var(--focus);border-color:var(--line)}
.be .caret{color:var(--faint);font-size:9px;width:9px;flex:none;
  transition:transform .12s ease}
.be[aria-expanded="true"] .caret{transform:rotate(90deg);color:var(--accent)}
.be .dot{width:7px;height:7px;border-radius:50%;flex:none;background:var(--up)}
.be.down .dot{background:var(--down)}
.be .addr{flex:none}
.be.down .addr{color:var(--down-ink)}
.be .facts{color:var(--muted);flex:none}
.be .why{color:var(--down-ink);flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;
  white-space:nowrap;font-family:system-ui,-apple-system,sans-serif}
.be .spacer{flex:1}

.detail{margin:2px 8px 8px;border:1px solid var(--line);border-left:3px solid var(--accent);
  border-radius:8px;background:var(--sunk);padding:11px 13px}
.sec{margin-top:13px}
.sec:first-child{margin-top:0}
h4{margin:0 0 6px;font-size:11px;font-weight:600;color:var(--faint);
  text-transform:uppercase;letter-spacing:.06em}
.note{color:var(--faint);font-size:11.5px;margin-top:4px}
.fail{display:flex;gap:9px;align-items:baseline;background:var(--down-bg);color:var(--down-ink);
  border-radius:7px;padding:6px 10px;font-size:12.5px}
.fail .k{font:11.5px ui-monospace,SFMono-Regular,Menlo,monospace;opacity:.85;flex:none}
.fail .m{word-break:break-word;min-width:0}

figure{margin:0;position:relative}
.tip{position:absolute;pointer-events:none;background:var(--panel);border:1px solid var(--line);
  border-radius:7px;padding:4px 8px;font-size:11.5px;box-shadow:0 4px 14px rgba(0,0,0,.14);
  white-space:nowrap;opacity:0;transition:opacity .08s;z-index:5}
.tip.on{opacity:1}
.tip .k{color:var(--muted)} .tip b{font-variant-numeric:tabular-nums}

.phasebar{display:flex;height:9px;border-radius:5px;overflow:hidden;gap:2px;margin-bottom:7px}
.phasebar span{height:100%}
.legend{display:flex;gap:12px;flex-wrap:wrap;font-size:11.5px;color:var(--muted)}
.legend i{display:inline-block;width:9px;height:9px;border-radius:2px;margin-right:5px;
  vertical-align:-1px}
.legend b{color:var(--ink);font-weight:600;font-variant-numeric:tabular-nums}

.kv{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:1px 18px;
  font-size:12.5px}
.kv div{display:flex;gap:10px;padding:2px 0;border-bottom:1px dotted var(--line)}
.kv .k{color:var(--muted);flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;
  white-space:nowrap}
.kv .v{font-variant-numeric:tabular-nums;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
table.stats{width:100%;border-collapse:collapse;font-size:12.5px}
table.stats th{text-align:right;color:var(--faint);font-weight:500;font-size:11px;padding:0 0 3px 12px}
table.stats th:first-child{text-align:left;padding-left:0}
table.stats td{text-align:right;padding:2px 0 2px 12px;font-variant-numeric:tabular-nums;
  font-family:ui-monospace,SFMono-Regular,Menlo,monospace;border-top:1px solid var(--line)}
table.stats td:first-child{text-align:left;font-family:inherit;padding-left:0}

.empty{padding:16px 14px;color:var(--faint);text-align:center;font-size:12.5px}
footer{margin-top:18px;color:var(--faint);font-size:12px;text-align:center;line-height:1.7}
noscript{display:block;padding:10px 14px;border:1px solid var(--line);border-radius:10px;
  background:var(--panel);margin-bottom:12px}
</style>
</head>
<body>
<div class="wrap">

<header>
  <img class="logo" src="/logo.png" alt="" width="26" height="26">
  <h1>argus <span>· {{.State.Node}}</span></h1>
  <div class="meta">
    <span id="live"><span class="dotlive"></span></span>
    <span id="uptime"></span>
    <span>v{{.State.Version}}</span>
    <span id="clock" class="mono"></span>
    <button id="theme" type="button"></button>
  </div>
</header>

<noscript>Charts and live updates need JavaScript. The current state is always available
at <a href="/status/data">/status/data</a> and <a href="/metrics">/metrics</a>.</noscript>

<div class="tiles" id="tiles"></div>

<div class="controls">
  <input id="find" type="search" placeholder="filter by probe, target or address" autocomplete="off">
  <button id="only" type="button" title="show only what is failing">problems only</button>
  <span style="flex:1"></span>
  <select id="every" title="how often to poll the node">
    <option value="2000">2s</option>
    <option value="5000" selected>5s</option>
    <option value="10000">10s</option>
    <option value="30000">30s</option>
  </select>
  <button id="pause" type="button" title="stop polling">pause</button>
</div>

<main id="probes"></main>

<footer>
  Charts are built in this tab and go with it — argus keeps no history.
  Series live {{.State.StaleAfter}} and then expire.<br>
  <a href="/metrics">/metrics</a> · <a href="/status/data">/status/data</a> · <a href="/status.json">/status.json</a>
</footer>

</div>
<script>
"use strict";
const INITIAL = JSON.parse({{.Initial}});
const KEEP = 240;              // points kept per backend
const SEP = "\n";              // key separator: no name or URL contains one
const $ = (id) => document.getElementById(id);

let state = INITIAL;
let every = 5000, paused = false, timer = null, misses = 0;
let query = "", onlyBad = false, shape = "";
const expanded = new Set();    // backends with an open panel
const seen = new Map();        // backend key -> {t: [], v: []}

// The live DOM, built once and then only written into.
const view = {probes: new Map(), targets: new Map(), backends: new Map()};

function dur(s){
  if (s === null || s === undefined || s !== s) return "";
  if (s >= 1) return s.toFixed(2) + " s";
  if (s >= 0.001) return Math.round(s * 1000) + " ms";
  if (s > 0) return Math.round(s * 1e6) + " us";
  return "0";
}
function bytes(n){
  const u = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + " " + u[i];
}
function num(v){
  if (v === null || v === undefined) return "";
  if (Number.isInteger(v)) return String(v);
  if (Math.abs(v) >= 100) return v.toFixed(0);
  if (Math.abs(v) >= 1) return v.toFixed(2);
  return v.toPrecision(3);
}
// The metric name suffix decides the unit a value is printed in.
function fmt(name, v){
  if (/_seconds$|^resolve/.test(name)) return dur(v);
  if (/_bytes$/.test(name)) return bytes(v);
  if (/_days$/.test(name)) return num(v) + " d";
  if (/_percent$/.test(name)) return num(v) + " %";
  return num(v);
}
function clock(ms){
  const d = new Date(ms), p = (n) => String(n).padStart(2, "0");
  return p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
}

function el(tag, cls, text){
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}
const NS = "http://www.w3.org/2000/svg";
function sv(tag, attrs){
  const n = document.createElementNS(NS, tag);
  for (const k in attrs) n.setAttribute(k, attrs[k]);
  return n;
}
// Writing a node that already says this invalidates layout for nothing.
function txt(n, s){ if (n.textContent !== s) n.textContent = s; }
function attr(n, k, v){ if (n.getAttribute(k) !== String(v)) n.setAttribute(k, v); }
function show(n, on){ if (n.hidden === on) n.hidden = !on; }

// fit reduces vals to at most max points, keeping each group's peak.
function fit(vals, max){
  if (vals.length <= max) return vals;
  const stride = vals.length / max;
  const out = new Array(max);
  for (let i = 0; i < max; i++){
    let peak = 0;
    for (let j = Math.floor(i * stride), end = Math.floor((i + 1) * stride); j < end; j++){
      if (vals[j] > peak) peak = vals[j];
    }
    out[i] = peak;
  }
  return out;
}

function path(vals, w, h, lo, hi, pad){
  const n = vals.length;
  if (!n) return "";
  const span = hi - lo || 1;
  const step = n < 2 ? 0 : w / (n - 1);
  const top = h - 2 * pad, base = h - pad;
  const parts = new Array(n);
  for (let i = 0; i < n; i++){
    const x = Math.round(n < 2 ? w : i * step);
    const y = Math.round(base - ((vals[i] - lo) / span) * top);
    parts[i] = (i ? "L" : "M") + x + " " + y;
  }
  return parts.join("");
}

// A sparkline is built once; a new point moves the two paths it is made of.
function makeSpark(w, h, color){
  const root = sv("svg", {viewBox: "0 0 " + w + " " + h, width: w, height: h,
    preserveAspectRatio: "none", "aria-hidden": "true"});
  const fill = sv("path", {fill: color, "fill-opacity": ".10", stroke: "none"});
  const stroke = sv("path", {fill: "none", stroke: color, "stroke-width": "1.5",
    "stroke-linejoin": "round", "stroke-linecap": "round"});
  root.appendChild(fill);
  root.appendChild(stroke);
  return {root, update(raw){
    const vals = fit(raw, w);
    if (vals.length < 2){ attr(fill, "d", ""); attr(stroke, "d", ""); return; }
    let lo = Math.min(...vals), hi = Math.max(...vals);
    if (hi - lo < 1e-12){ lo -= 1; hi += 1; }   // steady series sits mid-height
    const d = path(vals, w, h, lo, hi, 3);
    attr(stroke, "d", d);
    attr(fill, "d", d + "L" + w + " " + h + "L0 " + h + "Z");
  }};
}

function makeChart(){
  const w = 620, h = 104, padT = 10, padB = 16, padL = 54;
  const root = el("figure");
  const svg = sv("svg", {viewBox: "0 0 " + (w + padL) + " " + (h + padB),
    role: "img", "aria-label": "Response time since this tab was opened"});
  svg.style.width = "100%";
  svg.style.height = "auto";
  svg.style.display = "block";

  const ticks = [0, 1, 2].map(() => {
    const g = sv("line", {x1: padL, x2: w + padL, stroke: "var(--grid)", "stroke-width": "1"});
    const t = sv("text", {x: padL - 6, "text-anchor": "end", fill: "var(--faint)", "font-size": "10"});
    svg.appendChild(g);
    svg.appendChild(t);
    return {g, t};
  });

  const g = sv("g", {transform: "translate(" + padL + ",0)"});
  const fill = sv("path", {fill: "var(--s1)", "fill-opacity": ".12", stroke: "none"});
  const stroke = sv("path", {fill: "none", stroke: "var(--s1)", "stroke-width": "2",
    "stroke-linejoin": "round", "stroke-linecap": "round"});
  const head = sv("circle", {cx: w, r: "3.5", fill: "var(--s1)",
    stroke: "var(--sunk)", "stroke-width": "2"});
  const cross = sv("line", {y1: padT, y2: h - padB, stroke: "var(--faint)",
    "stroke-width": "1", opacity: "0"});
  g.appendChild(fill);
  g.appendChild(stroke);
  g.appendChild(head);
  g.appendChild(cross);
  svg.appendChild(g);
  root.appendChild(svg);

  const tip = el("div", "tip");
  root.appendChild(tip);

  let buf = {t: [], v: []}, hi = 1;
  svg.addEventListener("pointerleave", () => {
    tip.className = "tip";
    attr(cross, "opacity", "0");
  });
  svg.addEventListener("pointermove", (e) => {
    const n = buf.v.length;
    if (!n) return;
    const r = svg.getBoundingClientRect(), scale = (w + padL) / r.width;
    const px = (e.clientX - r.left) * scale - padL;
    const i = Math.max(0, Math.min(n - 1, Math.round(px / w * (n - 1))));
    const x = n < 2 ? w : (i / (n - 1)) * w;
    attr(cross, "x1", x);
    attr(cross, "x2", x);
    attr(cross, "opacity", ".5");
    tip.innerHTML = "<span class='k'>" + clock(buf.t[i]) + "</span> <b>" + dur(buf.v[i]) + "</b>";
    tip.className = "tip on";
    tip.style.left = Math.min(Math.max((x + padL) / scale - tip.offsetWidth / 2, 0),
      r.width - tip.offsetWidth) + "px";
    tip.style.top = "0px";
  });

  return {root, update(next){
    buf = next;
    const vals = fit(buf.v, w / 2);
    hi = Math.max(...vals, 1e-9) * 1.15;
    ticks.forEach((tick, i) => {
      const v = hi * i / 2;
      const y = (h - padB) - (v / hi) * (h - padB - padT);
      attr(tick.g, "y1", y);
      attr(tick.g, "y2", y);
      attr(tick.t, "y", y + 3.5);
      txt(tick.t, dur(v));
    });
    const d = path(vals, w, h - padB + padT, 0, hi, padT);
    attr(stroke, "d", d);
    attr(fill, "d", d + "L" + w + " " + (h - padB) + "L0 " + (h - padB) + "Z");
    const last = buf.v[buf.v.length - 1] || 0;
    attr(head, "cy", (h - padB) - (last / hi) * (h - padB - padT));
  }};
}

function keyOf(probe, target, backend){ return probe + SEP + target + SEP + (backend || ""); }

function record(s){
  const live = new Set();
  for (const p of s.probes) for (const t of p.targets) for (const b of t.backends){
    const key = keyOf(p.name, t.target, b.backend);
    live.add(key);
    let buf = seen.get(key);
    if (!buf){ buf = {t: [], v: []}; seen.set(key, buf); }
    buf.t.push(s.time);
    buf.v.push(b.latency || 0);
    if (buf.t.length > KEEP){ buf.t.shift(); buf.v.shift(); }
  }
  for (const k of seen.keys()) if (!live.has(k)) seen.delete(k);
}

// Named trend, not history: a top-level history() would shadow window.history.
function trend(key){ return seen.get(key) || {t: [], v: []}; }

const PHASE_COLORS = ["var(--s1)", "var(--s2)", "var(--s3)", "var(--s4)", "var(--s5)"];

// keyedSection updates in place and rebuilds only when its key list changes.
function keyedSection(title, build){
  const root = el("div", "sec");
  const head = el("h4", null, title);
  const body = el("div");
  root.appendChild(head);
  root.appendChild(body);
  let listed = "", cells = null;
  return {root, update(map, order){
    const keys = order || Object.keys(map || {}).sort();
    show(root, keys.length > 0);
    if (!keys.length) return;
    const join = keys.join("");
    if (join !== listed){
      listed = join;
      body.textContent = "";
      cells = build(body, keys, map);
    }
    cells(map, keys);
  }};
}

function makePanel(key){
  const root = el("div", "detail");

  const why = el("div", "sec");
  why.appendChild(el("h4", null, "Why it is down"));
  const fail = el("div", "fail");
  const failKind = el("span", "k");
  const failMsg = el("span", "m");
  fail.appendChild(failKind);
  fail.appendChild(failMsg);
  why.appendChild(fail);
  root.appendChild(why);

  const timeline = el("div", "sec");
  timeline.appendChild(el("h4", null, "Response time · this tab"));
  const chart = makeChart();
  timeline.appendChild(chart.root);
  root.appendChild(timeline);

  const phases = el("div", "sec");
  phases.appendChild(el("h4", null, "Request phases"));
  const bar = el("div", "phasebar");
  const legend = el("div", "legend");
  phases.appendChild(bar);
  phases.appendChild(legend);
  root.appendChild(phases);
  let phaseKeys = "", segs = [], labels = [];

  const stats = keyedSection("Statistics over the life of the series", (body, keys) => {
    const table = el("table", "stats");
    const head = el("tr");
    for (const h of ["metric", "checks", "mean", "p50", "p95"]) head.appendChild(el("th", null, h));
    table.appendChild(head);
    const rows = keys.map((k) => {
      const tr = el("tr");
      const cells = [el("td", null, k), el("td"), el("td"), el("td"), el("td")];
      for (const c of cells) tr.appendChild(c);
      table.appendChild(tr);
      return cells;
    });
    body.appendChild(table);
    body.appendChild(el("div", "note",
      "percentiles are estimated from the histogram buckets and can be no finer than their bounds"));
    return (map, order) => order.forEach((k, i) => {
      const v = map[k], c = rows[i];
      txt(c[1], String(v.count));
      txt(c[2], dur(v.mean));
      txt(c[3], dur(v.p50));
      txt(c[4], dur(v.p95));
    });
  });
  root.appendChild(stats.root);

  const grids = [["Gauges", fmt], ["Counters", (k, v) => num(v)], ["Facts", (k, v) => v]]
    .map(([title, format]) => {
      const sec = keyedSection(title, (body, keys) => {
        const kv = el("div", "kv");
        const values = keys.map((k) => {
          const row = el("div");
          const name = el("span", "k", k);
          name.title = k;
          const value = el("span", "v");
          row.appendChild(name);
          row.appendChild(value);
          kv.appendChild(row);
          return value;
        });
        body.appendChild(kv);
        return (map, order) => order.forEach((k, i) => txt(values[i], String(format(k, map[k]))));
      });
      root.appendChild(sec.root);
      return sec;
    });

  return {root, update(b){
    const broken = !b.up && (b.error || b.reason);
    show(why, !!broken);
    if (broken){
      show(failKind, !!b.reason);
      txt(failKind, b.reason || "");
      txt(failMsg, b.error || "no message was recorded");
    }

    const buf = trend(key);
    show(timeline, buf.v.length > 1);
    if (buf.v.length > 1) chart.update(buf);

    const names = Object.keys(b.phases || {});
    show(phases, names.length > 0);
    if (names.length){
      const join = names.join("");
      if (join !== phaseKeys){
        phaseKeys = join;
        bar.textContent = "";
        legend.textContent = "";
        segs = [];
        labels = [];
        names.forEach((k, i) => {
          const seg = el("span");
          seg.style.background = PHASE_COLORS[i % PHASE_COLORS.length];
          bar.appendChild(seg);
          segs.push(seg);
          const item = el("span");
          item.innerHTML = "<i style='background:" + PHASE_COLORS[i % PHASE_COLORS.length] + "'></i>";
          const name = el("span", null, k + " ");
          const value = el("b");
          item.appendChild(name);
          item.appendChild(value);
          legend.appendChild(item);
          labels.push({seg, value});
        });
      }
      const total = names.reduce((n, k) => n + b.phases[k], 0) || 1;
      names.forEach((k, i) => {
        segs[i].style.width = (b.phases[k] / total * 100) + "%";
        segs[i].title = k + " · " + dur(b.phases[k]);
        txt(labels[i].value, dur(b.phases[k]));
      });
    }

    stats.update(b.hist);
    grids[0].update(b.gauges);
    grids[1].update(b.counters);
    grids[2].update(b.info);
  }};
}

function makeBackend(pname, tname, backend, host){
  const key = keyOf(pname, tname, backend);
  const row = el("button", "be");
  row.type = "button";
  const caret = el("span", "caret", "▶");
  const dot = el("span", "dot");
  const addr = el("span", "addr", backend || "(no fan-out)");
  const facts = el("span", "facts");
  const why = el("span", "why");
  row.appendChild(caret);
  row.appendChild(dot);
  row.appendChild(addr);
  row.appendChild(facts);
  row.appendChild(why);
  host.appendChild(row);

  const entry = {key, row, facts, why, host, panel: null, data: null};
  row.addEventListener("click", () => toggle(key));
  view.backends.set(key, entry);
  return entry;
}

function updateBackend(entry, b){
  entry.data = b;
  entry.row.classList.toggle("down", !b.up);

  const parts = [];
  if (b.latency) parts.push(dur(b.latency));
  if (b.gauges && b.gauges.http_status_code) parts.push(String(b.gauges.http_status_code));
  if (b.age) parts.push("cert " + Math.round(b.age) + "d");
  txt(entry.facts, parts.join("  ·  "));

  // The error message, not just the reason category, goes on the row itself.
  const msg = b.up ? "" : (b.error || b.reason || "");
  txt(entry.why, msg);
  entry.why.title = msg;
  entry.why.className = msg ? "why" : "spacer";

  if (entry.panel) entry.panel.update(b);
}

function toggle(key){
  const entry = view.backends.get(key);
  if (!entry) return;
  if (entry.panel){
    entry.panel.root.remove();
    entry.panel = null;
    expanded.delete(key);
    attr(entry.row, "aria-expanded", "false");
  } else {
    entry.panel = makePanel(key);
    entry.row.after(entry.panel.root);
    expanded.add(key);
    attr(entry.row, "aria-expanded", "true");
    if (entry.data) entry.panel.update(entry.data);
  }
  window.history.replaceState(null, "",
    expanded.size ? "#" + [...expanded].map(encodeURIComponent).join(";") : "#");
}

function makeTarget(pname, t, host){
  const root = el("div", "target");
  const head = el("div", "thead");
  const name = el("div", "tname", t.target);
  const last = el("div", "tlast");
  const spark = makeSpark(110, 24, "var(--s1)");
  spark.root.classList.add("tspark");
  const tally = el("div", "tally");
  const seg = el("div", "seg");
  const count = el("span");
  tally.appendChild(seg);
  tally.appendChild(count);
  head.appendChild(name);
  head.appendChild(last);
  head.appendChild(spark.root);
  head.appendChild(tally);
  root.appendChild(head);

  const rows = el("div", "backends");
  root.appendChild(rows);
  host.appendChild(root);

  const entry = {root, last, spark, seg, count, rows, pname, target: t.target,
    marks: [], failure: null, backends: []};
  for (const b of t.backends) entry.backends.push(makeBackend(pname, t.target, b.backend, rows));
  view.targets.set(pname + SEP + t.target, entry);
  return entry;
}

function updateTarget(entry, t){
  // A target's own trend is the mean across its backends.
  const bufs = t.backends.map((b) => trend(keyOf(entry.pname, t.target, b.backend)))
    .filter((b) => b.v.length);
  const n = bufs.reduce((m, b) => Math.max(m, b.v.length), 0);
  const mean = [];
  for (let i = 0; i < n; i++){
    let sum = 0, cnt = 0;
    for (const b of bufs){
      const v = b.v[b.v.length - n + i];
      if (v > 0){ sum += v; cnt++; }
    }
    mean.push(cnt ? sum / cnt : 0);
  }
  txt(entry.last, mean.length ? dur(mean[mean.length - 1]) : "");
  entry.spark.update(mean);

  if (entry.marks.length !== t.backends.length){
    entry.seg.textContent = "";
    entry.marks = t.backends.map(() => {
      const i = el("i");
      entry.seg.appendChild(i);
      return i;
    });
  }
  t.backends.forEach((b, i) => entry.marks[i].classList.toggle("no", !b.up));
  txt(entry.count, t.up + "/" + t.total);

  // A target with no backends did not resolve, so the reason belongs here.
  if (!t.backends.length){
    if (!entry.failure){
      const f = el("div", "fail");
      const k = el("span", "k", "resolve");
      const m = el("span", "m");
      f.appendChild(k);
      f.appendChild(m);
      entry.rows.appendChild(f);
      entry.failure = m;
    }
    txt(entry.failure, t.error || "the target produced no result");
  }

  t.backends.forEach((b, i) => {
    const be = entry.backends[i];
    if (be) updateBackend(be, b);
  });
}

function makeProbe(p, host){
  const root = el("section", "probe");
  const head = el("div", "phead");
  head.appendChild(el("span", "name", p.name));
  head.appendChild(el("span", "tag", p.type));
  const sleeping = el("span", "tag warn", "paused by schedule");
  const negative = el("span", "tag warn", "negative check");
  const broken = el("span", "tag bad");
  head.appendChild(sleeping);
  head.appendChild(negative);
  head.appendChild(broken);
  head.appendChild(el("span", "every", "every " + p.interval));
  root.appendChild(head);

  const body = el("div");
  root.appendChild(body);
  host.appendChild(root);

  const entry = {root, sleeping, negative, broken, body, targets: []};
  for (const t of p.targets) entry.targets.push(makeTarget(p.name, t, body));
  if (!p.targets.length) body.appendChild(el("div", "empty", "no results yet"));
  view.probes.set(p.name, entry);
  return entry;
}

function updateProbe(entry, p){
  show(entry.sleeping, !!p.sleeping);
  show(entry.negative, !!p.negative);
  const down = p.targets.reduce((n, t) => n + (t.total - t.up) + (t.error ? 1 : 0), 0);
  show(entry.broken, down > 0);
  txt(entry.broken, down + " down");

  let visible = 0;
  p.targets.forEach((t, i) => {
    const te = entry.targets[i];
    if (!te) return;
    const on = matches(p.name, t.target, t.backends) && (!onlyBad || t.up < t.total || t.error);
    show(te.root, on);
    if (!on) return;   // a hidden row is not drawn
    visible++;
    updateTarget(te, t);
  });
  return visible;
}

function matches(probe, target, backends){
  if (!query) return true;
  const q = query.toLowerCase();
  if (probe.toLowerCase().includes(q) || target.toLowerCase().includes(q)) return true;
  return backends.some((b) => (b.backend || "").toLowerCase().includes(q));
}

const tiles = [];
function mountTiles(){
  const box = $("tiles");
  box.textContent = "";
  for (const [cls, label] of [["bad", "down"], ["ok", "up"], ["", "probes"],
    ["", "targets"], ["", "series"]]){
    const d = el("div", "tile " + cls);
    const b = el("b", null, "0");
    d.appendChild(b);
    d.appendChild(el("span", null, label));
    box.appendChild(d);
    tiles.push(b);
  }
}

function updateTiles(){
  let targets = 0;
  for (const p of state.probes) targets += p.targets.length;
  const vals = [state.down, state.up, state.probes.length, targets, state.series];
  tiles.forEach((n, i) => txt(n, String(vals[i])));
}

// shapeOf is which probes, targets and backends exist; only it forces a rebuild.
function shapeOf(s){
  let out = "";
  for (const p of s.probes){
    out += p.name + "|" + p.type + "|" + p.interval + ";";
    for (const t of p.targets){
      out += t.target + ">" + t.backends.map((b) => b.backend || "-").join(",") + ";";
    }
  }
  return out;
}

function mount(){
  const box = $("probes");
  box.textContent = "";
  view.probes.clear();
  view.targets.clear();
  view.backends.clear();

  for (const p of state.probes) makeProbe(p, box);
  if (!state.probes.length){
    const d = el("section", "probe");
    d.appendChild(el("div", "empty", "no probes configured"));
    box.appendChild(d);
  }
  // Panels that were open before the rebuild, or named by the link, open again.
  for (const key of [...expanded]){
    expanded.delete(key);
    toggle(key);
  }
  empty = el("section", "probe");
  empty.appendChild(el("div", "empty", "nothing matches the filter"));
  empty.hidden = true;
  box.appendChild(empty);
}

let empty = null;

function apply(){
  const next = shapeOf(state);
  if (next !== shape){
    shape = next;
    mount();
  }
  updateTiles();
  let shownProbes = 0;
  for (const p of state.probes){
    const entry = view.probes.get(p.name);
    if (!entry) continue;
    const visible = updateProbe(entry, p);
    const on = visible > 0 || (!query && !onlyBad);
    show(entry.root, on);
    if (on) shownProbes++;
  }
  if (empty) show(empty, shownProbes === 0 && state.probes.length > 0);
  txt($("uptime"), "up " + state.uptime);
  txt($("clock"), clock(state.time));
}

async function poll(){
  try {
    const r = await fetch("/status/data", {cache: "no-store"});
    if (!r.ok) throw new Error(r.status);
    state = await r.json();
    misses = 0;
    record(state);
    apply();
  } catch (e) {
    misses++;
  }
  $("live").firstChild.className = "dotlive" + (paused || misses ? " off" : "");
  $("live").title = misses ? "the node is not answering (" + misses + ")" : "live";
}

function schedule(){
  clearInterval(timer);
  if (!paused) timer = setInterval(poll, every);
}

$("every").addEventListener("change", (e) => { every = +e.target.value; schedule(); });
$("pause").addEventListener("click", () => {
  paused = !paused;
  $("pause").textContent = paused ? "resume" : "pause";
  $("pause").classList.toggle("on", paused);
  $("live").firstChild.className = "dotlive" + (paused ? " off" : "");
  schedule();
  if (!paused) poll();
});
// Filtering only hides rows, so typing does not rebuild anything.
$("find").addEventListener("input", (e) => { query = e.target.value.trim(); apply(); });
$("only").addEventListener("click", () => {
  onlyBad = !onlyBad;
  $("only").classList.toggle("on", onlyBad);
  apply();
});
const moonIcon = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20.7 13.2A8.5 8.5 0 0 1 10.8 3.3a8.5 8.5 0 1 0 9.9 9.9z"/></svg>';
const sunIcon = '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="4.2"/>' +
  '<path d="M12 1.6v2.8M12 19.6v2.8M4.6 4.6l2 2M17.4 17.4l2 2M1.6 12h2.8M19.6 12h2.8M4.6 19.4l2-2M17.4 6.6l2-2" ' +
  'stroke="currentColor" stroke-width="1.9" stroke-linecap="round" fill="none"/></svg>';

const prefersDark = window.matchMedia("(prefers-color-scheme: dark)");

// data-theme is unset until somebody chooses, and the system decides until then.
function themeNow(){
  return document.documentElement.getAttribute("data-theme") ||
    (prefersDark.matches ? "dark" : "light");
}

// The button shows the theme a click will give, not the current one.
function paintTheme(){
  const dark = themeNow() === "dark";
  const label = dark ? "switch to the light theme" : "switch to the dark theme";
  $("theme").innerHTML = dark ? sunIcon : moonIcon;
  $("theme").title = label;
  $("theme").setAttribute("aria-label", label);
}

$("theme").addEventListener("click", () => {
  const next = themeNow() === "dark" ? "light" : "dark";
  document.documentElement.setAttribute("data-theme", next);
  try { localStorage.setItem("argus-theme", next); } catch (e) {}
  paintTheme();
});
prefersDark.addEventListener("change", paintTheme);
document.addEventListener("visibilitychange", () => {
  if (document.hidden) clearInterval(timer);
  else { schedule(); if (!paused) poll(); }
});

try {
  const saved = localStorage.getItem("argus-theme");
  if (saved) document.documentElement.setAttribute("data-theme", saved);
} catch (e) {}
paintTheme();

// The fragment names backends to open, so a link can point at a failing one.
for (const k of (location.hash || "").slice(1).split(";")){
  if (k) expanded.add(decodeURIComponent(k));
}

mountTiles();
record(state);
apply();
schedule();


</script>
</body>
</html>
`
