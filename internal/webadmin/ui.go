package webadmin

const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>[gravinet]</title>
<style>
  :root {
    --bg:#0f1419; --panel:#1a2129; --line:#2a3441; --fg:#e6edf3; --mut:#8b98a5;
    --acc:#4493f8; --danger:#f85149; --ok:#3fb950; --sidebar:#141b22; --hover:#202a35;
  }
  :root[data-theme="light"] {
    --bg:#f6f8fa; --panel:#ffffff; --line:#d8dee4; --fg:#1f2328; --mut:#656d76;
    --acc:#0969da; --danger:#cf222e; --ok:#1a7f37; --sidebar:#eceff2; --hover:#e2e7ee;
  }
  * { box-sizing:border-box; }
  html, body { height:100%; }
  body { margin:0; font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace; background:var(--bg); color:var(--fg); }
  #app { height:100%; display:flex; flex-direction:column; }
  a { color:inherit; }
  .top { display:flex; justify-content:space-between; align-items:center; padding:12px 18px; border-bottom:1px solid var(--line); flex:0 0 auto; }
  .brand { font-size:16px; font-weight:600; letter-spacing:.5px; }
  .toggle { background:transparent; border:1px solid var(--line); color:var(--fg); border-radius:6px; padding:6px 11px; cursor:pointer; font:inherit; }
  .cluster { display:flex; gap:8px; align-items:center; margin-left:auto; margin-right:10px; }
  .peer-sel { display:flex; align-items:center; gap:8px; background:var(--panel); color:var(--fg); border:1px solid var(--line); border-radius:6px; padding:6px 9px; font:inherit; cursor:pointer; }
  .peer-sel:hover { background:var(--hover); }
  .peer-sel:focus-visible { outline:none; border-color:var(--acc); }
  .peer-caret { color:var(--mut); font-size:10px; line-height:1; }
  /* Compact variant for pickers inside a card's toolbar (the speedtest pair),
     matching the sizing the inline-styled <select>s there used to have. */
  .peer-sel-sm { padding:5px 9px; font-size:12px; background:var(--bg); }
  body.remote .content { border-color: var(--acc); border-radius:8px; }
  body.remote .peer-sel { border-color:var(--acc); font-weight:600; }
  /* search-select: a text input + filtered dropdown list standing in for a
     plain <select> wherever the option count can get large enough (hundreds
     of mesh nodes) that a native dropdown's scroll-only browsing stops being
     practical. Used by the header's node picker and the speedtest pickers. */
  .search-select { position:relative; display:inline-block; }
  .ss-input { background:var(--panel); color:var(--fg); border:1px solid var(--line); border-radius:6px; padding:6px 9px; font:inherit; width:100%; }
  .ss-input:focus { outline:none; border-color:var(--acc); }
  .ss-list { display:none; position:absolute; top:calc(100% + 4px); left:0; min-width:100%; max-width:340px; max-height:260px; overflow-y:auto; background:var(--panel); border:1px solid var(--line); border-radius:6px; box-shadow:0 8px 24px rgba(0,0,0,.35); z-index:50; }
  .ss-list.show { display:block; }
  .ss-opt { padding:6px 10px; font-size:13px; cursor:pointer; white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
  .ss-opt:hover, .ss-opt.sel { background:var(--hover); }
  .ss-opt.cur { font-weight:600; }
  /* An option the other picker currently holds: the speedtest can't run a node
     against itself. Visible (so you can see why it's unavailable) but inert. */
  .ss-opt.dis { opacity:.45; cursor:not-allowed; }
  .ss-opt.dis:hover { background:transparent; }
  .ss-empty { padding:6px 10px; font-size:13px; color:var(--mut); }
  /* Filter row pinned to the top of the dropdown itself (not beside it): the
     list opens, the filter is the first thing in it, and typing narrows the
     options below. Sticky so it stays put while the options scroll under it. */
  .ss-filter-row { position:sticky; top:0; background:var(--panel); border-bottom:1px solid var(--line); padding:6px; z-index:1; }
  .ss-filter { width:100%; box-sizing:border-box; padding:5px 8px; font-size:13px; }
  .ss-filter::placeholder { color:var(--mut); }
  .ss-list.ss-right { left:auto; right:0; }
  .global-search { width:220px; margin-right:14px; }
  .global-search .ss-opt { cursor:pointer; }
  .search-hit { animation: search-hit-flash 2s ease-out; }
  @keyframes search-hit-flash { 0% { background:var(--acc); } 100% { background:transparent; } }
  .peer-link { cursor:pointer; }
  .peer-link:hover { text-decoration:underline; }
  .toggle:hover { background:var(--hover); }
  .layout { display:flex; flex:1 1 auto; min-height:0; }
  .rail { width:188px; flex:0 0 188px; background:var(--sidebar); border-right:1px solid var(--line); display:flex; flex-direction:column; overflow:hidden; }
  .rail-groups-scroll { flex:1; overflow-y:auto; display:flex; flex-direction:column; gap:2px; padding:10px 8px 8px; min-height:0; }
  .rail-tab { display:flex; align-items:center; gap:10px; text-align:left; width:100%; background:transparent; border:1px solid transparent; color:var(--mut); padding:8px 12px; border-radius:7px; font-size:13px; letter-spacing:.02em; cursor:pointer; user-select:none; }
  .rail-tab:hover { background:var(--hover); color:var(--fg); }
  .rail-tab.active { background:var(--acc); color:#fff; }
  .rail-group { display:flex; flex-direction:column; }
  .rail-group + .rail-group { margin-top:8px; }
  .rail-group-items { display:flex; flex-direction:column; gap:2px; }
  .rail-group-label { display:flex; align-items:center; gap:7px; width:100%; text-align:left; background:transparent; border:none; cursor:pointer; font-family:inherit; padding:6px 12px 5px; font-size:13px; text-transform:uppercase; letter-spacing:.06em; color:var(--mut); }
  .rail-group-label:hover { color:var(--fg); }
  .rail-chevron { font-size:24px; color:var(--mut); transition:transform .15s; display:inline-flex; align-items:center; justify-content:center; width:24px; height:24px; }
  .rail-group.collapsed .rail-chevron { transform:rotate(-90deg); }
  .rail-group.collapsed .rail-group-items { display:none; }
  .rail-group .rail-tab { padding-left:58px; }
  .rail-group-items .rail-tab { text-transform:lowercase; }
  .rail-foot { flex-shrink:0; padding:8px 8px 10px; }
  .rail-foot .rail-tab { text-transform:uppercase; letter-spacing:.04em; }
  .rail-divider { height:1px; background:var(--line); margin:0 0 8px; }
  .rail-logout:hover { color:var(--danger); background:var(--hover); }
  .settings-row { display:flex; align-items:center; justify-content:space-between; padding:12px 0; border-bottom:1px solid var(--line); }
  .settings-row:last-child { border-bottom:0; }
  .local-only-disabled { opacity:.5; }
  .settings-label { font-size:14px; }
  .settings-desc { font-size:12px; color:var(--mut); margin-top:2px; }
  .sw { position:relative; display:inline-block; width:40px; height:22px; flex-shrink:0; }
  .sw input { opacity:0; width:0; height:0; }
  .sw-slider { position:absolute; cursor:pointer; inset:0; background:var(--line); border-radius:22px; transition:.2s; }
  .sw-slider:before { position:absolute; content:""; height:16px; width:16px; left:3px; bottom:3px; background:#fff; border-radius:50%; transition:.2s; }
  .sw input:checked + .sw-slider { background:var(--acc); }
  .sw input:checked + .sw-slider:before { transform:translateX(18px); }
  .sw input:disabled + .sw-slider { cursor:not-allowed; }
  .content { flex:1; padding:22px; overflow:auto; min-height:0; border:2px solid transparent; }
  .content > h2.sec { margin:0 0 16px; font-size:15px; letter-spacing:.5px; }
  button { background:var(--acc); color:#fff; border:0; border-radius:6px; padding:7px 12px; cursor:pointer; font:inherit; }
  button.ghost { background:transparent; border:1px solid var(--line); color:var(--fg); }
  button.danger { background:var(--danger); }
  button.ok { background:var(--ok); color:#fff; }
  button.sm { padding:3px 8px; font-size:12px; }
  /* Ban/Unban/Delete share the .sm row-button recipe but, unlike .tbar-btn,
     never got their own sizing — matching height via line-height (ratio or
     explicit pixel value) turned out to still misalign the text vertically;
     line-height-based centering depends on font metrics that vary by engine
     and don't behave consistently. Flexbox centering sidesteps that
     entirely — it centers the actual content box, not a font-metric-derived
     line box — so it's used here instead. .tbar-btn (below) gets the same
     treatment so every row button, icon or text label, lines up identically
     regardless of its own font-size. */
  button.sm.danger, button.sm.ok { height:25px; padding:0 10px; display:inline-flex; align-items:center; justify-content:center; }
  input,select { background:var(--bg); border:1px solid var(--line); color:var(--fg); border-radius:6px; padding:7px 9px; font:inherit; }
  /* Upgrade page's file picker(s) — widened from the default input size so
     a full filename (source tarball, signed manifest) is readable without
     truncating; vertical padding is untouched, same height as any other input. */
  .up-file, .up-bin, .up-man { min-width:420px; }
  /* Firewall rule editor's per-dimension NOT toggle: a small "Ø" button
     layered inside its input (services/src/dst) rather than a separate
     checkbox+label off to the side. .fwe-field is the positioning context;
     the input's own right padding is widened here just enough to keep typed
     text from running under the button. Off state is dim (matches a
     placeholder, not competing with real content); .active — meaning this
     dimension now matches anything EXCEPT what's typed — switches to the
     same danger color the table view's leading "!" implies, so the two
     stay visually linked. */
  .fwe-field { position:relative; display:inline-block; }
  .fwe-field input { padding-right:22px; }
  .fwe-neg { position:absolute; top:1px; right:1px; bottom:1px; width:20px; padding:0; border:none; background:transparent; color:var(--mut); font:inherit; font-size:13px; line-height:1; cursor:pointer; border-radius:0 5px 5px 0; }
  .fwe-neg:hover { color:var(--fg); }
  .fwe-neg.active { color:var(--danger,#b33); font-weight:bold; }
  .card { background:var(--panel); border:1px solid var(--line); border-radius:10px; padding:16px; margin-bottom:16px; }
  .tscroll { overflow-x:auto; max-width:100%; }
  .tscroll > table { width:auto; min-width:100%; }
  .card h3 { margin:0 0 12px; font-size:13px; text-transform:uppercase; letter-spacing:1px; color:var(--mut); }
  .net-id { color:var(--acc); }
  .card h3 .net-id { color:var(--acc); text-transform:none; letter-spacing:0; }
  .card h3 .net-name { color:var(--fg); text-transform:none; letter-spacing:0; }
  table { width:100%; border-collapse:collapse; }
  /* Fixed, explicit widths so this table's columns line up identically
     across every network's card — with the default auto layout, each
     table sizes its own columns from its own content, so two networks
     with different endpoint/hostname lengths drift out of alignment. */
  table.peers-table { table-layout:fixed; }
  table.peers-table col.c-sel { width:28px; }
  table.peers-table col.c-target { width:20%; }
  table.peers-table col.c-state { width:8%; }
  /* Mesh > Peers only has target/state/overlay left after Monitor > Mesh
     Peers took the rest, so it gets its own wider proportions instead of
     reusing c-target/c-state (still sized for the monitor page's fuller,
     7-column layout); otherwise the operate table would sit cramped on
     the left with one oversized overlay column soaking up all the room the
     removed columns freed up. */
  table.peers-table col.c-target-op { width:38%; }
  table.peers-table col.c-state-op { width:20%; }
  table.peers-table col.c-key { width:10%; }
  table.peers-table col.c-overlay { width:11%; }
  table.peers-table col.c-endpoint { width:17%; }
  table.peers-table col.c-reach { width:7%; }
  table.peers-table col.c-time { width:7%; }
  table.peers-table col.c-transport { width:auto; }
  table.peers-table col.c-fill { width:auto; }
  table.peers-table td, table.peers-table th { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  table.peers-table td.c-transport-cell, table.peers-table th:last-child { overflow:visible; white-space:normal; }
  th,td { text-align:left; padding:6px 10px; border-bottom:1px solid var(--line); font-size:13px; }
  th { color:var(--mut); font-weight:600; }
  tr:last-child td { border-bottom:0; }
  .row { display:flex; gap:8px; flex-wrap:wrap; align-items:center; }
  .login { max-width:320px; margin:90px auto; }
  .login h2 { margin:0 0 14px; font-size:18px; text-align:center; }
  .err { color:var(--danger); min-height:18px; }
  .pill { font-size:11px; padding:1px 8px; border-radius:20px; border:1px solid var(--line); color:var(--mut); }
  .on { color:var(--ok); } .off { color:var(--mut); }
  @keyframes latFlashUp { from { background:rgba(63,185,80,.28); } to { background:transparent; } }
  @keyframes latFlashDown { from { background:rgba(248,81,73,.28); } to { background:transparent; } }
  tr.lat-flash-up { animation:latFlashUp 2.5s ease-out; }
  tr.lat-flash-down { animation:latFlashDown 2.5s ease-out; }
  .empty { color:var(--mut); font-style:italic; padding:6px 10px; }
  .hint { color:var(--mut); font-size:12px; margin:-6px 0 14px; }
  td.editable { cursor:text; }
  .bw-edit { cursor:text; text-decoration:underline dotted var(--mut); text-underline-offset:3px; }
  .bw-edit:hover { text-decoration-color:var(--acc); color:var(--acc); }
  .bw-editing { display:inline-flex; gap:6px; align-items:center; }
  td.editable:hover { background:var(--line); border-radius:4px; box-shadow:inset 0 -1px 0 var(--acc); }
  td.editing { padding:2px 6px; }
  input.cell-edit { width:100%; min-width:90px; box-sizing:border-box; font:inherit; font-size:13px;
    padding:3px 6px; background:var(--bg); color:var(--fg); border:1px solid var(--acc); border-radius:4px; }
  .keycell .kval { font-family:ui-monospace,Menlo,Consolas,monospace; font-size:12px; word-break:break-all; }
  .keycell .kval.masked { color:var(--mut); letter-spacing:2px; }
  .tag-toggle { cursor:pointer; user-select:none; }
  .tag-toggle:hover { text-decoration:underline; text-underline-offset:2px; }
  tr.peer-dis td:not(.selcol) { opacity:.5; }
  tr.peer-self { font-style:italic; }
  tr.ban-locked td:not(.selcol) { opacity:.5; }
  tr.ban-locked { cursor:default; }
  input.rsel:disabled { cursor:not-allowed; opacity:.4; }
  tr.fwrow { cursor:grab; }
  tr.fwrow:active { cursor:grabbing; }
  tr.fwrow.dragging { opacity:.4; }
  tr.fwrow.drop-target td { box-shadow: inset 0 2px 0 var(--acc); }
  tr.fw-disabled td { opacity:.45; }
  tr.selrow td { background:var(--hover); }
  tr.selectable { cursor:pointer; }
  td.selcol, th.selcol { width:1%; white-space:nowrap; }
  input.rsel, input.rall { cursor:pointer; width:15px; height:15px; accent-color:var(--acc); margin:0; vertical-align:middle; }
  .toolbar { margin-bottom:10px; }
  .toolbar button[disabled] { opacity:.45; cursor:not-allowed; }
  th.sortable-th { cursor:pointer; user-select:none; }
  th.sortable-th:hover { color:var(--fg); }
  th.sortable-th[data-sort]::after { content:'▲'; font-size:9px; margin-left:5px; opacity:.7; }
  th.sortable-th[data-sort="desc"]::after { content:'▼'; }
  input.tfilter { width:440px; max-width:100%; padding:5px 9px; font-size:12px; }
  .tbar { display:flex; gap:6px; align-items:center; margin-bottom:9px; }
  .tbar-btn { width:28px; height:25px; padding:0; font-size:15px; text-align:center; display:inline-flex; align-items:center; justify-content:center; }
  .tbar-gap { margin-left:28px; } /* one button's width of extra separation from the +/- group */
  .modal-backdrop { position:fixed; inset:0; background:rgba(0,0,0,.55); display:flex; align-items:center; justify-content:center; z-index:1000; padding:24px; }
  .modal-panel { background:var(--panel); border:1px solid var(--line); border-radius:10px; max-width:720px; width:100%; max-height:86vh; display:flex; flex-direction:column; box-shadow:0 12px 40px rgba(0,0,0,.5); }
  .modal-head { display:flex; align-items:center; justify-content:space-between; padding:14px 18px; border-bottom:1px solid var(--line); flex:0 0 auto; }
  .modal-head h3 { margin:0; font-size:15px; font-weight:600; }
  .modal-close { background:transparent; border:1px solid var(--line); color:var(--fg); border-radius:6px; padding:4px 11px; cursor:pointer; font:inherit; }
  .modal-close:hover { background:var(--hover); }
  .modal-body { padding:16px 18px; overflow-y:auto; }
  .modal-body section { margin-bottom:16px; }
  .modal-body section:last-child { margin-bottom:0; }
  .modal-body h4 { margin:0 0 6px; font-size:11px; text-transform:uppercase; letter-spacing:1px; color:var(--mut); }
  .term-panel { max-width:920px; width:auto; }
  .term-screen { background:#0b0e12; padding:10px 12px; border-radius:0 0 8px 8px; }
  .term-status { padding:6px 12px; font-size:12px; color:var(--mut); border-top:1px solid var(--line); }
  .term-status.err { color:var(--danger); }
  .modal-body pre { white-space:pre-wrap; word-break:break-word; background:var(--bg); border:1px solid var(--line); border-radius:6px; padding:10px; font-size:12px; max-height:280px; overflow-y:auto; margin:0; }
  .pick-cat-empty { padding:8px 4px; color:var(--mut); font-size:12px; }
  .subcard { background:var(--bg); border:1px solid var(--line); border-radius:7px; padding:12px 14px; margin-top:14px; }
  .subcard:first-of-type { margin-top:10px; }
  .subcard h4 { margin:0 0 8px; font-size:11px; text-transform:uppercase; letter-spacing:1px; color:var(--mut); }
  .info-kv { display:grid; grid-template-columns:max-content 1fr; gap:8px 18px; align-items:baseline; }
  .info-kv .k { color:var(--mut); }
  .info-kv .v { color:var(--fg); font-variant-numeric:tabular-nums; }
  .mono-block { white-space:pre-wrap; word-break:break-word; margin:0; font-size:12.5px; line-height:1.5; color:var(--fg); font-family:ui-monospace,Menlo,Consolas,monospace; }
  .seg { display:inline-flex; gap:2px; background:var(--bg); border:1px solid var(--line); border-radius:8px; padding:2px; }
  .seg-btn { background:transparent; border:none; color:var(--mut); border-radius:6px; padding:5px 12px; cursor:pointer; font:inherit; font-size:12.5px; }
  .seg-btn:hover { color:var(--fg); }
  .seg-btn.active { background:var(--panel); color:var(--fg); }
  .metric-card { margin-bottom:16px; }
  .metric-head { display:flex; align-items:baseline; justify-content:space-between; margin-bottom:6px; }
  .metric-title { font-size:13px; color:var(--fg); }
  .metric-now { font-size:12px; color:var(--mut); font-variant-numeric:tabular-nums; }
  .metric-legend { font-size:11px; color:var(--mut); }
  .metric-legend b { font-weight:600; }
  svg.chart { display:block; width:100%; height:auto; }
  .chart-holder { position:relative; }
  svg.chart .capture { cursor:crosshair; }
  .chart-tip { position:absolute; pointer-events:none; transform:translate(12px,-50%); background:var(--panel); border:1px solid var(--line); border-radius:6px; padding:6px 8px; font-size:11.5px; color:var(--fg); white-space:nowrap; z-index:5; box-shadow:0 3px 10px rgba(0,0,0,.35); }
  .chart-tip.flip { transform:translate(calc(-100% - 12px),-50%); }
  .chart-tip .tip-t { color:var(--mut); margin-bottom:3px; font-variant-numeric:tabular-nums; }
  .chart-tip .tip-row { display:flex; align-items:center; gap:6px; }
  .chart-tip .tip-dot { width:8px; height:8px; border-radius:50%; display:inline-block; flex:0 0 auto; }
  .chart-tip b { font-variant-numeric:tabular-nums; }
</style>
<link rel="stylesheet" href="/static/xterm.css">
</head>
<body>
<div id="app"></div>
<script src="/static/xterm.js"></script>
<script>
const $ = (h) => { const d=document.createElement('div'); d.innerHTML=h.trim(); return d.firstChild; };
const app = document.getElementById('app');
const state = { section:'networks', status:[], cfg:[], restartPending:false, statusSig:'', polling:false, target:null, cluster:[], managed:false, manager:false, natStateTimeout:0, geoipLookup:false, enableUpnp:false, allowRemoteShell:false, shellSupported:true, bgpSupported:false, selfId:null, selfHostname:'', targetSeq:0, pendingBgpHighlight:null };
// setTarget is the only place state.target is ever assigned — bumping
// targetSeq alongside it, once, exactly when the *selection itself* actually
// changes. load()/startPolling()/refreshCluster() each capture targetSeq
// (read-only, no increment of their own — see their own comments) before
// firing a request and compare it again after, so multiple fetches launched
// together for the *same* switch (sel.onchange fires refresh() and
// refreshCluster() side by side, neither awaiting the other) share one
// generation and don't invalidate each other; only a genuine further switch
// while they're in flight bumps the generation and makes them stale.
function setTarget(v){ state.target = v; state.targetSeq++; }

// selection holds the currently ticked rows per multi-select section, keyed by
// "netid#rowid", so a selection survives the 4s status re-render (peers/bans
// repaint on their own). Top-of-table buttons act on whatever is ticked.
const selection = { peers:new Set(), mpeers:new Set(), keys:new Set(), bans:new Set() };

// DROPDOWN_FILTER_MIN is the option count above which a peer picker grows a
// filter box. Below this, the list is already easy to scan, so a filter would
// just be clutter for the common case of a handful of peers; above it (a large
// mesh), scrolling a single unfiltered list stops being practical and a filter
// is the only way to narrow it down — native <select> elements have no built-in
// search, only OS type-ahead (jumps to the next option starting with the typed
// letter, resets after a pause — not a real filter).
//
// Where that box lives differs by picker, for a reason worth stating once:
//   - the header's node picker is a hand-rolled listbox (buildPeerPicker), so
//     its filter sits at the TOP OF THE LIST ITSELF — open the dropdown, type,
//     the options below narrow.
//   - the speedtest source/target pickers are still native <select>s, whose
//     option list is an OS-drawn popup that no markup can be placed inside, so
//     their filter can only sit beside them. Converting them to the same listbox
//     is the obvious follow-up.
const DROPDOWN_FILTER_MIN = 10;
function selKey(net, id){ return net+'#'+id; }
function selectedIn(sec, net){
  const out=[], pre=net+'#';
  selection[sec].forEach(k => { if (k.indexOf(pre)===0) out.push(k.slice(pre.length)); });
  return out;
}
// wireSelectable turns a rendered table into a checkbox/row-click multi-select,
// restoring ticks from the selection set so re-renders don't lose them. The
// header "select all" box and per-row clicks stay in sync.
function wireSelectable(t, sec){
  const set = selection[sec];
  const boxes = [].slice.call(t.querySelectorAll('input.rsel')).filter(b => !b.disabled);
  const all = t.querySelector('input.rall');
  const reflect = (cb) => {
    const tr = cb.closest('tr');
    if (cb.checked){ set.add(cb.dataset.k); tr.classList.add('selrow'); }
    else { set.delete(cb.dataset.k); tr.classList.remove('selrow'); }
  };
  const syncAll = () => { if (all) all.checked = boxes.length>0 && boxes.every(b=>b.checked); };
  boxes.forEach(cb => {
    cb.checked = set.has(cb.dataset.k);
    if (cb.checked) cb.closest('tr').classList.add('selrow');
    cb.onclick = (e) => { e.stopPropagation(); reflect(cb); syncAll(); };
  });
  t.querySelectorAll('tr.selectable').forEach(tr => {
    tr.onclick = (e) => {
      if (e.target.tagName === 'INPUT') return; // checkbox handles its own click
      const cb = tr.querySelector('input.rsel'); if (!cb || cb.disabled) return;
      cb.checked = !cb.checked; reflect(cb); syncAll();
    };
  });
  if (all) all.onclick = (e) => { e.stopPropagation(); boxes.forEach(cb => { cb.checked = all.checked; reflect(cb); }); };
  syncAll();
}
// netCardHead builds a per-network card heading whose enabled/disabled tag is
// double-clicked to toggle the feature (no separate enable/disable button).
function netCardHead(cf, en, apiPath, enOp, disOp){
  enOp = enOp || 'enable'; disOp = disOp || 'disable';
  const h3 = $('<h3><span class="net-name">'+esc(cf.name)+'</span> <span class="net-id">'+esc(cf.id)+'</span> </h3>');
  const tag = $('<span class="pill tag-toggle '+(en?'on':'off')+'" title="double-click to '+(en?'disable':'enable')+'">'+(en?'enabled':'disabled')+'</span>');
  // Flip immediately and fire the request in the background rather than
  // waiting on it — see toggleTagState's doc comment for why. This one still
  // goes through quietRestart on the rare op that reports restart:true
  // instead of a plain background refresh(), same as edit()'s autoRestart
  // path did.
  tag.ondblclick = () => {
    const on = !en;
    en = on;
    tag.className = 'pill tag-toggle ' + (on ? 'on' : 'off');
    tag.textContent = on ? 'enabled' : 'disabled';
    tag.title = 'double-click to ' + (on ? 'disable' : 'enable');
    api(apiPath, { method:'POST', body: JSON.stringify({ op:(on?enOp:disOp), net:cf.name }) })
      .then(r => {
        if (!r.ok) { console.warn(apiPath+' toggle failed:', (r.body&&r.body.error)||'failed'); return; }
        if (r.body && r.body.restart) { quietRestart(); return; }
        refresh();
      });
  };
  h3.appendChild(tag);
  return h3;
}

function theme(){ return document.documentElement.getAttribute('data-theme') || 'dark'; }
function setTheme(t){ document.documentElement.setAttribute('data-theme', t); try{ localStorage.setItem('gravinet-theme', t); }catch(e){} }
(function(){
  let t='dark';
  try { t = localStorage.getItem('gravinet-theme') || (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches ? 'light':'dark'); } catch(e){}
  setTheme(t);
})();

// LOCAL_API paths always hit this node, even while a remote peer is selected.
// Most of these are about the local session/browsing state itself (cluster
// listing, login, the header dropdown's own polling). Managed/Manager mode
// are a deliberate exception to the "otherwise proxy everything" rule, not
// an oversight: by design they are never remotely configurable — only the
// node you're actually logged into can have its own Managed/Manager status
// changed from here, regardless of which peer is selected above. (An
// earlier version of this list proxied them like any other per-host
// setting, which produced its own confusing bug — an operator toggling what
// looked like a remote peer's Managed mode was actually toggling their own
// node's, silently. Making them local-only outright, rather than
// "correctly" proxied, is the fix.)
//
// Packet capture (/api/capture/*) does NOT belong here, even though it once
// did: it was swept into this list at the same time as the Managed/Manager
// exception above, on the same "local session state" reasoning — but
// capture is fundamentally about a *peer's* interface, not this browser's
// session, and forcing it local meant the Capture tab silently ignored the
// header dropdown and always showed this node's own interfaces no matter
// which peer was selected. It's proxied normally now; see handleProxy's own
// size-limit comment for the one real capture-specific wrinkle (the .pcap
// download can be larger than the generic proxy response cap).
const LOCAL_API = ['/api/proxy','/api/cluster','/api/managed','/api/manager','/api/shell/setting','/api/login','/api/logout','/api/ping',
  // Every /api/upgrade/* endpoint always runs on the node you are logged into
  // never on whichever peer happens to be selected in the header.
  // handleProxy and each handler's own upgradeLocalOnly check enforce this
  // server-side too (that is the actual boundary; this is just the client-side
  // half, so the browser doesn't even try). Upgrades are local-only outright:
  // a peer's own Upgrade page is something you visit by logging into *that*
  // node directly.
  '/api/upgrade','/api/upgrade/local-apply','/api/upgrade/stage','/api/upgrade/stage-source','/api/upgrade/rollback'];
async function api(path, opts={}, target) {
  let url = path;
  const t = target !== undefined ? target : state.target;
  if (t) {
    const base = path.split('?')[0];
    if (LOCAL_API.indexOf(base) < 0) {
      url = '/api/proxy?node='+encodeURIComponent(t)+'&path='+encodeURIComponent(path);
    }
  }
  const r = await fetch(url, { headers:{'Content-Type':'application/json'}, cache:'no-store', ...opts });
  const body = await r.json().catch(()=>({}));
  return { ok:r.ok, status:r.status, body };
}
// withTimeout races a promise against a timer so a caller can never be stuck
// waiting forever on it — a plain fetch()/api() call has no timeout of its
// own, and a wedged server-side handler (or a request that never reaches the
// server at all — a dropped connection, a proxy hop to an unreachable peer)
// would otherwise hang the UI on "loading…"/"checking…" with no way to tell
// a slow-but-working request from a genuinely stuck one. Rejects with a
// clear "timed out after Ns" Error when the timer wins, so callers can show
// that instead of spinning indefinitely.
function withTimeout(p, ms){
  return Promise.race([
    p, new Promise((_, rej) => setTimeout(() => rej(new Error('timed out after ' + (ms/1000) + 's')), ms)),
  ]);
}
function esc(s){ return String(s==null?'':s).replace(/[&<>"]/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])); }
// nodeCell renders a node as "hostname UID" with the id in blue (the same
// net-id styling used for network ids), falling back to just the id when the
// hostname isn't known. Shared by the bans, peers, and monitor > mesh peers
// tables. netId/endpoint are optional — when given, the whole cell gets a
// title tooltip carrying that node's seed/peer notes (see nodeNotesTitle);
// callers with no netId (or nothing to look up) just get the plain cell,
// unchanged from before.
function nodeCell(host, id, netId, endpoint){
  const idHtml = '<span class="net-id">'+esc(id)+'</span>';
  const label = host ? esc(host)+' '+idHtml : idHtml;
  const notes = netId ? nodeNotesTitle(netId, id, endpoint) : '';
  return notes ? '<span title="'+notes+'">'+label+'</span>' : label;
}
// splitHostPort splits "host:port" into parts, tolerating a bracketed IPv6
// literal ("[::1]:51820"). A bare address with more than one colon and no
// brackets is ambiguous (portless IPv6 vs. malformed host:port) and is
// returned whole as the host rather than guessed at.
function splitHostPort(addr){
  addr = (addr||'').trim();
  if (!addr) return { host:'', port:'' };
  if (addr[0] === '['){
    const end = addr.indexOf(']');
    if (end === -1) return { host:addr, port:'' };
    const rest = addr.slice(end+1);
    return { host:addr.slice(1,end), port: rest.startsWith(':') ? rest.slice(1) : '' };
  }
  if ((addr.match(/:/g)||[]).length > 1) return { host:addr, port:'' };
  const idx = addr.lastIndexOf(':');
  return idx === -1 ? { host:addr, port:'' } : { host:addr.slice(0,idx), port:addr.slice(idx+1) };
}
// seedNotesForAddr finds the notes on a configured seed (network netId)
// whose address matches a peer's observed underlay endpoint. Tries a full
// host:port match first (an operator who entered the exact address the peer
// answers on) — unambiguous by construction, since it pins down one exact
// seed row. Falling back to a host-only match is common and useful (a seed
// is often entered without a port, or the peer answered on an extra port
// rather than the one in the seed list) but only safe when every seed
// sharing that host agrees on the same note. Two seeds on the same host with
// two *different* notes — one box per forwarded port behind a single NAT'd
// IP, the exact case a seed list can express perfectly but a bare address
// can't disambiguate — means guessing would silently attach one box's note
// to the other's row the moment its live port drifts off its own seed (a
// NAT rebind after an idle timeout, a stretch spent relayed, anything that
// isn't the literal seeded port). Better to show nothing than show the
// wrong one confidently.
function seedNotesForAddr(netId, endpoint){
  if (!netId || !endpoint) return '';
  const cfg = state.cfg.find(c => c.id === netId);
  if (!cfg || !(cfg.seeds||[]).length) return '';
  const ep = splitHostPort(endpoint);
  const hostNotes = new Set();
  for (const s of cfg.seeds){
    const notes = s.notes||s.Notes||'';
    if (!notes) continue;
    const addr = stripScheme(s.address||s.Address||'');
    if (addr.toLowerCase() === endpoint.toLowerCase()) return notes; // exact host:port
    const sp = splitHostPort(addr);
    if (sp.host && ep.host && sp.host.toLowerCase() === ep.host.toLowerCase()) hostNotes.add(notes);
  }
  return hostNotes.size === 1 ? [...hostNotes][0] : '';
}
// peerNotesFor finds the local operator note on a network's peer by node id,
// whether it's currently connected or just locally disabled — both carry the
// Notes field sourced from Config.PeerNotes (see peerRowsForNet).
function peerNotesFor(netId, nodeId){
  if (!netId || !nodeId) return '';
  const status = state.status.find(s => s.id === netId);
  if (!status) return '';
  const p = (status.peers||[]).find(x => (x.NodeID||x.node_id)===nodeId);
  if (p) return p.Notes||p.notes||'';
  const d = (status.disabled_peers||[]).find(x => (x.NodeID||x.node_id)===nodeId);
  if (d) return d.Notes||d.notes||'';
  return '';
}
// nodeNotesTitle combines a node's seed and peer notes into one escaped
// tooltip value — seed notes win when both exist (a seed note identifies a
// whole site/box, deliberately entered before any peer relationship exists;
// a peer note is attached after the fact, so the seed note is treated as the
// more authoritative of the two rather than merging or picking arbitrarily).
function nodeNotesTitle(netId, nodeId, endpoint){
  const notes = seedNotesForAddr(netId, endpoint) || peerNotesFor(netId, nodeId);
  return notes ? esc(notes) : '';
}
// notesTitleForNetName is nodeNotesTitle for callers that only have a
// network *name* (Monitor → latency's /api/latency response has no network
// id) — resolves name -> id via state.cfg, then looks up that peer's
// observed endpoint in state.status so seed-address matching has something
// to compare against, same as everywhere else.
function notesTitleForNetName(netName, nodeId){
  const cfg = state.cfg.find(c => c.name === netName);
  if (!cfg) return '';
  const status = state.status.find(s => s.id === cfg.id);
  let endpoint = '';
  if (status){
    const p = (status.peers||[]).find(x => (x.NodeID||x.node_id)===nodeId);
    if (p) endpoint = p.Endpoint||p.endpoint||'';
  }
  return nodeNotesTitle(cfg.id, nodeId, endpoint);
}
// fmtElapsed formats a Unix-nanosecond timestamp as a compact "how long ago"
// duration (e.g. "3m12s", "2h5m", "1d4h") for the peers table's TIME column.
function fmtElapsed(estNano){
  if (!estNano) return '';
  let s = Math.round(Date.now()/1000 - estNano/1e9);
  if (s < 0) s = 0;
  const d = Math.floor(s/86400); s -= d*86400;
  const h = Math.floor(s/3600); s -= h*3600;
  const m = Math.floor(s/60); s -= m*60;
  if (d) return d+'d '+h+'h';
  if (h) return h+'h '+m+'m';
  if (m) return m+'m '+s+'s';
  return s+'s';
}
function rate(bps){ if(!bps||bps<=0) return 'unlimited'; const b=bps*8; if(b>=1e9) return (b/1e9).toFixed(2).replace(/\.?0+$/,'')+' Gbps'; if(b>=1e6) return (b/1e6).toFixed(2).replace(/\.?0+$/,'')+' Mbps'; if(b>=1e3) return (b/1e3).toFixed(2).replace(/\.?0+$/,'')+' Kbps'; return b+' bps'; }
function cfgOf(id){ return state.cfg.find(n=>n.id===id) || {}; }
function nameOf(id){ const c=cfgOf(id); return c.name || id; }

// netNameCmp orders networks for display: alphabetical by name, case- and
// accent-insensitive, with embedded numbers ordered naturally so "net2" sorts
// before "net10". Used everywhere per-network cards are listed so every section
// is consistently ordered.
function netNameCmp(an, bn){
  return (an || '').localeCompare(bn || '', undefined, { numeric: true, sensitivity: 'base' });
}

function showLogin(msg) {
  app.innerHTML = '';
  const box = $('<div class="card login"><h2>[gravinet]</h2>'
    + '<div class="row" style="flex-direction:column;align-items:stretch;gap:10px">'
    + '<input id="u" placeholder="username" autocomplete="username">'
    + '<input id="p" type="password" placeholder="password" autocomplete="current-password">'
    + '<button id="go">Sign in</button><div class="err" id="e"></div></div></div>');
  app.appendChild(box);
  const err = box.querySelector('#e'); if (msg) err.textContent = msg;
  const submit = async () => {
    err.textContent = '';
    const u = box.querySelector('#u').value, p = box.querySelector('#p').value;
    const r = await api('/api/login', { method:'POST', body: JSON.stringify({user:u, pass:p}) });
    if (r.ok) return dashboard();
    if (r.status === 429) err.textContent = 'Locked out. Retry in ' + (r.body.retry_after_seconds||'?') + 's.';
    else err.textContent = r.body.error || 'Login failed';
  };
  box.querySelector('#go').onclick = submit;
  box.querySelector('#p').onkeydown = e => { if (e.key==='Enter') submit(); };
}

// NAV_GROUPS mirrors parapet's grouped left-rail nav: related sections are
// bundled under a collapsible, uppercase group label instead of one long flat
// list. Each item is [sectionKey, tooltip]. Settings and Sign out aren't
// here — they're pinned in the rail's foot area, same as parapet's settings tab.
const NAV_GROUPS = [
  { name:'mesh', items: [
    ['networks', 'define overlay networks: subnets, addressing, MTU'],
    ['keys', 'cryptographic keys used to authenticate this network\u2019s peers'],
    ['seeds', 'bootstrap addresses used to find and reconnect to peers'],
    ['peers', 'enable, disable, or ban nodes known on this network'],
    ['bans', 'nodes blocked from joining or reconnecting'],
  ]},
  { name:'traffic', items: [
    ['firewall', 'rules controlling which traffic is allowed through the tunnel'],
    ['nat', 'port forwarding and address translation for tunnel traffic'],
    ['qos', 'traffic prioritization and queuing order'],
    ['bandwidth', 'rate limiting per peer or network'],
    ['routes', 'additional subnets redistributed across the mesh'],
    // BGP/BFD configuration — gravinet owns the config and drives the FRR
    // daemon. Present in the model unconditionally but shown only when this
    // host has vtysh (i.e. FRR is installed); sectionVisible() filters it out
    // of the rail and search everywhere else, so on a box without FRR the
    // entry simply doesn't appear.
    ['bgp', 'BGP and BFD configuration, applied to FRR (shown only when vtysh is present on this host)'],
  ]},
  { name:'naming', items: [
    ['dns', 'conditional forwarding of specific domains to mesh DNS servers'],
    ['hosts', 'custom hostname records advertised to peers'],
  ]},
  { name:'monitor', items: [
    ['metrics', 'live CPU, memory, disk, and per-overlay-interface throughput'],
    ['mesh-peers', 'live connection health, transport, and session detail for every peer'],
    ['capture', 'live packet capture on an overlay interface'],
    ['speedtest', 'measure throughput between this node and a managed peer'],
    ['latency', 'round-trip time from this host to every other mesh peer'],
    ['route-table', 'the live kernel routing table on this host'],
    // Live BGP session state from FRR — the routing analogue of route-table.
    // Read-only; the editor lives under Traffic > BGP. Gated on vtysh like the
    // editor (sectionVisible), so it's hidden on hosts without FRR.
    ['bgp-peers', 'live BGP peer sessions reported by FRR (shown only when vtysh is present on this host)'],
    ['hosts-file', 'the live contents of this host\u2019s hosts file'],
    ['dns-state', 'what\u2019s actually registered with this host\u2019s OS resolver right now'],
    ['logs', 'the daemon\u2019s recent log output'],
  ]},
  { name:'info', items: [
    ['upgrade', 'check and apply a new gravinet binary on this node; local only, no peer can trigger this'],
    ['readme', 'project documentation'],
    ['getting-started', 'the full onboarding walkthrough'],
    ['license', 'license information'],
    ['about', 'build and host identity'],
  ]},
];
const SECTIONS = NAV_GROUPS.flatMap(g => g.items.map(it => it[0]));
function label(s){
  if (s==='settings') return 'Settings';
  if (s==='bandwidth') return 'Shaping';
  if (s==='route-table') return 'Route Table';
  if (s==='hosts-file') return 'Hosts File';
  if (s==='dns-state') return 'DNS State';
  if (s==='mesh-peers') return 'Mesh Peers';
  if (s==='routes') return 'Mesh Routes';
  if (s==='bgp-peers') return 'BGP Peers';
  if (s==='getting-started') return 'Getting Started';
  return s==='nat'||s==='qos'||s==='dns'||s==='bgp' ? s.toUpperCase() : s.charAt(0).toUpperCase()+s.slice(1);
}

// sectionVisible gates sections whose availability depends on a runtime
// capability of the node being managed, rather than being universal. Today
// that's just BGP, which needs FRR's vtysh present (state.bgpSupported, set
// from /api/config); everything else is always visible. Both the rail nav and
// the global search index consult this, so a hidden section can't be reached
// by clicking or by searching, and renderSection guards the dispatch as a
// belt-and-suspenders backstop.
function sectionVisible(sec){
  if (sec === 'bgp' || sec === 'bgp-peers') return !!state.bgpSupported;
  return true;
}

// groupFor finds which NAV_GROUPS entry a section belongs to (settings/other
// non-grouped sections have none).
function groupFor(sec){ return NAV_GROUPS.find(g => g.items.some(it => it[0]===sec)); }

// ---- collapsible rail groups (state persisted across reloads) ----
// Accordion behavior, same as parapet: expanding one group collapses every
// other group, so only one section's items are visible at a time.
const RAIL_KEY = 'gravinet_rail_collapsed_v1';
function loadCollapsedRailGroups(){
  try { return new Set(JSON.parse(localStorage.getItem(RAIL_KEY) || '[]')); } catch (_) { return new Set(); }
}
function saveCollapsedRailGroups(set){
  try { localStorage.setItem(RAIL_KEY, JSON.stringify([...set])); } catch (_) {}
}
function expandOnlyRailGroup(name){
  const set = loadCollapsedRailGroups();
  document.querySelectorAll('.rail-group[data-group]').forEach(g => {
    const collapse = g.dataset.group !== name;
    g.classList.toggle('collapsed', collapse);
    const lbl = g.querySelector('.rail-group-label');
    if (lbl) lbl.setAttribute('aria-expanded', String(!collapse));
    if (collapse) set.add(g.dataset.group); else set.delete(g.dataset.group);
  });
  saveCollapsedRailGroups(set);
}

// setActiveRailTab highlights the tab for the given section and makes sure
// its group (if any) is expanded — used both for ordinary nav clicks and for
// anything that jumps straight to a section programmatically (e.g.
// double-clicking a peer's overlay address to hop to Networks).
function setActiveRailTab(sec){
  document.querySelectorAll('.rail-tab[data-sec]').forEach(b => b.classList.toggle('active', b.dataset.sec===sec));
  const g = groupFor(sec);
  if (g) expandOnlyRailGroup(g.name);
}

// buildRail renders the grouped, collapsible left nav — modeled on parapet's
// rail: a scrollable area of collapsible groups, with Settings and Sign out
// pinned in a foot area below, always visible regardless of scroll/collapse.
function buildRail(){
  const rail = $('<nav class="rail"></nav>');
  const scroll = $('<div class="rail-groups-scroll"></div>');
  const collapsed = loadCollapsedRailGroups();
  const curGroup = groupFor(state.section);
  for (const g of NAV_GROUPS) {
    const isCollapsed = curGroup ? g.name !== curGroup.name : collapsed.has(g.name);
    const grp = $('<div class="rail-group'+(isCollapsed?' collapsed':'')+'" data-group="'+g.name+'"></div>');
    const lbl = $('<button class="rail-group-label" title="collapse or expand this group" aria-expanded="'+String(!isCollapsed)+'"><span class="rail-chevron">\u25BE</span><span>'+esc(g.name)+'</span></button>');
    lbl.onclick = () => {
      const nowCollapsed = grp.classList.contains('collapsed');
      if (nowCollapsed) { expandOnlyRailGroup(g.name); return; }
      grp.classList.add('collapsed');
      lbl.setAttribute('aria-expanded','false');
      const set = loadCollapsedRailGroups(); set.add(g.name); saveCollapsedRailGroups(set);
    };
    const items = $('<div class="rail-group-items"></div>');
    for (const [s, tip] of g.items) {
      // Always create the button, even when this target can't currently serve
      // it — sectionVisible() only sets its *initial* display here; syncRailGating()
      // re-applies it on every subsequent load() so a section that's gated on a
      // per-target capability (BGP/vtysh) appears or disappears as the managed
      // target switches, instead of a stale button surviving from the previous
      // target. Skipping creation entirely (as before) meant a target that
      // *gains* the capability had no button to ever show.
      const b = $('<button class="rail-tab'+(s===state.section?' active':'')+'" title="'+esc(tip)+'" style="'+(sectionVisible(s)?'':'display:none')+'"></button>');
      b.textContent = label(s); b.dataset.sec = s;
      b.onclick = () => { state.section=s; setActiveRailTab(s); refresh(); };
      items.appendChild(b);
    }
    grp.appendChild(lbl); grp.appendChild(items);
    scroll.appendChild(grp);
  }
  rail.appendChild(scroll);

  const foot = $('<div class="rail-foot"></div>');
  foot.appendChild($('<div class="rail-divider"></div>'));
  const settingsLink = $('<button class="rail-tab'+(state.section==='settings'?' active':'')+'" title="console, security, and node-wide settings"></button>');
  settingsLink.textContent = 'Settings'; settingsLink.dataset.sec = 'settings';
  settingsLink.onclick = () => { state.section='settings'; setActiveRailTab('settings'); renderSection(); };
  foot.appendChild(settingsLink);
  const out = $('<button class="rail-tab rail-logout" title="end this session"></button>');
  out.textContent = 'Logout';
  out.onclick = async () => { await api('/api/logout',{method:'POST'}); showLogin(); };
  foot.appendChild(out);
  rail.appendChild(foot);
  return rail;
}

// load fetches this session's target (local, or a proxied peer if state.target
// is set) and refreshes state.status/state.nat/state.cfg from it. Proxied
// calls can legitimately take a couple of seconds (see sel.onchange's own
// comment on this), which is plenty of time for an older in-flight request —
// from the previous target, or an overlapping startPolling tick — to resolve
// *after* a newer one and silently overwrite it with the wrong node's data:
// the exact "the NAT banner / peers table doesn't seem to update when I
// switch targets" symptom this seq guard exists to close.
//
// seq captures state.targetSeq — it does NOT bump it (only setTarget does,
// exactly when state.target itself changes). sel.onchange fires this and
// refreshCluster() side by side, neither awaiting the other, both for the
// *same* switch: if this function bumped the counter itself, refreshCluster's
// own read of it moments later would already disagree, and vice versa,
// permanently discarding each other's results on every single switch rather
// than just the genuinely stale ones. Comparing against a shared value both
// only ever *read* (except at the one true source of a new generation,
// setTarget) is what lets them cooperate instead of racing each other.
async function load() {
  const seq = state.targetSeq;
  const r = await api('/api/status');
  if (seq !== state.targetSeq) return false; // a real switch happened while this was in flight; let that one win
  if (r.status === 401) {
    if (state.target) {
      // /api/status isn't in LOCAL_API, so with a peer selected this request
      // went through /api/proxy, and handleProxy relays the peer's response
      // body verbatim — so this 401 and its "error" text are the *peer's*
      // authed() rejecting the management hop (session expired there,
      // gossip about this node's Manager status hasn't reached it yet, or
      // it's no longer Managed), not anything wrong with this node's own
      // session. Showing the blank login form here re-authenticates the
      // wrong thing: logging back in only refreshes *this* session, which
      // was never the problem, so the exact same 401 would just recur on
      // the very next proxied call — the dead end refreshCluster's own
      // comment describes for the "peer stopped being manageable" case.
      // Here the peer briefly *looked* manageable (it's why it was
      // selectable at all) and only failed once actually addressed, so
      // that earlier fix — not listing peers that can't work — doesn't
      // catch this one. Fall back to local and surface the peer's actual
      // reason instead of a content-free prompt to re-enter a password
      // that was never the issue.
      const reason = (r.body && r.body.error) || 'the peer rejected the management connection';
      setTarget(null);
      document.body.classList.remove('remote');
      alert('Could not manage that node: ' + reason + '\n\nSwitched back to this node.');
      return await load(); // retry against local; a real expired session still reaches showLogin below
    }
    showLogin();
    return false;
  }
  state.status = r.body.nets || [];
  state.nat = { class: r.body.nat_class || '', public: r.body.public || '' };
  state.statusSig = JSON.stringify([state.status, state.nat]);
  const c = await api('/api/config');
  if (seq !== state.targetSeq) return false; // a real switch happened while /api/config was in flight
  // Show network cards alphabetically by name (id as fallback) across every
  // section. state.cfg has no status-signature dependency (unlike state.status),
  // so sorting the copy here is safe and keeps all config-driven sections
  // consistently ordered without each renderer re-sorting.
  state.cfg = ((c.body && c.body.nets) || []).slice().sort((a, b) =>
    netNameCmp(a.name || a.id, b.name || b.id));
  state.primaryPort = (c.body && c.body.primary_port) || 0;
  state.tcpPort = (c.body && c.body.tcp_fallback_port) || 0;
  state.tcpFallbackDisabled = !!(c.body && c.body.tcp_fallback_disabled);
  state.extraUDPPorts = (c.body && c.body.extra_listen_ports) || [];
  state.extraTCPPorts = (c.body && c.body.extra_tcp_listen_ports) || [];
  state.natStateTimeout = (c.body && c.body.nat_state_timeout) || 0;
  state.geoipLookup = !!(c.body && c.body.geoip_lookup);
  state.enableUpnp = !!(c.body && c.body.enable_upnp);
  state.allowRemoteShell = !!(c.body && c.body.allow_remote_shell);
  state.shellSupported = c.body ? !!c.body.shell_supported : true;
  // Whether this host can serve dynamic-routing (BGP) status — true only when
  // FRR's vtysh is installed here (see the server's bgpSupported()). Gates the
  // Traffic > BGP nav item and section entirely: absent vtysh, there's nothing
  // to show, so the entry is hidden rather than dead. Read fresh on every load,
  // so switching to manage a remote peer reflects that peer's capability, not
  // this node's.
  state.bgpSupported = !!(c.body && c.body.bgp_supported);
  state.logLevel = (c.body && c.body.log_level) || 'info';
  state.logMaxSize = (c.body && c.body.log_max_size) || '200M';
  // Node-global firewall object/service catalog — shared by every network
  // above (see the server's Config.FirewallObjects doc comment), so it lives
  // at this top level rather than nested under any one entry in state.cfg.
  state.fwObjects = (c.body && c.body.firewall_objects) || [];
  state.fwServices = (c.body && c.body.firewall_services) || [];
  state.fwObjectsSeeded = !!(c.body && c.body.firewall_objects_seeded);
  state.fwServicesSeeded = !!(c.body && c.body.firewall_services_seeded);
  return true;
}

// ---- global search ----
// Walks state.cfg — the full per-network config already held client-side —
// into a flat, searchable index covering every config entity with
// meaningful text: networks, keys, seeds, routes (+ reject), hosts (+
// reject), dns forwards (+ reject), firewall/NAT/QoS rules, and
// locally-disabled peers. Deliberately doesn't index live-only data (peer
// connection state, metrics, logs) — that's not stored text to jump to, and
// already has its own Monitor pages. Rebuilt fresh on every keystroke
// (state.cfg is already local; this is well under a millisecond even for a
// few hundred entries) rather than kept incrementally in sync, so there's
// no separate cache to invalidate as config changes.
function buildSearchIndex(){
  const idx = [];
  // label: the result's display text. sub: context shown under it. hay:
  // everything this entry should match against (lowercased once here, not
  // per-keystroke). match: enough to find the specific row later — see
  // searchRowSelector.
  const add = (label, sub, section, netId, match, extraHay) => {
    if (!label) return;
    idx.push({ label: String(label), sub, section, netId,
      hay: (label + ' ' + (extraHay||'')).toLowerCase(), match });
  };

  // Section and group names are text in the app too — the rail nav itself —
  // and are exactly what someone types when they just want to get
  // somewhere (typing "qos" to jump to the QoS section, not to find a rule
  // that happens to contain the word). Indexed once here, not per network.
  for (const g of NAV_GROUPS) {
    add(g.name, 'Section group', null, null, {kind:'group', firstSection:g.items[0][0]});
    for (const [s, tip] of g.items) { if (!sectionVisible(s)) continue; add(label(s), 'Section', s, null, {kind:'section', section:s}, tip); }
  }
  add('Settings', 'Section', 'settings', null, {kind:'section', section:'settings'}, 'console, security, and node-wide settings');

  // Settings' own content (Appearance/Cluster/Routing/Underlay/NAT/Privacy)
  // is node-global, same shape as the exempt list above — no per-network
  // scoping, so navigation is a plain getElementById rather than the
  // card-scoped searchRowSelector pattern every per-network section uses.
  // Managed/Manager's descriptions are dynamic (syncClusterModeRows swaps
  // them based on whether a remote peer is selected); indexed here using
  // their local-node text, the one most people would actually search for.
  const settingsRows = [
    ['dark-mode-row', 'Dark mode', 'Switch between dark and light interface theme.'],
    ['cluster-managed-row', 'Managed mode', 'Let Manager-mode peers in the cluster remotely configure this node.'],
    ['cluster-manager-row', 'Manager mode', 'Let this node browse and remotely configure other Managed-mode peers in the cluster.'],
    ['shell-allow-row', 'Remote shell', 'Let a Manager peer open a real OS shell on this node through the web admin.'],
    ['loglevel-row', 'Log level', 'How much this node logs (error, warn, info, debug). Applied immediately, no restart.'],
    ['logsize-row', 'Log size', 'Maximum size of the log file; once full the oldest lines are dropped (FIFO). e.g. 200M, 1G, 99K.'],
    ['routeadv-row', 'Route advertisement interval', 'How often this node re-advertises the routes it originates.'],
    ['udpport-row', 'UDP port', 'The UDP port(s) this node listens on; comma-separated for more than one, so a peer behind a restrictive firewall can reach it on a well-known port too.'],
    ['tcpport-row', 'TCP port', 'The TCP port(s) this node listens on for the TLS fallback; comma-separated for more than one.'],
    ['natstate-row', 'NAT state timeout', 'How long an idle translated NAT connection is remembered before its mapping is reclaimed.'],
    ['upnp-row', 'UPnP', 'Ask the LAN router to forward every port this node listens on \u2014 UDP, TCP fallback, and any extra ports \u2014 from its WAN side to this host automatically, so peers can reach it without a manual port forward. Off by default. upnp port forwarding nat traversal'],
    ['geoip-row', 'Geo-IP lookups', 'Show an approximate location on a peer or seed\u2019s info panel, looked up from a third-party service (ipapi.co). geoip'],
  ];
  for (const [id, lbl, desc] of settingsRows) {
    add(lbl, 'Settings', 'settings', null, {kind:'setting', id}, desc);
  }

  // Firewall grew sub-tabs in v330 (Rules / Allow List); a section hit alone
  // no longer says which one to land on, so section/group entries generally
  // leave state.firewallTab as whatever it already was — fine for every
  // section but Firewall, which is why Rules and Allow List each also get
  // their own direct entry below, explicit about which tab a hit should set.
  add('Rules', 'Section \u00b7 Firewall', 'firewall', null, {kind:'section', section:'firewall', tab:'rules'});
  add('Allow List', 'Section \u00b7 Firewall', 'firewall', null, {kind:'section', section:'firewall', tab:'allowlist'}, 'allowlist allow-list');

  // The global firewall allow list (Firewall → Allow List tab, moved out of
  // Settings in v330) is the one entity in this whole index that doesn't
  // live in state.cfg or state.status — it's fetched separately, on demand,
  // only when that tab (or, before v330, Settings) has actually been opened
  // (exemptReload). So it's only searchable once that's happened at least
  // once this session; state.exempt is empty (this loop a no-op) until
  // then. Lands on the Allow List tab like the explicit entry above rather
  // than pinpointing the row: exemptReload's own async fetch would
  // otherwise race a freshly-rendered tab's table not existing yet, for a
  // list that's normally three or four entries anyway.
  for (const e of (state.exempt||[])) {
    const label = (e.name||'')+' '+(e.proto||'any')+(e.port?' '+e.port:'');
    add(label, 'Firewall allow list', 'firewall', null, {kind:'section', section:'firewall', tab:'allowlist'});
  }

  for (const cf of (state.cfg||[])) {
    add(cf.name, 'Network', 'networks', cf.id, {kind:'net'}, cf.id+' '+(cf.subnet4||'')+' '+(cf.subnet6||'')+' '+(cf.notes||''));
    for (const k of (cf.keys||[])) {
      if (k.label || k.notes) add(k.label || ('slot '+k.slot), 'Key \u00b7 '+cf.name, 'keys', cf.id, {kind:'key', slot:k.slot}, k.notes||'');
    }
    for (const s of (cf.seeds||[])) {
      const addr = s.address||s.Address||'', notes = s.notes||s.Notes||'';
      if (addr) add(stripScheme(addr), 'Seed \u00b7 '+cf.name, 'seeds', cf.id, {kind:'seed', addr}, addr+' '+notes);
    }
    for (const r of (cf.routes||[])) add(r.cidr, 'Route \u00b7 '+cf.name, 'routes', cf.id, {kind:'route', cidr:r.cidr});
    for (const x of (cf.route_reject||[])) {
      const xc = (typeof x==='string') ? x : ((x&&x.cidr)||'');
      add(xc, 'Rejected route \u00b7 '+cf.name, 'routes', cf.id, {kind:'route-reject', cidr:xc});
    }
    for (const r of (cf.hosts_advertise||[])) add(r.name, 'Host \u00b7 '+cf.name+(r.ip?' \u00b7 '+r.ip:''), 'hosts', cf.id, {kind:'host', name:r.name}, r.ip||'');
    for (const r of (cf.hosts_reject||[])) add(r.name, 'Rejected host \u00b7 '+cf.name, 'hosts', cf.id, {kind:'host-reject', name:r.name});
    for (const f of (cf.dns_advertise||[])) add(f.domain, 'DNS forward \u00b7 '+cf.name, 'dns', cf.id, {kind:'dns', domain:f.domain}, (f.servers||[]).join(' '));
    for (const f of (cf.dns_reject||[])) add(f.domain, 'Rejected domain \u00b7 '+cf.name, 'dns', cf.id, {kind:'dns-reject', domain:f.domain});
    for (const f of ((cf.firewall||{}).rules||[])) {
      const label = (f.action||'rule')+' '+(fwSvcLabel(f)||'any')+(f.src?' from '+f.src:'')+(f.dst?' to '+f.dst:'');
      add(label, 'Firewall rule \u00b7 '+cf.name, 'firewall', cf.id, {kind:'fw', id:f.id, tab:'rules'}, f.notes||'');
    }
    ((cf.nat||{}).rules||[]).forEach((r,i) => {
      const tgt = r.interface ? (r.translate||'masquerade')+' ('+r.interface+')' : (r.translate||'masquerade');
      const label = (r.source||'any')+' \u2192 '+(r.dest||'any')+' \u21d2 '+tgt;
      add(label, 'NAT rule \u00b7 '+cf.name, 'nat', cf.id, {kind:'nat', idx:i});
    });
    ((cf.qos||{}).rules||[]).forEach(r => {
      add(qosSvcLabel(r)||'any', 'QoS rule \u00b7 '+cf.name, 'qos', cf.id, {kind:'qos', proto:r.protocol||'', port:r.port_min||0, services:(r.services||[]).join(',')}, (r.services||[]).join(' '));
    });
  }

  // Peers and bans live in state.status (the live per-network view), not
  // state.cfg — secPeers/secBans/peerRowsForNet all already read from here,
  // never from cf. Reusing peerRowsForNet directly (rather than re-deriving
  // NodeID/Hostname/Notes field access here a second time) is also what
  // avoids a shape mismatch: disabled peers are objects ({NodeID, Hostname,
  // Notes, ...}), not bare id strings, and a first pass at this indexed
  // them by looping cf.disabled_peers directly and passing the whole
  // object as the label — String(object) rather than the id or hostname.
  for (const n of (state.status||[])) {
    const netName = nameOf(n.id);
    for (const p of peerRowsForNet(n)) {
      if (p.self) continue; // this node itself isn't a useful search target
      add(p.host || p.id, (p.disabled?'Disabled peer':'Peer')+' \u00b7 '+netName, 'peers', n.id, {kind:'peer', nodeId:p.id}, p.id+' '+(p.notes||'')+' '+(p.overlay||'')+' '+(p.endpoint||''));
    }
    for (const b of (n.bans||[])) {
      const tgt = b.Target||b.target||'', tgtHost = b.Hostname||b.hostname||'', notes = b.Notes||b.notes||'';
      if (!tgt && !tgtHost) continue;
      add(tgtHost || tgt, 'Ban \u00b7 '+netName, 'bans', n.id, {kind:'ban', nodeId:tgt}, tgt+' '+notes);
    }
  }
  return idx;
}

// searchIndexQuery filters the index for query, capped to a scannable
// dropdown size. Empty/whitespace-only queries return nothing rather than
// the whole index.
function searchIndexQuery(query){
  const q = (query||'').trim().toLowerCase();
  if (!q) return [];
  const out = [];
  for (const e of buildSearchIndex()) {
    if (e.hay.includes(q)) { out.push(e); if (out.length >= 25) break; }
  }
  return out;
}

// searchRowSelector maps a search-index match descriptor to a CSS selector
// for the specific row it points at, scoped within the right network's
// card by navigateToSearchResult. A kind with no case here (networks,
// disabled peers) just scrolls to the card itself — still gets the operator
// to the right place, one level less precise.
function searchRowSelector(m){
  switch (m.kind) {
    case 'route': case 'route-reject': return 'tr[data-cidr="'+CSS.escape(m.cidr)+'"]';
    case 'host': return 'tr[data-name="'+CSS.escape(m.name)+'"]';
    case 'host-reject': return 'tr[data-rejname="'+CSS.escape(m.name)+'"]';
    case 'dns': return 'tr[data-domain="'+CSS.escape(m.domain)+'"]';
    case 'dns-reject': return 'tr[data-rejdomain="'+CSS.escape(m.domain)+'"]';
    case 'fw': return 'tr[data-fwid="'+CSS.escape(String(m.id))+'"]';
    case 'nat': return 'tr.natrow[data-idx="'+CSS.escape(String(m.idx))+'"]';
    case 'qos': return 'tr.qrow[data-proto="'+CSS.escape(m.proto)+'"][data-port="'+CSS.escape(String(m.port))+'"][data-services="'+CSS.escape(m.services||'')+'"]';
    case 'key': return 'td.klabel[data-slot="'+CSS.escape(String(m.slot))+'"]';
    case 'seed': return 'td.seed-addr[data-addr="'+CSS.escape(m.addr)+'"]';
    case 'peer': return '[data-peer="'+CSS.escape(m.nodeId)+'"]';
    case 'ban': return 'tr[data-target="'+CSS.escape(m.nodeId)+'"]';
    default: return null;
  }
}

// navigateToSearchResult jumps to the section/network a search result came
// from via the same rail navigation an ordinary tab click uses
// (setActiveRailTab + refresh), so the active tab and expanded rail group
// stay consistent with a manual click getting you there — then scrolls to
// and briefly highlights the specific row if one can be pinned down.
async function navigateToSearchResult(r){
  const targetSection = (r.match && r.match.kind === 'group') ? r.match.firstSection : r.section;
  state.section = targetSection;
  setActiveRailTab(targetSection);
  // Only Firewall has sub-tabs as of v330; a match that came from a
  // specific tab (see buildSearchIndex's 'fw'/exempt-list entries) sets it
  // before rendering, same as ordinary section/netId targeting above —
  // generalizable to state.<section>Tab if another section grows tabs later.
  if (r.match && r.match.tab) state.firewallTab = r.match.tab;
  // renderSection() alone (no reload) is what the existing Settings rail
  // link itself does — state.cfg/state.status aren't what that section
  // reads, so there's nothing for refresh()'s round trip to buy here.
  if (targetSection === 'settings') { renderSection(); } else { await refresh(); }

  // A pure navigation hit (a section or group name matched, not any
  // specific config entity) has no row to find — landing on the section
  // itself, already scrolled to top, is the whole result.
  if (!r.match || r.match.kind === 'section' || r.match.kind === 'group') return;

  // Settings rows are node-global with stable ids, not per-network cards —
  // a plain getElementById, no card-scoping needed.
  if (r.match.kind === 'setting') {
    flashAndScroll(document.getElementById(r.match.id));
    return;
  }

  // Networks is one shared table across every network (see secNetworks),
  // not one card per network like every other section here — its row is
  // findable directly by data-netid, without first locating "the" card the
  // per-network-card sections below need to disambiguate duplicate values.
  if (r.match.kind === 'net') {
    flashAndScroll(document.querySelector('#content tr.netrow[data-netid="'+CSS.escape(r.netId)+'"]')
      || document.querySelector('#content .card'));
    return;
  }

  let card = null;
  for (const c of document.querySelectorAll('#content .card')) {
    const idEl = c.querySelector('.net-id');
    if (idEl && idEl.textContent === r.netId) { card = c; break; }
  }
  let target = card;
  if (card) {
    const sel = searchRowSelector(r.match);
    if (sel) { const row = card.querySelector(sel); if (row) target = row; }
  }
  flashAndScroll(target);
}

// flashAndScroll scrolls target into view and gives it a brief highlight
// flash (the search-hit CSS animation), if target was found at all.
function flashAndScroll(target){
  if (!target) return;
  target.scrollIntoView({ behavior:'smooth', block:'center' });
  target.classList.add('search-hit');
  setTimeout(() => target.classList.remove('search-hit'), 2000);
}

// gotoMeshPeer switches to Monitor > mesh peers, ticks one peer's row (and
// clears every other tick), and flashes it, reusing the same card-scoped
// flashAndScroll the global search uses. Called from the latency table's peer
// names; those rows are keyed by network *name* (the /api/latency response
// carries no network id), so the caller resolves the id first and passes it
// here. The selection is set *before* refresh() so the render's own
// wireSelectable restores the tick from selection.mpeers rather than us
// poking checkboxes after the fact — same single source of truth every other
// selection path uses. Sole-selecting means a shell/info opened right after
// landing acts on this peer and nothing stale. Landing is no-op-safe: if the
// row can't be found (say the peer dropped between the click and the
// re-render), it still lands on the right network card rather than nowhere;
// the tick is keyed by id, so it's harmless if that row never renders.
async function gotoMeshPeer(netId, nodeId){
  if (!netId) return;
  state.section = 'mesh-peers';
  setActiveRailTab('mesh-peers');
  selection.mpeers.clear();
  selection.mpeers.add(selKey(netId, nodeId));
  await refresh();
  let card = null;
  for (const c of document.querySelectorAll('#content .card')){
    const idEl = c.querySelector('.net-id');
    if (idEl && idEl.textContent === netId){ card = c; break; }
  }
  const target = card ? (card.querySelector('tr[data-peer="'+CSS.escape(nodeId)+'"]') || card) : null;
  flashAndScroll(target);
}

// gotoNetwork switches to Mesh > networks, ticks one network's row (and clears
// every other tick), and flashes it. Unlike mesh peers, the networks table
// carries no persistent selection Set — its ticks live only in the rendered
// selbox checkboxes (see selAllWire / selCheckedRows) — so this ticks the
// DOM *after* the refresh rather than seeding a set before it. Sole-ticking
// means a delete/reset/token action taken right after landing acts on exactly
// this network. Called from the log linkifier's network id/name links.
// No-op-safe: if the row can't be found (network removed between click and
// re-render), it lands on the networks card rather than nowhere.
async function gotoNetwork(netId){
  if (!netId) return;
  state.section = 'networks';
  setActiveRailTab('networks');
  await refresh();
  const row = document.querySelector('#content tr.netrow[data-netid="'+CSS.escape(netId)+'"]');
  document.querySelectorAll('#content tr.netrow .selbox').forEach(cb => { cb.checked = false; });
  if (row){ const cb = row.querySelector('.selbox'); if (cb) cb.checked = true; }
  flashAndScroll(row || document.querySelector('#content .card'));
}

// gotoBgpNeighbor switches to Traffic > BGP and flashes the neighbor row
// matching peerAddr, so clicking a live peer under Monitor > BGP Peers lands
// on that peer's actual definition instead of just the bare section. Unlike
// gotoMeshPeer/gotoNetwork, the target can't be applied right after refresh()
// resolves: the BGP editor's own data comes from secBgp's local async load()
// (fetching /api/bgp/config, and possibly a background /api/bgp/import),
// which renderSection() kicks off but refresh() does not wait on. So the
// target peer is left in state.pendingBgpHighlight instead, and
// renderBgpEditor consumes it itself, right after it attaches its finished
// card — whenever that turns out to be, however many renders that takes.
// No-op-safe: a peer configured outside gravinet, with no matching row in the
// stored config, still lands on the BGP card itself rather than nowhere (see
// applyPendingBgpHighlight).
async function gotoBgpNeighbor(peerAddr){
  if (!peerAddr) return;
  state.pendingBgpHighlight = peerAddr;
  state.section = 'bgp';
  setActiveRailTab('bgp');
  await refresh();
}

// applyPendingBgpHighlight consumes state.pendingBgpHighlight (if any)
// against a just-attached BGP editor card: flashes the matching neighbor row
// by its data-peer attribute (set in renderNbrs), or the card itself if no
// row matches. Cleared unconditionally on the first render it's checked
// against, same as every other goto*-style jump here — one attempt, landing
// on the section is the fallback rather than chasing a row across multiple
// re-renders.
function applyPendingBgpHighlight(card){
  if (!state.pendingBgpHighlight) return;
  const peer = state.pendingBgpHighlight;
  state.pendingBgpHighlight = null;
  flashAndScroll(card.querySelector('tr[data-peer="'+CSS.escape(peer)+'"]') || card);
}


// existing .search-select/.ss-* component styling already in the
// stylesheet (unused until now) rather than introducing a second dropdown
// pattern. Arrow keys move the selection, Enter picks it, Escape clears —
// the interaction the .ss-opt.sel styling was already built for.
function buildGlobalSearch(){
  const wrap = $('<div class="search-select global-search"></div>');
  const inp = $('<input class="ss-input" type="text" autocomplete="off" spellcheck="false" placeholder="Search\u2026" title="search networks, routes, hosts, DNS, firewall/NAT/QoS rules, keys, and seeds">');
  const list = $('<div class="ss-list"></div>');
  wrap.appendChild(inp);
  wrap.appendChild(list);

  let results = [];
  let selIdx = -1;

  const markSel = () => list.querySelectorAll('.ss-opt').forEach((o,i) => o.classList.toggle('sel', i===selIdx));
  const scrollSelIntoView = () => { const el = list.querySelector('.ss-opt.sel'); if (el) el.scrollIntoView({block:'nearest'}); };
  const renderResults = () => {
    list.innerHTML = '';
    if (!results.length) {
      list.appendChild($('<div class="ss-empty">'+(inp.value.trim() ? 'no matches' : 'type to search')+'</div>'));
      return;
    }
    results.forEach((r, i) => {
      const opt = $('<div class="ss-opt'+(i===selIdx?' sel':'')+'"></div>');
      opt.innerHTML = '<div>'+esc(r.label)+'</div><div style="font-size:11px;color:var(--mut)">'+esc(r.sub)+'</div>';
      opt.onmouseenter = () => { selIdx = i; markSel(); };
      // mousedown, not click: fires before the input's blur (which hides
      // the list) would otherwise swallow the click.
      opt.onmousedown = (e) => { e.preventDefault(); pick(r); };
      list.appendChild(opt);
    });
  };
  const open = () => list.classList.add('show');
  const close = () => { list.classList.remove('show'); selIdx = -1; };
  const runQuery = () => {
    results = searchIndexQuery(inp.value);
    selIdx = results.length ? 0 : -1;
    renderResults();
  };
  const pick = (r) => {
    inp.value = '';
    results = [];
    close();
    navigateToSearchResult(r);
  };

  inp.oninput = () => { runQuery(); if (inp.value.trim()) open(); else close(); };
  inp.onfocus = () => { if (inp.value.trim()) { runQuery(); open(); } };
  inp.onblur = () => setTimeout(close, 150); // outlast onmousedown's pick() above
  inp.onkeydown = (e) => {
    if (!list.classList.contains('show')) {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { if (inp.value.trim()) { runQuery(); open(); } }
      return;
    }
    if (e.key === 'ArrowDown') { e.preventDefault(); if (results.length) { selIdx = (selIdx+1) % results.length; markSel(); scrollSelIntoView(); } }
    else if (e.key === 'ArrowUp') { e.preventDefault(); if (results.length) { selIdx = (selIdx-1+results.length) % results.length; markSel(); scrollSelIntoView(); } }
    else if (e.key === 'Enter') { e.preventDefault(); if (selIdx >= 0 && results[selIdx]) pick(results[selIdx]); }
    else if (e.key === 'Escape') { inp.value=''; results=[]; close(); inp.blur(); }
  };

  return wrap;
}

// peerPicker is the header's node picker (set by dashboard). refreshCluster
// feeds it the current peer list; nothing else touches it.
let peerPicker = null;

// setPeerTarget switches which node the GUI configures. Called only when the
// selection actually changes — picking the already-selected node is a no-op, the
// same way re-picking the current option in a <select> never fired onchange.
function setPeerTarget(value){
  const next = value || null;
  if ((state.target || null) === next) return;
  setTarget(next);
  document.body.classList.toggle('remote', !!state.target);
  // Gray out (and disable) the Managed/Manager toggles for the *new* target
  // immediately, synchronously, before anything async runs. Both are
  // local-only-editable — see syncClusterModeRows and LOCAL_API's comment —
  // POSTing /api/managed or /api/manager always changes *this* node's own mode,
  // never the selected peer's, no matter what's picked in the dropdown.
  // refresh() and refreshCluster() below both involve a real round trip
  // (refresh()'s /api/status and /api/config calls are now proxied to the new
  // target too, so possibly a couple of seconds on a slow or high-latency peer),
  // and until they resolve and rebuild the settings row, the *previous* render's
  // toggle — still on screen, still bound to its old onchange handler — would
  // otherwise stay clickable. Flipping it in that window silently changes this
  // node's own mode while the screen already looks like it's showing the newly
  // selected peer. Calling this here, before any of that, closes the window
  // outright rather than just narrowing it — syncClusterModeRows is cheap and
  // side-effect-free (it only ever reads state and toggles DOM attributes) so
  // there's no reason not to call it eagerly on every switch.
  syncClusterModeRows();
  refresh();
  refreshCluster(); // pull this peer's actual values now rather than waiting up to 6s for the next tick
}

// buildListPicker is the dropdown used everywhere a node gets picked: a button
// showing the current choice, and a list whose FIRST ROW is the filter box, with
// the options underneath it. Open the list, type, the options below narrow.
//
// It exists because a native <select> cannot do that. A <select>'s option list is
// an OS-drawn popup that no markup can be placed inside, so a filter for it can
// only ever sit *beside* it — which is what every node picker here used to do:
// two controls that read as unrelated, the filter taking up room whether or not
// it was being used. Hand-rolling the listbox is the only way the filter can live
// at the top of the list it filters. Styling reuses the .ss-* component the
// global search box already established, rather than a third dropdown look.
//
// cfg: { title, placeholder, filterPlaceholder, id, filterId, compact,
//        alignRight, onPick(value) }
//
// API: setItems(items), getValue(), setValue(v), count(). An item is
// { value, label, disabled } — disabled options render grayed and can't be
// picked or landed on with the keyboard, which is what lets the speedtest's two
// pickers exclude each other's current choice.
//
// Filtering only ever narrows what's *visible/pickable*; it never changes the
// selection. Typing a filter that hides the currently selected node does not
// silently reselect anything — the button keeps naming the real choice
// throughout, and the option reappears as soon as the filter stops excluding it.
// The filter clears when the list closes, so the next open always starts from the
// full list rather than a stale query the user has by then forgotten they typed.
function buildListPicker(cfg){
  cfg = cfg || {};
  const wrap = $('<div class="search-select list-picker"></div>');
  // The caret is written as an HTML entity on purpose. indexHTML is a Go raw
  // string literal, which does no escape processing, so a backslash-u unicode
  // escape written here would reach the browser as those literal characters and
  // render as visible text in the button rather than a caret glyph.
  const btn = $('<button type="button" class="peer-sel'+(cfg.compact ? ' peer-sel-sm' : '')+'" aria-haspopup="listbox" aria-expanded="false"><span class="peer-sel-label"></span><span class="peer-caret">&#9662;</span></button>');
  if (cfg.id) btn.id = cfg.id;
  if (cfg.title) btn.title = cfg.title;
  const label = btn.querySelector('.peer-sel-label');
  const list = $('<div class="ss-list'+(cfg.alignRight ? ' ss-right' : '')+'" role="listbox"></div>');
  // The filter row is part of the list, pinned to its top — not a sibling of the
  // button. Hidden below DROPDOWN_FILTER_MIN options, where a filter is pure
  // clutter and the list is already scannable at a glance.
  const filterRow = $('<div class="ss-filter-row"></div>');
  const filterInp = $('<input class="ss-input ss-filter" type="text" spellcheck="false" autocomplete="off" placeholder="'+esc(cfg.filterPlaceholder || 'filter nodes…')+'" title="filter this list by name or id">');
  if (cfg.filterId) filterInp.id = cfg.filterId;
  filterRow.appendChild(filterInp);
  const optBox = $('<div class="ss-opts"></div>');
  list.appendChild(filterRow);
  list.appendChild(optBox);
  wrap.appendChild(btn);
  wrap.appendChild(list);

  const placeholder = cfg.placeholder || '(none)';
  let items = [];      // every option
  let shown = [];      // items surviving the current filter — what's on screen
  let value = '';      // the current selection; independent of the filter
  let filterText = '';
  let selIdx = -1;     // keyboard cursor into shown
  let isOpen = false;

  const itemFor = (v) => items.find(it => it.value === v);
  const syncLabel = () => { const it = itemFor(value); label.textContent = it ? it.label : placeholder; };
  const markSel = () => optBox.querySelectorAll('.ss-opt').forEach((o, i) => o.classList.toggle('sel', i === selIdx));

  // Match on the displayed name or the raw node id, so a filter works whether or
  // not a peer has announced a hostname yet.
  const renderOpts = () => {
    const q = filterText.trim().toLowerCase();
    shown = items.filter(it => !q || it.label.toLowerCase().indexOf(q) >= 0 || it.value.toLowerCase().indexOf(q) >= 0);
    optBox.innerHTML = '';
    if (!shown.length){
      optBox.appendChild($('<div class="ss-empty">'+(items.length ? 'no matches' : 'no nodes')+'</div>'));
      selIdx = -1;
      return;
    }
    shown.forEach((it, i) => {
      const o = $('<div class="ss-opt'+(it.disabled ? ' dis' : '')+'" role="option"></div>');
      o.textContent = it.label;
      if (it.disabled) o.setAttribute('aria-disabled', 'true');
      if (it.value === value){ o.classList.add('cur'); o.setAttribute('aria-selected', 'true'); }
      if (!it.disabled){
        o.onmouseenter = () => { selIdx = i; markSel(); };
        // mousedown, not click: fires before the blur that closes the list would
        // otherwise swallow it (same reason as the global search box).
        o.onmousedown = (e) => { e.preventDefault(); pick(it); };
      }
      optBox.appendChild(o);
    });
    if (selIdx >= shown.length) selIdx = shown.length - 1;
    markSel();
  };

  const scrollSelIntoView = () => { const s = optBox.querySelector('.ss-opt.sel'); if (s) s.scrollIntoView({ block:'nearest' }); };

  const openList = () => {
    isOpen = true;
    list.classList.add('show');
    btn.setAttribute('aria-expanded', 'true');
    filterRow.style.display = items.length >= DROPDOWN_FILTER_MIN ? '' : 'none';
    renderOpts();
    selIdx = shown.findIndex(it => it.value === value && !it.disabled); // start the cursor on the current choice
    markSel();
    scrollSelIntoView();
    if (filterRow.style.display !== 'none') filterInp.focus(); else btn.focus();
  };

  const closeList = (refocus) => {
    isOpen = false;
    list.classList.remove('show');
    btn.setAttribute('aria-expanded', 'false');
    selIdx = -1;
    if (filterText){ filterText = ''; filterInp.value = ''; renderOpts(); }
    if (refocus) btn.focus();
  };

  const pick = (it) => {
    if (it.disabled) return;
    closeList(true);
    // Set the label from the picked option directly rather than waiting for a
    // caller's refresh to come back and call setItems: that can be a network hop
    // away (seconds, on a slow peer), and until it lands the button would still
    // be naming the node just switched *away* from.
    value = it.value;
    syncLabel();
    if (cfg.onPick) cfg.onPick(value);
  };

  btn.onclick = () => { if (isOpen) closeList(true); else openList(); };

  // Keyboard: one handler for both the button and the filter input, so arrows
  // work whether or not the filter row is showing. Steps over disabled options
  // rather than letting the cursor land on something that can't be picked.
  const step = (dir) => {
    if (!shown.length) return;
    for (let n = 0; n < shown.length; n++){
      selIdx = (selIdx + dir + shown.length) % shown.length;
      if (!shown[selIdx].disabled) break;
    }
    if (shown[selIdx] && shown[selIdx].disabled) selIdx = -1; // every option disabled
    markSel();
    scrollSelIntoView();
  };
  const onKey = (e) => {
    if (!isOpen){
      if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' '){ e.preventDefault(); openList(); }
      return;
    }
    if (e.key === 'ArrowDown'){ e.preventDefault(); step(1); }
    else if (e.key === 'ArrowUp'){ e.preventDefault(); step(-1); }
    else if (e.key === 'Enter'){ e.preventDefault(); if (selIdx >= 0 && shown[selIdx]) pick(shown[selIdx]); }
    else if (e.key === 'Escape'){ e.preventDefault(); closeList(true); }
  };
  btn.onkeydown = onKey;
  filterInp.onkeydown = onKey;
  filterInp.oninput = () => { filterText = filterInp.value; renderOpts(); };

  const onDocDown = (e) => {
    if (!document.body.contains(wrap)){ document.removeEventListener('mousedown', onDocDown); return; } // picker replaced by a re-render
    if (isOpen && !wrap.contains(e.target)) closeList(false);
  };
  document.addEventListener('mousedown', onDocDown);

  // setItems rebuilds the options and resyncs the button. Rebuilding while the
  // list is open is fine: a background poll re-rendering the options must not
  // throw away what the user is mid-way through typing, so the in-progress filter
  // is re-applied to the fresh list rather than reset. A selection that's no
  // longer in the list falls back to the placeholder.
  wrap.setItems = (next) => {
    items = (next || []).slice();
    if (value && !itemFor(value)) value = '';
    syncLabel();
    if (isOpen){
      filterRow.style.display = items.length >= DROPDOWN_FILTER_MIN ? '' : 'none';
      renderOpts();
    }
  };
  wrap.getValue = () => value;
  wrap.setValue = (v) => { value = v || ''; syncLabel(); if (isOpen) renderOpts(); };
  wrap.count = () => items.length;

  return wrap;
}

// buildPeerPicker is the header's node picker: which node the GUI configures.
// Thin wrapper over buildListPicker — see there for the filter-at-the-top-of-the
// -list behavior. setOptions is refreshCluster's entry point.
function buildPeerPicker(){
  const picker = buildListPicker({
    id: 'peerSel',
    filterId: 'peerFilter',
    title: 'choose which node this GUI configures',
    placeholder: 'This node',
    filterPlaceholder: 'filter nodes…',
    alignRight: true,
    onPick: (v) => setPeerTarget(v),
  });
  picker.setOptions = (sorted, selfName) => {
    picker.setItems([{ value:'', label:'This node (' + selfName + ')' }].concat(
      sorted.map(p => ({ value: p.node_id, label: p.hostname || p.node_id.slice(0, 8) }))
    ));
    // The picker's selection mirrors state.target, which refreshCluster may have
    // just reset to local (peer gone, or Manager mode turned off).
    picker.setValue(state.target || '');
  };
  return picker;
}

async function dashboard() {
  if (!(await load())) return;
  app.innerHTML = '';
  // top bar: brand + peer selector (managed-mode toggle and dark mode moved to Settings)
  const top = $('<div class="top"><div class="brand">[gravinet]</div></div>');

  // Cluster peer-selector: pick a remote node to configure. Stays in the top bar
  // so switching nodes is always one click away regardless of active section.
  const cluster = $('<div class="cluster"></div>');
  peerPicker = buildPeerPicker();
  cluster.appendChild(buildGlobalSearch());
  cluster.appendChild(peerPicker);
  top.appendChild(cluster);
  app.appendChild(top);

  const layout = $('<div class="layout"></div>');
  const rail = buildRail();
  layout.appendChild(rail);

  const content = $('<div class="content" id="content"></div>');
  layout.appendChild(content);
  app.appendChild(layout);
  renderSection();
  startPolling();
  refreshCluster();
  setInterval(refreshCluster, 6000);

  function renderNav(){
    setActiveRailTab(state.section);
  }
}

// sortPeersByName sorts an array of peer-like objects ({hostname, node_id})
// alphabetically by hostname (case-insensitive), falling back to a short id
// prefix for a peer with no hostname yet — the same display-name rule used
// everywhere else peers are listed, and the same order the server itself now
// returns them in (see Engine.ListPeers/ManagedPeers), kept here too since
// this sorts an already-filtered/mapped copy, not the raw API response.
function sortPeersByName(list){
  return list.slice().sort((a, b) => {
    const na = (a.hostname || a.node_id.slice(0,8)).toLowerCase();
    const nb = (b.hostname || b.node_id.slice(0,8)).toLowerCase();
    return na.localeCompare(nb);
  });
}

// computeSortedManageablePeers returns state.cluster's manageable peers,
// sorted alphabetically by hostname (falling back to a short id prefix, the
// same label shown in the option itself). Split out from refreshCluster so
// the header filter box's oninput can re-sort/re-render from the
// already-fetched list on every keystroke without a network round trip.
function computeSortedManageablePeers(){
  if (!state.manager) return []; // see refreshCluster's doc comment: no options when unusable
  return sortPeersByName(state.cluster.filter(p => p.manageable));
}

// refreshCluster pulls the managed-peer list + this node's managed/manager
// state and rebuilds the header dropdown, preserving the current selection. A
// peer that has aged past the server's TTL simply stops appearing.
//
// The dropdown only ever lists other peers when this node is itself in
// Manager mode. Without Manager, selecting a peer here used to still "work" —
// the option was there, clicking it flipped the GUI into remote mode — but
// every subsequent /api/proxy call to that peer was rejected 401 by the
// remote (its authed() correctly requires the caller to resolve to a Manager,
// which this node isn't), and the frontend's generic 401-means-session-expired
// handling popped the login modal. Logging back in only re-authenticates
// *this* node's own session, which was never the problem, so the modal just
// reappeared on the next proxied call — a dead end with no visible way out.
// Not offering those options at all when they can't work is the fix, not a
// better error message for a path that shouldn't be reachable in the first
// place.
//
// Reads (never bumps) state.targetSeq, same as load()/startPolling() — see
// load()'s own comment for why: sel.onchange fires this alongside refresh()
// for the same switch, and if each bumped the shared counter independently
// they'd permanently invalidate each other's results on every switch, not
// just the genuinely stale ones. This function can also reset state.target
// back to local itself (see below), which — via setTarget — *does* bump the
// counter, correctly invalidating any other fetch still in flight for the
// target this just moved away from.
async function refreshCluster(){
  if (!peerPicker) return;
  const seq = state.targetSeq;
  const r = await api('/api/cluster');
  if (seq !== state.targetSeq) return; // a real switch happened while this was in flight; let that one win
  if (!r.ok) return;
  state.cluster = (r.body && r.body.peers) || [];
  state.managed = !!(r.body && r.body.managed);
  state.manager = !!(r.body && r.body.manager);
  state.selfId = (r.body && r.body.self_id) || state.selfId;
  state.selfHostname = (r.body && r.body.self_hostname) || state.selfHostname;
  // If Manager mode is off, or the selected peer vanished/stopped being
  // manageable, fall back to local. The Manager check also covers Manager
  // being turned off *while* a remote node was selected — stay on it and the
  // next proxied call would just 401 into the same dead end described above.
  if (state.target && (!state.manager || !state.cluster.some(p => p.node_id === state.target && p.manageable))) {
    setTarget(null);
    document.body.classList.remove('remote');
  }
  const cur = state.target || '';
  const selfName = (r.body && r.body.self_hostname) || 'local';
  // Other peers are only listed at all when this node is in Manager mode —
  // see the function comment for why an unusable option is worse than none.
  const sorted = computeSortedManageablePeers();
  // Whether cur is genuinely gone is checked against the full (unfiltered)
  // list — an active filter box only hides options from view/selection, it
  // must never be the reason the real target selection gets cleared out from
  // under a user who just happens to be mid-search.
  if (cur && !sorted.some(p => p.node_id === cur)) {
    setTarget(null);
    document.body.classList.remove('remote');
  }
  // The picker owns its own filter row (shown/hidden by option count) and
  // re-applies any in-progress filter itself, so a poll landing mid-search
  // can't wipe what's being typed.
  peerPicker.setOptions(sorted, selfName);
  syncClusterModeRows(); // keeps the Settings panel's Managed/Manager rows in sync if visible
}

// syncClusterModeRows keeps the Settings panel's Managed/Manager rows
// truthful for whichever node is selected in the header. They're only ever
// *editable* for this node — Managed/Manager mode can't be changed remotely
// at all, no exceptions (see LOCAL_API's comment and handleProxy's hard
// rejection of both paths) — but while a remote peer is selected they still
// *display* that peer's real values, sourced from the cluster peer list
// refreshCluster already fetched rather than a proxied call to the peer
// itself. A peer only appears in that list once it advertises Managed mode,
// so "listed at all" already means Managed=true; Manager comes straight from
// that same list's per-peer Manager flag (propagated over gossip). No-ops
// quietly if the Settings panel isn't the current section.
function syncClusterModeRows(){
  const mtCb = document.getElementById('managed-toggle-cb');
  const mgCb = document.getElementById('manager-toggle-cb');
  if (!mtCb || !mgCb) return;
  const mtDesc = document.getElementById('managed-desc');
  const mgDesc = document.getElementById('manager-desc');
  const remote = !!state.target;
  const peer = remote ? state.cluster.find(p => p.node_id === state.target) : null;
  if (!remote) {
    mtCb.checked = state.managed;
    mgCb.checked = state.manager;
    if (mtDesc) mtDesc.textContent = 'Let Manager-mode peers in the cluster remotely configure this node.';
    if (mgDesc) mgDesc.textContent = 'Let this node browse and remotely configure other Managed-mode peers in the cluster.';
  } else if (peer) {
    const name = esc(peer.hostname || peer.node_id.slice(0,8));
    mtCb.checked = true; // it's only listed here at all once it advertises Managed mode
    mgCb.checked = !!peer.manager;
    if (mtDesc) mtDesc.textContent = name+"'s actual Managed mode — read-only here; change it from "+name+"'s own Settings.";
    if (mgDesc) mgDesc.textContent = name+"'s actual Manager mode — read-only here; change it from "+name+"'s own Settings.";
  } else {
    // Selected but not (yet) in the fetched list — a brief race, not a claim
    // either way about its actual state.
    mtCb.checked = false; mgCb.checked = false;
    if (mtDesc) mtDesc.textContent = 'Selected peer is not currently listed — its Managed mode is unknown.';
    if (mgDesc) mgDesc.textContent = 'Selected peer is not currently listed — its Manager mode is unknown.';
  }
  for (const [rowId, cb] of [['cluster-managed-row', mtCb], ['cluster-manager-row', mgCb]]) {
    const row = document.getElementById(rowId);
    if (row) row.classList.toggle('local-only-disabled', remote);
    cb.disabled = remote;
  }
  const shCb = document.getElementById('shell-allow-cb');
  const shRow = document.getElementById('shell-allow-row');
  if (shCb && state.shellSupported) {
    shCb.disabled = remote;
    if (shRow) shRow.classList.toggle('local-only-disabled', remote);
  }
}

// startPolling keeps the peers and bans views live: those change on their own
// (a peer connects, a ban propagates) with no user action, so without this the
// page looks stale until a manual reload. Installed once. It quietly re-fetches
// status, and only re-renders when something actually changed, the current view
// is one of the self-updating ones, and the user isn't typing into a field.
function startPolling(){
  if (state.polling) return;
  state.polling = true;
  setInterval(async () => {
    if (state.restartPending) return; // a restart has its own poll loop
    const seq = state.targetSeq;
    const r = await api('/api/status');
    if (seq !== state.targetSeq) return; // a real switch happened while this was in flight; let that one win
    if (!r || !r.ok) return;
    const nets = r.body.nets || [];
    const nat = { class: r.body.nat_class || '', public: r.body.public || '' };
    const sig = JSON.stringify([nets, nat]);
    if (sig === state.statusSig) return; // nothing moved
    state.status = nets;
    state.nat = nat;
    state.statusSig = sig;
    const ae = document.activeElement, tag = ae && ae.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return; // don't clobber edits
    if (state.section === 'peers' || state.section === 'mesh-peers' || state.section === 'bans') renderSection();
  }, 4000);
}

// syncRailGating re-applies sectionVisible() to every rail button already in
// the DOM. buildRail() only runs once, at dashboard() startup, but a
// capability-gated section's visibility (currently just BGP/BGP Peers, gated
// on state.bgpSupported) is per-*target* and load() re-reads it fresh on
// every switch. Without this, picking a peer from the managed-node list left
// the previous target's BGP tabs on screen — still visible, still clickable —
// on a target that doesn't have FRR; clicking one didn't error, it just fell
// through renderSection()'s sectionVisible backstop straight to Networks,
// which looked like a broken link rather than a hidden one. Called after
// every load() (see refresh()) so the rail always matches whichever node
// load() just fetched capabilities for.
function syncRailGating(){
  document.querySelectorAll('.rail-tab[data-sec]').forEach(b => {
    b.style.display = sectionVisible(b.dataset.sec) ? '' : 'none';
  });
}

async function refresh(){ await load(); syncRailGating(); renderSection(); }

// edit POSTs a config change, surfaces errors, flags a needed restart, refreshes.
// If autoRestart is true and the server signals a restart is needed, it restarts immediately.
async function edit(path, payload, autoRestart){
  const r = await api(path, { method:'POST', body: JSON.stringify(payload) });
  if (!r.ok){ alert((r.body && r.body.error) || 'request failed'); return false; }
  if (r.body && r.body.restart) {
    if (autoRestart) { quietRestart(); return true; }
    state.restartPending = true;
  }
  await refresh();
  return true;
}

// toggleTagState instantly flips a "tag-toggle" state badge (firewall/NAT/QoS
// rules, routes, hosts, DNS entries, and their reject lists all use this same
// markup: a <tr data-enabled="0|1"> containing a <span class="tag-toggle">)
// and fires the underlying API call in the background rather than waiting on
// it before updating. Some platforms apply certain changes far slower than
// others — e.g. Windows' PowerShell-driven WinNAT backend for kernel NAT can
// take on the order of 30s where Linux's netlink calls are near-instant —
// and waiting for that round trip before updating the tag made toggling look
// stuck on those platforms specifically.
//
// This trades that stuck-looking wait for a real window, on the slow
// platforms, where the tag says something the daemon hasn't actually applied
// yet. A background refresh() reconciles the whole section once the request
// settles (success or failure), and a failure is only logged to the console
// rather than blocking with an alert or reverting the tag — deliberately: an
// alert or revert would mean going back to waiting on the round trip for the
// common case to find out about the rare one. Rapid repeated toggles before
// a slow request has settled can also race server-side, since nothing here
// serializes them the way the old await-and-lock pattern did.
function toggleTagState(tag, path, buildPayload){
  const tr = tag.closest('tr');
  const on = tr.dataset.enabled !== '1';
  tr.dataset.enabled = on ? '1' : '0';
  tag.className = 'tag-toggle ' + (on ? 'on' : 'off');
  tag.textContent = on ? 'enabled' : 'disabled';
  tag.title = 'double-click to ' + (on ? 'disable' : 'enable');
  tr.classList.toggle('fw-disabled', !on);
  api(path, { method:'POST', body: JSON.stringify(buildPayload(on)) })
    .then(r => { if (!r.ok) console.warn(path+' toggle failed:', (r.body&&r.body.error)||'failed'); })
    .finally(refresh);
}

// startInlineEdit turns a network name/subnet cell into a text input on double-click.
// Enter commits, Escape cancels, blur commits. Name edits apply live; subnet edits
// confirm first (they need a restart and must be made on every node).
function startInlineEdit(td){
  if (td.querySelector('input')) return; // already editing
  const field = td.dataset.edit, net = td.dataset.net;
  const cur = (cfgOf(net)[field]) || '';
  const inp = $('<input class="cell-edit" type="text" spellcheck="false" autocapitalize="off">');
  inp.value = cur;
  if (field !== 'name' && field !== 'notes') inp.placeholder = 'CIDR, or "none" to clear';
  td.classList.add('editing'); td.innerHTML = ''; td.appendChild(inp);
  inp.focus(); inp.select();
  let done = false;
  const restore = () => { if (done) return; done = true; renderSection(); };
  const commit = async () => {
    if (done) return; done = true;
    const v = inp.value.trim();
    if (v === cur){ renderSection(); return; } // unchanged
    let payload;
    if (field === 'name'){
      if (!v){ alert('name cannot be empty'); renderSection(); return; }
      payload = { op:'rename', net, newName:v };
    } else if (field === 'notes'){
      payload = { op:'notes', net, notes:v };
    } else if (field === 'address4' || field === 'address6'){
      // Only warn about the restart if the banner isn't already up. Once a
      // restart is pending the user has seen the notice and it's still showing,
      // so don't re-prompt on each subsequent address change.
      if (!state.restartPending && !confirm('Change this node\'s overlay '+field+' for "'+nameOf(net)+'"?\n\nA running interface is not re-addressed live, so this node will restart immediately to apply it. Type "none" to clear and auto-assign.')){ renderSection(); return; }
      payload = { op:'address', net }; payload[field] = v;
    } else {
      if (!confirm('Change '+field+' for "'+nameOf(net)+'"?\n\nThis node will restart immediately to apply it, and the same change must be made on every other node in this network.')){ renderSection(); return; }
      payload = { op:'subnet', net }; payload[field] = v;
    }
    const ok = await edit('/api/network', payload, true); // restarts automatically once saved; refreshes on success, alerts on error
    if (!ok) renderSection(); // restore the cell on failure
  };
  inp.onkeydown = e => {
    if (e.key === 'Enter'){ e.preventDefault(); commit(); }
    else if (e.key === 'Escape'){ e.preventDefault(); restore(); }
  };
  inp.onblur = commit;
}

// inlineCellEdit turns any cell into a text input and calls onCommit(value,prev)
// on Enter/blur, or restores the section on Escape. Generic (used by keys).
function inlineCellEdit(td, current, placeholder, onCommit){
  if (td.querySelector('input')) return;
  const inp = $('<input class="cell-edit" type="text" spellcheck="false" autocapitalize="off">');
  inp.value = current || ''; if (placeholder) inp.placeholder = placeholder;
  td.classList.add('editing'); td.innerHTML = ''; td.appendChild(inp);
  inp.focus(); inp.select();
  let done = false;
  const restore = () => { if (done) return; done = true; renderSection(); };
  const commit = async () => { if (done) return; done = true; await onCommit(inp.value.trim(), current||''); };
  inp.onkeydown = e => {
    if (e.key === 'Enter'){ e.preventDefault(); commit(); }
    else if (e.key === 'Escape'){ e.preventDefault(); restore(); }
  };
  inp.onblur = commit;
}

// expiryDisp renders a key's expiry: "never" when unset, otherwise the local
// date/time, flagged when it has already passed.
function expiryDisp(iso){
  if (!iso) return '<span class="off">never</span>';
  const d = new Date(iso); if (isNaN(d)) return esc(iso);
  const exp = d.getTime() <= Date.now();
  return '<span class="'+(exp?'off':'on')+'" title="'+esc(iso)+'">'+esc(d.toLocaleString())+(exp?' (expired)':'')+'</span>';
}

// isoToLocalInput converts an RFC3339 timestamp to the value a
// <input type="datetime-local"> expects (local wall-clock, minute precision).
function isoToLocalInput(iso){
  const d = new Date(iso); if (isNaN(d)) return '';
  const pad = n => String(n).padStart(2,'0');
  return d.getFullYear()+'-'+pad(d.getMonth()+1)+'-'+pad(d.getDate())+'T'+pad(d.getHours())+':'+pad(d.getMinutes());
}

// defaultExpiryInput is the starting value for a key with no expiry set yet:
// today's date at 11:59pm — end of today, not the start of it, so a key
// someone means to expire "today" doesn't accidentally lapse the moment it's
// set. Picking a new date from the calendar replaces just the date portion
// and leaves the time alone, so this is what makes the time default to
// 11:59pm rather than requiring the user to set it explicitly — and, as a
// side effect, it starts the field already complete, so a user who only ever
// touches the date portion never hits the "incomplete date/time" case below.
function defaultExpiryInput(){
  const d = new Date();
  const pad = n => String(n).padStart(2,'0');
  return d.getFullYear()+'-'+pad(d.getMonth()+1)+'-'+pad(d.getDate())+'T23:59';
}

// editExpiry swaps an expires cell for a datetime picker; commit posts the new
// expiry (RFC3339, UTC) or clears it when left blank.
function editExpiry(td, netName){
  if (td.querySelector('input')) return;
  const iso = td.dataset.iso || '';
  const inp = document.createElement('input');
  const startValue = iso ? isoToLocalInput(iso) : defaultExpiryInput();
  inp.type = 'datetime-local'; inp.className = 'cell-edit'; inp.value = startValue;
  td.classList.add('editing'); td.innerHTML = ''; td.appendChild(inp); inp.focus();
  const slot = Number(td.dataset.slot);
  let done = false;
  const commit = () => {
    if (done) return; done = true;
    // A key with no expiry yet starts the field pre-filled at today/12:00am
    // (see defaultExpiryInput) rather than blank, so simply opening the
    // field and clicking away without touching it must not be mistaken for
    // "set the expiry to right now" — that would instantly expire the key.
    // Only a value that actually changed from the starting point counts as
    // an edit.
    if (!iso && inp.value === startValue){ renderSection(); return; }
    // A datetime-local input reports value === '' both when genuinely left
    // blank AND when it's incompletely filled in (e.g. a date was picked
    // from the calendar but the time segment was never touched) — the two
    // are indistinguishable from .value alone. Treating both as "clear the
    // expiry" is what made entering just a date appear to silently revert to
    // "never" the instant the field lost focus: nothing was actually wrong,
    // the browser just never considered the value complete. validity.badInput
    // is how the field itself reports "incomplete", so check that first and
    // let the user finish it instead of discarding what they typed.
    if (inp.value === '' && inp.validity && inp.validity.badInput){
      alert('that date/time isn\'t complete — set both the date and the time');
      inp.focus();
      done = false;
      return;
    }
    const v = inp.value.trim();
    if (!v){ if (!iso){ renderSection(); return; } edit('/api/key', { op:'expiry', net:netName, slot:slot, expires:'' }); return; }
    const d = new Date(v); if (isNaN(d)){ alert('invalid date/time'); renderSection(); return; }
    edit('/api/key', { op:'expiry', net:netName, slot:slot, expires:d.toISOString() });
  };
  inp.onkeydown = e => {
    if (e.key === 'Enter'){ e.preventDefault(); commit(); }
    else if (e.key === 'Escape'){ e.preventDefault(); done = true; renderSection(); }
  };
  inp.onblur = commit;
}

async function doRestart(){
  // Capture this process's boot id first, so we can recognise the NEW process
  // when it returns — even if the restart is faster than our poll interval.
  var before = null;
  try { var p = await fetch('/api/ping', { cache:'no-store' }); if (p.ok){ before = (await p.json()).boot; } } catch(e){}
  const r = await api('/api/restart', { method:'POST' });
  if (!r.ok){ alert((r.body && r.body.error) || 'restart failed'); return; }
  const c = document.getElementById('content');
  if (c) { c.innerHTML=''; c.appendChild($('<div class="card"><div class="empty">Restarting service… reconnecting.</div></div>')); }
  pollBack(before, 0, false);
}
// pollBack reloads once it sees a NEW daemon process: preferably by the boot id
// changing (robust to a restart shorter than the poll gap), or — if no baseline
// was captured — by the server going down and coming back. The cap is a last resort.
async function pollBack(before, n, wentDown){
  var boot = null, reachable = false;
  try {
    var r = await fetch('/api/ping', { cache:'no-store' });
    if (r.ok){ reachable = true; boot = (await r.json()).boot; }
  } catch(e){}
  if (!reachable) wentDown = true;
  var isNew = (before && boot && boot !== before) || (!before && reachable && wentDown);
  if (isNew){ state.restartPending = false; location.reload(); return; }
  if (n >= 20){ location.reload(); return; } // ~20s safety net
  setTimeout(function(){ pollBack(before, n+1, wentDown); }, 1000);
}

// quietRestart fires a service restart and, once the new process is up, calls
// refresh() in place — no page reload, no navigation, no "restarting" banner.
// Used for operations that the user expects to feel instant (rule enable/disable).
async function quietRestart(){
  var before = null;
  try { var p = await fetch('/api/ping', { cache:'no-store' }); if (p.ok){ before = (await p.json()).boot; } } catch(e){}
  const r = await api('/api/restart', { method:'POST' });
  if (!r.ok){ alert((r.body && r.body.error) || 'restart failed'); return; }
  quietPollBack(before, 0, false);
}
async function quietPollBack(before, n, wentDown){
  var boot = null, reachable = false;
  try {
    var r = await fetch('/api/ping', { cache:'no-store' });
    if (r.ok){ reachable = true; boot = (await r.json()).boot; }
  } catch(e){}
  if (!reachable) wentDown = true;
  var isNew = (before && boot && boot !== before) || (!before && reachable && wentDown);
  if (isNew){ await refresh(); return; }
  if (n >= 20){ await refresh(); return; }
  setTimeout(function(){ quietPollBack(before, n+1, wentDown); }, 1000);
}

// parseUnit turns a number + unit (Mbps/Gbps/Kbps) into bytes/sec.
function toBps(num, unit){ const bits={Gbps:1e9,Mbps:1e6,Kbps:1e3}[unit]||1e6; return Math.round(parseFloat(num)*bits/8); }

// systemInterfaces fetches the host's interface names once and caches them, for
// the NAT masquerade dropdown.
let _ifaceCache = null;
async function systemInterfaces(){
  if (_ifaceCache) return _ifaceCache;
  try { const r = await api('/api/interfaces'); _ifaceCache = (r.ok && r.body.interfaces) ? r.body.interfaces : []; }
  catch(e){ _ifaceCache = []; }
  return _ifaceCache;
}

// splitRate breaks a bytes/sec value into a clean {num, unit} for editing, picking
// the unit that reads most naturally. 0 / unset => empty number (i.e. unlimited).
function splitRate(bps){
  if (!bps || bps<=0) return { num:'', unit:'Mbps' };
  const b = bps*8, trim = (x)=>String(+x.toFixed(3));
  if (b>=1e9) return { num:trim(b/1e9), unit:'Gbps' };
  if (b>=1e3 && b<1e6) return { num:trim(b/1e3), unit:'Kbps' };
  return { num:trim(b/1e6), unit:'Mbps' };
}

// startBwEdit turns an up/down rate label into an inline number field + unit
// dropdown on double-click. Enter or focus leaving the editor commits, Escape
// cancels. Clearing the number means unlimited for that direction.
function startBwEdit(span, netName, dir, curBps){
  if (span.querySelector('input')) return;
  const cur = splitRate(curBps);
  const grp = $('<span class="bw-editing"></span>');
  const num = $('<input class="cell-edit" type="text" inputmode="decimal" spellcheck="false" style="width:74px" placeholder="rate">');
  num.value = cur.num;
  const unit = $('<select class="cell-edit" style="width:auto;padding:3px 4px"><option>Kbps</option><option>Mbps</option><option>Gbps</option></select>');
  unit.value = cur.unit;
  grp.appendChild(num); grp.appendChild(unit);
  span.textContent=''; span.appendChild(grp); num.focus(); num.select();
  let done=false;
  const restore = () => { if (done) return; done=true; renderSection(); };
  const commit = async () => {
    if (done) return;
    const raw = num.value.trim();
    let bps;
    if (raw===''){ bps = 0; } // unlimited
    else { const n = parseFloat(raw); if (isNaN(n) || n<0){ alert('Enter a number (e.g. 50), or clear it for unlimited.'); num.focus(); return; } bps = toBps(n, unit.value); }
    done=true;
    await edit('/api/bandwidth', { net:netName, dir, bps });
  };
  num.onkeydown = e => { if (e.key==='Enter'){ e.preventDefault(); commit(); } else if (e.key==='Escape'){ e.preventDefault(); restore(); } };
  unit.onkeydown = num.onkeydown;
  // commit only when focus leaves the whole group (not when moving number<->unit)
  grp.addEventListener('focusout', e => { if (!grp.contains(e.relatedTarget)) commit(); });
}

function restartBanner(c){
  if (!state.restartPending) return;
  const b = $('<div class="card" style="border-color:var(--acc)"><div class="row" style="justify-content:space-between">'
    + '<div>Structural changes saved — restart the service to apply them.</div></div></div>');
  const btn = $('<button class="sm" data-restart>Restart now</button>');
  b.querySelector('.row').appendChild(btn);
  btn.onclick = doRestart;
  c.appendChild(b);
}

// --- filter query parsing (AND / OR / NOT / "phrases" / (grouping)) --------
//
// Every filter box in the web admin (table cross-column filter, log/line
// filters, packet capture) shares this same small boolean query language
// instead of a plain substring match:
//   udp gn-cush1         two bare terms, space-separated = implicit AND
//   udp OR tcp           either term
//   NOT down  /  -down   exclude rows/lines containing "down"
//   (udp OR tcp) -down   parentheses group sub-expressions; NOT/- binds
//                        tightest, then AND (implicit or explicit), then OR
//   "no reply"           quote a phrase to match it (including the space)
//                        literally, or to search for AND/OR/NOT/-/parens as
//                        literal text instead of treating them as operators
// Matching is still always a case-insensitive substring test against the
// row's full text (or the line) - only the boolean combination around those
// substring tests is new. Deliberately forgiving: malformed input (stray
// operators, unmatched parens) degrades to a best-effort match rather than
// throwing, since this re-parses on every keystroke.
function tokenizeFilterQuery(q){
  const toks = [];
  const re = /"([^"]*)"|(\()|(\))|([^\s()]+)/g;
  let m;
  while ((m = re.exec(q))) {
    if (m[1] !== undefined) toks.push({t:'term', v:m[1]});
    else if (m[2]) toks.push({t:'('});
    else if (m[3]) toks.push({t:')'});
    else {
      const w = m[4], up = w.toUpperCase();
      if (up === 'AND' || up === 'OR' || up === 'NOT') toks.push({t:up});
      else if (w[0] === '-' && w.length > 1) { toks.push({t:'NOT'}); toks.push({t:'term', v:w.slice(1)}); }
      else toks.push({t:'term', v:w});
    }
  }
  return toks;
}

function parseFilterQuery(q){
  const toks = tokenizeFilterQuery(q);
  if (!toks.length) return null;
  let pos = 0;
  const peek = () => toks[pos];
  const next = () => toks[pos++];
  const parseAtom = () => {
    const tok = peek();
    if (!tok) return {op:'TERM', v:''};
    if (tok.t === '(') { next(); const n = parseOr(); if (peek() && peek().t === ')') next(); return n; }
    if (tok.t === 'term') { next(); return {op:'TERM', v: tok.v.toLowerCase()}; }
    next(); return parseAtom(); // stray operator where a term was expected - skip it
  };
  const parseNot = () => {
    if (!(peek() && peek().t === 'NOT')) return parseAtom();
    next();
    if (!peek()) return {op:'TERM', v:''}; // dangling NOT (nothing left to negate) - ignore rather than invert to "match nothing"
    return {op:'NOT', v:parseNot()};
  };
  const parseAnd = () => {
    let node = parseNot();
    while (peek() && peek().t !== 'OR' && peek().t !== ')') {
      if (peek().t === 'AND') next();
      node = {op:'AND', l:node, r:parseNot()};
    }
    return node;
  };
  const parseOr = () => {
    let node = parseAnd();
    while (peek() && peek().t === 'OR') { next(); node = {op:'OR', l:node, r:parseAnd()}; }
    return node;
  };
  return parseOr();
}

function evalFilterAst(node, haystack){
  if (!node) return true;
  if (node.op === 'TERM') return node.v === '' || haystack.indexOf(node.v) >= 0;
  if (node.op === 'AND') return evalFilterAst(node.l, haystack) && evalFilterAst(node.r, haystack);
  if (node.op === 'OR') return evalFilterAst(node.l, haystack) || evalFilterAst(node.r, haystack);
  if (node.op === 'NOT') return !evalFilterAst(node.v, haystack);
  return true;
}

const filterTitle = 'AND / OR / NOT (or -term), "quoted phrase", (parentheses)';

// enhanceTable gives a rendered table a live cross-column filter box and
// click-to-sort headers. It only reorders/hides existing <tr> nodes — never
// rebuilds them — so handlers, checkboxes, and inline editors already wired to
// rows keep working. No-ops on empty tables (header only / placeholder rows).
function enhanceTable(table){
  if (table.dataset.enh) return; table.dataset.enh = '1';
  const header = table.rows[0]; if (!header) return;
  const tb = table.tBodies[0] || table;
  const isData = (r) => r !== header && r.cells.length > 1 && !r.querySelector('[colspan]');
  const allRows = () => [].slice.call(table.rows);
  const hasData = allRows().filter(isData).length > 0;
  // Render the filter + toolbar whenever the table is interactive (has data to
  // sort/filter, or exposes +/- actions). An empty table that can grow still
  // needs its + button, so don't bail just because there are no rows yet.
  if (!hasData && !table._rowAdd && !table._rowRemove && !table._rowButtons) return;

  const filt = $('<input class="tfilter" type="text" spellcheck="false" placeholder="filter all columns…" title="'+esc(filterTitle)+'">');
  const bar = $('<div class="tbar"></div>');
  bar.appendChild(filt);
  if (table._rowAdd){ const b=$('<button class="sm tbar-btn" title="add a row">+</button>'); b.onclick=table._rowAdd; bar.appendChild(b); }
  if (table._rowRemove){ const b=$('<button class="sm tbar-btn" title="remove selected rows">\u2212</button>'); b.onclick=table._rowRemove; bar.appendChild(b); }
  if (table._rowButtons) table._rowButtons.forEach(spec => {
    const b=$('<button class="sm '+(spec.cls||'')+(spec.gap?' tbar-gap':'')+'" title="'+esc(spec.title||'')+'">'+esc(spec.label)+'</button>'); b.onclick=spec.onclick; bar.appendChild(b);
  });
  table.parentNode.insertBefore(bar, table);
  const applyFilter = () => {
    const q = filt.value.trim();
    const ast = q ? parseFilterQuery(q) : null;
    allRows().forEach(r => { if (r === header) return;
      r.style.display = (!ast || evalFilterAst(ast, r.textContent.toLowerCase())) ? '' : 'none'; });
  };
  filt.oninput = applyFilter;

  const ths = [].slice.call(header.cells);
  const pureNum = (s) => s !== '' && /^-?[\d.]+$/.test(s.replace(/[, ]/g,''));
  let col = -1, dir = 1;
  ths.forEach((th, i) => {
    if (!th.textContent.trim()) return; // skip checkbox / action columns
    th.classList.add('sortable-th');
    th.onclick = () => {
      if (col === i) dir = -dir; else { col = i; dir = 1; }
      ths.forEach(x => x.removeAttribute('data-sort'));
      th.setAttribute('data-sort', dir > 0 ? 'asc' : 'desc');
      const v = (r) => r.cells[i] ? r.cells[i].textContent.trim() : '';
      const rows = allRows().filter(isData);
      rows.sort((a,b) => { const x = v(a), y = v(b);
        const c = (pureNum(x) && pureNum(y))
          ? parseFloat(x.replace(/[, ]/g,'')) - parseFloat(y.replace(/[, ]/g,''))
          : x.toLowerCase().localeCompare(y.toLowerCase());
        return c * dir; });
      rows.forEach(r => tb.appendChild(r));                                  // sorted data first
      allRows().filter(r => r !== header && !isData(r)).forEach(r => tb.appendChild(r)); // placeholders last
      applyFilter();
    };
  });
}

// addLineFilter inserts a filter box (matching the table filter's look) above a
// <pre> block and live-hides lines that don't match a case-insensitive substring
// — the text-block analog of enhanceTable's cross-column filter.
function addLineFilter(container, pre, fullText){
  const lines = fullText.split('\n');
  const bar = $('<div class="tbar"></div>');
  const filt = $('<input class="tfilter" type="text" spellcheck="false" placeholder="filter lines…" title="'+esc(filterTitle)+'">');
  bar.appendChild(filt);
  container.insertBefore(bar, pre);
  filt.oninput = () => {
    const q = filt.value.trim();
    const ast = q ? parseFilterQuery(q) : null;
    pre.textContent = ast ? lines.filter(l => evalFilterAst(ast, l.toLowerCase())).join('\n') : fullText;
  };
}

function renderSection() {
  const c = document.getElementById('content'); if (!c) return;
  // A capability-gated section (e.g. BGP on a host without vtysh) shouldn't be
  // rendered even if something routed us here — the rail and search already
  // hide it, this is the backstop. Fall back to the default section.
  if (!sectionVisible(state.section)) { state.section = 'networks'; setActiveRailTab(state.section); }
  c.innerHTML = '';
  c.appendChild($('<h2 class="sec">'+label(state.section)+'</h2>'));
  restartBanner(c);
  const nets = state.status;
  if (state.section === 'settings') {
    secSettings(c);
  } else {
    ({ networks:secNetworks, keys:secKeys, peers:secPeers, bans:secBans, routes:secRoutes,
       firewall:secFirewall, nat:secNAT, qos:secQoS, bandwidth:secBandwidth, bgp:secBgp, seeds:secSeeds, hosts:secHosts, dns:secDNS,
       upgrade:secUpgrade,
       metrics:infoMetrics, 'mesh-peers':infoMeshPeers, capture:infoCapture, speedtest:infoSpeedtest, latency:infoLatency,
       'route-table':infoRoutes, 'bgp-peers':secBgpPeers, 'hosts-file':infoHosts, 'dns-state':infoDNS,
       logs:secLogs, readme:secReadme, 'getting-started':secGettingStarted, license:secLicense, about:infoAbout }[state.section])(c, nets);
  }
  c.querySelectorAll('table').forEach(enhanceTable);
}

function emptyCard(c, msg){ c.appendChild($('<div class="card"><div class="empty">'+esc(msg)+'</div></div>')); }

// showModal opens a simple overlay panel with a title and a body node the
// caller fills in (directly, or later via bodyEl.innerHTML once results come
// back). Closes on the × button, clicking the backdrop, or Escape. Returns
// {close} so a caller can dismiss it programmatically too.
function showModal(title, bodyEl){
  const bg = $('<div class="modal-backdrop"></div>');
  const panel = $('<div class="modal-panel"></div>');
  const head = $('<div class="modal-head"><h3></h3></div>');
  head.querySelector('h3').textContent = title;
  const closeBtn = $('<button class="modal-close" title="close">\u2715</button>');
  head.appendChild(closeBtn);
  const body = $('<div class="modal-body"></div>');
  body.appendChild(bodyEl);
  panel.appendChild(head); panel.appendChild(body);
  bg.appendChild(panel);
  document.body.appendChild(bg);
  const close = () => bg.remove();
  closeBtn.onclick = close;
  bg.onclick = (e) => { if (e.target === bg) close(); };
  const onKey = (e) => { if (e.key === 'Escape'){ close(); document.removeEventListener('keydown', onKey); } };
  document.addEventListener('keydown', onKey);
  return { close };
}

// secHint places explanatory text directly under the section title (above the
// cards), instead of inside each per-network box.
function secHint(c, html){ c.appendChild($('<div class="hint" style="margin:0 0 12px">'+html+'</div>')); }

// buildTabBar renders a small segmented control — reusing the .seg/.seg-btn
// styling already used for the Metrics duration selector, the same
// "switch what's showing within a section" role — for sub-tabs within a
// section (currently just Firewall: Rules vs Allow List). onSelect is
// called with the picked tab's id; it owns updating whatever state tracks
// the active tab and re-rendering.
function buildTabBar(tabs, active, onSelect){
  const bar = $('<div class="seg" style="margin-bottom:14px"></div>');
  for (const [id, lbl] of tabs) {
    const b = $('<button class="seg-btn'+(id===active?' active':'')+'">'+esc(lbl)+'</button>');
    b.onclick = () => onSelect(id);
    bar.appendChild(b);
  }
  return bar;
}

// buildPortListRow renders an inline-editable comma-separated port list for
// a Settings row — parses on blur and posts the list live. The first port is
// the primary/fallback port itself (UDP port and TCP port both use this,
// with any further ports being extras); reverts the *whole* field on any
// invalid entry, simpler and more predictable than partially accepting a
// list with one bad entry in it.
//
// Typing "-" instead of a port list turns that transport off entirely (UDP
// or the TCP/TLS fallback, depending on apiPath) — sent to the server as
// {disabled:true} rather than an empty port list, which the server refuses
// the same way an empty list is refused (each of handlePort and
// handleTCPPort also refuses to disable its own transport while the other
// is already off, so at least one always stays live; that error surfaces
// here via the usual alert). onSaved's second argument reports which case
// happened so callers can update local state accordingly either way.
function buildPortListRow(id, label, desc, initialPorts, apiPath, onSaved, initialDisabled){
  const row = $('<div class="settings-row" id="'+id+'-row"></div>');
  row.appendChild($('<div><div class="settings-label">'+esc(label)+'</div><div class="settings-desc">'+desc+'</div></div>'));
  const box = $('<div style="display:flex;gap:6px;align-items:center"></div>');
  const input = $('<input type="text" id="'+id+'-input" style="width:240px" placeholder="e.g. 65432, 443, 80, or - to turn off">');
  const fmtList = (ports) => (ports||[]).join(', ');
  const fmtValue = (ports, disabled) => disabled ? '-' : fmtList(ports);
  input.value = fmtValue(initialPorts, initialDisabled);
  box.appendChild(input);
  row.appendChild(box);
  let last = input.value;
  const save = async () => {
    const raw = input.value.trim();
    let body, normalized;
    if (raw === '-') {
      body = { disabled: true };
      normalized = '-';
    } else {
      const ports = [];
      for (const p of raw.split(',').map(s => s.trim()).filter(s => s.length)) {
        const v = parseInt(p, 10);
        if (isNaN(v) || v < 1 || v > 65535 || String(v) !== p) { input.value = last; return; } // revert on any invalid entry
        ports.push(v);
      }
      if (!ports.length) { input.value = last; return; } // at least one port, or "-" to turn it off
      body = { ports };
      normalized = fmtList(ports);
    }
    if (normalized === last) { input.value = normalized; return; } // unchanged
    const r = await api(apiPath, { method:'POST', body: JSON.stringify(body) });
    if (r.ok) { last = normalized; input.value = normalized; if (onSaved) onSaved(body.ports || [], !!body.disabled); }
    else { alert((r.body && r.body.error) || 'could not update ports'); input.value = last; }
  };
  input.onblur = save;
  input.onkeydown = (e) => { if (e.key === 'Enter') { input.blur(); } };
  return row;
}

function secSettings(c) {
  let card = $('<div class="card"></div>');
  card.appendChild($('<h3>Appearance</h3>'));

  // Dark mode toggle
  const dm = $('<div class="settings-row" id="dark-mode-row"></div>');
  const dmLabel = $('<div><div class="settings-label">Dark mode</div><div class="settings-desc">Switch between dark and light interface theme.</div></div>');
  const dmSw = $('<label class="sw"><input type="checkbox" id="dark-mode-cb"><span class="sw-slider"></span></label>');
  const dmCb = dmSw.querySelector('input');
  dmCb.checked = theme() === 'dark';
  dmCb.onchange = () => { setTheme(dmCb.checked ? 'dark' : 'light'); };
  dm.appendChild(dmLabel); dm.appendChild(dmSw);
  card.appendChild(dm);

  c.appendChild(card); card = $('<div class="card"></div>');
  card.appendChild($('<h3>Cluster</h3>'));

  // Managed toggle. Editable only for *this* node — the one you're actually
  // logged into — never the peer selected in the header dropdown, since
  // Managed/Manager mode are not remotely configurable at all (see
  // LOCAL_API's comment for why an earlier version that let them silently
  // follow the dropdown was worse, not better). While a remote peer is
  // selected this still *reads* as that peer's real value, sourced from the
  // already-fetched cluster peer list rather than a proxied call to it —
  // /api/managed and /api/manager are always rejected over the proxy (see
  // handleProxy), but ManagedPeers() already carries every listed peer's
  // Manager flag over gossip, and "listed at all" already means Managed=true
  // (that's ManagedPeers()'s own filter). Grayed out and disabled so it's
  // clear this is a read-only view of that peer, not an editable one — see
  // syncClusterModeRows.
  const mt = $('<div class="settings-row" id="cluster-managed-row"></div>');
  const mtLabel = $('<div><div class="settings-label">Managed mode</div><div class="settings-desc" id="managed-desc"></div></div>');
  const mtSw = $('<label class="sw"><input type="checkbox" id="managed-toggle-cb"><span class="sw-slider"></span></label>');
  const mtCb = mtSw.querySelector('input');
  mtCb.checked = state.managed;
  mtCb.onchange = () => {
    const want = mtCb.checked;
    // Assume success immediately rather than waiting on the round trip; see
    // toggleTagState's doc comment for why. A failure is logged, not alerted,
    // and the checkbox is left as the user set it rather than reverted.
    state.managed = want;
    api('/api/managed', { method:'POST', body: JSON.stringify({ on: want }) })
      .then(r => { if (!r.ok) console.warn('managed mode toggle failed:', (r.body&&r.body.error)||'failed'); });
  };
  mt.appendChild(mtLabel); mt.appendChild(mtSw);
  card.appendChild(mt);

  // Manager toggle — same local-only-edit, remote-read rule as Managed above.
  const mg = $('<div class="settings-row" id="cluster-manager-row"></div>');
  const mgLabel = $('<div><div class="settings-label">Manager mode</div><div class="settings-desc" id="manager-desc"></div></div>');
  const mgSw = $('<label class="sw"><input type="checkbox" id="manager-toggle-cb"><span class="sw-slider"></span></label>');
  const mgCb = mgSw.querySelector('input');
  mgCb.checked = state.manager;
  mgCb.onchange = () => {
    const want = mgCb.checked;
    // Assume success immediately; see mtCb's onchange above.
    state.manager = want;
    api('/api/manager', { method:'POST', body: JSON.stringify({ on: want }) })
      .then(r => { if (!r.ok) console.warn('manager mode toggle failed:', (r.body&&r.body.error)||'failed'); });
  };
  mg.appendChild(mgLabel); mg.appendChild(mgSw);
  card.appendChild(mg);

  // Allow remote shell — deliberately not part of the Managed/Manager
  // local-edit/remote-read pattern above: there is no gossiped "peer's
  // AllowRemoteShell" value to read (see config.WebAdmin's doc comment on
  // why this is intentionally never advertised), so unlike Managed/Manager
  // this row has nothing meaningful to show for a selected peer at all —
  // it's disabled whenever one is selected, full stop, with a description
  // saying so rather than a read-only peer value.
  const sh = $('<div class="settings-row" id="shell-allow-row"></div>');
  const shDesc = state.shellSupported
    ? 'Let a Manager peer open a real OS shell on this node through the web admin. Off by default: unlike the rest of Managed mode\u2019s API surface, this hands out a full shell as this daemon\u2019s own user (normally root). Every session is transcript-logged. Local-only; never remotely toggleable, even by an authorized Manager peer. This node restarts immediately to apply a change here.'
    : 'Not available on this platform/architecture yet.';
  const shLabel = $('<div><div class="settings-label">Remote shell</div><div class="settings-desc" id="shell-allow-desc">'+esc(shDesc)+'</div></div>');
  const shSw = $('<label class="sw"><input type="checkbox" id="shell-allow-cb"'+(state.shellSupported?'':' disabled')+'><span class="sw-slider"></span></label>');
  const shCb = shSw.querySelector('input');
  shCb.checked = state.allowRemoteShell;
  shCb.onchange = async () => {
    const want = shCb.checked;
    const ok = await edit('/api/shell/setting', { on: want }, true); // restarts automatically once saved
    if (ok) { state.allowRemoteShell = want; }
    else { shCb.checked = !want; }
  };
  sh.appendChild(shLabel); sh.appendChild(shSw);
  card.appendChild(sh);

  // Cluster card ends here. syncClusterModeRows looks its rows up via
  // document.getElementById, which can't find them while card is still a
  // detached DOM subtree, so append first and sync after. (Calling it before
  // the append silently no-ops: checked/disabled never get corrected to match
  // the selected peer, and the rows stay enabled with stale local values until
  // some later unrelated syncClusterModeRows call happens to fix it up — the
  // real bug behind being able to flip these for a "remote" node right after
  // navigating here.)
  c.appendChild(card);
  syncClusterModeRows();

  // Logging card — sits directly beneath Cluster. Holds Log level (moved here
  // from Cluster) and Log size. Both apply live on save: the daemon's reload
  // path updates the running logger's level and the rotating log file's size
  // cap without a restart (a restart would reset every session, backoff timer
  // and learned endpoint — the very state you raise the level to observe).
  // Both are proxied like any other setting, so a Manager can change a peer's.
  card = $('<div class="card"></div>');
  card.appendChild($('<h3>Logging</h3>'));

  // Log level.
  const lg = $('<div class="settings-row" id="loglevel-row"></div>');
  const lgLabel = $('<div><div class="settings-label">Log level</div><div class="settings-desc">How much this node logs. <b>debug</b> is the one worth knowing about. Most <i>rejection</i> paths in the mesh log only at debug: a replayed handshake, a clock-skew mismatch, a handshake claiming our own node id, a failed TLS dial. At <b>info</b>, a node that is receiving handshakes and refusing every one of them looks exactly like a node receiving nothing at all. Applied immediately; no restart, no sessions dropped. Leave it on info in normal operation; debug is chatty.</div></div>');
  const lgSel = $('<select class="sel" id="loglevel-sel"><option value="error">error</option><option value="warn">warn</option><option value="info">info</option><option value="debug">debug</option></select>');
  lgSel.value = state.logLevel || 'info';
  lgSel.onchange = async () => {
    const want = lgSel.value;
    const prev = state.logLevel || 'info';
    const ok = await edit('/api/loglevel', { level: want });
    if (ok) { state.logLevel = want; }
    else { lgSel.value = prev; }
  };
  lg.appendChild(lgLabel); lg.appendChild(lgSel);
  card.appendChild(lg);

  // Log size. The log file is a single rolling file capped at this size; once
  // full, the oldest lines are dropped from the front (FIFO) to make room for
  // new ones, so it never grows without bound and always holds the most recent
  // output. Accepts a human size with a unit suffix — 200M, 1G, 99K — or a bare
  // byte count. Committed on Enter or blur (not per keystroke); the box snaps to
  // the canonical form the server echoes back, and reverts on a rejected value.
  const lz = $('<div class="settings-row" id="logsize-row"></div>');
  const lzLabel = $('<div><div class="settings-label">Log size</div><div class="settings-desc">Maximum size of the log file. Once it fills, the oldest lines are dropped to make room for new ones (FIFO), so the file stays a rolling window of the most recent activity rather than growing without bound. Enter a size like <b>200M</b>, <b>1G</b>, or <b>99K</b>. Default is 200M.</div></div>');
  const lzInput = $('<input type="text" class="sel" id="logsize-input" style="width:90px" placeholder="200M">');
  lzInput.value = state.logMaxSize || '200M';
  let lzLast = lzInput.value;
  const saveLZ = async () => {
    const want = (lzInput.value || '').trim();
    if (!want || want === lzLast) { lzInput.value = lzLast; return; }
    const r = await api('/api/logsize', { method:'POST', body: JSON.stringify({ size: want }) });
    if (r.ok && r.body && r.body.size) {
      state.logMaxSize = r.body.size;
      lzLast = r.body.size;
      lzInput.value = r.body.size; // canonical form from the server
    } else {
      alert((r.body && r.body.error) || 'could not set log size');
      lzInput.value = lzLast; // revert
    }
  };
  lzInput.onkeydown = (e) => { if (e.key === 'Enter'){ e.preventDefault(); lzInput.blur(); } };
  lzInput.onblur = saveLZ;
  lz.appendChild(lzLabel); lz.appendChild(lzInput);
  card.appendChild(lz);
  c.appendChild(card);

  card = $('<div class="card"></div>');
  card.appendChild($('<h3>Routing</h3>'));
  const ra = $('<div class="settings-row" id="routeadv-row"></div>');
  const raLabel = $('<div><div class="settings-label">Route advertisement interval</div><div class="settings-desc">How often (seconds) this node re-advertises the routes it originates. Lower values recover lost routes faster and re-propagate routes sooner after a reject is removed; 0 uses the default (10s).</div></div>');
  const raBox = $('<div style="display:flex;gap:6px;align-items:center"></div>');
  const raInput = $('<input type="text" inputmode="numeric" id="routeadv-input" style="width:80px">');
  raBox.appendChild(raInput);
  ra.appendChild(raLabel); ra.appendChild(raBox);
  card.appendChild(ra);
  let raLast = '0';
  api('/api/routeadv').then(r => { if (r.ok && r.body) { raInput.value = r.body.interval || 10; raLast = String(r.body.interval || 10); } });
  const saveRA = async () => {
    const v = parseInt(raInput.value, 10);
    if (isNaN(v) || v < 0 || v > 86400) { raInput.value = raLast; return; } // revert invalid input
    if (String(v) === raLast) { raInput.value = v; return; }                // unchanged
    const r = await api('/api/routeadv', { method:'POST', body: JSON.stringify({ interval: v }) });
    if (r.ok) { raLast = String(v); raInput.value = v; }
    else { alert((r.body && r.body.error) || 'could not save interval'); raInput.value = raLast; }
  };
  raInput.onblur = saveRA;
  raInput.onkeydown = (e) => { if (e.key === 'Enter') { raInput.blur(); } };

  c.appendChild(card); card = $('<div class="card"></div>');
  card.appendChild($('<h3>Liveness</h3>'));
  const ka = $('<div class="settings-row" id="keepalive-row"></div>');
  const kaLabel = $('<div><div class="settings-label">Keepalive interval</div><div class="settings-desc">How often (seconds) this node pings each connected peer to hold NAT mappings open and measure round-trip time (used for relay selection). Lower values detect a dead link faster (see peer timeout below), at the cost of more background traffic. 0 uses the default (10s).</div></div>');
  const kaBox = $('<div style="display:flex;gap:6px;align-items:center"></div>');
  const kaInput = $('<input type="text" inputmode="numeric" id="keepalive-input" style="width:80px">');
  kaBox.appendChild(kaInput);
  ka.appendChild(kaLabel); ka.appendChild(kaBox);
  card.appendChild(ka);
  let kaLast = '0';
  api('/api/keepalive').then(r => { if (r.ok && r.body) { kaInput.value = r.body.interval || 10; kaLast = String(r.body.interval || 10); } });
  const saveKA = async () => {
    const v = parseInt(kaInput.value, 10);
    if (isNaN(v) || v < 0 || v > 86400) { kaInput.value = kaLast; return; } // revert invalid input
    if (String(v) === kaLast) { kaInput.value = v; return; }                // unchanged
    const r = await api('/api/keepalive', { method:'POST', body: JSON.stringify({ interval: v }) });
    if (r.ok) { kaLast = String(v); kaInput.value = v; }
    else { alert((r.body && r.body.error) || 'could not save interval'); kaInput.value = kaLast; }
  };
  kaInput.onblur = saveKA;
  kaInput.onkeydown = (e) => { if (e.key === 'Enter') { kaInput.blur(); } };

  const pt = $('<div class="settings-row" id="peertimeout-row"></div>');
  const ptLabel = $('<div><div class="settings-label">Peer timeout</div><div class="settings-desc">How long (seconds) a peer may go silent before its session is dropped. This is what governs how long a peer that\u2019s actually gone still shows as connected in the peers table. 0 uses the default (20s). Clamped up to the keepalive interval above if set lower: timing a session out before a single keepalive round trip could complete would just cause constant reconnection thrashing, not faster detection.</div></div>');
  const ptBox = $('<div style="display:flex;gap:6px;align-items:center"></div>');
  const ptInput = $('<input type="text" inputmode="numeric" id="peertimeout-input" style="width:80px">');
  ptBox.appendChild(ptInput);
  pt.appendChild(ptLabel); pt.appendChild(ptBox);
  card.appendChild(pt);
  let ptLast = '0';
  api('/api/peertimeout').then(r => { if (r.ok && r.body) { ptInput.value = r.body.interval || 20; ptLast = String(r.body.interval || 20); } });
  const savePT = async () => {
    const v = parseInt(ptInput.value, 10);
    if (isNaN(v) || v < 0 || v > 86400) { ptInput.value = ptLast; return; } // revert invalid input
    if (String(v) === ptLast) { ptInput.value = v; return; }                // unchanged
    const r = await api('/api/peertimeout', { method:'POST', body: JSON.stringify({ interval: v }) });
    if (r.ok) { ptLast = String(v); ptInput.value = v; }
    else { alert((r.body && r.body.error) || 'could not save interval'); ptInput.value = ptLast; }
  };
  ptInput.onblur = savePT;
  ptInput.onkeydown = (e) => { if (e.key === 'Enter') { ptInput.blur(); } };

  c.appendChild(card); card = $('<div class="card"></div>');
  card.appendChild($('<h3>Underlay</h3>'));
  card.appendChild(buildPortListRow('udpport', 'UDP port',
    'The UDP port(s) this node listens on; comma-separated for more than one. The first is the primary: used for outbound and advertised to peers. Changing it applies immediately: the node rebinds and connected peers migrate automatically; the old port keeps serving inbound for a couple of minutes so nothing drops. Peers that only know this node by a fixed seed address will need that seed updated if the primary changes. Any further ports are extra, inbound-only listeners (e.g. 65432, 443, 80), so a peer behind a restrictive firewall can still reach this node; best-effort, a port that\u2019s privileged or already in use is skipped, not rejected. Enter <b>-</b> to turn UDP off entirely and rely on the TCP fallback below; refused while that\u2019s also off, since the node needs at least one way to be reached.',
    [state.primaryPort, ...(state.extraUDPPorts||[])], '/api/port',
    (ports, disabled) => { state.primaryPort = disabled ? 0 : ports[0]; state.extraUDPPorts = disabled ? [] : ports.slice(1); },
    state.primaryPort === 0));

  card.appendChild(buildPortListRow('tcpport', 'TCP port',
    'The TCP port(s) this node listens on for the TLS fallback, used to reach the mesh when UDP is blocked; comma-separated for more than one. The first defaults to the same number as the UDP port (65432); set it to anything (e.g. 443) to make the fallback look like ordinary HTTPS. Changing it applies immediately: the node rebinds the fallback listener and dials peers on it. Keep it the same on every node in a mesh. Any further ports are extra fallback listeners, best-effort the same way as extra UDP ports. Enter <b>-</b> to turn the TCP fallback off entirely; refused while UDP above is also off.',
    [state.tcpPort, ...(state.extraTCPPorts||[])], '/api/tcpport',
    (ports, disabled) => { state.tcpFallbackDisabled = disabled; if (!disabled) { state.tcpPort = ports[0]; state.extraTCPPorts = ports.slice(1); } },
    state.tcpFallbackDisabled));

  c.appendChild(card); card = $('<div class="card"></div>');
  card.appendChild($('<h3>NAT</h3>'));
  const nt = $('<div class="settings-row" id="natstate-row"></div>');
  const ntLabel = $('<div><div class="settings-label">NAT state timeout</div><div class="settings-desc">How long an idle translated NAT connection is remembered before its mapping is reclaimed, in seconds. Applies to every network. 0 uses the default (120s).</div></div>');
  const ntBox = $('<div style="display:flex;gap:6px;align-items:center"></div>');
  const ntInput = $('<input type="text" inputmode="numeric" id="natstate-input" style="width:90px">');
  ntInput.value = state.natStateTimeout || 120;
  ntBox.appendChild(ntInput);
  nt.appendChild(ntLabel); nt.appendChild(ntBox);
  card.appendChild(nt);
  let ntLast = String(state.natStateTimeout || 120);
  const saveNatState = async () => {
    const v = ntInput.value.trim() === '' ? 0 : parseInt(ntInput.value, 10);
    if (isNaN(v) || v < 0 || v > 86400) { ntInput.value = ntLast; return; } // revert invalid
    if (String(v) === ntLast) { ntInput.value = (v||''); return; }           // unchanged
    const r = await api('/api/natstate', { method:'POST', body: JSON.stringify({ timeout: v }) });
    if (r.ok) { ntLast = String(v); state.natStateTimeout = v; ntInput.value = (v||''); }
    else { alert((r.body && r.body.error) || 'could not set NAT state timeout'); ntInput.value = ntLast; }
  };
  ntInput.onblur = saveNatState;
  ntInput.onkeydown = (e) => { if (e.key === 'Enter') { ntInput.blur(); } };

  const up = $('<div class="settings-row" id="upnp-row"></div>');
  const upLabel = $('<div><div class="settings-label">UPnP</div><div class="settings-desc">Ask this node\u2019s LAN router, via UPnP, to forward every port this node listens on \u2014 the UDP port, the TCP fallback port, and any extra ports configured above \u2014 from its WAN side to this host, so a node behind a home/office router with no manual port forward configured can still be reached directly by peers. Off by default: unlike the settings above (this host\u2019s own kernel), this reaches out and asks a different device \u2014 the router \u2014 to reconfigure itself. Best-effort per port: a router with UPnP off, or without it at all, is a silent no-op, not an error, and one port being rejected doesn\u2019t stop the others from mapping. This node restarts to apply a change here.</div></div>');
  const upSw = $('<label class="sw"><input type="checkbox" id="upnp-toggle-cb"><span class="sw-slider"></span></label>');
  const upCb = upSw.querySelector('input');
  upCb.checked = state.enableUpnp;
  upCb.onchange = async () => {
    const want = upCb.checked;
    const ok = await edit('/api/upnp', { on: want }, true); // restarts automatically once saved
    if (ok) { state.enableUpnp = want; }
    else { upCb.checked = !want; }
  };
  up.appendChild(upLabel); up.appendChild(upSw);
  card.appendChild(up);

  c.appendChild(card); card = $('<div class="card"></div>');
  card.appendChild($('<h3>Privacy</h3>'));
  const gi = $('<div class="settings-row" id="geoip-row"></div>');
  const giLabel = $('<div><div class="settings-label">Geo-IP lookups</div><div class="settings-desc">Show an approximate location (city/region/country) and a map on a peer or seed\u2019s info (\ud83d\udec8) panel, looked up from its public endpoint address. On by default, alongside that same panel\u2019s forward/reverse DNS and WHOIS. Unlike those, which use the internet\u2019s own decentralized lookup protocols, this sends the address to one specific third-party service (ipapi.co) over HTTPS, so turn it off here if you\u2019d rather this node never contact one. This node restarts immediately to apply a change here.</div></div>');
  const giSw = $('<label class="sw"><input type="checkbox" id="geoip-toggle-cb"><span class="sw-slider"></span></label>');
  const giCb = giSw.querySelector('input');
  giCb.checked = state.geoipLookup;
  giCb.onchange = async () => {
    const want = giCb.checked;
    const ok = await edit('/api/geoip', { on: want }, true); // restarts automatically once saved
    if (ok) { state.geoipLookup = want; }
    else { giCb.checked = !want; }
  };
  gi.appendChild(giLabel); gi.appendChild(giSw);
  card.appendChild(gi);

  c.appendChild(card);
}

// secAlwaysAllowed renders the node-global "always allowed" firewall
// exemption list — the Allow List tab under Firewall (moved out of Settings
// in v330; still one flat list, still node-global regardless of which
// network's Firewall page you're looking at it from). Global (not
// per-network): the same allowlist protects every network so a broad deny
// can't lock you out of management or routing protocols. + adds a row, -
// removes ticked rows, and any cell edits on double-click — all applied
// live (no restart). The management entry shows the real web-admin port
// number.
// exParseSvc parses the Allow List's combined proto/port field. Unlike the
// firewall rules table's services field there's no named-service catalog
// here — a FirewallExempt is exactly one proto+port leg, never a list — so
// this only ever accepts "proto" or "proto/port": tcp/udp/icmp/ospf/any or
// a raw 0-255 protocol number (see config.ParseExemptProto), port 0-65535
// with 0 or blank meaning any (matching validateExempt's own range, which
// unlike the rules table's port field allows 0 explicitly, not just >=1).
function exParseSvc(raw){
  const t = (raw||'').trim();
  if (!t) return {proto:'any', port:0};
  const m = /^(any|tcp|udp|icmp|ospf|\d{1,3})(?:\/(\d{1,5}))?$/i.exec(t);
  if (!m) return {error: '"'+t+'": expected proto or proto/port (tcp|udp|icmp|ospf|any|<0-255>, port 0\u201365535)'};
  const proto = m[1].toLowerCase();
  if (/^\d+$/.test(proto) && Number(proto) > 255) return {error: '"'+t+'": protocol number must be 0-255'};
  let port = 0;
  if (m[2]){
    port = Number(m[2]);
    if (!Number.isInteger(port) || port < 0 || port > 65535) return {error: '"'+t+'": port must be 0\u201365535'};
  }
  return {proto, port};
}

// exSvcLabel renders an exempt entry's proto+port as one combined string for
// display and for prefilling the field on edit. A management entry shows the
// live-resolved admin port (mgmtPort), not its stored port — same value the
// two separate cells used to show.
function exSvcLabel(e, mgmtPort){
  const proto = e.proto || 'any';
  const port = e.mgmt ? mgmtPort : (e.port || 0);
  return proto + (port ? '/'+port : '');
}

function secAlwaysAllowed(c){
  const card = $('<div class="card"></div>');
  card.appendChild($('<h3>Allow List</h3>'));
  card.appendChild($('<div class="hint" style="margin:-4px 0 10px">Never allow the firewall to filter these protocols, on any network. Double-click the state tag to toggle an entry.</div>'));
  const t = $('<div></div>');
  t.innerHTML = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>state</th><th>name</th><th>service</th></tr></table>';
  const table = t.querySelector('table');
  card.appendChild(t);
  selAllWire(t);
  table._rowAdd = () => exemptAddRow(table);
  table._rowRemove = () => exemptRemoveChecked(table);
  const resetBar = $('<div style="margin-top:8px"></div>');
  const resetBtn = $('<button class="ghost sm">reset to defaults</button>');
  resetBar.appendChild(resetBtn); card.appendChild(resetBar);
  resetBtn.onclick = async () => {
    if(!confirm('Reset the always-allowed list to the built-in defaults (management, BGP, OSPF, RIP)?')) return;
    const r = await api('/api/exempt', { method:'POST', body: JSON.stringify({ reset:true }) });
    if(!r.ok){ alert((r.body&&r.body.error)||'reset failed'); return; }
    renderSection();
  };
  c.appendChild(card);
  exemptReload(table);
}

// exemptReload fetches the global allowlist and (re)builds the table rows, wiring
// double-click editors. Stores the live list + management port on the table so
// the +/- handlers can rebuild the payload.
async function exemptReload(table){
  const r = await api('/api/exempt');
  const list = (r.ok && r.body && r.body.exempt) || [];
  const mgmtPort = (r.ok && r.body && r.body.mgmt_port) || 0;
  table._exempt = list; table._mgmtPort = mgmtPort;
  state.exempt = list; // lets buildSearchIndex see this without its own round trip — see that function's comment
  const tb = table.tBodies[0] || table;
  [...table.rows].forEach((row,i) => { if(i>0) row.remove(); });
  if(!list.length){
    const tr = document.createElement('tr');
    tr.innerHTML = '<td colspan="4" class="empty">no exemptions; every protocol is subject to the rules</td>';
    tb.appendChild(tr); return;
  }
  list.forEach((e,i) => {
    const enabled = !e.disabled;
    const tr = document.createElement('tr'); tr.dataset.idx = i;
    if(!enabled) tr.className = 'fw-disabled';
    const stTag = '<span class="tag-toggle '+(enabled?'on':'off')+'" data-exstate="1" title="double-click to '+(enabled?'disable':'enable')+'">'+(enabled?'enabled':'disabled')+'</span>';
    tr.innerHTML = '<td class="selcol"><input type="checkbox" class="selbox"></td>'
      + '<td class="ex-state">'+stTag+'</td>'
      + '<td class="ex-name">'+esc(e.name||'')+'</td>'
      + '<td class="ex-service">'+esc(exSvcLabel(e, mgmtPort))+'</td>';
    tb.appendChild(tr);
    // Double-click the state tag to toggle this entry. Like the other edits in
    // this section it flips the in-memory list and re-saves the whole allowlist.
    const stTd = tr.querySelector('.ex-state');
    stTd.ondblclick = () => { list[i].disabled = !list[i].disabled; exemptSave(list); };
    const nameTd = tr.querySelector('.ex-name'), svcTd = tr.querySelector('.ex-service');
    nameTd.title = svcTd.title = 'double-click to edit';
    nameTd.ondblclick = () => inlineCellEdit(nameTd, e.name||'', 'name', v => {
      v = v.trim(); if(!v){ alert('name cannot be empty'); renderSection(); return; }
      list[i].name = v; exemptSave(list); });
    // Editing this field always takes manual control of the port — same rule
    // the old separate port cell followed (any save there cleared .mgmt,
    // even editing proto-only used to leave .mgmt alone since it was a
    // different cell; merged into one field, there's no way left to change
    // proto without also re-stating the port, so any save here clears mgmt).
    svcTd.ondblclick = () => inlineCellEdit(svcTd, exSvcLabel(e, mgmtPort), 'proto or proto/port (blank = any)', v => {
      const parsed = exParseSvc(v);
      if (parsed.error){ alert(parsed.error); renderSection(); return; }
      list[i].proto = parsed.proto; list[i].port = parsed.port; list[i].mgmt = false;
      exemptSave(list); });
  });
}

// exemptPayload normalizes the in-memory list for the wire: management entries
// keep their mgmt flag (so they keep following the admin port) and drop the
// resolved number; everything else sends its fixed port.
function exemptPayload(list){
  return list.map(e => ({ name:e.name, proto:e.proto, port: e.mgmt ? 0 : (e.port||0), mgmt: !!e.mgmt, disabled: !!e.disabled }));
}

async function exemptSave(list){
  const r = await api('/api/exempt', { method:'POST', body: JSON.stringify({ exempt: exemptPayload(list) }) });
  if(!r.ok){ alert((r.body && r.body.error) || 'save failed'); }
  renderSection();
}

function exemptAddRow(table){
  const tr = document.createElement('tr');
  tr.innerHTML = '<td class="selcol"></td>'
    + '<td class="ex-state"><span class="on">enabled</span></td>'
    + '<td><input class="exa-name" placeholder="name" style="width:120px"></td>'
    + '<td><input class="exa-service" placeholder="tcp/443, or any" value="any" style="width:140px"> <button class="sm exa-save">save</button> <button class="ghost sm exa-cancel">cancel</button></td>';
  if(!insertNewRow(table, tr)) return;
  tr.querySelector('.exa-cancel').onclick = () => renderSection();
  tr.querySelector('.exa-save').onclick = () => {
    const name = tr.querySelector('.exa-name').value.trim(); if(!name){ alert('name required'); return; }
    const parsed = exParseSvc(tr.querySelector('.exa-service').value);
    if (parsed.error){ alert(parsed.error); return; }
    const list = (table._exempt || []).slice();
    list.push({ name:name, proto:parsed.proto, port:parsed.port, mgmt:false });
    exemptSave(list);
  };
}

async function exemptRemoveChecked(table){
  const sel = selCheckedRows(table);
  if(!sel.length){ alert('tick one or more rows to remove'); return; }
  const drop = new Set(sel.map(tr => Number(tr.dataset.idx)));
  const list = (table._exempt || []).filter((e,i) => !drop.has(i));
  exemptSave(list);
}

function secKeys(c) {
  if (!state.cfg.length) return emptyCard(c, 'No networks yet — create one first.');
  secHint(c, 'Tick an <b>empty</b> slot and use Generate or Import to fill it; tick filled slots for Enable/Disable/Reveal/Copy/Delete. Double-click a label, state, or key to change it in place. All enabled keys authenticate joiners — to rotate, generate a new key, tick <b>distributed</b> to push it to every peer currently connected over the mesh itself (no copy/paste needed, and it lands in the same slot number there when that slot is free), then disable the old one once you\'re sure it reached everyone. Untick <b>distributed</b> to retract the key from every peer holding a copy — the key stays here, just pulled from everyone else. Renaming a distributed key\'s label, or changing its expiry, pushes the new value to those peers automatically. Double-click <b>expires</b> to set a date/time after which a key stops authenticating (its sessions then re-handshake on a remaining key).');
  for (const cf of state.cfg) {
    const card = $('<div class="card"></div>');
    card.appendChild($('<h3><span class="net-name">'+esc(cf.name)+'</span> <span class="net-id">'+esc(cf.id)+'</span></h3>'));

    const keys = cf.keys||[];
    const bySlot = {}; for (const k of keys) bySlot[k.slot] = k;
    let h = '<table><tr><th class="selcol"><input type="checkbox" class="rall"></th><th>slot</th><th>label</th><th>state</th><th>key</th><th title="ticked: this key has been pushed to every peer currently connected on this network, over the mesh itself">distributed</th><th>expires</th><th>notes</th></tr>';
    for (const k of keys) {
      const sk = esc(selKey(cf.id, k.slot));
      if (!k.set) { h += '<tr class="selectable"><td class="selcol"><input type="checkbox" class="rsel" data-k="'+sk+'"></td>'
        + '<td>'+k.slot+'</td><td colspan="6" class="empty">empty</td></tr>'; continue; }
      const st = k.enabled ? '<span class="on">enabled</span>' : '<span class="off">disabled</span>';
      h += '<tr class="selectable"><td class="selcol"><input type="checkbox" class="rsel" data-k="'+sk+'"></td>'
        + '<td>'+k.slot+'</td>'
        + '<td class="klabel" data-slot="'+k.slot+'" title="double-click to rename">'+esc(k.label||'')+'</td>'
        + '<td class="kstate" data-slot="'+k.slot+'" data-en="'+(k.enabled?1:0)+'" title="double-click to toggle">'+st+'</td>'
        + '<td class="keycell" data-slot="'+k.slot+'" title="double-click to replace"><span class="kval masked" data-slot="'+k.slot+'">••••••••••••••••</span></td>'
        + '<td class="kdist"><input type="checkbox" class="dist-cb" data-slot="'+k.slot+'"'+(k.enabled?'':' disabled')+(k.distributed?' checked':'')
          + ' title="'+(k.enabled?'tick to push this key to every peer currently connected on this network; untick to stop managing it together (peers keep their own copy, unaffected). Deleting a distributed key is what removes it from every peer':'enable this key first')+'"></td>'
        + '<td class="kexp" data-slot="'+k.slot+'" data-iso="'+esc(k.expires||'')+'" title="double-click to set an expiry date/time">'+expiryDisp(k.expires)+'</td>'
        + '<td class="knotes" data-slot="'+k.slot+'" title="double-click to edit — local-only, never pushed to peers even when distributed">'+esc(k.notes||'')+'</td></tr>';
    }
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; card.appendChild(t);
    wireSelectable(t, 'keys');

    t.querySelectorAll('td.klabel').forEach(td => td.ondblclick = () =>
      inlineCellEdit(td, td.textContent, 'label', async (v, prev) => {
        if (v === prev){ renderSection(); return; }
        if (!v){ alert('label cannot be empty'); renderSection(); return; }
        edit('/api/key', { op:'label', net:cf.name, slot:Number(td.dataset.slot), label:v });
      }));
    t.querySelectorAll('td.knotes').forEach(td => td.ondblclick = () =>
      inlineCellEdit(td, td.textContent, 'notes', async (v, prev) => {
        if (v === prev){ renderSection(); return; }
        edit('/api/key', { op:'notes', net:cf.name, slot:Number(td.dataset.slot), notes:v });
      }));
    t.querySelectorAll('td.kstate').forEach(td => {
      td.ondblclick = () => {
        // Flip immediately and fire the request in the background — see
        // toggleTagState's doc comment for why.
        const on = td.dataset.en !== '1';
        td.dataset.en = on ? '1' : '0';
        td.innerHTML = on ? '<span class="on">enabled</span>' : '<span class="off">disabled</span>';
        api('/api/key', { method:'POST', body: JSON.stringify({ op:(on?'enable':'disable'), net:cf.name, slot:Number(td.dataset.slot) }) })
          .then(r => { if (!r.ok) console.warn('key toggle failed:', (r.body&&r.body.error)||'failed'); })
          .finally(refresh);
      };
    });
    t.querySelectorAll('td.keycell').forEach(td => td.ondblclick = () =>
      inlineCellEdit(td, '', 'paste a new key to replace this one', async (v) => {
        if (!v){ renderSection(); return; }
        edit('/api/key', { op:'set', net:cf.name, slot:Number(td.dataset.slot), key:v });
      }));
    t.querySelectorAll('td.kexp').forEach(td => td.ondblclick = () => editExpiry(td, cf.name));
    // The "distributed" checkbox is a persisted toggle, not a one-shot
    // trigger: ticking it floods this key to every peer currently connected
    // on the network (landing in the same slot there when free) and marks it
    // Distributed so it stays ticked across a reload; unticking it detaches
    // the key from mesh-wide management without retracting anything — every
    // peer keeps its own copy exactly as-is, free to drift independently from
    // here on (this node's own copy is never touched either way by either
    // direction). Deleting a distributed key is the one action that actually
    // retracts it from every peer holding a copy — see the delete confirm
    // dialog below. A successful change just re-renders from the server's
    // now-updated state — no popup — a failed one reverts the box and
    // explains why.
    t.querySelectorAll('td.kdist input.dist-cb').forEach(cb => {
      cb.onchange = () => {
        // Assume success immediately and fan the change out to peers in the
        // background — see toggleTagState's doc comment for why.
        const slot = Number(cb.dataset.slot);
        const goingOn = cb.checked;
        const op = goingOn ? 'distribute' : 'undistribute';
        api('/api/key', { method:'POST', body: JSON.stringify({ op, net:cf.name, slot }) })
          .then(r => { if (!r.ok) console.warn('key '+op+' failed:', (r.body&&r.body.error)||'failed'); })
          .finally(refresh);
      };
    });

    const slots = () => selectedIn('keys', cf.id).map(Number);
    const setSlots = () => slots().filter(s => bySlot[s] && bySlot[s].set);
    const emptySlots = () => slots().filter(s => bySlot[s] && !bySlot[s].set);
    const needSet = () => { const s = setSlots(); if (!s.length){ alert('select one or more filled keys first'); return null; } return s; };
    const revealOne = async (slot) => {
      const r = await api('/api/key', { method:'POST', body: JSON.stringify({ op:'reveal', net:cf.name, slot:Number(slot) }) });
      if (!r.ok){ alert((r.body && r.body.error) || 'failed'); return null; }
      return r.body.key;
    };

    const genFn = async () => {
      const empties = emptySlots();
      if (!empties.length){ alert('tick one or more empty slots to generate into'); return; }
      for (const slot of empties){
        const r = await api('/api/key',{method:'POST',body:JSON.stringify({op:'generate',net:cf.name,slot})});
        if (!r.ok){ alert((r.body&&r.body.error)||'failed'); break; }
        if (r.body.restart) state.restartPending = true;
      }
      selection.keys.clear();
      refresh();
    };
    const importFn = () => {
      const empties = emptySlots();
      if (empties.length !== 1){ alert('tick exactly one empty slot to import into'); return; }
      const key = window.prompt('Paste the key to import into slot '+empties[0]+' on "'+cf.name+'":'); if (key===null) return;
      if (!key.trim()){ alert('no key provided'); return; }
      selection.keys.clear();
      edit('/api/key', { op:'set', net:cf.name, slot:empties[0], key:key.trim() });
    };
    const enableFn = async () => { const s=needSet(); if(!s) return;
      for (const slot of s) await api('/api/key',{method:'POST',body:JSON.stringify({op:'enable',net:cf.name,slot})});
      selection.keys.clear(); refresh(); };
    const disableFn = async () => { const s=needSet(); if(!s) return;
      for (const slot of s) await api('/api/key',{method:'POST',body:JSON.stringify({op:'disable',net:cf.name,slot})});
      selection.keys.clear(); refresh(); };
    const deleteFn = async () => { const s=needSet(); if(!s) return;
      const anyDist = s.some(slot => bySlot[slot] && bySlot[slot].distributed);
      const msg = 'Delete '+s.length+' key'+(s.length>1?'s':'')+' on "'+cf.name+'"?'
        + (anyDist ? ' This includes a distributed key; it will be retracted from every peer holding a copy, not just removed here.' : '');
      if (!confirm(msg)) return;
      for (const slot of s) await api('/api/key',{method:'POST',body:JSON.stringify({op:'delete',net:cf.name,slot})});
      selection.keys.clear(); refresh(); };
    const revealFn = async () => { const s=needSet(); if(!s) return;
      for (const slot of s){
        const span = t.querySelector('.kval[data-slot="'+slot+'"]'); if (!span) continue;
        if (span.classList.contains('masked')){
          const key = await revealOne(slot); if (key===null) continue;
          span.textContent = key; span.classList.remove('masked');
        } else { span.textContent = '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022'; span.classList.add('masked'); }
      } };
    const copyFn = async (e) => { const s=needSet(); if(!s) return;
      const vals=[]; for (const slot of s){ const k=await revealOne(slot); if (k!==null) vals.push(k); }
      if (!vals.length) return;
      const text = vals.join('\\n'); const btn = e && e.currentTarget;
      try { await navigator.clipboard.writeText(text); if(btn){ btn.textContent='copied'; setTimeout(()=>{ btn.textContent='Copy'; }, 1200); } }
      catch(err){ window.prompt('Copy the selected key'+(vals.length>1?'s':'')+':', text); } };

    t.querySelector('table')._rowButtons = [
      { label:'Generate', title:'generate a key into each ticked empty slot', onclick: genFn },
      { label:'Import',   title:'import a key into a ticked empty slot',     onclick: importFn },
      { label:'Enable',  cls:'ghost', title:'enable ticked keys',  onclick: enableFn },
      { label:'Disable', cls:'ghost', title:'disable ticked keys', onclick: disableFn },
      { label:'Reveal',  cls:'ghost', title:'reveal ticked keys',  onclick: revealFn },
      { label:'Copy',    cls:'ghost', title:'copy ticked keys',    onclick: copyFn },
      { label:'Delete',  cls:'danger', title:'delete ticked keys', onclick: deleteFn },
    ];
    c.appendChild(card);
  }
}

function secNetworks(c) {
  const cfgs = state.cfg;
  secHint(c, 'Double-click a name, subnet, overlay address, or notes to edit, or the state tag to enable/disable. <b>overlay4/6</b> is this node\'s own address on the network (blank = auto-assigned); changing it persists now but takes effect on this node\'s next restart. <b>notes</b> is a free-form operator note (e.g. purpose, owner) — purely local, never sent to peers. <b>+</b> creates a network, <b>=</b> joins an existing one (paste a join token, or enter id/key/seed), <b>\u25cf</b> generates a join token for a ticked network to paste on another node, <b>\u2212</b> deletes the ticked networks (and their keys), <b>\u21bb reset</b> drops the ticked networks\' peer connections and immediately reconnects to their peers and seeds.');
  const card = $('<div class="card"></div>');
  let h = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>name</th><th>id</th><th>state</th><th>subnet4</th><th>overlay4</th><th>subnet6</th><th>overlay6</th><th>peers</th><th>seeds</th><th>notes</th></tr>';
  if (!cfgs.length) h += '<tr><td colspan="11" class="empty">no networks — click + to create one, or = to join an existing one</td></tr>';
  else for (const cf of cfgs) {
    const live = state.status.find(s=>s.id===cf.id) || {};
    const en = cf.enabled !== false;
    const st = '<span class="'+(en?'on':'off')+' tag-toggle" data-nettoggle="'+esc(cf.id)+'" data-en="'+(en?1:0)+'" title="double-click to '+(en?'disable':'enable')+'">'+(en?'enabled':'disabled')+'</span>';
    h += '<tr class="netrow" data-netid="'+esc(cf.id)+'" data-netname="'+esc(cf.name)+'">'
      + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
      + '<td class="editable" data-edit="name" data-net="'+esc(cf.id)+'" title="double-click to rename">'+esc(cf.name)+'</td>'
      + '<td><span class="net-id">'+esc(cf.id)+'</span></td><td>'+st+'</td>'
      + '<td class="editable" data-edit="subnet4" data-net="'+esc(cf.id)+'" title="double-click to edit">'+esc(cf.subnet4||'—')+'</td>'
      + '<td class="editable" data-edit="address4" data-net="'+esc(cf.id)+'" title="double-click to edit this node\'s overlay address">'+esc(cf.address4||'auto')+'</td>'
      + '<td class="editable" data-edit="subnet6" data-net="'+esc(cf.id)+'" title="double-click to edit">'+esc(cf.subnet6||'—')+'</td>'
      + '<td class="editable" data-edit="address6" data-net="'+esc(cf.id)+'" title="double-click to edit this node\'s overlay address">'+esc(cf.address6||'auto')+'</td>'
      + '<td>'+((live.peers||[]).length)+'</td><td>'+((cf.seeds||[]).length)+'</td>'
      + '<td class="editable" data-edit="notes" data-net="'+esc(cf.id)+'" title="double-click to edit">'+esc(cf.notes||'')+'</td></tr>';
  }
  const t = $('<div class="tscroll"></div>'); t.innerHTML = h+'</table>'; card.appendChild(t);
  const table = t.querySelector('table');
  t.querySelectorAll('td.editable').forEach(td => td.ondblclick = () => startInlineEdit(td));
  t.querySelectorAll('[data-nettoggle]').forEach(s => {
    s.ondblclick = () => {
      // Flip immediately and fire the request in the background — see
      // toggleTagState's doc comment for why. Note this can trigger the same
      // Windows-specific slow paths (kernel NAT reprogram, interface
      // teardown) as the NAT/firewall toggles above.
      const on = s.dataset.en !== '1';
      s.dataset.en = on ? '1' : '0';
      s.className = (on?'on':'off') + ' tag-toggle';
      s.textContent = on ? 'enabled' : 'disabled';
      s.title = 'double-click to ' + (on ? 'disable' : 'enable');
      api('/api/network', { method:'POST', body: JSON.stringify({ op:(on?'enable':'disable'), net:s.dataset.nettoggle }) })
        .then(r => { if (!r.ok) console.warn('network toggle failed:', (r.body&&r.body.error)||'failed'); })
        .finally(refresh);
    };
  });
  selAllWire(t);
  table._rowAdd = () => netAddRow(table);
  table._rowButtons = [
    { label:'=', cls:'tbar-btn', title:'join an existing network (paste a token, or enter id/key/seed)', onclick:() => netJoinRow(table) },
    { label:'\u25cf', cls:'tbar-btn', title:'generate a join token for the ticked network', onclick:() => netTokenRow(table) },
    { label:'\u21bb', cls:'tbar-btn', gap:true, title:'reset', onclick:() => netResetRow(table) },
  ];
  table._rowRemove = async () => {
    const sel = selCheckedRows(table);
    if (!sel.length){ alert('tick one or more networks to remove'); return; }
    const names = sel.map(tr => tr.dataset.netname).join(', ');
    if (!confirm('Delete '+sel.length+' network'+(sel.length>1?'s':'')+' ('+names+')? This permanently removes them and their keys.')) return;
    for (const tr of sel){ const r = await api('/api/network',{method:'POST',body:JSON.stringify({op:'delete',net:tr.dataset.netid})}); if(!r.ok){ alert((r.body&&r.body.error)||'delete failed'); break; } }
    refresh();
  };
  c.appendChild(card);
}

function netAddRow(table){
  const tr = document.createElement('tr');
  tr.innerHTML = '<td class="selcol"></td>'
    + '<td><input class="ne-name" placeholder="name" style="width:110px"></td>'
    + '<td><span class="net-id">—</span></td><td>—</td>'
    + '<td><input class="ne-s4" placeholder="subnet4 (optional)" style="width:160px"></td>'
    + '<td><input class="ne-s6" placeholder="subnet6 (optional)" style="width:140px"></td>'
    + '<td colspan="3"><button class="sm ne-save">create</button> <button class="ghost sm ne-cancel">cancel</button></td>';
  if (!insertNewRow(table, tr)) return;
  tr.querySelector('.ne-name').focus();
  tr.querySelector('.ne-cancel').onclick = () => refresh();
  tr.querySelector('.ne-save').onclick = () => {
    const name = tr.querySelector('.ne-name').value.trim();
    if (!name){ alert('name required'); return; }
    edit('/api/network', { op:'add', net:name, subnet4:tr.querySelector('.ne-s4').value.trim(), subnet6:tr.querySelector('.ne-s6').value.trim() });
  };
}

function netJoinRow(table){
  const tr = document.createElement('tr');
  const cols = table.rows[0].cells.length;
  tr.innerHTML = '<td class="selcol"></td><td colspan="'+(cols-1)+'">'
    + '<div class="row" style="gap:6px;flex-wrap:wrap;align-items:center">'
    + '<input class="je-token" placeholder="paste join token (grav1\u2026)" style="flex:1;min-width:280px">'
    + '<button class="sm je-save">join</button> <button class="ghost sm je-cancel">cancel</button></div>'
    + '<div class="hint" style="margin:6px 0 0">or enter the details manually:</div>'
    + '<div class="row" style="gap:6px;flex-wrap:wrap;align-items:center;margin-top:4px">'
    + '<input class="je-id" placeholder="network id (hex)" style="width:160px">'
    + '<input class="je-key" placeholder="shared key (base64)" style="width:250px">'
    + '<input class="je-peer" placeholder="seed peer host[:port]" style="width:180px">'
    + '</div></td>';
  if (!insertNewRow(table, tr)) return;
  tr.querySelector('.je-token').focus();
  tr.querySelector('.je-cancel').onclick = () => refresh();
  tr.querySelector('.je-save').onclick = () => {
    const token = tr.querySelector('.je-token').value.trim();
    if (token){ edit('/api/network', { op:'join-token', token:token }); return; }
    const id = tr.querySelector('.je-id').value.trim(), key = tr.querySelector('.je-key').value.trim(), peer = tr.querySelector('.je-peer').value.trim();
    if (!id || !key){ alert('paste a join token, or enter a network id and key'); return; }
    if (!peer){ alert('a seed peer is required to learn the network'); return; }
    edit('/api/network', { op:'join', id:id, key:key, peer:peer });
  };
}

// netTokenRow generates a 1-hour join token for the single ticked network and
// shows it with a Copy button that copies the token and closes the box. No
// inputs, no prompts. The token bundles the network's keys, subnets, and every
// seed/cached peer the host knows.
async function netTokenRow(table){
  const sel = selCheckedRows(table);
  if (sel.length !== 1){ alert('tick exactly one network to generate a join token for'); return; }
  const net = sel[0].dataset.netname || sel[0].dataset.netid;
  const tr = document.createElement('tr');
  const cols = table.rows[0].cells.length;
  tr.innerHTML = '<td class="selcol"></td><td colspan="'+(cols-1)+'">'
    + '<div class="row" style="gap:8px;align-items:center;margin-bottom:4px"><span style="color:var(--mut)">join token for <b>'+esc(net)+'</b> (expires in 1 hour):</span><button class="sm tk-copy" disabled>Copy</button></div>'
    + '<textarea class="tk-tok" readonly rows="3" style="width:100%;font-family:monospace;font-size:11px;resize:vertical;box-sizing:border-box">generating\u2026</textarea>'
    + '</td>';
  if (!insertNewRow(table, tr)) return;
  const tok = tr.querySelector('.tk-tok'), copyBtn = tr.querySelector('.tk-copy');
  copyBtn.onclick = async () => {
    try { await navigator.clipboard.writeText(tok.value); }
    catch(err){ tok.focus(); tok.select(); window.prompt('Copy the join token:', tok.value); }
    refresh(); // copying closes the box
  };
  const r = await api('/api/network/token', { method:'POST', body: JSON.stringify({ net:net, expires:'1h' }) });
  if (!r.ok || !r.body || r.body.error){ tok.value = (r.body && r.body.error) || 'could not generate token'; return; }
  tok.value = r.body.token;
  copyBtn.disabled = false;
  tok.focus(); tok.select();
}

// netResetRow drops every current peer session on each ticked network and
// clears seed retry backoff, so the engine immediately re-handshakes with
// every peer and reconnects to every seed instead of waiting out any existing
// timeout. Live and in-place — no config change, no restart.
async function netResetRow(table){
  const sel = selCheckedRows(table);
  if (!sel.length){ alert('tick one or more networks to reset'); return; }
  const names = sel.map(tr => tr.dataset.netname).join(', ');
  if (!confirm('Reset '+sel.length+' network'+(sel.length>1?'s':'')+' ('+names+')? This drops all current peer connections and immediately reconnects to peers and seeds.')) return;
  for (const tr of sel){
    const r = await api('/api/network/reset', { method:'POST', body: JSON.stringify({ net:tr.dataset.netid }) });
    if (!r.ok){ alert((r.body && r.body.error) || 'reset failed'); break; }
  }
  refresh();
}

function perNet(c, render) {
  if (!state.status.length) return emptyCard(c, 'No networks.');
  // Display the per-network cards alphabetically by network name (falls back to
  // id). Sort a copy so the underlying status order/signature is untouched.
  const nets = state.status.slice().sort((a,b) =>
    netNameCmp(nameOf(a.id), nameOf(b.id)));
  for (const n of nets) {
    const card = $('<div class="card"></div>');
    card.appendChild($('<h3><span class="net-name">'+esc(nameOf(n.id))+'</span> <span class="net-id">'+esc(n.id)+'</span></h3>'));
    render(card, n);
    c.appendChild(card);
  }
}

// peerRowsForNet computes the peer rows for network n: connected peers,
// locally-disabled peers (pulled from n.disabled_peers so they can still be
// found), and peers just re-enabled but not yet reconnected. Shared by
// secPeers (mesh > peers, for operating them) and infoMeshPeers (monitor >
// mesh peers, for observing them) so both read from one place and can't
// drift on what a peer row's fields mean between the two pages.
function peerRowsForNet(n) {
  const rows = [];
  // This node's own row, so it shows up in its own peers list instead of
  // being the one thing missing from it. Comes from n.self (see the Go
  // side's SelfPeer) rather than state.selfHostname/self_id (those are
  // this-session-wide, from /api/cluster; n.self is scoped to this
  // network, matching how overlay addresses are per-network).
  const self = n.self || {};
  const selfID = self.NodeID || self.node_id || '';
  if (selfID) {
    // endpoint: a peer's endpoint is its underlay address as observed from
    // here (NAT mapping included); the equivalent for self is this node's
    // own observed public address (state.nat.public, the same value shown
    // in the "This node: ..." banner above the table) — not empty, so the
    // 🛈 info lookup has something to resolve for self the same way it does
    // for any other row. Empty until NAT discovery completes, same as a
    // peer's endpoint is empty before its first handshake.
    rows.push({ id:selfID, host:self.Hostname||self.hostname||'',
      overlay:self.Overlay4||self.overlay4||self.Overlay6||self.overlay6||'',
      endpoint:(state.nat && state.nat.public) || '', endpointText:(state.nat && state.nat.public) || '', relayed:false,
      disabled:false, self:true, notes:'' });
  }
  for (const p of (n.peers||[])) {
    const pid = p.NodeID||p.node_id||'';
    // Should no longer happen (mesh.install now refuses to register a
    // session for our own node id), but a node already running when this
    // ships may still be holding a stale self-session from before it
    // restarted onto the fix — don't let that resurrect the duplicate row.
    if (selfID && pid === selfID) continue;
    // Overlay4 preferred (unchanged for a dual-stack or v4-only peer); a
    // v6-only peer (no Overlay4 assigned at all — a fully supported
    // configuration, see mesh.newNetState's independent need4/need6 gating)
    // previously showed a blank overlay column here instead of falling
    // back to their actual (v6) address.
    // A relayed peer has no direct underlay endpoint of its own to report —
    // ps.endpoint is deliberately the zero value for one (see mesh's
    // peerSession.endpoint doc comment) — so p.Endpoint there is Go's raw
    // zero-value AddrPort string ("invalid AddrPort"), not a real address.
    // Sanitized to '' here (not just cosmetic — peerInfoRow's lookup gate
    // and nodeNotesTitle's seed-address matching both key off endpoint
    // being empty/absent, and neither should try to treat that placeholder
    // string as real). endpointText is separate and display-only: "via
    // <relay>" for a relayed row, the real address otherwise — used only
    // where the value is actually shown as visible text, never passed to
    // the lookup API or note matching, which still want the raw endpoint.
    const relayed = !!(p.Relayed||p.relayed);
    const relayVia = p.RelayVia||p.relay_via||'';
    const rawEndpoint = p.Endpoint||p.endpoint||'';
    const endpoint = rawEndpoint === 'invalid AddrPort' ? '' : rawEndpoint;
    const endpointText = relayed && relayVia ? ('via '+relayVia) : endpoint;
    rows.push({ id:pid, host:p.Hostname||p.hostname||'',
      overlay:p.Overlay4||p.overlay4||p.Overlay6||p.overlay6||'', endpoint:endpoint, endpointText:endpointText,
      relayed:relayed, disabled:false,
      transport:(p.Transport||p.transport||'udp'),
      est:(p.EstablishedAt||p.established_at_unix_nano||0),
      mtu:p.PathMTU||p.path_mtu||0,
      keyLabel:p.KeyLabel||p.key_label||'',
      notes:p.Notes||p.notes||'',
      fsent:p.FragsSent||p.frags_sent||0, fsdrop:p.FragSendDrop||p.frag_send_drop||0,
      frcvd:p.FragsRcvd||p.frags_rcvd||0, rdrop:p.ReasmDrop||p.reasm_drop||0 });
  }
  const seen = new Set(rows.map(r => r.id));
  const disabledSet = new Set();
  for (const d of (n.disabled_peers||[])) {
    const id = d.NodeID||d.node_id||'';
    disabledSet.add(id);
    if (seen.has(id)) continue;
    rows.push({ id, host:d.Hostname||d.hostname||'', overlay:'', endpoint:'disabled', relayed:false, disabled:true, notes:d.Notes||d.notes||'' });
  }
  // Peers the user just re-enabled but that haven't reconnected yet: keep them
  // visible as "connecting" rather than vanishing until the session forms. The
  // entry is pruned the moment the peer shows up connected (or disabled again).
  state.pendingEnable = state.pendingEnable || {};
  const pend = state.pendingEnable[n.id] || {};
  for (const id of Object.keys(pend)) {
    if (seen.has(id) || disabledSet.has(id)) { delete pend[id]; continue; }
    rows.push({ id, host:pend[id].host||'', overlay:'', endpoint:'connecting\u2026', relayed:false, disabled:false, pending:true });
  }
  state.pendingEnable[n.id] = pend;
  // Sort by what's actually shown (nodeCell puts the hostname first, the id
  // second and dimmer) rather than by id: node ids are opaque hex strings
  // with no relation to hostnames, so id-order didn't match "alphabetical by
  // peer" at all — and since it has no visible pattern, any shift in the
  // peer set (a reconnect, a new peer, one going disabled) looked like the
  // list reshuffling at random rather than the stable, predictable order it
  // technically was. Peers without a known hostname yet sort by id (their
  // only identifier); id is always the tiebreaker so the order stays fully
  // deterministic even if two peers ever share a hostname.
  rows.sort((a, b) => netNameCmp(a.host || a.id, b.host || b.id) || a.id.localeCompare(b.id));
  return rows;
}

// openPeerShellFromSel opens a shell on the single ticked row of a peers table,
// reading the selection from namespace sec so both Mesh > peers ('peers') and
// Monitor > mesh peers ('mpeers') can share the exact same gate. Rows are
// recomputed from peerRowsForNet rather than captured, so a peer that dropped
// between render and click is caught here instead of acting on a stale row.
function openPeerShellFromSel(n, sec) {
  const ids = selectedIn(sec, n.id);
  if (ids.length !== 1){ alert('tick exactly one peer (or this node) to open a shell on'); return; }
  const p = peerRowsForNet(n).find(x => x.id === ids[0]);
  if (!p){ alert('that row is no longer available'); return; }
  // A shell on self runs locally and only needs Remote shell enabled
  // (checked server-side), but that's only true when self means *this*
  // browser session's own node. When a remote node is selected up top
  // (state.target) and its own self row is ticked here, opening a shell
  // on it is exactly the same cross-node relay as any other peer row on
  // that node: it still needs Manager mode and a live connection.
  const trulyLocalSelf = p.self && !state.target;
  if (!trulyLocalSelf) {
    if (!state.manager){ alert('enable Manager mode (Settings \u2192 Cluster) to open a shell on a peer'); return; }
    if (p.disabled || p.pending){ alert('that peer is not currently connected'); return; }
  }
  openShellModal(p.id, p.host || p.id);
}

function secPeers(c) {
  c.appendChild($('<div class="hint" style="margin:0 0 10px">Peers connected to this node, grouped by network. This node is listed too (<b>this node</b>), tickable to look up or shell into, but not disabled, edited, or banned. Double-click a peer to disable it; double-click <b>notes</b> for a local, permanent note on its node id (auto-filled from a matching seed\'s note on first connect, unless you\'ve set your own). Tick rows and Ban to block mesh-wide, or tick one and \ud83d\udec8 for DNS/WHOIS. See Monitor \u2192 mesh peers for connection health and transport detail.</div>'));
  if (state.nat && state.nat.class && state.nat.class !== 'unknown') {
    const m = {
      open: ['directly reachable (no NAT)', 'on'],
      cone: ['behind NAT, consistent mapping (hole-punchable)', 'on'],
      nat: ['behind NAT', 'on'],
      symmetric: ['behind NAT, symmetric mapping (a relay may be needed)', 'off']
    };
    const info = m[state.nat.class] || [state.nat.class, 'on'];
    let txt = 'This node: <span class="'+info[1]+'">'+esc(info[0])+'</span>';
    if (state.nat.public) txt += ', public endpoint <b>'+esc(state.nat.public)+'</b>';
    c.appendChild($('<div class="card" style="margin:0 0 12px">'+txt+'</div>'));
  }
  perNet(c, (card, n) => {
    const rows = peerRowsForNet(n);

    let h = '<table class="peers-table"><colgroup><col class="c-sel"><col class="c-target-op"><col class="c-state-op"><col class="c-overlay"><col class="c-fill"><col class="c-fill"></colgroup><tr><th class="selcol"><input type="checkbox" class="rall"></th><th>target</th><th>state</th><th>overlay</th><th>endpoint</th><th>notes</th></tr>';
    if (!rows.length) h += '<tr><td colspan="6" class="empty">no peers</td></tr>';
    for (const p of rows) {
      let stCls, stLabel, stTitle, stDis;
      if (p.self) { stCls=''; stLabel='this node'; stTitle='this is the node you\'re currently on'; stDis=''; }
      else if (p.pending) { stCls=''; stLabel='connecting…'; stTitle='enabled — waiting to connect'; stDis='0'; }
      else if (p.disabled) { stCls='off'; stLabel='disabled'; stTitle='double-click to enable this peer locally'; stDis='1'; }
      else { stCls='on'; stLabel='enabled'; stTitle='double-click to disable this peer locally'; stDis='0'; }
      // The self row's state cell is plain text, not a tag-toggle: it has no
      // data-peer attribute, so the double-click-to-disable handler wired up
      // below simply never finds it — there's no "locally disable yourself"
      // operation to offer.
      const st = p.self
        ? '<span class="hint" title="'+stTitle+'">'+stLabel+'</span>'
        : '<span class="'+(stCls?stCls+' ':'')+'tag-toggle" data-peer="'+esc(p.id)+'" data-host="'+esc(p.host)+'" data-disabled="'+stDis+'" title="'+stTitle+'">'+stLabel+'</span>';
      // The overlay address belongs to the peer (it announces its own). For a
      // peer we can manage remotely, offer a jump to edit it on that node; for
      // others it is read-only here. Requires Manager mode locally — the save
      // is proxied to the peer's own node (see peerOverlayEdit), which now
      // requires the caller to be a Manager (see the header dropdown's
      // refreshCluster comment for the 401/login-loop this avoids). Self's own
      // overlay address is edited from Networks, not here, so it's never
      // offered as editable in this table regardless of Manager mode.
      const canEditOv = !p.self && !p.disabled && !p.pending && p.overlay && state.manager && (state.cluster||[]).some(cp => cp.node_id===p.id && cp.manageable);
      const ovCell = canEditOv
        ? '<td class="peer-ov" data-peer-ov="'+esc(p.id)+'" style="cursor:pointer" title="double-click to edit this peer\'s overlay address on its own node">'+esc(p.overlay)+'</td>'
        : '<td'+(p.overlay?' title="'+(p.self?'this node\'s own overlay address':'overlay address is set by the peer itself — change it on that node')+'"':'')+'>'+esc(p.overlay)+'</td>';
      // The self row IS selectable — ticking it and using 🛈 or the shell
      // button both work (see peerInfoRow and the shell button below); it's
      // only Ban that refuses to act on it once selected. Its notes cell has
      // no data-peer-notes attribute, so double-click-to-edit never attaches
      // there — a local note on your own node's id isn't a meaningful thing
      // to attach.
      h += '<tr class="selectable'+(p.self?' peer-self':(p.disabled?' peer-dis':''))+'"><td class="selcol"><input type="checkbox" class="rsel" data-k="'+esc(selKey(n.id,p.id))+'"'+(p.self?' title="this is the current node"':'')+'></td>'
        + '<td>'+nodeCell(p.host,p.id,n.id,p.endpoint)+'</td><td>'+st+'</td>'
        + ovCell+'<td title="'+(p.self?'this node\'s own observed public address (same as the NAT summary above)':(p.relayed?'no direct underlay address — reached through the relay named here':'observed underlay address — for a peer behind NAT this is its public mapping as seen from here'))+'">'+esc(p.endpointText)+'</td>'
        + (p.self
          ? '<td class="hint" title="a local note on your own node\'s id isn\'t meaningful here">\u2013</td>'
          : '<td class="peer-notes" data-peer-notes="'+esc(p.id)+'" title="double-click to edit — local-only, never sent to the peer">'+esc(p.notes||'')+'</td>')
        + '</tr>';
    }
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; card.appendChild(t);
    wireSelectable(t, 'peers');

    // Double-click a manageable peer's overlay to edit it in place. The
    // address genuinely lives in that peer's own config, not this node's, so
    // the save still has to reach its node — but only the save is proxied
    // there now (see peerOverlayEdit); the admin session itself stays put.
    t.querySelectorAll('[data-peer-ov]').forEach(td => {
      const p = rows.find(x => x.id === td.dataset.peerOv);
      if (p) td.ondblclick = () => peerOverlayEdit(td, n, p);
    });

    // Double-click a peer's notes to edit them — unlike the overlay address,
    // this is genuinely local metadata (see Config.PeerSetNotes), so no
    // proxying to the peer's own node is needed even in Manager mode.
    t.querySelectorAll('[data-peer-notes]').forEach(td => {
      td.ondblclick = () => inlineCellEdit(td, td.textContent, 'notes', async (v, prev) => {
        if (v === prev){ renderSection(); return; }
        const r = await api('/api/peer',{method:'POST',body:JSON.stringify({net:n.id,node:td.dataset.peerNotes,op:'notes',notes:v})});
        if (!r.ok){ alert((r.body&&r.body.error)||'failed'); renderSection(); return; }
        await refresh();
      });
    });

    // Double-click the state tag to toggle local enable/disable — the same
    // gesture used to change state everywhere else in the UI, which also guards
    // against fat-fingering a peer off. /api/peer mutates config + reloads, so it
    // is live immediately — no restart. Bans are mesh-wide; this is local-only.
    // The busy flag prevents a re-entrant call before the first one returns.
    t.querySelectorAll('[data-peer]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        // Flip immediately and fire the request in the background — see
        // toggleTagState's doc comment for why.
        const node = tag.dataset.peer;
        const wasDisabled = tag.dataset.disabled === '1';
        const goingEnable = wasDisabled;
        tag.dataset.disabled = goingEnable ? '0' : '1';
        tag.className = (goingEnable ? 'on' : 'off') + ' tag-toggle';
        tag.textContent = goingEnable ? 'connecting…' : 'disabled';
        tag.title = goingEnable ? 'enabled — waiting to connect' : 'double-click to enable this peer locally';
        state.pendingEnable = state.pendingEnable || {};
        state.pendingEnable[n.id] = state.pendingEnable[n.id] || {};
        if (goingEnable) state.pendingEnable[n.id][node] = { host: tag.dataset.host || '' }; // enabling: bridge the reconnect gap
        else delete state.pendingEnable[n.id][node];                                        // disabling: drop any pending entry
        api('/api/peer',{method:'POST',body:JSON.stringify({net:n.id,node,op:(goingEnable?'enable':'disable')})})
          .then(r => { if (!r.ok) console.warn('peer toggle failed:', (r.body&&r.body.error)||'failed'); })
          .finally(refresh);
      };
    });

    const ptable = t.querySelector('table');
    ptable._rowButtons = [
      { label:'Ban', cls:'danger', title:'ban selected peers (mesh-wide)', onclick: async () => {
      const ids = selectedIn('peers', n.id);
      if (!ids.length){ alert('select one or more peers first'); return; }
      const self = rows.find(x => x.self);
      if (self && ids.includes(self.id)){ alert('can\'t ban this node itself; untick "this node" first'); return; }
      for (const id of ids) await api('/api/ban',{method:'POST',body:JSON.stringify({net:n.id,node:id,notes:'banned via admin'})});
      selection.peers.clear(); refresh();
    }},
      { label:'\u25a0', cls:'tbar-btn', gap:true, title:'open a shell on the selected peer, or on this node itself (a remote peer additionally needs Manager mode here and Remote shell on that peer)', onclick: () => openPeerShellFromSel(n, 'peers') },
      { label:'\ud83d\udec8', cls:'tbar-btn', gap:true, title:'info', onclick: () => peerInfoRow(n, 'peers') }];
  });
}

// peerOverlayEdit turns a manageable peer's overlay cell into an inline text
// input, saving straight to that peer's own node via a one-off proxied API
// call (api()'s target parameter) rather than navigating the whole admin
// session there the way this used to. The address still genuinely lives in
// that peer's own config — not this node's — so the write is still proxied
// to it; only the navigation is gone. Deliberately doesn't go through the
// shared edit()/state.restartPending path: that banner and its "Restart now"
// button always act on whichever node this admin session is currently
// *on* (state.target), and saving here doesn't change that — the restart
// this needs has to happen on the peer's node, so it's called out in an
// alert instead of a banner that would restart the wrong node if clicked.
function peerOverlayEdit(td, n, p){
  if (td.querySelector('input')) return;
  const cur = p.overlay || '';
  const inp = $('<input class="cell-edit" type="text" spellcheck="false" autocapitalize="off" placeholder="address, or &quot;none&quot; to clear">');
  inp.value = cur;
  td.classList.add('editing'); td.innerHTML = ''; td.appendChild(inp);
  inp.focus(); inp.select();
  let done = false;
  const restore = () => { if (done) return; done = true; refresh(); };
  const commit = async () => {
    if (done) return; done = true;
    const v = inp.value.trim();
    if (v === cur){ refresh(); return; }
    const who = p.host || p.id.slice(0,8);
    if (!confirm('Change '+who+'\'s overlay address for "'+nameOf(n.id)+'"?\n\nThis saves on '+who+'\'s own node now but takes effect on its next restart, not this node\'s.')){ refresh(); return; }
    // Which family to submit under: a typed address's own notation is
    // unambiguous (contains ':' => v6). "none" (clearing) carries no family
    // of its own, so that targets whichever family the value being cleared
    // (cur) actually was — sending it as address4 unconditionally used to
    // mean clearing a v6-only peer's address silently did nothing (address6
    // was never sent, so NetworkSetAddress left it untouched) while
    // pointlessly clearing a v4 address that was never set in the first
    // place.
    const targetsV6 = v.toLowerCase() === 'none' ? cur.includes(':') : v.includes(':');
    const body = { op:'address', net:n.id };
    if (targetsV6) body.address6 = v; else body.address4 = v;
    const r = await api('/api/network', { method:'POST', body: JSON.stringify(body) }, p.id);
    if (!r.ok){ alert((r.body && r.body.error) || 'save failed'); refresh(); return; }
    alert('Saved; takes effect next time '+who+' restarts.');
    refresh();
  };
  inp.onkeydown = e => {
    if (e.key === 'Enter'){ e.preventDefault(); commit(); }
    else if (e.key === 'Escape'){ e.preventDefault(); restore(); }
  };
  inp.onblur = commit;
}
// infoMeshPeers renders the same peer set as Mesh > peers (both read from
// the shared peerRowsForNet), but surfaces connection health and session
// detail instead of operating controls: no state toggle, no ban, and no
// mutating double-click. It does allow ticking a row to open a shell or look
// one up (info), the same two read-and-connect conveniences Mesh > peers
// offers, wired to its own selection namespace ('mpeers') so a tick here never
// carries over into the operate view's Ban button. Kept as its own page under
// Monitor, alongside the rest of that group's live-diagnostic views (metrics,
// latency, route-table, ...), rather than a mode flag on secPeers.
function infoMeshPeers(c) {
  c.appendChild($('<div class="hint" style="margin:0 0 10px">Live connection detail for peers on this node, grouped by network. Peer state is read-only here; tick a row and use \u25a0 to open a shell or \ud83d\udec8 to look one up, or see Mesh \u2192 peers to enable, disable, or ban one. <b>key</b> is the label (from this node\'s own Keys table) of the key currently authenticating that peer\'s session, handy for confirming everyone has moved onto a newly-distributed key before you retire the old one. <b>endpoint</b> is the peer\'s observed underlay address; for a peer behind NAT this is its public mapping as seen from here. <b>reach</b> is <i>direct</i> when there\'s a working direct path, or <i>relayed</i> when the peer could only be reached through another node (a strong sign of a restrictive NAT or firewall). <b>time</b> is how long the current session has been established, and it resets on every reconnect.</div>'));
  perNet(c, (card, n) => {
    const rows = peerRowsForNet(n);

    let h = '<table class="peers-table"><colgroup><col class="c-sel"><col class="c-target"><col class="c-key"><col class="c-overlay"><col class="c-endpoint"><col class="c-reach"><col class="c-time"><col class="c-transport"></colgroup><tr><th class="selcol"><input type="checkbox" class="rall"></th><th>target</th><th title="the label (from this node\'s own Keys table) of the key currently authenticating this peer\'s session">key</th><th>overlay</th><th>endpoint</th><th>reach</th><th title="how long the current session with this peer has been established; resets on every reconnect">time</th><th title="discovered path MTU to the peer, fragment counts (tx/rx), and fragment loss (send/reassembly). Clean counters here mean a connectivity problem is not inside the mesh.">transport</th></tr>';
    if (!rows.length) h += '<tr><td colspan="8" class="empty">no peers</td></tr>';
    for (const p of rows) {
      // None of reach/key/time/transport describe a connection to yourself,
      // so the self row leaves them blank rather than showing something
      // that looks like a (misleading) live session — same treatment as a
      // disabled/pending row, just with its own label.
      const reach = (p.disabled || p.self) ? '' : (p.pending ? '<span class="hint">connecting…</span>' : (p.relayed ? '<span class="off" title="reached via a relay — likely behind a restrictive NAT or firewall">relayed</span>' : '<span class="on" title="direct path to the peer\'s endpoint">direct</span>'));
      let xport = '';
      if (!p.disabled && !p.pending && !p.self) {
        const proto = (p.transport==='tcp')
          ? '<span class="off" title="UDP to this peer is blocked or failing; running over the TCP/TLS fallback">tcp</span>'
          : '<span class="on" title="UDP, the normal transport">udp</span>';
        const mtu = p.mtu ? (p.mtu+' B') : 'probing';
        const sd = p.fsdrop||0, rd = p.rdrop||0;
        const health = (sd+rd > 0)
          ? '<span class="off" title="lost fragments to/from this peer: '+sd+' on send (path too small / EMSGSIZE), '+rd+' on reassembly (missing pieces). A climbing count localizes packet loss to the underlay path here.">drops '+sd+'/'+rd+'</span>'
          : '<span class="on" title="no fragment loss to or from this peer">clean</span>';
        xport = proto + ' <span class="hint" title="discovered underlay datagram size to this peer">'+esc(mtu)+'</span> '
          + '<span class="hint" title="fragment datagrams sent / received">tx '+(p.fsent||0)+' rx '+(p.frcvd||0)+'</span> '+health;
      }
      const timeCell = (p.disabled || p.pending || p.self) ? '' : '<span class="hint" title="established '+esc(new Date(p.est/1e6).toLocaleString())+'">'+esc(fmtElapsed(p.est))+'</span>';
      const keyCell = (p.disabled || p.pending || p.self) ? '' : '<span class="hint">'+esc(p.keyLabel||'')+'</span>';
      const stTitle = p.self ? 'this is the node you\'re currently on' : (p.pending ? 'enabled — waiting to connect' : (p.disabled ? 'disabled locally' : 'enabled'));
      // Self's "endpoint" is this node's own observed public address
      // (peerRowsForNet fills it from state.nat.public) — a real value once
      // NAT discovery completes, not a placeholder, so it's shown the same
      // way a peer's endpoint is rather than overridden to a dash.
      h += '<tr class="selectable'+(p.self?' peer-self':'')+'" title="'+stTitle+'" data-peer="'+esc(p.id)+'"><td class="selcol"><input type="checkbox" class="rsel" data-k="'+esc(selKey(n.id,p.id))+'"'+(p.self?' title="this is the current node"':'')+'></td><td>'+nodeCell(p.host,p.id,n.id,p.endpoint)+'</td><td>'+keyCell+'</td>'
        + '<td'+(p.overlay?' title="'+(p.self?'this node\'s own overlay address':'overlay address is set by the peer itself')+'">':'>')+esc(p.overlay)+'</td>'
        + '<td'+(p.self?' title="this node\'s own observed public address">':'>')+esc(p.endpointText)+'</td><td>'+reach+'</td><td>'+timeCell+'</td><td class="c-transport-cell">'+xport+'</td></tr>';
    }
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; card.appendChild(t);
    wireSelectable(t, 'mpeers');
    t.querySelector('table')._rowButtons = [
      { label:'\u25a0', cls:'tbar-btn', gap:true, title:'open a shell on the selected peer, or on this node itself (a remote peer additionally needs Manager mode here and Remote shell on that peer)', onclick: () => openPeerShellFromSel(n, 'mpeers') },
      { label:'\ud83d\udec8', cls:'tbar-btn', gap:true, title:'info', onclick: () => peerInfoRow(n, 'mpeers') }];
  });
}

function secBans(c) {
  c.appendChild($('<div class="hint" style="margin:0 0 10px">Banned peers, grouped by network — the banned target, the node that issued the ban (origin), and the notes. Bans propagate across the mesh from the node that created them; tick rows to lift one. Double-click the notes on a ban you issued to edit them — the change re-floods to every node.</div>'));
  perNet(c, (card, n) => {
    const bans = (n.bans||[]).slice().sort((a,b) =>
      (a.Target||a.target||'').localeCompare(b.Target||b.target||''));
    let h = '<table><tr><th class="selcol"><input type="checkbox" class="rall"></th><th>target</th><th>origin</th><th>notes</th></tr>';
    if (!bans.length) h += '<tr><td colspan="4" class="empty">no bans</td></tr>';
    for (const b of bans) {
      const tgt = b.Target||b.target||'';
      const tgtHost = b.Hostname||b.hostname||'';
      const origin = b.Origin||b.origin||'';
      const originHost = b.OriginHostname||b.origin_hostname||'';
      const notes = b.Notes||b.notes||'';
      const mine = !!(b.Mine||b.mine);
      // The notes are editable only on bans this node originated (the mesh
      // enforces origin-only edits too); edits re-flood to every node. Bans from
      // other origins are read-only here.
      const notesCell = mine
        ? '<td class="editable ban-notes" data-net="'+esc(n.id)+'" data-node="'+esc(tgt)+'" title="double-click to edit these notes — it re-floods to all nodes">'+esc(notes)+'</td>'
        : '<td title="only the node that issued this ban can edit its notes">'+esc(notes)+'</td>';
      // Only the origin node can unban (or edit) its own ban. On other nodes the
      // row is shown read-only: the checkbox is disabled and the row greyed, so
      // it's clear you can't act on it here rather than offering an Unban that
      // silently no-ops.
      const selCell = mine
        ? '<td class="selcol"><input type="checkbox" class="rsel" data-k="'+esc(selKey(n.id,tgt))+'"></td>'
        : '<td class="selcol"><input type="checkbox" class="rsel" disabled title="only '+esc(originHost||origin||'the issuing node')+' can lift this ban"></td>';
      h += '<tr class="selectable'+(mine?'':' ban-locked')+'" data-target="'+esc(tgt)+'">'+selCell
        + '<td>'+nodeCell(tgtHost,tgt,n.id)+'</td><td>'+nodeCell(originHost,origin,n.id)+'</td>'+notesCell+'</tr>';
    }
    const el = $('<div></div>'); el.innerHTML = h+'</table>'; card.appendChild(el);
    // Wire double-click editing on notes cells for locally-originated bans.
    el.querySelectorAll('td.ban-notes').forEach(td => td.ondblclick = () => {
      inlineCellEdit(td, td.textContent, 'notes', async (val) => {
        const ok = await edit('/api/ban/notes', { net: td.dataset.net, node: td.dataset.node, notes: val });
        if (!ok) renderSection();
      });
    });
    wireSelectable(el, 'bans');
    const btable = el.querySelector('table');
    btable._rowButtons = [{ label:'Unban', cls:'ok', title:'unban selected peers', onclick: async () => {
      const ids = selectedIn('bans', n.id);
      if (!ids.length){ alert('select one or more bans first'); return; }
      for (const id of ids) await api('/api/unban',{method:'POST',body:JSON.stringify({net:n.id,node:id})});
      selection.bans.clear(); refresh();
    }}];
  });
}

function secRoutes(c) {
  if (!state.cfg.length) return emptyCard(c, 'No networks.');
  secHint(c, 'CIDRs advertised into the mesh from this node. Double-click a cidr or metric to edit it (lower metric wins); double-click the state tag to toggle the route.');
  secHint(c, 'CIDRs rejected when advertised by other nodes. A reject matches only that exact CIDR; tick inclusive to also reject every more-specific network inside it. Double-click a cidr to edit it, the inclusive cell to toggle it, or the state tag to toggle the entry.');
  for (const cf of state.cfg) {
    const card = $('<div class="card"></div>');
    card.appendChild($('<h3><span class="net-name">'+esc(cf.name)+'</span> <span class="net-id">'+esc(cf.id)+'</span></h3>'));

    // --- Advertise sub-card ---
    const rsub = $('<div class="subcard"></div>');
    rsub.appendChild($('<h4>Advertise</h4>'));
    const rs = cf.routes||[];
    let h = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>state</th><th>cidr</th><th>metric</th></tr>';
    if (!rs.length) h += '<tr><td colspan="4" class="empty">none — click + to advertise a cidr</td></tr>';
    else for (const r of rs){
      const enabled = r.enabled!==false;
      const stTag = '<span class="tag-toggle '+(enabled?'on':'off')+'" data-rtstate="1" title="double-click to '+(enabled?'disable':'enable')+'">'+(enabled?'enabled':'disabled')+'</span>';
      h += '<tr'+(enabled?'':' class="fw-disabled"')+' data-cidr="'+esc(r.cidr)+'" data-enabled="'+(enabled?1:0)+'">'
        + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
        + '<td class="rt-state">'+stTag+'</td>'
        + '<td class="cidr-cell">'+esc(r.cidr)+'</td><td class="metric-cell">'+esc(r.metric||0)+'</td></tr>';
    }
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; rsub.appendChild(t);
    const rtable = t.querySelector('table'); selAllWire(t);
    rtable._rowAdd = () => routeAddCidr(rtable, cf.name, 'add', 3);
    rtable._rowRemove = () => removeCheckedRows(rtable, tr => api('/api/route',{method:'POST',body:JSON.stringify({op:'delete',net:cf.name,cidr:tr.dataset.cidr})}), false);
    // Double-click the state tag to toggle the route enabled/disabled (live).
    t.querySelectorAll('[data-rtstate]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        toggleTagState(tag, '/api/route', on => ({op:(on?'enable':'disable'),net:cf.name,cidr:tag.closest('tr').dataset.cidr}));
      };
    });
    // Double-click a metric cell to edit it in place; saving re-advertises live.
    // Re-advertising re-enables the route, so a disabled route's state is
    // re-applied after the metric change.
    rtable.querySelectorAll('tr[data-cidr] .metric-cell').forEach(cell => {
      const tr = cell.closest('tr');
      const wasDisabled = tr.dataset.enabled === '0';
      cell.title = 'double-click to edit metric';
      cell.ondblclick = () => {
        const cur = cell.textContent.trim();
        cell.innerHTML = '<input type="text" inputmode="numeric" value="'+esc(cur)+'" style="width:60px">';
        const inp = cell.querySelector('input'); inp.focus(); inp.select();
        let done = false;
        const commit = async () => { if(done) return; done=true; const m=Math.max(0,parseInt(inp.value,10)||0);
          const ar = await api('/api/route',{method:'POST',body:JSON.stringify({op:'add',net:cf.name,cidr:tr.dataset.cidr,metric:m})});
          if (!ar.ok){ alert((ar.body&&ar.body.error)||'edit failed'); refresh(); return; }
          if (wasDisabled){ await api('/api/route',{method:'POST',body:JSON.stringify({op:'disable',net:cf.name,cidr:tr.dataset.cidr})}); }
          refresh(); };
        inp.onkeydown = (e) => { if(e.key==='Enter'){ commit(); } else if(e.key==='Escape'){ done=true; refresh(); } };
        inp.onblur = commit;
      };
    });
    // Double-click a cidr to edit the advertised network in place.
    rtable.querySelectorAll('tr[data-cidr] .cidr-cell').forEach(cell => routeCidrEdit(cell, cf, 'advertise'));
    card.appendChild(rsub);

    // --- Reject sub-card ---
    const xsub = $('<div class="subcard"></div>');
    xsub.appendChild($('<h4>Reject</h4>'));
    const rej = cf.route_reject||[];
    let rh = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>state</th><th>cidr</th><th>inclusive</th></tr>';
    if (!rej.length) rh += '<tr><td colspan="4" class="empty">none — click + to reject a cidr</td></tr>';
    else for (const x of rej) {
      const xc = (typeof x==='string')?x:((x&&x.cidr)||'');
      const xi = (typeof x==='object'&&x)?!!x.inclusive:false;
      const enabled = (typeof x==='object'&&x)?!x.disabled:true;
      const stTag = '<span class="tag-toggle '+(enabled?'on':'off')+'" data-rjstate="1" title="double-click to '+(enabled?'disable':'enable')+'">'+(enabled?'enabled':'disabled')+'</span>';
      rh += '<tr'+(enabled?'':' class="fw-disabled"')+' data-cidr="'+esc(xc)+'" data-inc="'+(xi?'1':'0')+'" data-enabled="'+(enabled?1:0)+'">'
        + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
        + '<td class="rj-state">'+stTag+'</td>'
        + '<td class="cidr-cell">'+esc(xc)+'</td><td class="inc-cell">'+(xi?'yes':'no')+'</td></tr>';
    }
    const rt = $('<div></div>'); rt.innerHTML = rh+'</table>'; xsub.appendChild(rt);
    const xtable = rt.querySelector('table'); selAllWire(rt);
    xtable._rowAdd = () => routeAddCidr(xtable, cf.name, 'reject', 2);
    xtable._rowRemove = () => removeCheckedRows(xtable, tr => api('/api/route',{method:'POST',body:JSON.stringify({op:'delete',net:cf.name,cidr:tr.dataset.cidr})}), false);
    // Double-click the state tag to toggle the reject entry (live). The inclusive
    // edit below uses op:'reject', which preserves the disabled flag.
    rt.querySelectorAll('[data-rjstate]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        toggleTagState(tag, '/api/route', on => ({op:(on?'reject-enable':'reject-disable'),net:cf.name,cidr:tag.closest('tr').dataset.cidr}));
      };
    });
    // Double-click the inclusive cell to toggle it (applies live).
    xtable.querySelectorAll('tr[data-cidr] .inc-cell').forEach(cell => {
      const tr = cell.closest('tr');
      cell.title = 'double-click to toggle inclusive';
      cell.ondblclick = () => { const now = tr.dataset.inc==='1';
        edit('/api/route', { op:'reject', net:cf.name, cidr:tr.dataset.cidr, inclusive:!now }, false); };
    });
    // Double-click a cidr to edit the rejected network in place.
    rt.querySelectorAll('tr[data-cidr] .cidr-cell').forEach(cell => routeCidrEdit(cell, cf, 'reject'));
    card.appendChild(xsub);

    c.appendChild(card);
  }
}

// routeCidrEdit makes a route/reject cidr cell editable in place. Because the
// cidr is the record key, a change is applied as add-new then delete-old,
// carrying over the metric (advertise) or inclusive flag (reject) and re-applying
// the disabled state to the new entry.
function routeCidrEdit(cell, cf, kind){
  const tr = cell.closest('tr');
  cell.title = 'double-click to edit cidr';
  cell.ondblclick = () => {
    const old = tr.dataset.cidr;
    cell.innerHTML = '<input type="text" value="'+esc(old)+'" style="width:200px">';
    const inp = cell.querySelector('input'); inp.focus(); inp.select();
    let done = false;
    const commit = async () => {
      if (done) return; done = true;
      const nv = inp.value.trim();
      if (!nv || nv===old){ refresh(); return; }
      const wasDisabled = tr.dataset.enabled === '0';
      let addOp;
      if (kind==='advertise'){
        const mc = tr.querySelector('.metric-cell');
        const m = mc ? (parseInt(mc.textContent,10)||0) : 0;
        addOp = { op:'add', net:cf.name, cidr:nv, metric:m };
      } else {
        addOp = { op:'reject', net:cf.name, cidr:nv, inclusive: tr.dataset.inc==='1' };
      }
      const ar = await api('/api/route', { method:'POST', body: JSON.stringify(addOp) });
      if (!ar.ok){ alert((ar.body&&ar.body.error)||'invalid cidr'); refresh(); return; }
      await api('/api/route', { method:'POST', body: JSON.stringify({ op:'delete', net:cf.name, cidr:old }) });
      if (wasDisabled){
        await api('/api/route', { method:'POST', body: JSON.stringify({ op:(kind==='advertise'?'disable':'reject-disable'), net:cf.name, cidr:nv }) });
      }
      refresh();
    };
    inp.onkeydown = (e) => { if(e.key==='Enter'){ commit(); } else if(e.key==='Escape'){ done=true; refresh(); } };
    inp.onblur = commit;
  };
}

function routeAddCidr(table, net, op, cols){
  const tr = document.createElement('tr');
  const btns = ' <button class="sm re-save">save</button> <button class="ghost sm re-cancel">cancel</button>';
  if (cols===3) tr.innerHTML = '<td class="selcol"></td><td><input class="re-cidr" placeholder="cidr e.g. 192.168.0.0/24" style="width:210px"></td><td><input class="re-metric" type="text" inputmode="numeric" value="0" style="width:60px" title="metric (lower wins)">'+btns+'</td>';
  else tr.innerHTML = '<td class="selcol"></td><td><input class="re-cidr" placeholder="cidr e.g. 192.168.0.0/24" style="width:210px"></td><td><label style="font-size:12px;white-space:nowrap"><input type="checkbox" class="re-inc"> inclusive</label>'+btns+'</td>';
  if (!insertNewRow(table, tr)) return;
  tr.querySelector('.re-cancel').onclick = () => refresh();
  tr.querySelector('.re-save').onclick = () => { const v=tr.querySelector('.re-cidr').value.trim(); if(!v){ alert('cidr required'); return; }
    const mEl=tr.querySelector('.re-metric'); const m=mEl?Math.max(0,parseInt(mEl.value,10)||0):0;
    const incEl=tr.querySelector('.re-inc'); const inc=incEl?incEl.checked:false;
    edit('/api/route', { op:op, net:net, cidr:v, metric:m, inclusive:inc }, false); };
}

function portLabel(min, max){ if(!min && !max) return 'any'; if(min===max) return String(min); return min+'-'+max; }

// secSeeds renders, per network, the configured bootstrap seed addresses
// (host or host:port). Unlike connected peers — which are discovered and vanish
// when they disconnect — seeds persist in config, so this is where you view and
// change the addresses the node dials. Adding applies live; removing takes
// effect on the next restart.
function secSeeds(c, nets){
  const cfgs = state.cfg;
  c.appendChild($('<div class="hint" style="margin:0 0 10px">Seed addresses (host, host:port, or host:port,port,... for more than one) this node dials to find each network; persist whether or not a peer is connected. <b>udp</b> bootstraps over UDP with automatic TCP/TLS fallback; <b>tcp</b> goes straight over TCP/TLS, for cold-starting when UDP\u2019s blocked entirely. Double-click <b>address</b>, <b>transport</b>, or <b>notes</b> to edit. + to add (live), tick rows and \u2212 to remove (next restart), or tick one and \ud83d\udec8 for DNS/WHOIS.</div>'));
  if (!cfgs.length){ emptyCard(c, 'no networks — create one under Networks first'); return; }
  for (const cf of cfgs){
    const card = $('<div class="card"></div>');
    card.appendChild($('<h3><span class="net-name">'+esc(cf.name)+'</span> <span class="net-id">'+esc(cf.id)+'</span></h3>'));
    const seeds = cf.seeds||[];
    let h = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>address</th><th title="how this seed is dialed. Seeds bootstrap over UDP; if UDP is blocked the node automatically falls back to the TCP/TLS port. Double-click this cell to switch a seed between udp and tcp.">transport</th><th>notes</th></tr>';
    if (!seeds.length) h += '<tr><td colspan="4" class="empty">no seeds — click + to add one</td></tr>';
    else for (const s of seeds) { const addr = s.address||s.Address||''; const notes = s.notes||s.Notes||'';
      h += '<tr data-addr="'+esc(addr)+'"><td class="selcol"><input type="checkbox" class="selbox"></td>'
      + '<td class="seed-addr" data-addr="'+esc(addr)+'" style="cursor:pointer" title="double-click to edit this seed\u2019s address">'+esc(stripScheme(addr))+'</td>'
      + '<td class="seed-proto" data-addr="'+esc(addr)+'" style="cursor:pointer" title="double-click to switch transport (udp \u2194 tcp) without opening the full editor">'+seedProtoBadge(addr)+'</td>'
      + '<td class="seed-notes" data-addr="'+esc(addr)+'" title="double-click to edit">'+esc(notes)+'</td></tr>'; }
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; card.appendChild(t);
    const table = t.querySelector('table'); selAllWire(t);
    table._rowAdd = () => seedAddRow(table, cf.name);
    table._rowButtons = [
      { label:'\ud83d\udec8', cls:'tbar-btn', gap:true, title:'info', onclick: () => seedInfoRow(table, cf.name) },
    ];
    table._rowRemove = () => removeCheckedRows(table, tr => api('/api/seed',{method:'POST',body:JSON.stringify({op:'remove',net:cf.name,addr:tr.dataset.addr})}), false);
    t.querySelectorAll('.seed-addr').forEach(td => { td.ondblclick = () => seedEdit(td, cf.name, td.dataset.addr); });
    t.querySelectorAll('.seed-proto').forEach(td => { td.ondblclick = () => seedSetProto(cf.name, td.dataset.addr); });
    t.querySelectorAll('.seed-notes').forEach(td => { td.ondblclick = () =>
      inlineCellEdit(td, td.textContent, 'notes', async (v, prev) => {
        if (v === prev){ renderSection(); return; }
        const ok = await edit('/api/seed', { op:'notes', net:cf.name, addr:td.dataset.addr, notes:v }, false);
        if (!ok) renderSection();
      }); });
    c.appendChild(card);
  }
}

// seedProtoBadge shows how a seed is dialed. Seeds bootstrap over UDP today (the
// TCP/TLS path is an automatic fallback when UDP is blocked); a scheme prefix on
// the address is honored if present so the badge stays correct if explicit TCP
// seeds are added later.
function seedProtoBadge(addr){
  const a = (addr||'').toLowerCase();
  if (a.startsWith('tcp://') || a.startsWith('tcp:'))
    return '<span class="off" title="dialed over the TCP/TLS fallback">tcp</span>';
  return '<span class="hint" title="dialed over UDP; falls back to TCP/TLS automatically if UDP is blocked">udp</span>';
}

// stripScheme removes a leading tcp:// or udp:// from a seed address.
function stripScheme(addr){
  const low = (addr||'').toLowerCase();
  if (low.startsWith('tcp://') || low.startsWith('udp://')) return addr.slice(6);
  return addr;
}
// seedWithScheme applies the chosen transport to a bare address: tcp seeds carry
// a tcp:// prefix; udp seeds are left bare (the default).
function seedWithScheme(addr, transport){
  addr = stripScheme(addr);
  return transport === 'tcp' ? 'tcp://' + addr : addr;
}

// seedEdit turns a seed address cell into an inline editor for the address
// only. The transport (udp/tcp) is changed by double-clicking the transport
// column (seedSetProto), not here — the existing scheme is preserved as-is on
// save. Saving calls the update-addr op, which renames the seed's address in
// place server-side — same live effect as an add-new-then-remove-old would
// (the daemon dials whatever's in the new address list regardless), but
// without wiping the seed's notes or moving its row to the bottom of the
// table, which an add-then-remove used to do on every edit. Saving only
// happens once focus leaves the field (or Enter is pressed).
function seedEdit(cell, net, oldAddr){
  if (cell._editing) return;
  cell._editing = true;
  const curTr = (oldAddr||'').toLowerCase().startsWith('tcp://') ? 'tcp' : 'udp';
  const inp = document.createElement('input');
  inp.value = stripScheme(oldAddr); inp.style.width = '240px';
  cell.textContent = ''; cell.appendChild(inp); inp.focus(); inp.select();
  let done = false;
  const cancel = () => { if (done) return; done = true; refresh(); };
  const save = async () => {
    if (done) return;
    const bare = inp.value.trim();
    const v = seedWithScheme(bare, curTr); // keep the existing transport
    if (!bare || v === oldAddr) { cancel(); return; }
    done = true;
    const a = await api('/api/seed', { method:'POST', body:JSON.stringify({ op:'update-addr', net:net, addr:oldAddr, newAddr:v }) });
    if (!a.ok) { alert('could not update ' + oldAddr + ': ' + (a.body||a.status)); }
    refresh();
  };
  inp.onkeydown = (e) => {
    if (e.key === 'Enter') { e.preventDefault(); save(); }
    else if (e.key === 'Escape') { e.preventDefault(); cancel(); }
  };
  inp.onblur = save;
}

// seedSetProto flips a seed between udp and tcp (double-click the transport
// cell), rewriting the address with the other scheme via update-addr — an
// in-place rename, so the seed's notes and row position are left alone (see
// seedEdit above for why this replaced an earlier add-then-remove).
async function seedSetProto(net, oldAddr){
  const cur = (oldAddr||'').toLowerCase().startsWith('tcp://') ? 'tcp' : 'udp';
  const next = cur === 'tcp' ? 'udp' : 'tcp';
  const v = seedWithScheme(stripScheme(oldAddr), next);
  if (v === oldAddr) return;
  const a = await api('/api/seed', { method:'POST', body:JSON.stringify({ op:'update-addr', net:net, addr:oldAddr, newAddr:v }) });
  if (!a.ok) { alert('could not switch transport: ' + (a.body||a.status)); }
  refresh();
}

// seedInfoRow looks up forward DNS, reverse DNS, and WHOIS for exactly one
// ticked seed and shows them in a modal. Opens the modal immediately with a
// loading message, then fills it in once the (potentially slow — WHOIS in
// particular can take a few seconds) lookup returns.
async function seedInfoRow(table, net){
  const sel = selCheckedRows(table);
  if (sel.length !== 1){ alert('tick exactly one seed to look up'); return; }
  const addr = sel[0].dataset.addr;
  const shown = stripScheme(addr);
  const body = $('<div class="hint">looking up '+esc(shown)+'\u2026</div>');
  showModal('Seed info: ' + shown, body);
  const r = await api('/api/seed-info', { method:'POST', body:JSON.stringify({ net: net, addr: addr }) });
  body.className = ''; // was "hint" for the loading message; real content shouldn't be dimmed
  if (!r.ok){ body.innerHTML = '<div class="hint">'+esc((r.body && r.body.error) || 'lookup failed')+'</div>'; return; }
  renderInfoLookup(body, r.body || {});
}

// renderInfoLookup fills a modal body with the forward DNS / reverse DNS /
// WHOIS / location sections from a lookupSeedInfo-shaped API result. Shared
// by the seed info modal (seedInfoRow) and the peer info modal
// (peerInfoRow) — both look up a host the same way server-side, just
// starting from a different kind of address (a configured seed vs. a peer's
// observed underlay endpoint), so the result shape and its rendering are
// identical.
function renderInfoLookup(body, d){
  const list = (arr) => arr && arr.length ? arr.map(esc).join('<br>') : '<span class="hint">no results</span>';
  let h = '';
  h += '<section><h4>forward dns</h4>';
  h += d.isIP ? '<div class="hint">host is already an IP; forward DNS not applicable</div>'
     : d.forwardErr ? '<div class="hint">'+esc(d.forwardErr)+'</div>'
     : '<div>'+list(d.forward)+'</div>';
  h += '</section>';
  h += '<section><h4>reverse dns'+(d.reverseTarget ? ' ('+esc(d.reverseTarget)+')' : '')+'</h4>';
  h += d.reverseErr ? '<div class="hint">'+esc(d.reverseErr)+'</div>' : '<div>'+list(d.reverse)+'</div>';
  h += '</section>';
  h += '<section><h4>whois'+(d.whoisTarget ? ' ('+esc(d.whoisTarget)+')' : '')+'</h4>';
  h += d.whoisErr ? '<div class="hint">'+esc(d.whoisErr)+'</div>'
     : d.whois ? '<pre>'+esc(d.whois)+'</pre>'
     : '<span class="hint">no results</span>';
  h += '</section>';
  h += '<section><h4>location'+(d.geoTarget ? ' ('+esc(d.geoTarget)+')' : '')+'</h4>';
  h += geoIPSectionHTML(d);
  h += '</section>';
  body.innerHTML = h;
}

// geoIPSectionHTML renders the "location" section's body: an explanation of
// why there's nothing to show when Geo-IP lookups are off (an operator
// explicitly disabled them — see config.WebAdmin.GeoIPLookup, on by default)
// rather than a bare "no results" indistinguishable from a lookup that ran
// and found nothing; the lookup's own error when it ran and failed; or the
// city/region/country, network operator, and an embedded map when it
// succeeded.
function geoIPSectionHTML(d){
  if (!d.geoEnabled){
    return '<div class="hint">Geo-IP lookups are turned off. Enable them under Settings \u2192 Privacy to see an approximate location here.</div>';
  }
  if (d.geoErr){
    return '<div class="hint">'+esc(d.geoErr)+'</div>';
  }
  const g = d.geo;
  if (!g){
    return '<span class="hint">no results</span>';
  }
  const line = [g.city, g.region, g.country].filter(Boolean).join(', ') || 'unknown location';
  let out = '<div>'+esc(line)+(g.org ? ' <span class="hint">('+esc(g.org)+')</span>' : '')+'</div>';
  const lat = g.latitude, lon = g.longitude;
  if (typeof lat === 'number' && typeof lon === 'number' && (lat !== 0 || lon !== 0)){
    // A modest box around the point, not a tight zoom: IP geolocation is
    // typically only accurate to city level at best (sometimes far less),
    // so a street-level zoom would overstate the precision this actually has.
    const box = 0.15;
    const bbox = [lon-box, lat-box, lon+box, lat+box].join(',');
    const marker = lat+','+lon;
    const largeUrl = 'https://www.openstreetmap.org/?mlat='+encodeURIComponent(lat)+'&mlon='+encodeURIComponent(lon)+'#map=10/'+encodeURIComponent(lat)+'/'+encodeURIComponent(lon);
    out += '<div class="hint" style="margin:4px 0 6px">approximate; IP geolocation is usually accurate to city level at best, sometimes far less.</div>';
    out += '<iframe style="width:100%;height:220px;border:1px solid var(--line);border-radius:6px" loading="lazy" referrerpolicy="no-referrer"'
         + ' src="https://www.openstreetmap.org/export/embed.html?bbox='+encodeURIComponent(bbox)+'&layer=mapnik&marker='+encodeURIComponent(marker)+'"></iframe>';
    out += '<div style="margin-top:4px"><a href="'+esc(largeUrl)+'" target="_blank" rel="noopener">view larger map</a></div>';
  }
  return out;
}

// peerInfoRow is the peer analog of seedInfoRow: looks up forward DNS,
// reverse DNS, and WHOIS for exactly one ticked peer's observed underlay
// endpoint (not its overlay address, which is mesh-internal and has nothing
// to look up). Uses the same tick-then-click-info gesture and the same
// selectedIn('peers', ...) selection Peers already uses for Ban, rather than
// the seed table's simpler selCheckedRows, since that's how this table's
// selection actually persists across its periodic re-renders.
async function peerInfoRow(n, sec){
  const ids = selectedIn(sec || 'peers', n.id);
  if (ids.length !== 1){ alert('tick exactly one peer to look up'); return; }
  const p = peerRowsForNet(n).find(x => x.id === ids[0]);
  if (!p || p.disabled || p.pending || !p.endpoint){ alert('no underlay endpoint to look up yet for this peer'); return; }
  const shown = p.host || p.id.slice(0,8);
  const body = $('<div class="hint">looking up '+esc(p.endpoint)+'\u2026</div>');
  showModal('Peer info: ' + shown, body);
  const r = await api('/api/peer-info', { method:'POST', body:JSON.stringify({ net:n.id, node:p.id, endpoint:p.endpoint }) });
  body.className = '';
  if (!r.ok){ body.innerHTML = '<div class="hint">'+esc((r.body && r.body.error) || 'lookup failed')+'</div>'; return; }
  renderInfoLookup(body, r.body || {});
}

function seedAddRow(table, net){
  const tr = document.createElement('tr');
  tr.innerHTML = '<td class="selcol"></td>'
    + '<td><input class="se-addr" placeholder="host or host:port[,port,...] e.g. 203.0.113.5:65432,443" style="width:240px"></td>'
    + '<td><select class="se-tr"><option value="udp">udp</option><option value="tcp">tcp</option></select> <button class="sm se-save">save</button> <button class="ghost sm se-cancel">cancel</button></td>';
  if(!insertNewRow(table, tr)) return;
  tr.querySelector('.se-cancel').onclick = () => refresh();
  tr.querySelector('.se-save').onclick = () => {
    const bare=tr.querySelector('.se-addr').value.trim(); if(!bare){ alert('address required'); return; }
    const v=seedWithScheme(bare, tr.querySelector('.se-tr').value);
    edit('/api/seed', { op:'add', net:net, addr:v }, false); };
}

function secHosts(c, nets){
  const cfgs = state.cfg;
  if (!cfgs.length){ emptyCard(c, 'no networks — create one under Networks first'); return; }
  secHint(c, 'Custom name \u2192 IP records this node advertises to the whole mesh; every peer adds them to its hosts file, on top of the automatic peer-hostname entries. The IP can be anything: an overlay address, or a LAN service reachable over an advertised route. Double-click a name or ip to edit it, or the state tag to toggle the record.');
  secHint(c, 'Hostnames rejected from the mesh: a record peers advertise for one of these names is never written into this node\'s hosts file. This is a local filter (it doesn\'t affect other nodes), the host analog of rejecting a route. Double-click the state tag to toggle; use + to add, tick rows and \u2212 to remove.');
  for (const cf of cfgs){
    const card = $('<div class="card"></div>');
    card.appendChild($('<h3><span class="net-name">'+esc(cf.name)+'</span> <span class="net-id">'+esc(cf.id)+'</span></h3>'));

    // --- Advertise sub-card ---
    const asub = $('<div class="subcard"></div>');
    asub.appendChild($('<h4>Advertise</h4>'));
    const recs = cf.hosts_advertise||[];
    let h = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>state</th><th>name</th><th>ip</th></tr>';
    if (!recs.length) h += '<tr><td colspan="4" class="empty">no advertised hosts — click + to add one</td></tr>';
    else for (const r of recs){
      const enabled = !r.disabled;
      const stTag = '<span class="tag-toggle '+(enabled?'on':'off')+'" data-hoststate="1" title="double-click to '+(enabled?'disable':'enable')+'">'+(enabled?'enabled':'disabled')+'</span>';
      h += '<tr'+(enabled?'':' class="fw-disabled"')+' data-name="'+esc(r.name)+'" data-ip="'+esc(r.ip)+'" data-enabled="'+(enabled?1:0)+'">'
        + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
        + '<td class="ho-state">'+stTag+'</td>'
        + '<td class="ho-name-cell">'+esc(r.name)+'</td><td class="ho-ip-cell">'+esc(r.ip)+'</td></tr>';
    }
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; asub.appendChild(t);
    const table = t.querySelector('table'); selAllWire(t);
    // Double-click the state tag to toggle the record enabled/disabled — the
    // change applies live via the reload, no restart. The busy flag guards
    // against a re-entrant click before the first request returns.
    t.querySelectorAll('[data-hoststate]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        toggleTagState(tag, '/api/host', on => ({op:(on?'enable':'disable'),net:cf.name,name:tag.closest('tr').dataset.name}));
      };
    });
    // Double-click a name or ip cell to edit it in place. Both go through the
    // update op, which renames/re-IPs the record while keeping its position and
    // enabled/disabled state. Editing the name passes the current ip and vice
    // versa, so the untouched field is preserved.
    t.querySelectorAll('tr[data-name]').forEach(tr => {
      const nameTd = tr.querySelector('.ho-name-cell');
      const ipTd = tr.querySelector('.ho-ip-cell');
      if (nameTd) nameTd.title = 'double-click to edit';
      if (ipTd) ipTd.title = 'double-click to edit';
      if (nameTd) nameTd.ondblclick = () => inlineCellEdit(nameTd, tr.dataset.name, 'web.local', async (v) => {
        v = v.trim();
        if (!v){ alert('name cannot be empty'); renderSection(); return; }
        if (v === tr.dataset.name){ renderSection(); return; }
        const r = await api('/api/host',{method:'POST',body:JSON.stringify({op:'update',net:cf.name,name:tr.dataset.name,newname:v,ip:tr.dataset.ip})});
        if (!r.ok){ alert((r.body&&r.body.error)||'rename failed'); }
        refresh();
      });
      if (ipTd) ipTd.ondblclick = () => inlineCellEdit(ipTd, tr.dataset.ip, '192.168.5.5', async (v) => {
        v = v.trim();
        if (!v){ alert('ip cannot be empty'); renderSection(); return; }
        if (v === tr.dataset.ip){ renderSection(); return; }
        const r = await api('/api/host',{method:'POST',body:JSON.stringify({op:'update',net:cf.name,name:tr.dataset.name,newname:tr.dataset.name,ip:v})});
        if (!r.ok){ alert((r.body&&r.body.error)||'update failed'); }
        refresh();
      });
    });
    table._rowAdd = () => hostAddRow(table, cf.name);
    table._rowRemove = () => removeCheckedRows(table, tr => api('/api/host',{method:'POST',body:JSON.stringify({op:'remove',net:cf.name,name:tr.dataset.name})}), false);
    card.appendChild(asub);

    // --- Reject sub-card ---
    const xsub = $('<div class="subcard"></div>');
    xsub.appendChild($('<h4>Reject</h4>'));
    const rejs = cf.hosts_reject||[];
    let rh = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>state</th><th>name</th></tr>';
    if (!rejs.length) rh += '<tr><td colspan="3" class="empty">no rejected hosts — click + to add one</td></tr>';
    else for (const r of rejs){
      const enabled = !r.disabled;
      const stTag = '<span class="tag-toggle '+(enabled?'on':'off')+'" data-hostrejstate="1" title="double-click to '+(enabled?'disable':'enable')+'">'+(enabled?'enabled':'disabled')+'</span>';
      rh += '<tr'+(enabled?'':' class="fw-disabled"')+' data-rejname="'+esc(r.name)+'" data-enabled="'+(enabled?1:0)+'">'
        + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
        + '<td class="ho-state">'+stTag+'</td>'
        + '<td>'+esc(r.name)+'</td></tr>';
    }
    const rt = $('<div></div>'); rt.innerHTML = rh+'</table>'; xsub.appendChild(rt);
    const rtable = rt.querySelector('table'); selAllWire(rt);
    rt.querySelectorAll('[data-hostrejstate]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        toggleTagState(tag, '/api/host', on => ({op:(on?'reject-enable':'reject-disable'),net:cf.name,name:tag.closest('tr').dataset.rejname}));
      };
    });
    rtable._rowAdd = () => hostRejectAddRow(rtable, cf.name);
    rtable._rowRemove = () => removeCheckedRows(rtable, tr => api('/api/host',{method:'POST',body:JSON.stringify({op:'reject-remove',net:cf.name,name:tr.dataset.rejname})}), false);
    card.appendChild(xsub);
    c.appendChild(card);
  }
}

function hostAddRow(table, net){
  const tr = document.createElement('tr');
  tr.innerHTML = '<td class="selcol"></td>'
    + '<td class="ho-state"><span class="on">enabled</span></td>'
    + '<td><input class="ho-name" placeholder="web.local" style="width:160px"></td>'
    + '<td><input class="ho-ip" placeholder="192.168.5.5" style="width:140px"> <button class="sm ho-save">save</button> <button class="ghost sm ho-cancel">cancel</button></td>';
  if(!insertNewRow(table, tr)) return;
  tr.querySelector('.ho-cancel').onclick = () => refresh();
  tr.querySelector('.ho-save').onclick = () => {
    const name = tr.querySelector('.ho-name').value.trim();
    const ip = tr.querySelector('.ho-ip').value.trim();
    if(!name || !ip){ alert('name and ip required'); return; }
    edit('/api/host', { op:'add', net:net, name:name, ip:ip }, false);
  };
}

function hostRejectAddRow(table, net){
  const tr = document.createElement('tr');
  tr.innerHTML = '<td class="selcol"></td>'
    + '<td class="ho-state"><span class="on">enabled</span></td>'
    + '<td><input class="hr-name" placeholder="host.local" style="width:200px"> <button class="sm hr-save">save</button> <button class="ghost sm hr-cancel">cancel</button></td>';
  if(!insertNewRow(table, tr)) return;
  tr.querySelector('.hr-cancel').onclick = () => refresh();
  tr.querySelector('.hr-save').onclick = () => {
    const name = tr.querySelector('.hr-name').value.trim();
    if(!name){ alert('name required'); return; }
    edit('/api/host', { op:'reject', net:net, name:name }, false);
  };
}

function secDNS(c, nets){
  const cfgs = state.cfg;
  if (!cfgs.length){ emptyCard(c, 'no networks — create one under Networks first'); return; }
  secHint(c, 'Domains this node forwards to specific DNS servers, advertised mesh-wide. Peers register them with their OS resolver (systemd-resolved on Linux, /etc/resolver on macOS, NRPT on Windows); only queries under the domain are affected; the machine\'s default DNS and plain hostnames (still resolved via Hosts first) are untouched. Double-click a domain, servers, or state to edit.');
  secHint(c, 'Domains never registered with this node\'s resolver, even if a peer advertises a forward for them: a local-only filter, the DNS analog of rejecting a hosts record. Double-click state to toggle; + to add, tick rows and \u2212 to remove.');
  secHint(c, 'Each domain above also acts as a search suffix: an unqualified query like "grafana" is retried as "grafana.corp.internal". Linux and Windows only. Windows uses just the first domain (one suffix per adapter); macOS/FreeBSD have no per-interface equivalent.');
  for (const cf of cfgs){
    const card = $('<div class="card"></div>');
    card.appendChild($('<h3><span class="net-name">'+esc(cf.name)+'</span> <span class="net-id">'+esc(cf.id)+'</span></h3>'));

    // --- Advertise sub-card ---
    const asub = $('<div class="subcard"></div>');
    asub.appendChild($('<h4>Advertise</h4>'));
    const fwds = cf.dns_advertise||[];
    let h = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>state</th><th>domain</th><th>servers</th></tr>';
    if (!fwds.length) h += '<tr><td colspan="4" class="empty">no advertised forwards — click + to add one</td></tr>';
    else for (const f of fwds){
      const enabled = !f.disabled;
      const servers = (f.servers||[]).join(', ');
      const stTag = '<span class="tag-toggle '+(enabled?'on':'off')+'" data-dnsstate="1" title="double-click to '+(enabled?'disable':'enable')+'">'+(enabled?'enabled':'disabled')+'</span>';
      h += '<tr'+(enabled?'':' class="fw-disabled"')+' data-domain="'+esc(f.domain)+'" data-servers="'+esc(servers)+'" data-enabled="'+(enabled?1:0)+'">'
        + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
        + '<td class="ho-state">'+stTag+'</td>'
        + '<td class="dn-domain-cell">'+esc(f.domain)+'</td><td class="dn-servers-cell">'+esc(servers)+'</td></tr>';
    }
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; asub.appendChild(t);
    const table = t.querySelector('table'); selAllWire(t);
    // Double-click the state tag to toggle the record enabled/disabled — the
    // change applies live via the reload, no restart.
    t.querySelectorAll('[data-dnsstate]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        toggleTagState(tag, '/api/dns', on => ({op:(on?'enable':'disable'),net:cf.name,domain:tag.closest('tr').dataset.domain}));
      };
    });
    // Double-click a domain or servers cell to edit it in place, same pattern
    // as the hosts name/ip cells: update preserves position and enabled state,
    // and editing one field passes the current value of the other through.
    t.querySelectorAll('tr[data-domain]').forEach(tr => {
      const domTd = tr.querySelector('.dn-domain-cell');
      const srvTd = tr.querySelector('.dn-servers-cell');
      if (domTd) domTd.title = 'double-click to edit';
      if (srvTd) srvTd.title = 'double-click to edit';
      if (domTd) domTd.ondblclick = () => inlineCellEdit(domTd, tr.dataset.domain, 'corp.internal', async (v) => {
        v = v.trim();
        if (!v){ alert('domain cannot be empty'); renderSection(); return; }
        if (v === tr.dataset.domain){ renderSection(); return; }
        const r = await api('/api/dns',{method:'POST',body:JSON.stringify({op:'update',net:cf.name,domain:tr.dataset.domain,newdomain:v,servers:tr.dataset.servers})});
        if (!r.ok){ alert((r.body&&r.body.error)||'rename failed'); }
        refresh();
      });
      if (srvTd) srvTd.ondblclick = () => inlineCellEdit(srvTd, tr.dataset.servers, '1.1.1.1, 2.2.2.2', async (v) => {
        v = v.trim();
        if (!v){ alert('at least one server is required'); renderSection(); return; }
        if (v === tr.dataset.servers){ renderSection(); return; }
        const r = await api('/api/dns',{method:'POST',body:JSON.stringify({op:'update',net:cf.name,domain:tr.dataset.domain,newdomain:tr.dataset.domain,servers:v})});
        if (!r.ok){ alert((r.body&&r.body.error)||'update failed'); }
        refresh();
      });
    });
    table._rowAdd = () => dnsAddRow(table, cf.name);
    table._rowRemove = () => removeCheckedRows(table, tr => api('/api/dns',{method:'POST',body:JSON.stringify({op:'remove',net:cf.name,domain:tr.dataset.domain})}), false);
    card.appendChild(asub);

    // --- Reject sub-card ---
    const xsub = $('<div class="subcard"></div>');
    xsub.appendChild($('<h4>Reject</h4>'));
    const rejs = cf.dns_reject||[];
    let rh = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>state</th><th>domain</th></tr>';
    if (!rejs.length) rh += '<tr><td colspan="3" class="empty">no rejected domains — click + to add one</td></tr>';
    else for (const r of rejs){
      const enabled = !r.disabled;
      const stTag = '<span class="tag-toggle '+(enabled?'on':'off')+'" data-dnsrejstate="1" title="double-click to '+(enabled?'disable':'enable')+'">'+(enabled?'enabled':'disabled')+'</span>';
      rh += '<tr'+(enabled?'':' class="fw-disabled"')+' data-rejdomain="'+esc(r.domain)+'" data-enabled="'+(enabled?1:0)+'">'
        + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
        + '<td class="ho-state">'+stTag+'</td>'
        + '<td>'+esc(r.domain)+'</td></tr>';
    }
    const rt = $('<div></div>'); rt.innerHTML = rh+'</table>'; xsub.appendChild(rt);
    const rtable = rt.querySelector('table'); selAllWire(rt);
    rt.querySelectorAll('[data-dnsrejstate]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        toggleTagState(tag, '/api/dns', on => ({op:(on?'reject-enable':'reject-disable'),net:cf.name,domain:tag.closest('tr').dataset.rejdomain}));
      };
    });
    rtable._rowAdd = () => dnsRejectAddRow(rtable, cf.name);
    rtable._rowRemove = () => removeCheckedRows(rtable, tr => api('/api/dns',{method:'POST',body:JSON.stringify({op:'reject-remove',net:cf.name,domain:tr.dataset.rejdomain})}), false);
    card.appendChild(xsub);

    c.appendChild(card);
  }
}

function dnsAddRow(table, net){
  const tr = document.createElement('tr');
  tr.innerHTML = '<td class="selcol"></td>'
    + '<td class="ho-state"><span class="on">enabled</span></td>'
    + '<td><input class="dn-domain" placeholder="corp.internal" style="width:160px"></td>'
    + '<td><input class="dn-servers" placeholder="1.1.1.1, 2.2.2.2" style="width:180px"> <button class="sm dn-save">save</button> <button class="ghost sm dn-cancel">cancel</button></td>';
  if(!insertNewRow(table, tr)) return;
  tr.querySelector('.dn-cancel').onclick = () => refresh();
  tr.querySelector('.dn-save').onclick = () => {
    const domain = tr.querySelector('.dn-domain').value.trim();
    const servers = tr.querySelector('.dn-servers').value.trim();
    if(!domain || !servers){ alert('domain and servers required'); return; }
    edit('/api/dns', { op:'add', net:net, domain:domain, servers:servers }, false);
  };
}

function dnsRejectAddRow(table, net){
  const tr = document.createElement('tr');
  tr.innerHTML = '<td class="selcol"></td>'
    + '<td class="ho-state"><span class="on">enabled</span></td>'
    + '<td><input class="dr-domain" placeholder="blocked.internal" style="width:200px"> <button class="sm dr-save">save</button> <button class="ghost sm dr-cancel">cancel</button></td>';
  if(!insertNewRow(table, tr)) return;
  tr.querySelector('.dr-cancel').onclick = () => refresh();
  tr.querySelector('.dr-save').onclick = () => {
    const domain = tr.querySelector('.dr-domain').value.trim();
    if(!domain){ alert('domain required'); return; }
    edit('/api/dns', { op:'reject', net:net, domain:domain }, false);
  };
}

// ---- selectable editable tables (shared by firewall / nat / qos / routes) ----
// Each data row carries a leading .selcol cell with a .selbox checkbox; the
// header has a .selall. enhanceTable renders + / - next to the filter when a
// table sets _rowAdd / _rowRemove: + inserts a blank editable row, - removes the
// ticked rows.
function selAllWire(t){ const a=t.querySelector('.selall'); if(a) a.onclick=()=>t.querySelectorAll('.selbox').forEach(x=>{ x.checked=a.checked; }); }
function selCheckedRows(table){ return [...table.querySelectorAll('.selbox')].filter(x=>x.checked).map(x=>x.closest('tr')); }
async function removeCheckedRows(table, delFn, autoRestart){
  const sel=selCheckedRows(table);
  if(!sel.length){ alert('tick one or more rows to remove'); return; }
  for(const tr of sel){ const r=await delFn(tr); if(r&&!r.ok){ alert((r.body&&r.body.error)||'remove failed'); break; } }
  if(autoRestart) doRestart(); else refresh();
}
function insertNewRow(table, tr){
  if(table.querySelector('.newrow')) return false; // one blank row at a time
  tr.classList.add('newrow');
  const tb=table.tBodies[0]||table, hdr=table.rows[0];
  if(hdr&&hdr.nextSibling) tb.insertBefore(tr,hdr.nextSibling); else tb.appendChild(tr);
  const ph=table.querySelector('td.empty'); if(ph) ph.parentNode.remove();
  return true;
}

function fwActOpts(sel){ return ['allow','deny'].map(a => '<option value="'+a+'"'+(a===sel?' selected':'')+'>'+a+'</option>').join(''); }

// fwSvcLabel renders a rule's inline proto/port leg (if any) plus its named
// services into one combined display string, e.g. "tcp/443, https, dns" —
// used for the read-only table cell and to prefill the editor's single
// combined field. Mirrors resolveLegs/compileRule's server-side union of an
// inline leg with every named service (internal/mesh/firewall.go): the UI
// was showing that union as three separate widgets, this shows — and edits
// — it as the one thing it actually is.
function fwSvcLabel(f){
  const parts = [];
  if (f.proto || f.dport_min || f.dport_max) {
    const pl = portLabel(f.dport_min, f.dport_max);
    parts.push((f.proto||'any') + (pl==='any' ? '' : '/'+pl));
  }
  for (const s of (f.services||[])) parts.push(s);
  return parts.join(', ');
}

// fwParseSvc parses the combined services field back into an inline
// proto/port leg plus a list of named-service references. Each comma-
// separated token is either "proto", "proto/port", or bare "any" (all
// three treated as the inline leg — at most one of these per rule, since
// FirewallRule has exactly one inline proto/port slot), or otherwise taken
// as a named service and left for the server to resolve/validate against
// the catalog (same as before — this never checked service names against
// the datalist client-side, the datalist is a suggestion, not a filter).
function fwParseSvc(raw){
  const toks = (raw||'').split(',').map(t=>t.trim()).filter(Boolean);
  let proto = '', port = 0, sawRaw = false;
  const services = [];
  const legRe = /^(any|tcp|udp|icmp)(?:\/(\d+))?$/i;
  for (const t of toks){
    const m = legRe.exec(t);
    if (!m){ services.push(t); continue; }
    const p = m[1].toLowerCase();
    let portNum = 0;
    if (m[2]){
      portNum = Number(m[2]);
      if (!Number.isInteger(portNum) || portNum < 1 || portNum > 65535) return {error: '"'+t+'": port must be 1-65535'};
    }
    if (p === 'any' && !portNum) continue; // explicit "any" with no port is a no-op — same as omitting it
    if (sawRaw) return {error: 'only one raw proto/port entry is allowed per rule (e.g. "tcp/443"); for more than one, add them to a named service instead'};
    sawRaw = true;
    proto = (p === 'any') ? '' : p;
    port = portNum;
  }
  return {proto, port, services};
}

// fwAutoPopulateCatalog keeps the node-global object and service catalog
// (shared by every network — see Config.FirewallObjects' doc comment)
// stocked with the full well-known set from FW_COMMON_WILDCARD_OBJECTS/
// FW_COMMON_SERVICES — automatically, once, ever, with no button and no
// click. Runs at most once per catalog: after a successful pass it marks
// the catalog seeded (op:mark-objects-seeded / op:mark-services-seeded),
// and every render after that just sees the seeded flag already set and
// does nothing — so deleting a well-known entry afterward sticks; nothing
// ever re-adds it. fwAutoAddBusy guards against overlapping runs when
// secFirewall renders again (e.g. switching tabs) before a prior pass has
// finished.
var fwAutoAddBusy = false;
async function fwAutoPopulateCatalog(){
  if (fwAutoAddBusy) return;
  let did = false;
  fwAutoAddBusy = true;
  try {
    if (!state.fwObjectsSeeded){
      const objs = state.fwObjects || [];
      const have = {}; objs.forEach(o=>{ have[(o.name||'').toLowerCase()]=true; });
      const missing = FW_COMMON_WILDCARD_OBJECTS.filter(d=>!have[d.name.toLowerCase()]);
      let ok = true;
      if (missing.length){
        const additions = missing.map(def => ({ name: def.name, kind: 'fqdn', addresses: def.addresses.slice(), members: [], notes: def.notes||'' }));
        const r = await api('/api/firewall',{method:'POST',body:JSON.stringify({op:'objects', objects:objPayload(objs.concat(additions))})});
        ok = r.ok;
      }
      if (ok){
        const mr = await api('/api/firewall',{method:'POST',body:JSON.stringify({op:'mark-objects-seeded'})});
        if (mr.ok) did = true;
      }
    }
    if (!state.fwServicesSeeded){
      const svcs = state.fwServices || [];
      const have = {}; svcs.forEach(s=>{ have[(s.name||'').toLowerCase()]=true; });
      const missing = FW_COMMON_SERVICES.filter(d=>!have[d.name.toLowerCase()]);
      let ok = true;
      if (missing.length){
        const additions = missing.map(def => ({ name: def.name, ports: def.ports.map(p=>({proto:p.proto, port_min:p.port_min, port_max:p.port_max})), notes: def.notes||'' }));
        const r = await api('/api/firewall',{method:'POST',body:JSON.stringify({op:'services', services:svcPayload(svcs.concat(additions))})});
        ok = r.ok;
      }
      if (ok){
        const mr = await api('/api/firewall',{method:'POST',body:JSON.stringify({op:'mark-services-seeded'})});
        if (mr.ok) did = true;
      }
    }
  } finally {
    fwAutoAddBusy = false;
  }
  if (did) await refresh();
}

function secFirewall(c) {
  state.firewallTab = state.firewallTab || 'rules';
  c.appendChild(buildTabBar([['rules','Rules'],['objects','Objects'],['services','Services'],['allowlist','Allow List']], state.firewallTab,
    (tab) => { state.firewallTab = tab; renderSection(); }));

  // The node-global object/service catalog should already contain the full
  // well-known set — no button, no double-click — from the first time
  // anyone looks at a firewall tab, ever, on this node. fwAutoPopulateCatalog
  // checks the seeded flags and, the first time only, saves any gaps in and
  // marks itself done; not awaited here since secFirewall itself is a
  // synchronous render, same pattern as any other background-then-refresh
  // update in this file.
  fwAutoPopulateCatalog();

  // Allow List is node-global, not per-network — unlike Rules below, it has
  // something to show (and something useful to do) even with zero networks
  // configured, so it's handled before the no-networks early-out.
  if (state.firewallTab === 'allowlist') { secAlwaysAllowed(c); return; }
  if (state.firewallTab === 'objects') { secFwObjects(c); return; }
  if (state.firewallTab === 'services') { secFwServices(c); return; }

  if (!state.cfg.length) return emptyCard(c, 'No networks.');
  secHint(c, 'Disabled = all traffic passes. Enabled = rules evaluated top-to-bottom, first match wins; unmatched traffic is allowed (stateful — replies to allowed flows pass automatically). <b>services</b> takes a comma-separated mix of named services and raw <code>proto</code>/<code>proto/port</code> entries (e.g. <code>https, tcp/8443, udp/53</code>); at most one raw entry per rule, any number of named services. Each of <b>source</b>, <b>destination</b>, and <b>services</b> has a <b>\u00d8</b> button inside its editor; click it to match anything <i>except</i> that field (shown as a leading <b>!</b> here), and next to it, a filterable dropdown of this node\u2019s objects/services catalog: start typing to narrow it, or just type a literal CIDR/proto/port instead, since these fields take either. Use + to add a rule, drag to reorder, double-click to edit or toggle state, tick rows and use \u2212 to remove.');

  for (const cf of state.cfg) {
    const fw = cf.firewall||{}; const en = !!fw.enabled;
    const live = state.status.find(s => s.id===cf.id) || {};
    const rules = live.firewall || fw.rules || [];
    const card = $('<div class="card"></div>');
    card.appendChild(netCardHead(cf, en, '/api/firewall'));

    let h = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>state</th><th>source</th><th>destination</th><th>services</th><th>action</th><th>log</th><th>hits</th><th>notes</th></tr>';
    if (!rules.length) h += '<tr><td colspan="9" class="empty">no rules — all traffic allowed (click + to add one)</td></tr>';
    else for (let i=0;i<rules.length;i++) { const f=rules[i];
      const enabled = !f.disabled;
      const svcTxt = fwSvcLabel(f);
      const stTag = '<span class="tag-toggle '+(enabled?'on':'off')+'" data-fwstate="'+esc(String(f.id))+'" title="double-click to '+(enabled?'disable':'enable')+'">'+(enabled?'enabled':'disabled')+'</span>';
      h += '<tr draggable="true" class="fwrow'+(enabled?'':' fw-disabled')+'" data-fwid="'+esc(f.id)+'" data-idx="'+i+'" data-enabled="'+(enabled?1:0)+'" data-action="'+esc(f.action)+'" data-services="'+esc(svcTxt)+'" data-src="'+esc(f.src||'')+'" data-dst="'+esc(f.dst||'')+'" data-src-negate="'+(f.src_negate?1:0)+'" data-dst-negate="'+(f.dst_negate?1:0)+'" data-services-negate="'+(f.services_negate?1:0)+'" data-log="'+(f.log?1:0)+'" data-notes="'+esc(f.notes||'')+'">'
        + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
        + '<td class="fw-state">'+stTag+'</td>'
        + '<td class="fw-src" title="'+(f.src_negate?'anything EXCEPT this':'')+'">'+(f.src_negate?'<b>!</b>':'')+esc(f.src||'any')+'</td>'
        + '<td class="fw-dst" title="'+(f.dst_negate?'anything EXCEPT this':'')+'">'+(f.dst_negate?'<b>!</b>':'')+esc(f.dst||'any')+'</td>'
        + '<td class="fw-services" title="'+(f.services_negate?'any service EXCEPT this':'')+'">'+(f.services_negate?'<b>!</b>':'')+esc(svcTxt||'any')+'</td>'
        + '<td class="fw-action">'+esc(f.action)+'</td>'
        + '<td class="fw-log">'+(f.log?'<span class="on" title="matches are logged">log</span>':'')+'</td>'
        + '<td class="fw-hits" title="'+fmtHits(f)+'">'+esc(String(f.packets||0))+'</td>'
        + '<td class="fw-notes">'+esc(f.notes||'')+'</td></tr>';
    }
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; card.appendChild(t);
    const table = t.querySelector('table');
    t.querySelectorAll('tr.fwrow').forEach(tr => {
      tr.ondblclick = (e) => {
        if (e.target.closest('.fw-state')) return; // state cell has its own click handler
        startFwEdit(tr, cf.id);
      };
      tr.addEventListener('dragstart', e => { e.dataTransfer.setData('text/plain', tr.dataset.fwid); e.dataTransfer.effectAllowed='move'; tr.classList.add('dragging'); });
      tr.addEventListener('dragend', () => { tr.classList.remove('dragging'); t.querySelectorAll('.drop-target').forEach(x=>x.classList.remove('drop-target')); });
      tr.addEventListener('dragover', e => { e.preventDefault(); e.dataTransfer.dropEffect='move'; tr.classList.add('drop-target'); });
      tr.addEventListener('dragleave', () => tr.classList.remove('drop-target'));
      tr.addEventListener('drop', async e => {
        e.preventDefault(); tr.classList.remove('drop-target');
        const draggedId = Number(e.dataTransfer.getData('text/plain'));
        const toIdx = Number(tr.dataset.idx);
        if (!draggedId || Number.isNaN(toIdx) || draggedId===Number(tr.dataset.fwid)) return;
        const r = await api('/api/firewall',{method:'POST',body:JSON.stringify({net:cf.id,op:'move',ids:[draggedId],to:toIdx})});
        if (!r.ok) { alert((r.body&&r.body.error)||'reorder failed'); return; }
        await refresh();
      });
    });
    // Double-click the state tag to toggle the rule enabled/disabled — matching
    // the change-state gesture used across the UI. mutateConfig already calls
    // s.reload() so the change is live immediately — no restart. The busy flag
    // prevents a re-entrant call before the first one returns.
    t.querySelectorAll('[data-fwstate]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        toggleTagState(tag, '/api/firewall', on => ({net:cf.id,op:(on?'rule-enable':'rule-disable'),ids:[Number(tag.closest('tr').dataset.fwid)]}));
      };
    });
    selAllWire(t);
    table._rowAdd = () => fwAddRow(table, cf.id);
    table._rowRemove = async () => {
      const sel=selCheckedRows(table);
      if(!sel.length){ alert('tick one or more rows to remove'); return; }
      for(const tr of sel){ const r=await api('/api/firewall',{method:'POST',body:JSON.stringify({net:cf.id,op:'del',ids:[Number(tr.dataset.fwid)]})}); if(r&&!r.ok){ alert((r.body&&r.body.error)||'remove failed'); return; } }
      await refresh();
    };
    // Reset-counters control: zeroes every rule's hit tally on this network.
    if (rules.length){
      const bar = $('<div style="margin-top:8px"></div>');
      const rc = $('<button class="ghost sm">reset counters</button>');
      rc.onclick = async () => {
        const r = await api('/api/firewall',{method:'POST',body:JSON.stringify({net:cf.id,op:'reset-counters',ids:[]})});
        if(!r.ok){ alert((r.body&&r.body.error)||'reset failed'); return; }
        await refresh();
      };
      bar.appendChild(rc); card.appendChild(bar);
    }
    c.appendChild(card);
  }
}

// fmtHits renders a rule's byte tally for the hits-cell tooltip.
function fmtHits(f){
  const b = f.bytes||0;
  return (f.packets||0)+' packets, '+b+' bytes';
}

// fwNegToggle renders the small "Ø" toggle button that sits inside its
// paired input (see .fwe-field below) — click to flip; the .active class is
// both the visual state and what fwCollectRule reads back out, no hidden
// checkbox involved.
function fwNegToggle(cls, title){ return '<button type="button" class="fwe-neg '+cls+'" aria-pressed="false" title="'+esc(title)+'">\u00d8</button>'; }

// fwWireNegToggles finds every .fwe-neg button within scope and gives it its
// click behavior — toggle .active, keep aria-pressed in sync. Called once
// after each row's editor markup (add or edit) is in the DOM.
function fwWireNegToggles(scope){
  scope.querySelectorAll('.fwe-neg').forEach(btn => {
    btn.onclick = () => {
      const on = btn.classList.toggle('active');
      btn.setAttribute('aria-pressed', String(on));
    };
  });
}

// fwCatalogCombobox turns a plain text input (a rule editor's src/dst/
// services field) into a filterable combobox against this node's real
// object/service catalog, reusing the .ss-list/.ss-opt styling built for
// buildListPicker (see its comment) rather than inventing new dropdown
// styling. Narrows as you type — case-insensitive substring match against
// whatever getNames() returns at the moment the list opens or the input
// changes, so an edit made on the Objects/Services tab in the meantime is
// picked up without needing this row re-rendered. Unlike buildListPicker
// this stays a genuine free-typing input throughout: a literal CIDR or
// proto/port is a perfectly valid value and isn't in any catalog, so
// there's no separate filter row and no button standing in for the input
// — the input itself is both the value and the filter. Relies on the
// input already sitting inside a '.fwe-field' (position:relative; see its
// CSS) so the dropdown positions directly below it with no coordinate math.
function fwCatalogCombobox(input, getNames){
  const field = input.closest('.fwe-field') || input.parentElement;
  const list = $('<div class="ss-list" role="listbox"></div>');
  field.appendChild(list);
  let shown = [], selIdx = -1;

  const markSel = () => list.querySelectorAll('.ss-opt').forEach((o,i) => o.classList.toggle('sel', i===selIdx));
  const render = () => {
    const q = input.value.trim().toLowerCase();
    const all = getNames() || [];
    shown = q ? all.filter(n => n.toLowerCase().indexOf(q) >= 0) : all;
    list.innerHTML = '';
    selIdx = -1;
    if (!shown.length){ list.classList.remove('show'); return; }
    shown.slice(0, 200).forEach((name, i) => {
      const o = $('<div class="ss-opt" role="option"></div>');
      o.textContent = name;
      o.onmouseenter = () => { selIdx = i; markSel(); };
      // mousedown, not click: fires before the blur that closes the list
      // would otherwise swallow it (same reason as the global search box).
      o.onmousedown = (e) => { e.preventDefault(); pick(name); };
      list.appendChild(o);
    });
    list.classList.add('show');
  };
  const pick = (name) => {
    input.value = name;
    list.classList.remove('show');
    input.focus();
  };
  const close = () => { list.classList.remove('show'); selIdx = -1; };

  input.addEventListener('input', render);
  input.addEventListener('focus', render);
  input.addEventListener('blur', () => setTimeout(close, 150));
  input.addEventListener('keydown', (e) => {
    if (!list.classList.contains('show')){
      if (e.key === 'ArrowDown') render();
      return;
    }
    if (e.key === 'ArrowDown'){ e.preventDefault(); selIdx = Math.min(selIdx+1, shown.length-1); markSel(); list.querySelector('.ss-opt.sel')?.scrollIntoView({block:'nearest'}); }
    else if (e.key === 'ArrowUp'){ e.preventDefault(); selIdx = Math.max(selIdx-1, 0); markSel(); list.querySelector('.ss-opt.sel')?.scrollIntoView({block:'nearest'}); }
    else if (e.key === 'Enter'){ if (selIdx>=0 && shown[selIdx]){ e.preventDefault(); pick(shown[selIdx]); } }
    else if (e.key === 'Escape'){ close(); }
  });
}

function fwAddRow(table, net){
  const tr = document.createElement('tr');
  tr.innerHTML = '<td class="selcol"></td>'
    + '<td class="fw-state"><span class="on">enabled</span></td>'
    + '<td><span class="fwe-field"><input class="fwe-src" placeholder="cidr / object" style="width:150px">'+fwNegToggle('fwe-src-negate','match anything EXCEPT this')+'</span></td>'
    + '<td><span class="fwe-field"><input class="fwe-dst" placeholder="cidr / object" style="width:150px">'+fwNegToggle('fwe-dst-negate','match anything EXCEPT this')+'</span></td>'
    + '<td><span class="fwe-field"><input class="fwe-services" placeholder="tcp/443, https" style="width:180px">'+fwNegToggle('fwe-services-negate','match any service EXCEPT this')+'</span></td>'
    + '<td><select class="fwe-action">'+fwActOpts('allow')+'</select></td>'
    + '<td><label class="fwe-log-l" title="log matches"><input type="checkbox" class="fwe-log"> log</label></td>'
    + '<td></td>'
    + '<td><input class="fwe-notes" placeholder="notes" style="width:140px"> <button class="sm fwe-save">save</button> <button class="ghost sm fwe-cancel">cancel</button></td>';
  if (!insertNewRow(table, tr)) return;
  fwWireNegToggles(tr);
  fwCatalogCombobox(tr.querySelector('.fwe-src'), () => (state.fwObjects||[]).map(o=>o.name));
  fwCatalogCombobox(tr.querySelector('.fwe-dst'), () => (state.fwObjects||[]).map(o=>o.name));
  fwCatalogCombobox(tr.querySelector('.fwe-services'), () => (state.fwServices||[]).map(s=>s.name));
  tr.querySelector('.fwe-cancel').onclick = () => refresh();
  tr.querySelector('.fwe-save').onclick = async () => {
    const rule = fwCollectRule(tr); if (!rule) return;
    if (!fwValidateNegate(rule)) return;
    const r = await api('/api/firewall',{method:'POST',body:JSON.stringify({net:net,op:'add',at:-1,rule})});
    if (!r.ok){ alert((r.body && r.body.error) || 'failed'); return; }
    await refresh();
  };
}

// fwCollectRule reads the editor widgets within scope (a row being added or
// edited) into a rule object: action, the combined services field — parsed
// by fwParseSvc into an inline proto/port leg plus named services — each
// dimension's NOT toggle (the .fwe-neg button's .active class), the log
// flag, and notes. Returns null (after alerting) if the services field
// couldn't be parsed, same "bail out before saving" pattern the old
// standalone port field used.
function fwCollectRule(scope){
  const q = s => scope.querySelector(s);
  const parsed = fwParseSvc(q('.fwe-services').value);
  if (parsed.error){ alert(parsed.error); return null; }
  return {
    action: q('.fwe-action').value,
    proto: parsed.proto,
    dport_min: parsed.port || 0,
    dport_max: parsed.port || 0,
    src: q('.fwe-src').value.trim(),
    src_negate: q('.fwe-src-negate').classList.contains('active'),
    dst: q('.fwe-dst').value.trim(),
    dst_negate: q('.fwe-dst-negate').classList.contains('active'),
    services: parsed.services,
    services_negate: q('.fwe-services-negate').classList.contains('active'),
    log: q('.fwe-log').checked,
    notes: q('.fwe-notes').value.trim(),
  };
}

// fwValidateNegate warns (and refuses to save) when a dimension's Ø toggle
// is on but that dimension is otherwise empty/any. The engine accepts this
// as written — negating "any" just means "match nothing" (see
// mesh.FirewallRule's doc comment) — but reaching that state from an empty
// field plus a stray click is almost always a mistake, not something
// anyone meant on purpose, so it's caught here before it's saved rather
// than silently producing a rule that can never match.
function fwValidateNegate(rule){
  if (rule.src_negate && !rule.src){ alert('src \u00d8 is on but src is empty (any): that would match nothing; set src or turn its \u00d8 off'); return false; }
  if (rule.dst_negate && !rule.dst){ alert('dst \u00d8 is on but dst is empty (any): that would match nothing; set dst or turn its \u00d8 off'); return false; }
  const noLeg = !rule.proto && !rule.dport_min && !rule.services.length;
  if (rule.services_negate && noLeg){ alert('services \u00d8 is on but no proto/port/service is set (any): that would match nothing; set one or turn its \u00d8 off'); return false; }
  return true;
}

// startFwEdit turns a firewall rule row into inline editors; saving deletes the
// old rule and re-adds the edited one at the same position (order is preserved).
function startFwEdit(tr, net){
  if (tr.querySelector('.fwe-action')) return; // already editing
  tr.draggable = false;
  const oldId = Number(tr.dataset.fwid), oldIdx = Number(tr.dataset.idx);
  const a=tr.querySelector('.fw-action'),
        sv=tr.querySelector('.fw-services'), s=tr.querySelector('.fw-src'), d=tr.querySelector('.fw-dst'),
        lg=tr.querySelector('.fw-log'), no=tr.querySelector('.fw-notes');
  a.innerHTML = '<select class="fwe-action">'+fwActOpts(tr.dataset.action)+'</select>';
  sv.innerHTML = '<span class="fwe-field"><input class="fwe-services" style="width:180px" value="'+esc(tr.dataset.services||'')+'">'+fwNegToggle('fwe-services-negate','match any service EXCEPT this')+'</span>';
  s.innerHTML = '<span class="fwe-field"><input class="fwe-src" style="width:150px" value="'+esc(tr.dataset.src||'')+'">'+fwNegToggle('fwe-src-negate','match anything EXCEPT this')+'</span>';
  d.innerHTML = '<span class="fwe-field"><input class="fwe-dst" style="width:150px" value="'+esc(tr.dataset.dst||'')+'">'+fwNegToggle('fwe-dst-negate','match anything EXCEPT this')+'</span>';
  lg.innerHTML = '<label class="fwe-log-l" title="log matches"><input type="checkbox" class="fwe-log"'+(tr.dataset.log==='1'?' checked':'')+'> log</label>';
  no.innerHTML = '<input class="fwe-notes" style="width:140px" value="'+esc(tr.dataset.notes||'')+'"> <button class="sm fwe-save">save</button>';
  fwWireNegToggles(tr);
  fwCatalogCombobox(s.querySelector('.fwe-src'), () => (state.fwObjects||[]).map(o=>o.name));
  fwCatalogCombobox(d.querySelector('.fwe-dst'), () => (state.fwObjects||[]).map(o=>o.name));
  fwCatalogCombobox(sv.querySelector('.fwe-services'), () => (state.fwServices||[]).map(x=>x.name));
  if (tr.dataset.srcNegate==='1'){ const b=s.querySelector('.fwe-src-negate'); b.classList.add('active'); b.setAttribute('aria-pressed','true'); }
  if (tr.dataset.dstNegate==='1'){ const b=d.querySelector('.fwe-dst-negate'); b.classList.add('active'); b.setAttribute('aria-pressed','true'); }
  if (tr.dataset.servicesNegate==='1'){ const b=sv.querySelector('.fwe-services-negate'); b.classList.add('active'); b.setAttribute('aria-pressed','true'); }
  tr.ondblclick = null;
  no.querySelector('.fwe-save').onclick = async () => {
    const rule = fwCollectRule(tr); if (!rule) return;
    if (!fwValidateNegate(rule)) return;
    const dr = await api('/api/firewall',{method:'POST',body:JSON.stringify({net:net,op:'del',ids:[oldId]})});
    if (!dr.ok){ alert((dr.body&&dr.body.error)||'edit failed'); refresh(); return; }
    const ar = await api('/api/firewall',{method:'POST',body:JSON.stringify({net:net,op:'add',at:oldIdx,rule:rule})});
    if (!ar.ok) { alert((ar.body&&ar.body.error)||'edit failed'); refresh(); return; }
    await refresh();
  };
}

// secFwObjects renders the node-global address-object catalog: reusable
// named address sets a rule on any network references by name in src/dst
// (see Config.FirewallObjects' doc comment — one catalog, shared by every
// network on this node, not one per network, so this is one table, not one
// per network either). Whole-list save, applied live via /api/firewall
// op:objects (no restart).
function secFwObjects(c){
  secHint(c, 'Reusable address objects a rule can name in its <b>src</b>/<b>dst</b>; shared by every network on this node, edited once and usable everywhere. <b>kind</b>: host (literal IPs), subnet (CIDRs), range (a\u2011b), fqdn (domain names, re\u2011resolved live; an entry can be a literal name or a <b>*.domain.tld</b> wildcard covering every subdomain, learned passively from real DNS traffic; see docs), or group (a bundle of other objects, by name). Edit an object once and every rule that names it, on any network, follows. Double\u2011click a cell to edit; + adds a row, tick rows and \u2212 removes. Every well\u2011known domain gravinet knows about is already a real row here; nothing to add, nothing to click.');
  const objs = (state.fwObjects || []).map(cloneObj);
  const card = $('<div class="card"></div>');
  const t = $('<div></div>');
  let h = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>name</th><th>kind</th><th>addresses / members</th><th>notes</th></tr>';
  objs.forEach((o,i)=>{ h += '<tr data-idx="'+i+'">'
    + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
    + '<td class="ob-name">'+esc(o.name||'')+'</td>'
    + '<td class="ob-kind">'+esc(o.kind||'')+'</td>'
    + '<td class="ob-val">'+esc(objValStr(o))+'</td>'
    + '<td class="ob-notes">'+esc(o.notes||'')+'</td></tr>'; });
  if (!objs.length) h += '<tr><td colspan="5" class="empty">no objects; click + to add one</td></tr>';
  t.innerHTML = h+'</table>';
  const table = t.querySelector('table'); card.appendChild(t);
  table._objs = objs;
  if (objs.length) t.querySelectorAll('tr[data-idx]').forEach(tr=>{
    const i = Number(tr.dataset.idx);
    const nameTd=tr.querySelector('.ob-name'), kindTd=tr.querySelector('.ob-kind'), valTd=tr.querySelector('.ob-val'), notesTd=tr.querySelector('.ob-notes');
    nameTd.title=kindTd.title=valTd.title=notesTd.title='double-click to edit';
    nameTd.ondblclick=()=>inlineCellEdit(nameTd,objs[i].name||'','name',v=>{ v=v.trim(); if(!v){alert('name required');renderSection();return;} objs[i].name=v; objSave(objs); });
    kindTd.ondblclick=()=>inlineCellEdit(kindTd,objs[i].kind||'','host|subnet|range|fqdn|group',v=>{ v=v.trim().toLowerCase(); if(['host','subnet','range','fqdn','group'].indexOf(v)<0){alert('kind must be host, subnet, range, fqdn, or group');renderSection();return;} objs[i].kind=v; objSave(objs); });
    valTd.ondblclick=()=>inlineCellEdit(valTd,objValStr(objs[i]), objs[i].kind==='group'?'member object names, comma-separated':'addresses / CIDRs / ranges / domains, comma-separated', v=>{ setObjVal(objs[i], v); objSave(objs); });
    notesTd.ondblclick=()=>inlineCellEdit(notesTd,objs[i].notes||'','notes',v=>{ objs[i].notes=v.trim(); objSave(objs); });
  });
  selAllWire(t);
  table._rowAdd = ()=>objAddRow(table);
  table._rowRemove = ()=>{ const sel=selCheckedRows(table); if(!sel.length){alert('tick one or more rows to remove');return;} const keep=objs.filter((_,i)=>!sel.some(tr=>Number(tr.dataset.idx)===i)); objSave(keep); };
  c.appendChild(card);
}
function cloneObj(o){ return { name:o.name||'', kind:o.kind||'host', addresses:(o.addresses||[]).slice(), members:(o.members||[]).slice(), notes:o.notes||'' }; }
function objValStr(o){ return ((o.kind==='group')?(o.members||[]):(o.addresses||[])).join(', '); }
function setObjVal(o, v){ const parts=v.split(',').map(x=>x.trim()).filter(Boolean); if(o.kind==='group'){ o.members=parts; o.addresses=[]; } else { o.addresses=parts; o.members=[]; } }
function objPayload(objs){ return objs.map(o=>({ name:o.name, kind:o.kind, addresses:(o.kind==='group'?[]:(o.addresses||[])), members:(o.kind==='group'?(o.members||[]):[]), notes:o.notes||'' })); }
async function objSave(objs){
  const r = await api('/api/firewall',{method:'POST',body:JSON.stringify({op:'objects', objects:objPayload(objs)})});
  if(!r.ok){ alert((r.body&&r.body.error)||'save failed'); }
  await refresh();
}
function objAddRow(table){
  const tr=document.createElement('tr');
  tr.innerHTML='<td class="selcol"></td>'
    + '<td><input class="oba-name" placeholder="name" style="width:110px"></td>'
    + '<td><select class="oba-kind">'+['host','subnet','range','fqdn','group'].map(k=>'<option>'+k+'</option>').join('')+'</select></td>'
    + '<td><input class="oba-val" placeholder="addresses, comma-separated" style="width:230px"></td>'
    + '<td><input class="oba-notes" placeholder="notes" style="width:100px"> <button class="sm oba-save">save</button> <button class="ghost sm oba-cancel">cancel</button></td>';
  if(!insertNewRow(table, tr)) return;
  const kindSel=tr.querySelector('.oba-kind'), valInp=tr.querySelector('.oba-val');
  kindSel.onchange=()=>{ valInp.placeholder = kindSel.value==='group' ? 'member object names, comma-separated' : 'addresses, comma-separated'; };
  tr.querySelector('.oba-cancel').onclick=()=>renderSection();
  tr.querySelector('.oba-save').onclick=()=>{
    const name=tr.querySelector('.oba-name').value.trim(); if(!name){alert('name required');return;}
    const o={name:name, kind:kindSel.value, addresses:[], members:[], notes:tr.querySelector('.oba-notes').value.trim()};
    setObjVal(o, valInp.value);
    const list=(table._objs||[]).slice(); list.push(o);
    objSave(list);
  };
}

// FW_COMMON_WILDCARD_OBJECTS is a curated catalog of well-known domains,
// ready to add as fqdn-kind address objects — the same convenience
// gravinet's sibling project parapet gives its users by seeding a large
// wildcard-fqdn object list from the Tranco top-sites ranking.
//
// This is deliberately a hand-curated list of major, globally-recognized
// services rather than a mechanical port of Tranco's top-N: gravinet's
// object catalog is part of each network's config, and that config is
// gossiped to every peer on the network on every change (see the mesh
// package's config sync) — a standalone router like parapet pays only its
// own local storage and CPU for a 1,000+ or 10,000+-entry seed list;
// gravinet would pay that cost on every node, on every sync, network-wide.
// A few dozen to a hundred-odd entries for the services people actually
// write rules against most often gets most of the real value without
// turning every config change into a large gossip payload. Nothing stops
// an operator adding their own less-common wildcard fqdn objects by hand
// the same way any other object is added — this catalog is a convenience
// for the common case, not a ceiling.
//
// fwAutoPopulateCatalog (internal/webadmin's admin UI) saves every entry
// here into every configured network's real catalog automatically, no
// action required — so unlike an earlier version of this feature, none of
// this stays opt-in per network anymore; it's all real, always, as soon
// as the admin UI is open. The curation above is what keeps that
// unconditional every-network cost bounded to "a few dozen to a
// hundred-odd" rather than 1,000+ or 10,000+: this list staying small on
// purpose is now the only thing standing between "always populated" and
// "always populated with a genuinely large gossip payload."
//
// Each entry seeds a single wildcard address ("*.google.com"), not the
// bare domain alongside it. fqdnPatternMatch (internal/mesh/
// firewall_dns_sniff.go) matches the bare domain against the wildcard
// pattern on its own now, so a separate literal isn't needed for
// matching — and keeping one anyway was rejected on a different ground:
// every one of these ~150 entries is a template. Whatever shape they're
// seeded in is the shape someone copies when adding their own domain by
// hand, and "google.com, *.google.com" teaches that both are required
// when only the wildcard is.
//
// Trade-off accepted knowingly, not missed: a literal entry gets
// proactively resolved by the periodic resolver in firewall_fqdn.go on a
// schedule, independent of live traffic, whereas a wildcard-only entry's
// address set only grows once the passive DNS sniffer observes real
// traffic naming that domain — so immediately after adding one of these
// objects, the bare domain itself isn't yet enforceable, only its
// subdomains become so as traffic to them is observed (subdomains were
// always passive-only either way; there's no way to enumerate them by
// lookup). If a specific domain needs to be enforceable from the moment
// it's added, add its literal name back into that one object by hand.
function wcoDef(name, cat, notes){ return { name: name, cat: cat, addresses: ['*.'+name], notes: notes||'' }; }
var FW_COMMON_WILDCARD_OBJECTS = [
  // Search & Google
  wcoDef('google.com', 'Search & Google'),
  wcoDef('gmail.com', 'Search & Google'),
  wcoDef('youtube.com', 'Search & Google'),
  wcoDef('googleapis.com', 'Search & Google', 'Google API/service endpoints'),
  wcoDef('googleusercontent.com', 'Search & Google', 'Google-hosted user content/CDN'),
  wcoDef('gstatic.com', 'Search & Google', 'Google static asset CDN'),
  wcoDef('bing.com', 'Search & Google'),
  wcoDef('duckduckgo.com', 'Search & Google'),
  // Social & messaging
  wcoDef('facebook.com', 'Social & messaging'),
  wcoDef('instagram.com', 'Social & messaging'),
  wcoDef('whatsapp.com', 'Social & messaging'),
  wcoDef('x.com', 'Social & messaging', 'formerly Twitter'),
  wcoDef('twitter.com', 'Social & messaging', 'legacy domain, still in active use'),
  wcoDef('linkedin.com', 'Social & messaging'),
  wcoDef('reddit.com', 'Social & messaging'),
  wcoDef('pinterest.com', 'Social & messaging'),
  wcoDef('snapchat.com', 'Social & messaging'),
  wcoDef('tiktok.com', 'Social & messaging'),
  wcoDef('discord.com', 'Social & messaging'),
  wcoDef('telegram.org', 'Social & messaging'),
  wcoDef('slack.com', 'Social & messaging'),
  // Microsoft
  wcoDef('microsoft.com', 'Microsoft'),
  wcoDef('office.com', 'Microsoft'),
  wcoDef('outlook.com', 'Microsoft'),
  wcoDef('live.com', 'Microsoft'),
  wcoDef('msn.com', 'Microsoft'),
  wcoDef('xbox.com', 'Microsoft'),
  wcoDef('azure.com', 'Microsoft'),
  wcoDef('windows.net', 'Microsoft', 'Azure-hosted service endpoints'),
  // Apple
  wcoDef('apple.com', 'Apple'),
  wcoDef('icloud.com', 'Apple'),
  wcoDef('me.com', 'Apple'),
  // Amazon
  wcoDef('amazon.com', 'Amazon'),
  wcoDef('amazonaws.com', 'Amazon', 'AWS service endpoints'),
  wcoDef('awsstatic.com', 'Amazon'),
  // Streaming & entertainment
  wcoDef('netflix.com', 'Streaming & entertainment'),
  wcoDef('spotify.com', 'Streaming & entertainment'),
  wcoDef('hulu.com', 'Streaming & entertainment'),
  wcoDef('disneyplus.com', 'Streaming & entertainment'),
  wcoDef('twitch.tv', 'Streaming & entertainment'),
  wcoDef('vimeo.com', 'Streaming & entertainment'),
  // Dev & cloud platforms
  wcoDef('github.com', 'Dev & cloud platforms'),
  wcoDef('githubusercontent.com', 'Dev & cloud platforms'),
  wcoDef('gitlab.com', 'Dev & cloud platforms'),
  wcoDef('npmjs.com', 'Dev & cloud platforms'),
  wcoDef('docker.com', 'Dev & cloud platforms'),
  wcoDef('cloudflare.com', 'Dev & cloud platforms'),
  wcoDef('digitalocean.com', 'Dev & cloud platforms'),
  wcoDef('herokuapp.com', 'Dev & cloud platforms'),
  wcoDef('vercel.com', 'Dev & cloud platforms'),
  wcoDef('netlify.com', 'Dev & cloud platforms'),
  // Productivity & communication
  wcoDef('zoom.us', 'Productivity & communication'),
  wcoDef('dropbox.com', 'Productivity & communication'),
  wcoDef('box.com', 'Productivity & communication'),
  wcoDef('atlassian.com', 'Productivity & communication'),
  wcoDef('notion.so', 'Productivity & communication'),
  wcoDef('trello.com', 'Productivity & communication'),
  wcoDef('salesforce.com', 'Productivity & communication'),
  wcoDef('zendesk.com', 'Productivity & communication'),
  // Shopping & finance
  wcoDef('ebay.com', 'Shopping & finance'),
  wcoDef('paypal.com', 'Shopping & finance'),
  wcoDef('stripe.com', 'Shopping & finance'),
  wcoDef('shopify.com', 'Shopping & finance'),
  wcoDef('walmart.com', 'Shopping & finance'),
  wcoDef('etsy.com', 'Shopping & finance'),
  // News & reference
  wcoDef('wikipedia.org', 'News & reference'),
  wcoDef('nytimes.com', 'News & reference'),
  wcoDef('bbc.co.uk', 'News & reference'),
  wcoDef('cnn.com', 'News & reference'),
  wcoDef('medium.com', 'News & reference'),
  // Gaming
  wcoDef('steampowered.com', 'Gaming'),
  wcoDef('steamcommunity.com', 'Gaming'),
  wcoDef('epicgames.com', 'Gaming'),
  wcoDef('playstation.com', 'Gaming'),
  wcoDef('ea.com', 'Gaming'),
  wcoDef('riotgames.com', 'Gaming'),
  wcoDef('blizzard.com', 'Gaming'),
  wcoDef('roblox.com', 'Gaming'),
  // CDN & infrastructure
  wcoDef('akamai.net', 'CDN & infrastructure'),
  wcoDef('akamaized.net', 'CDN & infrastructure'),
  wcoDef('fastly.net', 'CDN & infrastructure'),
  wcoDef('cloudfront.net', 'CDN & infrastructure', 'Amazon CloudFront CDN'),
  wcoDef('edgesuite.net', 'CDN & infrastructure'),
];



// bundles a network's service list can be quick-populated from, grouped by
// category — the same convenience gravinet's sibling project parapet gives
// its users via a large pre-filled default service list. Unlike an earlier
// version of this feature, these aren't opt-in anymore: fwAutoPopulateCatalog
// saves every entry here into every configured network's real catalog
// automatically, no button and no per-entry click. Keeping this list
// curated (a few dozen to a hundred-odd entries, not parapet's much larger
// default set) is what keeps that unconditional per-network cost bounded —
// see FW_COMMON_WILDCARD_OBJECTS's doc comment on the gossip-payload
// reasoning, which applies here the same way.
//
// Only protocols gravinet's firewall engine actually matches on are used
// here (see protoNum in internal/mesh/firewall.go): "tcp"/"udp" by port,
// "icmp"/"icmpv6" and the named "ospf" by protocol alone, and — for
// protocols with no further name recognition there — the raw IP protocol
// number as a string (e.g. "88" for EIGRP), which protoNum falls back to
// parsing numerically. A protocol name protoNum doesn't recognize silently
// matches "any" instead of erroring, so an unrecognized name here would be a
// silent bug, not a build error — every non-tcp/udp/icmp/icmp6/ospf entry
// below deliberately uses the numeric form for exactly that reason.
function svcLeg(proto, lo, hi){ return { proto: proto, port_min: lo||0, port_max: (hi!==undefined ? hi : (lo||0)) }; }
function svcDef(name, cat, legs, notes){ return { name: name, cat: cat, ports: legs, notes: notes||'' }; }
var FW_COMMON_SERVICES = [
  // Web
  svcDef('HTTP', 'Web', [svcLeg('tcp',80)]),
  svcDef('HTTPS', 'Web', [svcLeg('tcp',443)]),
  svcDef('HTTP-ALT', 'Web', [svcLeg('tcp',8080), svcLeg('tcp',8443)], 'common alternate web ports'),
  svcDef('PROXY', 'Web', [svcLeg('tcp',3128), svcLeg('tcp',8888)], 'common HTTP proxy ports (Squid and similar)'),
  // Remote access
  svcDef('SSH', 'Remote access', [svcLeg('tcp',22)]),
  svcDef('TELNET', 'Remote access', [svcLeg('tcp',23)], 'unencrypted; avoid outside trusted management segments'),
  svcDef('RDP', 'Remote access', [svcLeg('tcp',3389)]),
  svcDef('VNC', 'Remote access', [svcLeg('tcp',5900,5901)], 'displays :0\u2013:1; widen the range for more displays'),
  // Name & directory services
  svcDef('DNS', 'Name & directory', [svcLeg('udp',53), svcLeg('tcp',53)]),
  svcDef('MDNS', 'Name & directory', [svcLeg('udp',5353)]),
  svcDef('LDAP', 'Name & directory', [svcLeg('tcp',389)]),
  svcDef('LDAPS', 'Name & directory', [svcLeg('tcp',636)]),
  svcDef('KERBEROS', 'Name & directory', [svcLeg('tcp',88), svcLeg('udp',88)]),
  svcDef('SMB', 'Name & directory', [svcLeg('tcp',445)]),
  svcDef('WINS', 'Name & directory', [svcLeg('udp',137), svcLeg('udp',138)], 'legacy NetBIOS name/datagram services'),
  // DHCP
  svcDef('DHCP', 'DHCP', [svcLeg('udp',67), svcLeg('udp',68)]),
  svcDef('DHCPV6', 'DHCP', [svcLeg('udp',546), svcLeg('udp',547)]),
  // Time
  svcDef('NTP', 'Time', [svcLeg('udp',123)]),
  // Mail
  svcDef('SMTP', 'Mail', [svcLeg('tcp',25), svcLeg('tcp',465), svcLeg('tcp',587)]),
  svcDef('IMAP', 'Mail', [svcLeg('tcp',143), svcLeg('tcp',993)]),
  svcDef('POP3', 'Mail', [svcLeg('tcp',110), svcLeg('tcp',995)]),
  // File transfer
  svcDef('FTP', 'File transfer', [svcLeg('tcp',21)], 'control channel only; passive/active data ports depend on the server'),
  svcDef('TFTP', 'File transfer', [svcLeg('udp',69)]),
  svcDef('GIT', 'File transfer', [svcLeg('tcp',9418)]),
  // Databases
  svcDef('MYSQL', 'Databases', [svcLeg('tcp',3306)]),
  svcDef('POSTGRESQL', 'Databases', [svcLeg('tcp',5432)]),
  svcDef('MSSQL', 'Databases', [svcLeg('tcp',1433)]),
  svcDef('REDIS', 'Databases', [svcLeg('tcp',6379)]),
  svcDef('MONGODB', 'Databases', [svcLeg('tcp',27017)]),
  // VPN & tunneling
  svcDef('IPSEC-IKE', 'VPN & tunneling', [svcLeg('udp',500), svcLeg('udp',4500)], 'IKE + NAT-T'),
  svcDef('IPSEC-ESP', 'VPN & tunneling', [svcLeg('50')], 'raw IP protocol 50; needed alongside IKE when NAT-T isn\u2019t used'),
  svcDef('OPENVPN', 'VPN & tunneling', [svcLeg('udp',1194)]),
  svcDef('WIREGUARD', 'VPN & tunneling', [svcLeg('udp',51820)]),
  svcDef('L2TP', 'VPN & tunneling', [svcLeg('udp',1701)]),
  svcDef('PPTP', 'VPN & tunneling', [svcLeg('tcp',1723)], 'also needs GRE (below) for the data channel'),
  svcDef('GRE', 'VPN & tunneling', [svcLeg('47')], 'raw IP protocol 47'),
  // Voice & streaming
  svcDef('SIP', 'Voice & streaming', [svcLeg('tcp',5060), svcLeg('udp',5060)]),
  svcDef('SIP-TLS', 'Voice & streaming', [svcLeg('tcp',5061)]),
  svcDef('RTSP', 'Voice & streaming', [svcLeg('tcp',554)]),
  // Monitoring & management
  svcDef('SNMP', 'Monitoring & management', [svcLeg('udp',161), svcLeg('udp',162)]),
  svcDef('SYSLOG', 'Monitoring & management', [svcLeg('udp',514)]),
  svcDef('RADIUS', 'Monitoring & management', [svcLeg('udp',1812), svcLeg('udp',1813)]),
  // DevOps & observability
  svcDef('PROMETHEUS', 'DevOps & observability', [svcLeg('tcp',9090)]),
  svcDef('GRAFANA', 'DevOps & observability', [svcLeg('tcp',3000)]),
  svcDef('ELASTICSEARCH', 'DevOps & observability', [svcLeg('tcp',9200), svcLeg('tcp',9300)]),
  svcDef('DOCKER', 'DevOps & observability', [svcLeg('tcp',2375), svcLeg('tcp',2376)], '2375 plaintext, 2376 TLS'),
  svcDef('KUBERNETES-API', 'DevOps & observability', [svcLeg('tcp',6443)]),
  svcDef('ETCD', 'DevOps & observability', [svcLeg('tcp',2379), svcLeg('tcp',2380)], 'client (2379) + peer (2380)'),
  svcDef('KAFKA', 'DevOps & observability', [svcLeg('tcp',9092)]),
  svcDef('RABBITMQ', 'DevOps & observability', [svcLeg('tcp',5672), svcLeg('tcp',5671)], '5672 plaintext, 5671 TLS'),
  // Routing protocols
  svcDef('BGP', 'Routing protocols', [svcLeg('tcp',179)]),
  svcDef('OSPF', 'Routing protocols', [svcLeg('ospf')], 'raw IP protocol 89; covers both OSPFv2 and OSPFv3'),
  svcDef('RIP', 'Routing protocols', [svcLeg('udp',520)]),
  svcDef('RIPNG', 'Routing protocols', [svcLeg('udp',521)]),
  svcDef('EIGRP', 'Routing protocols', [svcLeg('88')], 'raw IP protocol 88'),
  svcDef('VRRP', 'Routing protocols', [svcLeg('112')], 'raw IP protocol 112'),
  svcDef('PIM', 'Routing protocols', [svcLeg('103')], 'raw IP protocol 103'),
  // Diagnostics
  svcDef('PING', 'Diagnostics', [svcLeg('icmp')]),
  svcDef('ICMPV6', 'Diagnostics', [svcLeg('icmpv6')]),
  svcDef('TRACEROUTE', 'Diagnostics', [svcLeg('udp',33434,33534)]),
  // Wildcards
  svcDef('ANY', 'Wildcards', [svcLeg('any')], 'matches every protocol and port'),
  svcDef('ALL-TCP', 'Wildcards', [svcLeg('tcp')], 'matches any TCP port'),
  svcDef('ALL-UDP', 'Wildcards', [svcLeg('udp')], 'matches any UDP port'),
];

// secFwServices renders the node-global service catalog: reusable protocol/
// port bundles a rule on any network references by name in its services
// field (see Config.FirewallObjects' doc comment — one catalog, shared by
// every network on this node, not one per network, so this is one table,
// not one per network either).
function secFwServices(c){
  secHint(c, 'Reusable protocol/port bundles a rule can name in its <b>services</b> field; shared by every network on this node, edited once and usable everywhere. e.g. a "DNS" service carrying udp/53 and tcp/53. Write ports as <i>proto/port</i> or <i>proto/lo\u2011hi</i>, comma\u2011separated; a proto alone (like <i>icmp</i>) matches any port. Double\u2011click a cell to edit; + adds a row, tick rows and \u2212 removes. Every well\u2011known service gravinet knows about is already a real row here; nothing to add, nothing to click.');
  const svcs = (state.fwServices || []).map(cloneSvc);
  const card = $('<div class="card"></div>');
  const t=$('<div></div>');
  let h='<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>name</th><th>ports</th><th>notes</th></tr>';
  svcs.forEach((s,i)=>{ h+='<tr data-idx="'+i+'">'
    + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
    + '<td class="sv-name">'+esc(s.name||'')+'</td>'
    + '<td class="sv-ports">'+esc(svcPortsFmt(s.ports))+'</td>'
    + '<td class="sv-notes">'+esc(s.notes||'')+'</td></tr>'; });
  if (!svcs.length) h += '<tr><td colspan="4" class="empty">no services; click + to add one</td></tr>';
  t.innerHTML=h+'</table>';
  const table=t.querySelector('table'); card.appendChild(t);
  table._svcs=svcs;
  if(svcs.length) t.querySelectorAll('tr[data-idx]').forEach(tr=>{
    const i=Number(tr.dataset.idx);
    const nameTd=tr.querySelector('.sv-name'), portsTd=tr.querySelector('.sv-ports'), notesTd=tr.querySelector('.sv-notes');
    nameTd.title=portsTd.title=notesTd.title='double-click to edit';
    nameTd.ondblclick=()=>inlineCellEdit(nameTd,svcs[i].name||'','name',v=>{v=v.trim(); if(!v){alert('name required');renderSection();return;} svcs[i].name=v; svcSave(svcs);});
    portsTd.ondblclick=()=>inlineCellEdit(portsTd,svcPortsFmt(svcs[i].ports),'e.g. udp/53, tcp/53',v=>{ const p=svcPortsParse(v); if(p===null){alert('bad ports: use proto/port or proto/lo-hi, comma-separated');renderSection();return;} svcs[i].ports=p; svcSave(svcs);});
    notesTd.ondblclick=()=>inlineCellEdit(notesTd,svcs[i].notes||'','notes',v=>{svcs[i].notes=v.trim(); svcSave(svcs);});
  });
  selAllWire(t);
  table._rowAdd=()=>svcAddRow(table);
  table._rowRemove=()=>{ const sel=selCheckedRows(table); if(!sel.length){alert('tick one or more rows to remove');return;} const keep=svcs.filter((_,i)=>!sel.some(tr=>Number(tr.dataset.idx)===i)); svcSave(keep); };
  c.appendChild(card);
}
function cloneSvc(s){ return {name:s.name||'', ports:(s.ports||[]).map(p=>({proto:p.proto||'', port_min:p.port_min||0, port_max:p.port_max||0})), notes:s.notes||''}; }
function svcPortsFmt(ports){ return (ports||[]).map(p=>{ let s=p.proto||'any'; if(p.port_min){ s+='/'+p.port_min; if(p.port_max && p.port_max!==p.port_min) s+='-'+p.port_max; } return s; }).join(', '); }
function svcPortsParse(str){ const out=[]; for(let tok of str.split(',')){ tok=tok.trim(); if(!tok) continue; const sl=tok.indexOf('/'); if(sl<0){ out.push({proto:tok.toLowerCase(), port_min:0, port_max:0}); continue; } const proto=tok.slice(0,sl).trim().toLowerCase(); const ps=tok.slice(sl+1).trim(); const dash=ps.indexOf('-'); let lo,hi; if(dash<0){ lo=hi=Number(ps); } else { lo=Number(ps.slice(0,dash)); hi=Number(ps.slice(dash+1)); } if(!proto || Number.isNaN(lo) || Number.isNaN(hi) || lo<0 || hi>65535 || lo>hi) return null; out.push({proto:proto, port_min:lo, port_max:hi}); } return out; }
function svcPayload(svcs){ return svcs.map(s=>({ name:s.name, ports:(s.ports||[]).map(p=>({proto:p.proto, port_min:p.port_min||0, port_max:p.port_max||0})), notes:s.notes||'' })); }
async function svcSave(svcs){
  const r=await api('/api/firewall',{method:'POST',body:JSON.stringify({op:'services', services:svcPayload(svcs)})});
  if(!r.ok){ alert((r.body&&r.body.error)||'save failed'); }
  await refresh();
}
function svcAddRow(table){
  const tr=document.createElement('tr');
  tr.innerHTML='<td class="selcol"></td>'
    + '<td><input class="sva-name" placeholder="name" style="width:110px"></td>'
    + '<td><input class="sva-ports" placeholder="udp/53, tcp/53" style="width:230px"></td>'
    + '<td><input class="sva-notes" placeholder="notes" style="width:100px"> <button class="sm sva-save">save</button> <button class="ghost sm sva-cancel">cancel</button></td>';
  if(!insertNewRow(table, tr)) return;
  tr.querySelector('.sva-cancel').onclick=()=>renderSection();
  tr.querySelector('.sva-save').onclick=()=>{
    const name=tr.querySelector('.sva-name').value.trim(); if(!name){alert('name required');return;}
    const ports=svcPortsParse(tr.querySelector('.sva-ports').value); if(ports===null){alert('bad ports: use proto/port or proto/lo-hi, comma-separated');return;}
    const list=(table._svcs||[]).slice(); list.push({name:name, ports:ports, notes:tr.querySelector('.sva-notes').value.trim()});
    svcSave(list);
  };
}

function secNAT(c) {
  if (!state.cfg.length) return emptyCard(c, 'No networks.');
  secHint(c, 'NAT rewrites IPv4 addresses (IPv4-only). <b>source</b> and <b>dest</b> select which packets a rule matches (blank = any). <b>translate</b> is where those packets get rewritten to, and which direction the rewrite runs, all in one value: <i>masquerade</i> (rewrite the source to the chosen interface\u2019s address, many\u21921, for outbound traffic sharing one address), a literal IPv4 (rewrite the source to that fixed address instead), or <code>port-forward:</code> followed by an IPv4 (rewrite the destination to that address instead, for inbound traffic reaching an internal host). Use + to add a rule; double-click a field to edit a rule, or the state tag to toggle it; tick rows and \u2212 to remove.');
  for (const cf of state.cfg) {
    const nat = cf.nat||{}; const en = !!nat.enabled;
    const card = $('<div class="card"></div>');
    card.appendChild(netCardHead(cf, en, '/api/nat'));

    const rules = nat.rules||[];
    let h = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>state</th><th>source</th><th>dest</th><th>translate</th></tr>';
    if (!rules.length) h += '<tr><td colspan="5" class="empty">no NAT rules — click + to add one</td></tr>';
    else rules.forEach((r, i) => {
      const tgt = r.interface ? (esc(r.translate||'masquerade')+' ('+esc(r.interface)+')') : esc(r.translate||'');
      const enabled = r.enabled!==false;
      const stTag = '<span class="tag-toggle '+(enabled?'on':'off')+'" data-natstate="1" title="double-click to '+(enabled?'disable':'enable')+'">'+(enabled?'enabled':'disabled')+'</span>';
      h += '<tr class="natrow'+(enabled?'':' fw-disabled')+'" data-idx="'+i+'" data-enabled="'+(enabled?1:0)+'"'
        + ' data-source="'+esc(r.source||'')+'" data-dest="'+esc(r.dest||'')+'" data-translate="'+esc(r.translate||'')+'" data-iface="'+esc(r.interface||'')+'">'
        + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
        + '<td class="nat-state">'+stTag+'</td>'
        + '<td class="nat-field nat-src-cell">'+esc(r.source||'any')+'</td>'
        + '<td class="nat-field nat-dst-cell">'+esc(r.dest||'any')+'</td>'
        + '<td class="nat-field nat-tr-cell">'+tgt+'</td></tr>';
    });
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; card.appendChild(t);
    const table = t.querySelector('table');
    // Double-click any field cell to edit the rule in place (row-level editor,
    // since translate and interface are interdependent). The state cell has its
    // own toggle and is excluded.
    t.querySelectorAll('tr.natrow').forEach(tr => {
      tr.querySelectorAll('.nat-field').forEach(td => {
        td.title = 'double-click to edit';
        td.ondblclick = () => startNATEdit(tr, cf.name);
      });
    });
    // Double-click the state tag to toggle the rule enabled/disabled — applied
    // live via the reload, no restart. The busy flag guards against a
    // re-entrant click before the request returns.
    t.querySelectorAll('[data-natstate]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        toggleTagState(tag, '/api/nat', on => ({op:(on?'rule-enable':'rule-disable'),net:cf.name,index:parseInt(tag.closest('tr').dataset.idx,10)}));
      };
    });
    selAllWire(t);
    table._rowAdd = () => natAddRow(table, cf.name);
    table._rowRemove = () => removeCheckedRows(table, tr => api('/api/nat',{method:'POST',body:JSON.stringify({op:'delete',net:cf.name,index:parseInt(tr.dataset.idx,10)})}));
    c.appendChild(card);
  }
}

function natAddRow(table, net){
  const tr = document.createElement('tr');
  tr.innerHTML = '<td class="selcol"></td>'
    + '<td class="nat-state"><span class="on">enabled</span></td>'
    + '<td><input class="nate-src" placeholder="any or CIDR" style="width:120px"></td>'
    + '<td><input class="nate-dst" placeholder="any or CIDR" style="width:120px"></td>'
    + '<td><input class="nate-tr" value="masquerade" title="masquerade, a literal IPv4, or port-forward:IPv4" style="width:150px"> <select class="nate-iface" style="width:108px"><option value="">iface…</option></select> <button class="sm nate-save">save</button> <button class="ghost sm nate-cancel">cancel</button></td>';
  if (!insertNewRow(table, tr)) return;
  const sel = tr.querySelector('.nate-iface');
  systemInterfaces().then(list => { sel.innerHTML = '<option value="">iface…</option>' + list.map(n => '<option value="'+esc(n)+'">'+esc(n)+'</option>').join(''); });
  tr.querySelector('.nate-cancel').onclick = () => refresh();
  tr.querySelector('.nate-save').onclick = () => {
    edit('/api/nat', {
      op:'add', net:net,
      source: tr.querySelector('.nate-src').value.trim(),
      dest: tr.querySelector('.nate-dst').value.trim(),
      translate: tr.querySelector('.nate-tr').value.trim(),
      iface: sel.value
    });
  };
}

// startNATEdit turns a NAT rule row into an inline editor (reusing the add-row
// field layout) prefilled from the row's data attributes. Saving sends the
// update op, which replaces the rule in place while preserving its enabled
// state. translate + interface are edited together because they are
// interdependent (masquerade needs an interface; a literal IPv4 or
// port-forward:IPv4 clears it).
function startNATEdit(tr, net){
  if (tr.querySelector('.nate-src')) return; // already editing
  const idx = parseInt(tr.dataset.idx, 10);
  const srcCell = tr.querySelector('.nat-src-cell');
  const dstCell = tr.querySelector('.nat-dst-cell');
  const trCell  = tr.querySelector('.nat-tr-cell');
  srcCell.innerHTML = '<input class="nate-src" placeholder="any or CIDR" style="width:120px" value="'+esc(tr.dataset.source||'')+'">';
  dstCell.innerHTML = '<input class="nate-dst" placeholder="any or CIDR" style="width:120px" value="'+esc(tr.dataset.dest||'')+'">';
  trCell.innerHTML  = '<input class="nate-tr" title="masquerade, a literal IPv4, or port-forward:IPv4" style="width:150px" value="'+esc(tr.dataset.translate||'')+'"> <select class="nate-iface" style="width:108px"><option value="">iface…</option></select> <button class="sm nate-save">save</button> <button class="ghost sm nate-cancel">cancel</button>';
  const sel = trCell.querySelector('.nate-iface');
  const curIface = tr.dataset.iface || '';
  systemInterfaces().then(list => {
    const opts = list.slice();
    if (curIface && !opts.includes(curIface)) opts.push(curIface); // keep an iface no longer present
    sel.innerHTML = '<option value="">iface…</option>' + opts.map(n => '<option value="'+esc(n)+'"'+(n===curIface?' selected':'')+'>'+esc(n)+'</option>').join('');
  });
  tr.querySelector('.nate-cancel').onclick = () => refresh();
  tr.querySelector('.nate-save').onclick = async () => {
    const r = await api('/api/nat', { method:'POST', body: JSON.stringify({
      op:'update', net:net, index:idx,
      source: srcCell.querySelector('.nate-src').value.trim(),
      dest: dstCell.querySelector('.nate-dst').value.trim(),
      translate: trCell.querySelector('.nate-tr').value.trim(),
      iface: sel.value
    })});
    if (!r.ok){ alert((r.body&&r.body.error)||'update failed'); }
    refresh();
  };
}

function qosClassLabel(i, classes){
  if (i===0) return 'class 0 (highest)';
  if (i===3) return 'class 3 (normal)';
  if (i===classes-1) return 'class '+i+' (bulk)';
  return 'class '+i;
}
function qosClassOpts(classes, sel){
  let o=''; for (let i=0;i<classes;i++) o += '<option value="'+i+'"'+(i===sel?' selected':'')+'>'+qosClassLabel(i,classes)+'</option>';
  return o;
}

function secQoS(c) {
  if (!state.cfg.length) return emptyCard(c, 'No networks.');
  secHint(c, '5 priority classes (0 = highest, 4 = lowest/bulk). Unmatched traffic uses class 3 (normal). Strict priority — higher classes drain first under contention. <b>match</b> takes a comma-separated mix of named services (the same catalog as Firewall \u203a Services) and raw <code>proto</code>/<code>proto/port</code> entries (e.g. <code>https, tcp/8443, udp/53</code>); at most one raw entry per rule, any number of named services; leave it blank to match anything. Use + to add a rule, double-click a rule to edit it, double-click the state tag to toggle it, tick rows and use \u2212 to remove.');
  for (const cf of state.cfg) {
    const q = cf.qos||{}; const en = !!q.enabled; const classes = q.classes||5;
    const dflt = (q.default_class!=null)?q.default_class:3;
    const card = $('<div class="card"></div>');
    card.appendChild(netCardHead(cf, en, '/api/qos'));
    const rules = q.rules||[];
    let h = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>state</th><th>match</th><th>class</th></tr>';
    if (!rules.length) h += '<tr><td colspan="4" class="empty">no QoS rules — click + to add one</td></tr>';
    else for (const r of rules) {
      const enabled = !r.disabled;
      const svcTxt = qosSvcLabel(r);
      const stTag = '<span class="tag-toggle '+(enabled?'on':'off')+'" data-qosstate="1" title="double-click to '+(enabled?'disable':'enable')+'">'+(enabled?'enabled':'disabled')+'</span>';
      h += '<tr class="qrow'+(enabled?'':' fw-disabled')+'" data-proto="'+esc(r.protocol||'')+'" data-port="'+esc(r.port_min||0)+'" data-services="'+esc((r.services||[]).join(','))+'" data-class="'+esc(r.class)+'" data-enabled="'+(enabled?1:0)+'">'
        + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
        + '<td class="q-state">'+stTag+'</td>'
        + '<td class="q-services">'+esc(svcTxt||'any')+'</td>'
        + '<td class="q-class">'+esc(qosClassLabel(r.class,classes))+'</td></tr>';
    }
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; card.appendChild(t);
    const table = t.querySelector('table');
    t.querySelectorAll('tr.qrow').forEach(tr => tr.ondblclick = (e) => {
      if (e.target.closest('.q-state')) return; // state cell has its own toggle
      startQoSEdit(tr, cf.name, classes);
    });
    // Double-click the state tag to toggle the rule enabled/disabled — applied
    // live via the reload, no restart. The busy flag guards against a
    // re-entrant click before the request returns.
    t.querySelectorAll('[data-qosstate]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        toggleTagState(tag, '/api/qos', on => ({op:(on?'rule-enable':'rule-disable'),net:cf.name,proto:tag.closest('tr').dataset.proto,port:Number(tag.closest('tr').dataset.port),services:qosServicesFromRow(tag.closest('tr'))}));
      };
    });
    selAllWire(t);
    table._rowAdd = () => qosAddRow(table, cf.name, classes);
    table._rowRemove = () => removeCheckedRows(table, tr => api('/api/qos',{method:'POST',body:JSON.stringify({op:'delete',net:cf.name,proto:tr.dataset.proto,port:Number(tr.dataset.port),services:qosServicesFromRow(tr)})}));
    c.appendChild(card);
  }
}

// qosServicesFromRow reads a QoS row's data-services attribute (a
// comma-joined list of named services, kept separate from the row's literal
// proto/port so the two combine unambiguously) back into an array, filtering
// out the stray empty string ''.split(',') would otherwise produce for a
// rule with no services.
function qosServicesFromRow(tr){
  return (tr.dataset.services||'').split(',').map(s=>s.trim()).filter(Boolean);
}

// qosSvcLabel renders a rule's literal proto/port leg (if any) plus its named
// services into one combined display/edit string, e.g. "tcp/443, ssh" —
// mirrors fwSvcLabel (internal/mesh/firewall.go's resolveLegs union, shown
// the same way there).
function qosSvcLabel(r){
  const parts = [];
  if (r.protocol || r.port_min || r.port_max) {
    const pl = portLabel(r.port_min, r.port_max);
    parts.push((r.protocol||'any') + (pl==='any' ? '' : '/'+pl));
  }
  for (const s of (r.services||[])) parts.push(s);
  return parts.join(', ');
}

// qosAddRow inserts a blank editable row at the top of the table; saving creates
// the rule via the add op.
function qosAddRow(table, net, classes){
  const tr = document.createElement('tr');
  tr.innerHTML = '<td class="selcol"></td>'
    + '<td class="q-state"><span class="on">enabled</span></td>'
    + '<td><span class="fwe-field"><input class="qe-services" placeholder="tcp/443, ssh, or blank for any" style="width:220px"></span></td>'
    + '<td><select class="qe-class">'+qosClassOpts(classes,3)+'</select> <button class="sm qe-save">save</button> <button class="ghost sm qe-cancel">cancel</button></td>';
  if (!insertNewRow(table, tr)) return;
  fwCatalogCombobox(tr.querySelector('.qe-services'), () => (state.fwServices||[]).map(s=>s.name));
  tr.querySelector('.qe-cancel').onclick = () => refresh();
  tr.querySelector('.qe-save').onclick = () => {
    const rule = qosCollectRule(tr); if (!rule) return;
    edit('/api/qos', { op:'add', net:net, proto:rule.proto, port:rule.port, services:rule.services, class:rule.class });
  };
}

// qosCollectRule reads the combined match field (reusing fwParseSvc — same
// "proto"/"proto/port"/named-service comma-separated grammar the firewall
// rule editor uses) plus the class select into a rule payload. Returns null
// (after alerting) if the field couldn't be parsed.
function qosCollectRule(scope){
  const parsed = fwParseSvc(scope.querySelector('.qe-services').value);
  if (parsed.error){ alert(parsed.error); return null; }
  return { proto: parsed.proto, port: parsed.port, services: parsed.services, class: Number(scope.querySelector('.qe-class').value) };
}

// startQoSEdit turns a QoS rule row into an inline editor; saving re-keys the
// rule by deleting the old (proto:port:services) and adding the edited one.
function startQoSEdit(tr, net, classes){
  if (tr.querySelector('.qe-services')) return; // already editing
  const oldProto = tr.dataset.proto, oldPort = Number(tr.dataset.port);
  const oldServices = qosServicesFromRow(tr);
  const wasDisabled = tr.dataset.enabled === '0';
  const sc = tr.querySelector('.q-services'), cc = tr.querySelector('.q-class');
  const combo = qosSvcLabel({protocol:oldProto, port_min:oldPort, port_max:oldPort, services:oldServices});
  sc.innerHTML = '<span class="fwe-field"><input class="qe-services" style="width:220px" value="'+esc(combo)+'"></span>';
  cc.innerHTML = '<select class="qe-class">'+qosClassOpts(classes, Number(tr.dataset.class))+'</select> <button class="sm qe-save">save</button>';
  fwCatalogCombobox(sc.querySelector('.qe-services'), () => (state.fwServices||[]).map(s=>s.name));
  tr.ondblclick = null;
  cc.querySelector('.qe-save').onclick = async () => {
    const rule = qosCollectRule(tr); if (!rule) return;
    const dr = await api('/api/qos',{method:'POST',body:JSON.stringify({op:'delete',net:net,proto:oldProto,port:oldPort,services:oldServices})});
    if (!dr.ok){ alert((dr.body&&dr.body.error)||'edit failed'); refresh(); return; }
    const ar = await api('/api/qos',{method:'POST',body:JSON.stringify({op:'add',net:net,proto:rule.proto,port:rule.port,services:rule.services,class:rule.class})});
    if (!ar.ok){ alert((ar.body&&ar.body.error)||'edit failed'); refresh(); return; }
    // An edit re-keys the rule via delete+add, which would reset it to enabled;
    // carry the prior disabled state across so editing doesn't silently
    // re-enable a paused rule.
    if (wasDisabled){
      const xr = await api('/api/qos',{method:'POST',body:JSON.stringify({op:'rule-disable',net:net,proto:rule.proto,port:rule.port,services:rule.services})});
      if (!xr.ok) alert((xr.body&&xr.body.error)||'edit failed');
    }
    refresh();
  };
}

function secBandwidth(c) {
  if (!state.cfg.length) return emptyCard(c, 'No networks.');
  secHint(c, 'Double-click a rate to set it — enter a number and pick the unit; clear the number for unlimited. Double-click the tag above to turn the cap on or off.');
  for (const cf of state.cfg) {
    const t = cf.throttle||{}; const en = !!t.enabled; const card = $('<div class="card"></div>');
    card.appendChild(netCardHead(cf, en, '/api/bandwidth'));
    const disp = $('<div>up: <span class="bw-edit" data-dir="up" title="double-click to set">'+esc(rate(t.up_bytes_per_sec))+'</span>'
      + ' · down: <span class="bw-edit" data-dir="down" title="double-click to set">'+esc(rate(t.down_bytes_per_sec))+'</span></div>');
    const spans = disp.querySelectorAll('.bw-edit');
    spans[0].ondblclick = () => startBwEdit(spans[0], cf.name, 'up', t.up_bytes_per_sec);
    spans[1].ondblclick = () => startBwEdit(spans[1], cf.name, 'down', t.down_bytes_per_sec);
    card.appendChild(disp);
    c.appendChild(card);
  }
}

// ---- Upgrade (Mesh -> Upgrade) --------------------------------------------
//
// The tab is deliberately opinionated about what it puts in front of you, in
// this order: what THIS node is running, what it has staged, what the FLEET is
// running, and only then the button that changes any of it. That ordering is the
// same argument the package itself makes -- you cannot sensibly roll a binary
// out to ten nodes until you can see all ten -- and it is the reason the fleet
// table sits above the rollout form rather than beside it.
//
// Every action here goes to the same daemon entry point the CLI uses, so a
// rollout started from this form is the same rollout, with the same canary and
// the same abort rule, as one started from a terminal.
function secUpgrade(c){
  secHint(c, 'Get a new [gravinet] build running on this node: upload it, and this node applies it and restarts. This only ever happens on the machine you\u2019re logged into; no peer, not even a Manager, can start or control an upgrade here. See docs/UPGRADES.md.');
  const host = $('<div></div>');
  c.appendChild(host);
  drawUpgrade(host);
}

async function drawUpgrade(host){
  const r = await api('/api/upgrade');
  const u = r.body || {};
  host.innerHTML = '';
  if (!u.enabled){
    const card = $('<div class="card"></div>');
    card.appendChild($('<div class="empty">Upgrades are unavailable on this node.<br><br>' + esc(u.reason||'') + '</div>'));
    host.appendChild(card);
    return;
  }

  // Every /api/upgrade/* call is in LOCAL_API — it always lands on the node
  // this session is actually logged into, never on whichever peer is
  // selected in the header. Silently applying to "local" while the picker
  // reads a peer's name would look exactly like it's targeting that peer
  // and isn't; disabling here, visibly, is what stops that misread rather
  // than just being technically correct underneath a normal-looking form.
  const remote = !!state.target;
  const peer = remote ? state.cluster.find(p => p.node_id === state.target) : null;
  const peerName = peer ? (peer.hostname || peer.node_id.slice(0,8)) : (state.target || '').slice(0,8);

  const stCard = $('<div class="card"></div>');
  if (remote) stCard.classList.add('local-only-disabled');
  stCard.appendChild($('<h3>Upload</h3>'));
  if (remote){
    stCard.appendChild($('<div class="hint" style="margin:0 0 10px; color:var(--danger,#b33); opacity:1">Upgrades are local-only: this always acts on the node you\u2019re logged into, never on \u2018'
      + esc(peerName) + '\u2019, which is selected above. Disabled here so it can\u2019t look like it\u2019s upgrading that peer when it isn\u2019t. Select \u201cThis node\u201d to use this on the node you\u2019re actually on, or log into '
      + esc(peerName) + '\u2019s own web admin to upgrade it there.</div>'));
  }
  stCard.appendChild($('<div class="hint" style="margin:0 0 10px">' + (u.signing_required
    ? 'Upload a build and its signed manifest (make one with <b>gravinet upgrade sign</b>). The signature is checked before the upload is accepted.'
    : 'Upload a source archive and it\u2019s built and applied automatically.'
    ) + '</div>'));

  const up = $('<div class="tbar"></div>');
  if (u.signing_required){
    const binIn = $('<input type="file" class="up-bin">');
    const manIn = $('<input type="file" class="up-man" accept=".json,application/json">');
    const stageBtn = $('<button class="sm">Stage</button>');
    up.appendChild($('<span class="hint">binary</span>')); up.appendChild(binIn);
    up.appendChild($('<span class="hint">manifest</span>')); up.appendChild(manIn);
    up.appendChild(stageBtn);
    stageBtn.onclick = async () => {
      if (!binIn.files[0] || !manIn.files[0]){ alert('Pick both the binary and its signed .json manifest.'); return; }
      const fd = new FormData();
      // Order matters: the manifest is read first so the artifact can be
      // verified against it as it streams in, rather than after it has
      // already been written somewhere it could be executed from.
      fd.append('manifest', manIn.files[0]);
      fd.append('artifact', binIn.files[0]);
      stageBtn.disabled = true; stageBtn.textContent = 'Staging\u2026';
      try {
        const resp = await fetch('/api/upgrade/stage', { method:'POST', body: fd });
        const body = await resp.json().catch(()=>({}));
        if (!resp.ok){ alert(body.error || 'staging failed'); return; }
        drawUpgrade(host);
      } finally { stageBtn.disabled = false; stageBtn.textContent = 'Stage'; }
    };
  } else {
    // Source only -- no binary upload, no format choice on this side either:
    // .tgz/.tar.gz and .zip are both accepted, and which one a given upload
    // is gets sniffed from its content server-side (extractSourceArchive),
    // not from this accept list or the filename — the accept attribute below
    // is just what makes the file picker's own dialog filter sensibly.
    const fileIn = $('<input type="file" class="up-file" accept=".tgz,.tar.gz,.zip,application/gzip,application/zip">');
    const upgradeBtn = $('<button class="sm" style="margin-left:16px">Upgrade</button>');
    up.appendChild(fileIn);
    up.appendChild(upgradeBtn);
    upgradeBtn.onclick = async () => {
      if (!fileIn.files[0]){ alert('Pick a source .tgz/.tar.gz or .zip first.'); return; }
      if (!confirm('Upload this, build it, apply it, and restart into it?\n\nNothing is touched until the build succeeds. If the new binary can\u2019t rejoin the mesh, it reverts itself automatically.')) return;
      upgradeBtn.disabled = true; upgradeBtn.textContent = 'Building\u2026';
      try {
        const resp = await fetch('/api/upgrade/stage-source', { method:'POST', body: fileIn.files[0] });
        const body = await resp.json().catch(()=>({}));
        if (!resp.ok){ alert(body.error || 'build failed'); return; }
        upgradeBtn.textContent = 'Installing\u2026';
        const ar = await api('/api/upgrade/local-apply', { method:'POST', body: JSON.stringify({ id: body.staged }) });
        if (!ar.ok){ alert((ar.body && ar.body.error) || 'apply failed'); return; }
        alert('Applied. This node is restarting into ' + body.staged + '.\n\nIf it cannot get its peers back within the confirm window, it will revert itself.');
        drawUpgrade(host);
      } finally { upgradeBtn.disabled = false; upgradeBtn.textContent = 'Upgrade'; }
    };
  }
  if (remote){
    up.querySelectorAll('input,button').forEach(el => { el.disabled = true; });
  }
  stCard.appendChild(up);
  host.appendChild(stCard);
}

// infoMetrics renders the Metrics tab: a duration selector plus live CPU,
// memory, disk, and per-overlay-interface throughput graphs. It self-refreshes
// every few seconds and stops when the tab/section is left.
let metricsTimer = null;
let metricsMinutes = 1;
let metricsHover = false;
const CH = {W:640,H:150,padL:76,padR:10,padT:8,padB:20};
function infoMetrics(c){
  if (metricsTimer){ clearInterval(metricsTimer); metricsTimer = null; }
  metricsHover = false;
  const durBar = $('<div class="seg" style="margin-bottom:14px"></div>');
  for (const [m,lbl] of [[1,'1 min'],[5,'5 min'],[15,'15 min'],[30,'30 min'],[60,'60 min']]){
    const b = $('<button class="seg-btn'+(metricsMinutes===m?' active':'')+'">'+lbl+'</button>');
    b.onclick = () => {
      metricsMinutes = m;
      durBar.querySelectorAll('.seg-btn').forEach(x => x.className = 'seg-btn');
      b.className = 'seg-btn active';
      load();
    };
    durBar.appendChild(b);
  }
  c.appendChild(durBar);
  const body = $('<div></div>'); body.innerHTML = '<div class="hint">loading\u2026</div>'; c.appendChild(body);

  const load = async () => {
    if (state.section!=='metrics'){ if(metricsTimer){clearInterval(metricsTimer); metricsTimer=null;} return; }
    const r = await api('/api/metrics?minutes='+metricsMinutes);
    if (!r.ok || !r.body){ body.innerHTML = '<div class="hint">could not load metrics.</div>'; return; }
    if (!r.body.available){ body.innerHTML = '<div class="hint">live metrics aren\u2019t available on this host; the CPU/memory/disk/network readers here couldn\u2019t read anything usable.</div>'; return; }
    renderMetricGraphs(body, r.body, metricsMinutes);
  };
  load();
  metricsTimer = setInterval(load, 3000);
}

// metricSpecs turns a metrics payload into one card-spec per graph. The key is
// used to decide whether the existing cards can be updated in place or must be
// rebuilt (e.g. when the set of interfaces changes).
function metricSpecs(data){
  const specs = [
    { key:'cpu', title:'CPU', series:[{name:'usage', color:'var(--acc)', points:data.cpu}], yMax:100, fmtY:pctFmt, single:true },
    { key:'mem', title:'Memory', series:[{name:'used', color:'#e0883b', points:data.mem}], yMax:100, fmtY:pctFmt, single:true },
    { key:'disk', title:'Disk (' + esc(data.disk_path||'/') + ')', series:[{name:'used', color:'#7a9fe0', points:data.disk}], yMax:100, fmtY:pctFmt, single:true },
  ];
  // CPU, Memory, then Disk stay pinned on top; the per-interface bandwidth
  // cards follow in alphabetical order (by network, then interface name).
  const ifaces = (data.ifaces||[]).slice().sort((a,b)=>{
    const an=(a.network||'')+' \u00b7 '+a.iface, bn=(b.network||'')+' \u00b7 '+b.iface;
    return an.localeCompare(bn, undefined, {numeric:true, sensitivity:'base'});
  });
  for (const ifc of ifaces){
    specs.push({
      key:'if:'+(ifc.network||'')+'/'+ifc.iface,
      title:(ifc.network?esc(ifc.network):'(unnamed)')+' \u00b7 '+esc(ifc.iface),
      series:[{name:'rx', color:'var(--acc)', points:ifc.rx},{name:'tx', color:'#e0883b', points:ifc.tx}],
      yMax:Math.max(1, maxOf(ifc.rx), maxOf(ifc.tx)), fmtY:rateFmt, single:false,
    });
  }
  return specs;
}

function renderMetricGraphs(body, data, minutes){
  const win = minutes*60;
  // The chart's right edge ("now") is anchored to the newest actual sample
  // timestamp across all series — NOT to wall-clock (neither the browser's
  // Date.now() nor the server's server_now). Reasoning: the line's last
  // point is drawn at the x-position of its own timestamp, so the line
  // reaches the edge only if the edge sits at that timestamp. On platforms
  // whose readers are instant, the newest point is ~microseconds old when
  // served, so wall-clock and newest-point coincide and it looked fine. On
  // macOS the readers take ~1s (top -l 1's startup delay), so the newest
  // point is meaningfully older than wall-clock, and anchoring the edge to
  // wall-clock (as v454's server_now did — worse, server_now is read fresh
  // at request time, strictly newer than any point) left every line ending
  // short by that margin. Anchoring to the data itself makes the window end
  // exactly where the freshest data is, on every platform, so "now" means
  // "the most recent reading" — which is what the edge should represent.
  // server_now is still used, but only as the anchor when there are no
  // points at all yet (fresh boot), and Date.now() as the last resort.
  let nowRef = 0;
  const allSeries = [data.cpu, data.mem, data.disk];
  for (const ifc of (data.ifaces||[])){ allSeries.push(ifc.rx); allSeries.push(ifc.tx); }
  for (const arr of allSeries){
    if (arr && arr.length){ const t = arr[arr.length-1].t; if (t > nowRef) nowRef = t; }
  }
  if (!nowRef) nowRef = data.server_now || Math.floor(Date.now()/1000);
  const specs = metricSpecs(data);
  const key = specs.map(s=>s.key).join('|');
  // Same set of graphs as last time: update each in place so the hover
  // crosshair/tooltip survive and the lines keep scrolling while inspected.
  if (body._cardsKey === key && body._cards && body._cards.length === specs.length){
    for (let i=0;i<specs.length;i++){ body._cards[i]._redraw(specs[i].series, specs[i].yMax, win, specs[i].fmtY, nowRef); }
    updateUptimeCard(body, data);
    return;
  }
  body.innerHTML = ''; body._cards = []; body._cardsKey = key; body._uptimeEl = null;
  for (const sp of specs){
    const card = graphCard(sp.title, sp.series, sp.yMax, sp.fmtY, win, sp.single, nowRef);
    body.appendChild(card); body._cards.push(card);
  }
  if (!(data.ifaces||[]).length){
    body.appendChild($('<div class="card metric-card"><div class="hint">no overlay interfaces yet — create a network under Networks.</div></div>'));
  }
  updateUptimeCard(body, data);
}

// updateUptimeCard appends (once) or refreshes the system-uptime stat card at
// the bottom of the Metrics page, underneath the bandwidth graphs — deliberately
// outside the graph-cards fast-path/rebuild logic above so it keeps refreshing
// every poll regardless of whether the set of graphs (which it isn't part of)
// happened to change. It's a single live value, not a history graph like CPU/
// memory/disk/bandwidth above it: seconds-since-boot only ever counts up at
// the same rate the clock does, so a chart of it would just be a straight
// diagonal line — nothing worth plotting. Omitted (rather than shown as 0)
// when the backend's reader couldn't get a value on this platform.
function updateUptimeCard(body, data){
  if (data.uptime_seconds == null){
    if (body._uptimeEl){ body._uptimeEl.remove(); body._uptimeEl = null; }
    return;
  }
  const text = fmtUptime(data.uptime_seconds);
  if (body._uptimeEl){
    body._uptimeEl.querySelector('.metric-now').textContent = text;
    return;
  }
  const card = $('<div class="card metric-card"><div class="metric-head"><span class="metric-title">System uptime</span><span class="metric-now"></span></div></div>');
  card.querySelector('.metric-now').textContent = text;
  body.appendChild(card);
  body._uptimeEl = card;
}

// fmtUptime formats a seconds-since-boot count the same compact way
// fmtElapsed formats a peer session's connected time ("3d 4h", "2h5m",
// "12m34s") — but takes a plain duration directly rather than a timestamp to
// diff against "now", since uptime already IS the duration, not a moment to
// measure from.
function fmtUptime(seconds){
  let s = Math.max(0, Math.round(seconds));
  const d = Math.floor(s/86400); s -= d*86400;
  const h = Math.floor(s/3600); s -= h*3600;
  const m = Math.floor(s/60); s -= m*60;
  if (d) return d+'d '+h+'h';
  if (h) return h+'h '+m+'m';
  if (m) return m+'m '+s+'s';
  return s+'s';
}

// graphCard builds one metric card: header (with current values) plus an
// interactive line chart. card._redraw(series,yMax,win,fmtY) updates the data
// layer in place, leaving the hover overlay (and its listeners) untouched.
function graphCard(title, series, yMax, fmtY, win, single, nowRef){
  const card = $('<div class="card metric-card"></div>');
  const head = $('<div class="metric-head"></div>');
  const setHead = (sr) => {
    if (single){
      head.innerHTML = '<span class="metric-title">'+esc(title)+'</span><span class="metric-now">'+esc(fmtY(lastVal(sr[0].points)))+'</span>';
    } else {
      const lg = sr.map((s,i) => '<b style="color:'+s.color+'">'+(i===0?'\u25bc':'\u25b2')+' '+esc(s.name)+'</b> '+esc(fmtY(lastVal(s.points)))).join(' &nbsp; ');
      head.innerHTML = '<span class="metric-title">'+title+'</span><span class="metric-legend">'+lg+'</span>';
    }
  };
  setHead(series);
  card.appendChild(head);
  const holder = $('<div class="chart-holder"></div>');
  holder.innerHTML = chartSVG(series, yMax, win, fmtY, nowRef);
  card.appendChild(holder);
  const hs = { series:series, yMax:yMax, win:win, fmtY:fmtY, now:nowRef };
  attachChartHover(holder, hs);
  card._redraw = (sr, ym, wn, ff, nr) => {
    hs.series = sr; hs.yMax = ym; hs.win = wn; hs.fmtY = ff; hs.now = nr;
    const svg = holder.querySelector('svg.chart');
    if (svg){
      const L = chartLayers(sr, ym, wn, ff, nr);
      const grid = svg.querySelector('.grid'), dataG = svg.querySelector('.data');
      if (grid) grid.innerHTML = L.grid;
      if (dataG) dataG.innerHTML = L.data;
    }
    setHead(sr);
  };
  return card;
}

// attachChartHover wires a crosshair + value tooltip to a rendered chart. It
// reads the live series/scale from hs so updates-in-place stay accurate, and
// recomputes the time window each move so the x-axis tracks "now".
function attachChartHover(holder, hs){
  const svg = holder.querySelector('svg.chart');
  if (!svg) return;
  const cap = svg.querySelector('.capture');
  const hov = svg.querySelector('.hover');
  const vline = svg.querySelector('.hover-x');
  const dots = svg.querySelectorAll('.hover-dot');
  const tip = $('<div class="chart-tip" style="display:none"></div>');
  holder.appendChild(tip);
  const W=CH.W, padL=CH.padL, padR=CH.padR, padT=CH.padT, padB=CH.padB, H=CH.H;
  const plotL=padL, plotR=W-padR;
  const nearest = (pts, t) => {
    if (!pts || !pts.length) return -1;
    let best=0, bd=Infinity;
    for (let i=0;i<pts.length;i++){ const d=Math.abs(pts[i].t-t); if (d<bd){ bd=d; best=i; } }
    return best;
  };
  cap.addEventListener('mouseenter', () => { metricsHover = true; });
  cap.addEventListener('mouseleave', () => { metricsHover = false; hov.style.display='none'; tip.style.display='none'; });
  cap.addEventListener('mousemove', (ev) => {
    const series = hs.series, yMax = hs.yMax, win = hs.win, fmtY = hs.fmtY;
    const now=hs.now, t0=now-win;
    const ys = v => padT + (H-padT-padB)*(1 - Math.min(v,yMax)/yMax);
    const xOf = t => plotL + (plotR-plotL)*Math.max(0,Math.min(1,(t-t0)/win));
    const rect = svg.getBoundingClientRect();
    const vx = (ev.clientX-rect.left)/rect.width*W;
    const frac = Math.max(0, Math.min(1, (vx-plotL)/(plotR-plotL)));
    const t = t0 + frac*win;
    let snapT=null;
    for (const s of series){ const i=nearest(s.points,t); if (i>=0){ snapT=s.points[i].t; break; } }
    if (snapT===null) return;
    const sx = xOf(snapT);
    vline.setAttribute('x1', sx.toFixed(1));
    vline.setAttribute('x2', sx.toFixed(1));
    let rows='';
    dots.forEach((dot,di) => {
      const pts = series[di] ? series[di].points : null;
      const i = nearest(pts, snapT);
      if (i<0){ dot.style.display='none'; return; }
      const v = pts[i].v;
      dot.style.display='';
      dot.setAttribute('cx', sx.toFixed(1));
      dot.setAttribute('cy', ys(v).toFixed(1));
      rows += '<div class="tip-row"><span class="tip-dot" style="background:'+series[di].color+'"></span>'
        + esc(series[di].name)+' <b>'+esc(fmtY(v))+'</b></div>';
    });
    hov.style.display='';
    tip.innerHTML = '<div class="tip-t">'+esc(clockOf(snapT))+'</div>'+rows;
    tip.style.display='';
    tip.classList.toggle('flip', frac>0.6);
    const hrect = holder.getBoundingClientRect();
    tip.style.left = (ev.clientX-hrect.left)+'px';
    tip.style.top = (ev.clientY-hrect.top)+'px';
  });
}

function clockOf(t){ const d=new Date(t*1000); const p=n=>String(n).padStart(2,'0'); return p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds()); }

// chartLayers builds the grid (axes/labels) and data (line paths) layers for a
// chart, separately so the data can be redrawn in place without disturbing the
// hover overlay.
function chartLayers(series, yMax, win, fmtY, nowRef){
  const W=CH.W, H=CH.H, padL=CH.padL, padR=CH.padR, padT=CH.padT, padB=CH.padB;
  const now=nowRef, t0=now-win;
  const xs = t => padL + (W-padL-padR)*Math.max(0,Math.min(1,(t-t0)/win));
  const ys = v => padT + (H-padT-padB)*(1 - Math.min(v,yMax)/yMax);
  let grid='';
  for (const f of [0,0.5,1]){
    const yy = padT + (H-padT-padB)*(1-f);
    grid += '<line x1="'+padL+'" y1="'+yy.toFixed(1)+'" x2="'+(W-padR)+'" y2="'+yy.toFixed(1)+'" stroke="var(--line)" stroke-width="1"/>';
    grid += '<text x="'+(padL-8)+'" y="'+(yy+3).toFixed(1)+'" text-anchor="end" font-size="10" fill="var(--mut)">'+esc(fmtY(yMax*f))+'</text>';
  }
  grid += '<text x="'+padL+'" y="'+(H-6)+'" font-size="10" fill="var(--mut)">-'+(win>=60?(win/60+'m'):(win+'s'))+'</text>';
  grid += '<text x="'+(W-padR)+'" y="'+(H-6)+'" text-anchor="end" font-size="10" fill="var(--mut)">now</text>';
  let data='';
  for (const s of series){
    const pts = s.points||[];
    if (!pts.length) continue;
    let d='';
    for (let i=0;i<pts.length;i++){ d += (i?'L':'M')+xs(pts[i].t).toFixed(1)+' '+ys(pts[i].v).toFixed(1)+' '; }
    data += '<path d="'+d+'" fill="none" stroke="'+s.color+'" stroke-width="1.5" stroke-linejoin="round"/>';
  }
  return { grid: grid, data: data };
}

// chartSVG draws a fixed-viewBox line chart (scales to the card width via CSS),
// with the grid and data in their own groups, plus a hover crosshair and a
// transparent capture rect on top.
function chartSVG(series, yMax, win, fmtY, nowRef){
  const W=CH.W, H=CH.H, padL=CH.padL, padR=CH.padR, padT=CH.padT, padB=CH.padB;
  const L = chartLayers(series, yMax, win, fmtY, nowRef);
  let dots='';
  for (const s of series){ dots += '<circle class="hover-dot" r="3.2" fill="'+s.color+'" cx="0" cy="0" style="display:none"/>'; }
  let g = '<g class="grid">'+L.grid+'</g><g class="data">'+L.data+'</g>';
  g += '<g class="hover" style="display:none"><line class="hover-x" x1="0" y1="'+padT+'" x2="0" y2="'+(H-padB)+'" stroke="var(--mut)" stroke-width="1" stroke-dasharray="3 3"/>'+dots+'</g>';
  g += '<rect class="capture" x="'+padL+'" y="'+padT+'" width="'+(W-padL-padR)+'" height="'+(H-padT-padB)+'" fill="transparent" pointer-events="all"/>';
  return '<svg class="chart" viewBox="0 0 '+W+' '+H+'">'+g+'</svg>';
}

function lastVal(pts){ return (pts && pts.length) ? pts[pts.length-1].v : 0; }
function maxOf(pts){ let m=0; for (const p of (pts||[])) if (p.v>m) m=p.v; return m; }
function pctFmt(v){ return (v||0).toFixed(0)+'%'; }
function rateFmt(v){
  // v is bytes/sec; network throughput is conventionally bits/sec, decimal steps.
  const b = (v||0)*8;
  if (b < 1000) return b.toFixed(0)+' bps';
  if (b < 1e6) return (b/1e3).toFixed(1)+' Kbps';
  if (b < 1e9) return (b/1e6).toFixed(1)+' Mbps';
  return (b/1e9).toFixed(2)+' Gbps';
}

// infoSpeedtest measures overlay throughput between two managed peers. The first
// peer acts as the client (run locally if it's this node, else via the proxy)
// and tests against the second; results render as Metrics-style graphs.
//
// The first (client) picker only offers peers in Manager mode. The client
// connects to the target's /api/speedtest/source and /sink directly over the
// overlay, and the target's authed() bypass for that kind of connection
// accepts only a caller currently advertising Manager mode (see
// webadmin.authed / mesh.IsManagerAddr) — a merely-Managed peer looks like a
// perfectly good option here but is guaranteed a 401 the moment it tries to
// run as the client. Listing it anyway just relocates the failure from "can't
// select it" to "selected it, ran the test, got an auth error," so it's left
// out up front. The second (target) picker keeps the wider Managed-only
// filter, since the target only needs to accept the client's request, not
// originate one.
function infoSpeedtest(c){
  const card = $('<div class="card"></div>');
  card.appendChild($('<div class="hint" style="margin:0 0 10px">Measure overlay throughput between two managed peers. The first peer runs the test against the second; download and upload are each measured for ~4s. Only peers in Manager mode can run a test as the first peer.</div>'));

  const bar = $('<div class="tbar"></div>');
  // Both pickers are buildListPicker listboxes, same as the header's node picker:
  // the filter sits at the TOP OF EACH DROPDOWN, not beside it. These were the
  // last native <select>s with a filter input parked next to them — the pattern
  // that made the header read as two unrelated controls.
  //
  // The two pickers exclude each other: a node can't be both endpoints of a test
  // against itself. With a <select> that was done by flipping option.disabled on
  // the other box; here it's the same idea expressed as the item's disabled flag,
  // which buildListPicker renders grayed, refuses to pick, and steps over during
  // keyboard navigation. (No backticks in this comment: indexHTML is a Go raw
  // string literal, and a backtick would end it mid-file.)
  let pickA = null, pickB = null;
  const exclude = () => {
    const av = pickA.getValue(), bv = pickB.getValue();
    pickA.setItems(itemsFor(managerPeers, selfIsManager).map(it => ({ ...it, disabled: it.value === bv })));
    pickB.setItems(itemsFor(peers, true).map(it => ({ ...it, disabled: it.value === av })));
    pickA.setValue(av);
    pickB.setValue(bv);
    runBtn.disabled = pickA.count() === 0;
  };
  // If a pick collides with the other box's node, shift that other box to the
  // first node it can still legally hold — same rule the <select> version used.
  const onPickOne = (self, other, otherPool, otherIncludesSelf) => {
    if (self.getValue() === other.getValue()){
      const alt = itemsFor(otherPool, otherIncludesSelf).find(it => it.value !== self.getValue());
      other.setValue(alt ? alt.value : '');
    }
    exclude();
  };
  pickA = buildListPicker({
    title: 'the peer that runs the test (must be in Manager mode)',
    placeholder: '(no Manager-mode peer)',
    filterPlaceholder: 'filter…',
    compact: true,
    onPick: () => onPickOne(pickA, pickB, peers, true),
  });
  pickB = buildListPicker({
    title: 'the peer the test runs against',
    placeholder: '(no peer)',
    filterPlaceholder: 'filter…',
    compact: true,
    onPick: () => onPickOne(pickB, pickA, managerPeers, selfIsManager),
  });
  const arrow = $('<span style="color:var(--mut)">&rarr;</span>');
  const runBtn = $('<button class="sm">Run</button>');
  bar.appendChild(pickA); bar.appendChild(arrow); bar.appendChild(pickB); bar.appendChild(runBtn);
  card.appendChild(bar);
  const status = $('<div class="hint" style="display:none;margin-top:4px"></div>');
  card.appendChild(status);
  const out = $('<div style="margin-top:6px"></div>');
  card.appendChild(out);
  c.appendChild(card);

  let selfDesc = { ip:'', port:0, hostname: state.selfHostname||'local', isSelf:true };
  let peers = [];        // all managed, reachable peers — the target (B) pool
  let managerPeers = []; // subset currently in Manager mode — the client (A) pool
  let selfIsManager = false;
  const descFor = (val) => val==='__self__' ? selfDesc : (peers.find(p => p.node_id===val) || {});
  // itemsFor maps a peer pool to picker options, optionally led by this node.
  // The display-name rule (hostname, else a short id prefix) is the same one used
  // everywhere else peers are listed.
  const itemsFor = (list, includeSelf) => {
    const out = [];
    if (includeSelf) out.push({ value:'__self__', label:'This node ('+(selfDesc.hostname||'local')+')' });
    for (const p of list) out.push({ value:p.node_id, label: p.hostname || p.node_id.slice(0,8) });
    return out;
  };

  (async () => {
    // Always use this node's own frame (its peer list + self address), independent
    // of any remote peer selected in the header — speedtest orchestrates from here.
    let body = {};
    try { const resp = await fetch('/api/cluster', { headers:{'Content-Type':'application/json'} }); body = await resp.json().catch(()=>({})); } catch(e){}
    selfDesc = { ip: body.self_overlay||'', port: body.self_web_port||0, hostname: body.self_hostname||'local', isSelf:true };
    state.selfId = body.self_id || state.selfId; state.selfHostname = selfDesc.hostname;
    selfIsManager = !!body.manager;
    // Sorted alphabetically by hostname, same rule (and now the same order
    // the server itself returns) as the header peer picker — previously this
    // list went straight into fill() in whatever order /api/cluster returned
    // it, which used to reshuffle on every 6s poll (see Engine.ManagedPeers).
    peers = sortPeersByName((body.peers||[]).filter(p => p.manageable).map(p => ({ node_id:p.node_id, hostname:p.hostname, ip:p.overlay, port:p.web_port, manager: !!p.manager })));
    managerPeers = peers.filter(p => p.manager); // filter() preserves order, so this stays sorted too
    const itemsA = itemsFor(managerPeers, selfIsManager), itemsB = itemsFor(peers, true);
    if (itemsA.length) pickA.setValue(itemsA[0].value);
    if (itemsB.length > 1) pickB.setValue(itemsB[1].value); // default the target to the first peer, not this node
    else if (itemsB.length) pickB.setValue(itemsB[0].value);
    exclude(); // also sets runBtn.disabled, and each picker shows its own filter row past DROPDOWN_FILTER_MIN
    if (!peers.length){
      status.style.display = '';
      status.textContent = 'No other managed peers are reachable — at least one is required to run a test between two nodes.';
    } else if (pickA.count() === 0){
      status.style.display = '';
      status.textContent = 'No peer here is in Manager mode, so none can run as the first (initiating) peer — turn on Manager mode for this node or a reachable peer first.';
    }
  })();

  runBtn.onclick = async () => {
    const aVal = pickA.getValue(), bVal = pickB.getValue();
    if (!aVal){ alert('no peer here is in Manager mode — turn on Manager mode for this node or a reachable peer to run a test'); return; }
    if (aVal === bVal){ alert('pick two different nodes'); return; }
    const aDesc = descFor(aVal), bDesc = descFor(bVal);
    const aRemote = aVal !== '__self__';
    if (!bDesc.ip || !bDesc.port){ alert('the target node has no reachable overlay web-admin'); return; }
    if (aRemote && !aDesc.node_id){ alert('the client node is unavailable — try again in a moment'); return; }
    runBtn.disabled = true; const prev = runBtn.textContent; runBtn.textContent = 'Running…';
    status.style.display = ''; status.textContent = 'Running download and upload (~8s)…'; out.innerHTML = '';
    try {
      // The client is peer A: run locally if it's this node, else proxy to A.
      let url = '/api/speedtest/run';
      if (aRemote) url = '/api/proxy?node='+encodeURIComponent(aDesc.node_id)+'&path='+encodeURIComponent('/api/speedtest/run');
      const payload = { target_ip: bDesc.ip, target_port: bDesc.port, target_hostname: bDesc.hostname || '' };
      const resp = await fetch(url, { method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(payload) });
      const body = await resp.json().catch(()=>({}));
      if (!resp.ok || body.error){ status.textContent = body.error || ('test failed ('+resp.status+')'); }
      else { status.style.display='none'; renderSpeedResult(out, body); }
    } catch(e){ status.textContent = 'test failed: '+e; }
    finally { runBtn.disabled = false; runBtn.textContent = prev; }
  };
}

function renderSpeedResult(out, body){
  out.innerHTML = '';
  const to = body.target_hostname ? esc(body.target_hostname) : 'peer';
  out.appendChild(speedGraph('Download (from '+to+')', body.download||{}, 'var(--acc)'));
  out.appendChild(speedGraph('Upload (to '+to+')', body.upload||{}, '#e0883b'));
}

function speedGraph(title, result, color){
  const card = $('<div class="card metric-card"></div>');
  card.appendChild($('<div class="metric-head"><span class="metric-title">'+esc(title)+'</span><span class="metric-now">avg '+esc(fmtMbps(result.avg_mbps||0))+'</span></div>'));
  if (result.error){ card.appendChild($('<div class="hint" style="margin:0">'+esc(result.error)+'</div>')); return card; }
  const holder = $('<div class="chart-holder"></div>');
  holder.innerHTML = speedChartSVG(result.samples||[], color);
  card.appendChild(holder);
  return card;
}

// speedChartSVG mirrors the Metrics chart style, with x = elapsed seconds.
function speedChartSVG(samples, color){
  const W=CH.W, H=CH.H, padL=CH.padL, padR=CH.padR, padT=CH.padT, padB=CH.padB;
  let maxV=0, maxT=0;
  for (const s of samples){ if (s.mbps>maxV) maxV=s.mbps; if (s.t>maxT) maxT=s.t; }
  const yMax = maxV>0 ? maxV*1.15 : 1;
  const win = maxT>0 ? maxT : 1;
  const xs = t => padL + (W-padL-padR)*(t/win);
  const ys = v => padT + (H-padT-padB)*(1 - Math.min(v,yMax)/yMax);
  let g='';
  for (const f of [0,0.5,1]){
    const yy = padT + (H-padT-padB)*(1-f);
    g += '<line x1="'+padL+'" y1="'+yy.toFixed(1)+'" x2="'+(W-padR)+'" y2="'+yy.toFixed(1)+'" stroke="var(--line)" stroke-width="1"/>';
    g += '<text x="'+(padL-8)+'" y="'+(yy+3).toFixed(1)+'" text-anchor="end" font-size="10" fill="var(--mut)">'+esc(fmtMbps(yMax*f))+'</text>';
  }
  g += '<text x="'+padL+'" y="'+(H-6)+'" font-size="10" fill="var(--mut)">0s</text>';
  g += '<text x="'+(W-padR)+'" y="'+(H-6)+'" text-anchor="end" font-size="10" fill="var(--mut)">'+win.toFixed(1)+'s</text>';
  if (samples.length){
    let d='';
    for (let i=0;i<samples.length;i++){ d += (i?'L':'M')+xs(samples[i].t).toFixed(1)+' '+ys(samples[i].mbps).toFixed(1)+' '; }
    g += '<path d="'+d+'" fill="none" stroke="'+color+'" stroke-width="1.5" stroke-linejoin="round"/>';
  } else {
    g += '<text x="'+(W/2)+'" y="'+(H/2)+'" text-anchor="middle" font-size="11" fill="var(--mut)">no samples</text>';
  }
  return '<svg class="chart" viewBox="0 0 '+W+' '+H+'">'+g+'</svg>';
}

function fmtMbps(v){ v = v||0; return (v>=100?v.toFixed(0):v>=10?v.toFixed(1):v.toFixed(2))+' Mbps'; }

// infoCapture is a live tcpdump-style packet capture on a chosen interface of
// whichever node is selected in the header (this node, or a remote peer via
// the proxy), with a line filter and a .pcap export. The download button
// builds its own proxy URL rather than going through api() (window.location
// navigation can't use api()'s fetch-based wrapper), and handleProxy gives
// /api/capture/pcap specifically a higher response-size cap than the rest of
// the API surface, since a well-populated capture buffer can genuinely
// exceed the generic limit.
let captureTimer = null, captureCursor = 0, captureRunning = false;
let captureLines = [];
function infoCapture(c){
  if (captureTimer){ clearInterval(captureTimer); captureTimer = null; }
  const card = $('<div class="card"></div>');
  card.appendChild($('<h3>Packet capture</h3>'));
  card.appendChild($('<div class="hint" style="margin:-4px 0 10px">Live tcpdump-style capture on an interface of this node. Read-only; needs raw-socket privileges. The buffer keeps the most recent ~5000 packets; Download saves a .pcap of what\'s buffered.</div>'));

  const bar = $('<div class="tbar"></div>');
  const sel = $('<select style="padding:5px 9px;font-size:12px;background:var(--bg);color:var(--fg);border:1px solid var(--line);border-radius:6px"></select>');
  const filt = $('<input class="tfilter" type="text" spellcheck="false" placeholder="filter packets…" title="'+esc(filterTitle)+'">');
  const startBtn = $('<button class="sm">Start</button>');
  const clearBtn = $('<button class="ghost sm">Clear</button>');
  const dlBtn = $('<button class="ghost sm">Download</button>');
  bar.appendChild(sel); bar.appendChild(filt); bar.appendChild(startBtn); bar.appendChild(clearBtn); bar.appendChild(dlBtn);
  card.appendChild(bar);
  const pre = $('<pre class="mono-block" style="max-height:58vh;overflow:auto;margin:0"></pre>');
  card.appendChild(pre);
  c.appendChild(card);

  const render = () => {
    const q = filt.value.trim();
    const ast = q ? parseFilterQuery(q) : null;
    pre.textContent = (ast ? captureLines.filter(l => evalFilterAst(ast, l.toLowerCase())) : captureLines).join('\n');
  };
  const scrollEnd = () => { pre.scrollTop = pre.scrollHeight; };
  const nearBottom = () => (pre.scrollHeight - pre.scrollTop - pre.clientHeight) < 40;
  const stopPoll = () => { if (captureTimer){ clearInterval(captureTimer); captureTimer = null; } };
  const setRunning = (on) => {
    captureRunning = on;
    startBtn.textContent = on ? 'Stop' : 'Start';
    startBtn.className = on ? 'danger sm' : 'sm';
    sel.disabled = on;
  };

  const poll = async () => {
    if (state.section!=='capture'){ stopPoll(); return; }
    const r = await api('/api/capture/packets?since='+captureCursor);
    if (!r.ok || !r.body) return;
    if (r.body.cursor!=null) captureCursor = r.body.cursor;
    const pk = r.body.packets||[];
    if (pk.length){
      const stick = nearBottom();
      for (const p of pk) captureLines.push(p.time+'  '+p.summary);
      if (captureLines.length > 4000) captureLines = captureLines.slice(captureLines.length-4000);
      render();
      if (stick) scrollEnd();
    }
    if (!r.body.running && captureRunning){ setRunning(false); stopPoll(); }
  };

  filt.oninput = render;
  startBtn.onclick = async () => {
    if (captureRunning){ await api('/api/capture/stop', {method:'POST'}); setRunning(false); stopPoll(); return; }
    const iface = sel.value;
    if (!iface){ alert('select an interface first'); return; }
    const r = await api('/api/capture/start', {method:'POST', body: JSON.stringify({iface})});
    if (!r.ok || (r.body&&r.body.error)){ alert((r.body&&r.body.error)||'could not start capture'); return; }
    captureLines = []; captureCursor = 0; render();
    setRunning(true);
    stopPoll(); captureTimer = setInterval(poll, 1000); poll();
  };
  clearBtn.onclick = async () => { await api('/api/capture/clear', {method:'POST'}); captureLines = []; render(); };
  dlBtn.onclick = () => {
    // Direct navigation, not api(): a fetch-based wrapper can't drive
    // window.location. Built by hand instead, mirroring what api() would do.
    window.location = state.target
      ? '/api/proxy?node='+encodeURIComponent(state.target)+'&path='+encodeURIComponent('/api/capture/pcap')
      : '/api/capture/pcap';
  };

  (async () => {
    const ri = await api('/api/capture/interfaces');
    if (ri.ok && ri.body && ri.body.supported===false){
      bar.style.display = 'none';
      card.appendChild($('<div class="hint">Packet capture isn\u2019t available on this host.</div>'));
      return;
    }
    if (ri.ok && ri.body){
      for (const ifc of (ri.body.interfaces||[])){
        const o = document.createElement('option');
        o.value = ifc.name; o.textContent = ifc.name + (ifc.up?'':' (down)');
        sel.appendChild(o);
      }
    }
    // Re-sync with any capture already running on this node.
    const r = await api('/api/capture/packets?since=0');
    if (r.ok && r.body){
      captureLines = (r.body.packets||[]).map(p => p.time+'  '+p.summary);
      captureCursor = r.body.cursor||0;
      if (r.body.iface){ for (const o of sel.options){ if (o.value===r.body.iface) sel.value = r.body.iface; } }
      render(); scrollEnd();
      setRunning(!!r.body.running);
      if (r.body.running){ stopPoll(); captureTimer = setInterval(poll, 1000); }
    }
  })();
}

// infoRoutes shows the host kernel routing table, read live (not from config).
function infoRoutes(c){
  const card = $('<div class="card"></div>');
  card.appendChild($('<h3>Local routing table</h3>'));
  card.appendChild($('<div class="hint" style="margin:-4px 0 10px">The host kernel routing table on this node, read live and independent of config. Entries pointing at a gravinet interface are the ones installed for the mesh.</div>'));
  const body = $('<div></div>'); body.innerHTML = '<div class="hint">loading\u2026</div>'; card.appendChild(body); c.appendChild(card);
  (async () => {
    const r = await api('/api/localroutes');
    if (!r.ok || !r.body){ body.innerHTML = '<div class="hint">could not read the routing table.</div>'; return; }
    const ent = r.body.entries||[];
    if (!ent.length && r.body.text){ body.innerHTML=''; const pre=$('<pre class="mono-block"></pre>'); pre.textContent=r.body.text; body.appendChild(pre); addLineFilter(body, pre, r.body.text); return; }
    if (!ent.length){ body.innerHTML = '<div class="hint">no routes found'+(r.body.error?(': '+esc(r.body.error)):'')+'.</div>'; return; }
    let h = '<table><tr><th>destination</th><th>gateway</th><th>iface</th><th>metric</th><th>family</th></tr>';
    for (const e of ent) h += '<tr><td>'+esc(e.dest)+'</td><td>'+esc(e.gateway||'\u2013')+'</td><td>'+esc(e.iface)+'</td><td>'+esc(e.metric)+'</td><td>'+(e.family===6?'IPv6':'IPv4')+'</td></tr>';
    body.innerHTML = h+'</table>';
    enhanceTable(body.querySelector('table')); // async render missed renderSection's pass
  })();
}

// secBgp is the BGP/BFD control panel: gravinet owns the configuration and
// drives the FRR daemon from it. The top card is an editor (local AS, router
// id, redistribution, neighbors — each with its own BFD toggle — advertised
// networks) that POSTs to /api/bgp/config, which persists the config and
// reconciles FRR (render frr.conf, sync the daemon set, reload FRR). The
// bottom card shows the live peer table FRR reports (via vtysh), read-only.
// The whole section is only reachable when state.bgpSupported is true (vtysh
// present); see sectionVisible().
function secBgp(c){
  secHint(c, 'BGP configuration for dynamic routing. For neighbors and advertised networks: use + to add a row, double-click a field to edit it (double-click BFD to toggle it), tick rows and \u2212 to remove. Click the \ud83d\udc41\ufe0f next to a neighbor\u2019s MD5 password to reveal or mask it.');
  const editWrap = $('<div></div>'); c.appendChild(editWrap);

  const fail = (msg) => {
    editWrap.innerHTML = '';
    const card = $('<div class="card"></div>');
    card.appendChild($('<div class="hint">Could not load BGP configuration: '+esc(msg)+'</div>'));
    const retry = $('<button class="sm" style="margin-top:10px">Retry</button>');
    retry.onclick = load;
    card.appendChild(retry);
    editWrap.appendChild(card);
  };

  async function load(){
    editWrap.innerHTML = '<div class="card"><div class="hint">loading configuration\u2026</div></div>';
    let r;
    try {
      // A bounded request: the config GET reads local disk and never touches
      // FRR, so it should be near-instant — if it hasn't answered in 12s
      // something is wrong and we must say so rather than spin on
      // "loading…" forever.
      r = await withTimeout(api('/api/bgp/config'), 12000);
    } catch (e){
      // Request rejected or timed out — surface the reason instead of hanging.
      fail((e && e.message) || 'request failed'); return;
    }
    if (state.section !== 'bgp') return; // navigated away while it ran
    if (!r || !r.ok || !r.body){
      fail((r && r.body && r.body.error) || ('server returned ' + ((r && r.status) || 'no response'))); return;
    }
    // Render the editor from the stored config — this never waits on FRR, so the
    // page is usable at once. Guard the render: a bug in here used to throw and
    // leave the "loading…" placeholder up with no hint why, which looked exactly
    // like a hung request. Now the actual error is shown.
    try {
      renderBgpEditor(editWrap, r.body.bgp || {}, !!r.body.installed, false);
    } catch (e){
      fail('editor error \u2014 ' + ((e && e.message) || e)); return;
    }
    // If gravinet isn't managing BGP yet but FRR is reachable, reflect the live
    // FRR config by importing it in the background. Best-effort: the editor is
    // already on screen, so any failure here must never take it back down.
    try {
      if (!r.body.active && (r.body.supported || r.body.installed)){
        const im = await api('/api/bgp/import');
        if (state.section !== 'bgp') return; // navigated away while it ran
        if (im.ok && im.body && im.body.imported && im.body.bgp){
          renderBgpEditor(editWrap, im.body.bgp, true, true);
        }
        // Nothing to reflect otherwise (no existing FRR BGP config found, or
        // the import itself failed) — the editor already rendered from the
        // stored config is left as-is, with no extra banner about it.
      }
    } catch (e){ /* editor already rendered; import reflection is best-effort */ }
  }
  load();
}

// secBgpPeers is the Monitor › BGP Peers view: the live BGP session table FRR
// reports, read-only — the routing analogue of Monitor › Route Table. It's kept
// separate from the Traffic › BGP editor so "configure" and "observe" stay
// cleanly split, matching the rest of the app (Traffic holds config editors;
// Monitor holds live read-only state). Gated on vtysh presence, same as the
// editor.
function secBgpPeers(c){
  secHint(c, 'Live BGP and BFD session status as reported by FRR on this host. Read-only \u2014 configure BGP under Traffic \u203a BGP.');
  const card = $('<div class="card"></div>');
  card.appendChild($('<h3>BGP Neighbors</h3>'));
  const meta = $('<div class="hint" style="margin:-4px 0 10px" id="bgp-live-meta"></div>'); card.appendChild(meta);
  const body = $('<div id="bgp-live-body"></div>'); body.innerHTML = '<div class="hint">loading\u2026</div>'; card.appendChild(body);
  c.appendChild(card);
  bgpLiveStatus(meta, body);

  // A BFD session can back a BGP neighbor, an OSPF adjacency, or a monitored
  // static route — it isn't itself BGP-specific — so it gets its own card
  // rather than being folded into the BGP peers table above, even though
  // both live on this same Monitor page and both come from FRR via vtysh.
  const bfdCard = $('<div class="card"></div>');
  bfdCard.appendChild($('<h3>BFD Neighbors</h3>'));
  const bfdBody = $('<div id="bfd-live-body"></div>'); bfdBody.innerHTML = '<div class="hint">loading\u2026</div>'; bfdCard.appendChild(bfdBody);
  c.appendChild(bfdCard);
  bfdLiveStatus(bfdBody);

  // The full BGP table (every prefix, next hop, AS path, and status code FRR
  // holds) — a level below the per-peer summary above. Its own card, same
  // reasoning as BFD Neighbors: a different FRR command ('show bgp', plain
  // text, no JSON form) and a different shape (a route dump, not a session
  // list), so it doesn't fold into the peers table.
  const tableCard = $('<div class="card"></div>');
  tableCard.appendChild($('<h3>BGP Table</h3>'));
  const tableBody = $('<div id="bgp-table-live-body"></div>'); tableBody.innerHTML = '<div class="hint">loading\u2026</div>'; tableCard.appendChild(tableBody);
  c.appendChild(tableCard);
  bgpTableLiveStatus(tableBody);
}

// bfdLiveStatus fills the BFD Neighbors card body with FRR's live BFD
// session table (GET /api/bfd), degrading to an explanatory line when FRR
// isn't answering — same shape and reasoning as bgpLiveStatus, just against
// bfdd instead of bgpd.
async function bfdLiveStatus(body){
  const r = await api('/api/bfd');
  if (!r.ok || !r.body){ body.innerHTML = '<div class="hint">could not read BFD status.</div>'; return; }
  if (r.body.available === false){
    body.innerHTML = '<div class="empty">'+esc(r.body.reason || 'BFD status is unavailable.')+'</div>';
    return;
  }
  const peers = r.body.peers || [];
  if (!peers.length){ body.innerHTML = '<div class="empty">No BFD sessions yet.</div>'; return; }
  // show bfd peers json reports uptime/downtime as raw seconds-elapsed, not a
  // pre-formatted string the way FRR's BGP summary reports peerUptime — so
  // this converts to fmtElapsed's expected shape (a nanosecond-scale
  // timestamp to diff against now) rather than duplicating its d/h/m/s logic.
  const durAgo = (secs) => fmtElapsed((Date.now() - (secs||0)*1000) * 1e6);
  let h = '<table><tr><th>peer</th><th>local</th><th>interface</th><th>state</th><th>up/down time</th><th>diagnostic</th></tr>';
  for (const p of peers){
    const est = (p.status||'').toLowerCase() === 'up';
    const stateCell = '<span class="pill" style="'+(est?'color:var(--ok);border-color:var(--ok)':'color:var(--mut)')+'">'+esc(p.status||'\u2013')+'</span>';
    const time = est ? durAgo(p.uptime) : ((p.status||'').toLowerCase()==='down' ? durAgo(p.downtime) : '\u2013');
    h += '<tr><td>'+esc(p.peer||'\u2013')+'</td><td>'+esc(p.local||'\u2013')+'</td><td>'+esc(p.interface||'\u2013')+
      '</td><td>'+stateCell+'</td><td>'+time+'</td><td>'+esc(p.diagnostic||'\u2013')+'</td></tr>';
  }
  body.innerHTML = h+'</table>';
  enhanceTable(body.querySelector('table'));
}

// bgpStateLabel maps FRR's raw BGP FSM state to what's shown in the BGP
// Neighbors table. "Active" is technically correct — the FSM is retrying the
// TCP connection to the peer — but reads as if something's actively working,
// when what it actually means is simply that the session isn't up. Every
// other non-Established state already reads as clearly-not-up on its own
// (Idle, Connect, OpenSent, OpenConfirm), so Active is the one state
// substituted with a plain "down" instead. The real FSM state is still
// available as a tooltip on the pill (see its call site) for anyone who
// wants the precise value.
function bgpStateLabel(state){
  return (state||'').toLowerCase() === 'active' ? 'down' : (state || '\u2013');
}

// bgpLiveStatus fills a meta line and body with FRR's live BGP peer table
// (GET /api/bgp), degrading to an explanatory line when FRR isn't answering.
async function bgpLiveStatus(meta, body){
  const r = await api('/api/bgp');
  if (!r.ok || !r.body){ meta.textContent=''; body.innerHTML = '<div class="hint">could not read BGP status.</div>'; return; }
  if (r.body.available === false){
    meta.textContent='';
    body.innerHTML = '<div class="empty">'+esc(r.body.reason || 'BGP status is unavailable.')+'</div>';
    return;
  }
  const rid = r.body.router_id || '', las = r.body.local_as || 0;
  meta.innerHTML = ((las ? 'local AS <b>'+esc(String(las))+'</b>' : '') +
    (las && rid ? ' \u00b7 ' : '') + (rid ? 'router id <b>'+esc(rid)+'</b>' : '')) ||
    'FRR is reachable.';
  const peers = r.body.peers || [];
  if (!peers.length){ body.innerHTML = '<div class="empty">No BGP peers yet.</div>'; return; }
  let h = '<table><tr><th>peer</th><th>remote AS</th><th>state</th><th>uptime</th><th>prefixes</th><th>family</th></tr>';
  for (const p of peers){
    const rawState = p.state || '';
    const est = rawState.toLowerCase() === 'established';
    const label = bgpStateLabel(rawState);
    const stateCell = '<span class="pill" style="'+(est?'color:var(--ok);border-color:var(--ok)':'color:var(--mut)')+'"'
      + (label !== rawState && rawState ? ' title="FRR reports this as \u2018'+esc(rawState)+'\u2019"' : '')
      + '>'+esc(label)+'</span>';
    const peerCell = '<span class="peer-link" data-peer="'+esc(p.peer)+'" title="show this neighbor\u2019s definition under Traffic \u2192 BGP">'+esc(p.peer)+'</span>';
    h += '<tr><td>'+peerCell+'</td><td>'+esc(String(p.remote_as||'\u2013'))+'</td><td>'+stateCell+
      '</td><td>'+esc(p.uptime||'\u2013')+'</td><td>'+esc(String(p.prefixes_received!=null?p.prefixes_received:'\u2013'))+
      '</td><td>'+(p.afi==='ipv6Unicast'?'IPv6':'IPv4')+'</td></tr>';
  }
  body.innerHTML = h+'</table>';
  const tbl = body.querySelector('table');
  enhanceTable(tbl);
  // Clicking a peer address jumps to that neighbor's row under Traffic > BGP
  // (see gotoBgpNeighbor), following the same peer-link pattern used for
  // mesh-peer names elsewhere. Wired after enhanceTable, which only reorders
  // existing nodes so the handler survives a sort.
  tbl.querySelectorAll('.peer-link').forEach(el => {
    el.onclick = (e) => { e.stopPropagation(); gotoBgpNeighbor(el.dataset.peer); };
  });
}

// bgpTableLiveStatus fills the BGP Table card with the raw text of FRR's
// 'show bgp' (GET /api/bgp/table) — the full prefix/next-hop/AS-path table,
// not just the per-peer summary bgpLiveStatus shows. 'show bgp' has no JSON
// form, so this is rendered verbatim in a <pre> (same mono-block pattern as
// infoHosts' raw hosts-file view) rather than reparsed into columns, with the
// same line-filter box for finding a prefix in a large table.
async function bgpTableLiveStatus(body){
  const r = await api('/api/bgp/table');
  if (!r.ok || !r.body){ body.innerHTML = '<div class="hint">could not read the BGP table.</div>'; return; }
  if (r.body.available === false){
    body.innerHTML = '<div class="empty">'+esc(r.body.reason || 'BGP table is unavailable.')+'</div>';
    return;
  }
  const text = r.body.text || '';
  if (!text.trim()){ body.innerHTML = '<div class="empty">BGP table is empty.</div>'; return; }
  body.innerHTML = '';
  const pre = $('<pre class="mono-block"></pre>'); pre.textContent = text; body.appendChild(pre);
  addLineFilter(body, pre, text);
}

// renderBgpEditor builds the editable BGP/BFD form into host from the stored
// config b. Neighbors and networks are held in local arrays kept in sync with
// their input fields, so Save gathers a clean object to POST. installed=false
// (FRR absent) still lets the operator author config — it just warns it won't
// be applied until FRR is present. imported=true means b was read from FRR's
// running config (gravinet isn't managing BGP yet) rather than gravinet's own
// stored config — see isNewCfg below, which uses it to tell "freshly imported"
// from "genuinely never configured" so the debounce timing feels right either way.
function renderBgpEditor(host, b, installed, imported){
  const neighbors = (b.neighbors || []).map(n => ({
    peer: n.peer||'', remote_as: n.remote_as||0, description: n.description||'',
    password: n.password||'', bfd: !!n.bfd, shutdown: !!n.shutdown,
  }));
  const networks = (b.networks || []).slice();

  const card = $('<div class="card"></div>');
  if (!installed){
    card.appendChild($('<div class="empty" style="margin-bottom:10px">FRR is not installed on this host, so a saved configuration is stored but not applied to a running daemon. Install FRR to have gravinet bring these sessions up.</div>'));
  }

  const rowTog = (labelText, desc, checked) => {
    const row = $('<div class="settings-row"></div>');
    row.appendChild($('<div><div class="settings-label">'+esc(labelText)+'</div><div class="settings-desc">'+desc+'</div></div>'));
    const sw = $('<label class="sw"><input type="checkbox"><span class="sw-slider"></span></label>');
    if (checked) sw.querySelector('input').checked = true;
    row.appendChild(sw); card.appendChild(row);
    return sw.querySelector('input');
  };
  const rowInput = (labelText, desc, value, placeholder, width) => {
    const row = $('<div class="settings-row"></div>');
    row.appendChild($('<div><div class="settings-label">'+esc(labelText)+'</div><div class="settings-desc">'+desc+'</div></div>'));
    const inp = $('<input type="text" style="width:'+(width||220)+'px">');
    inp.value = value==null ? '' : String(value); inp.placeholder = placeholder||'';
    row.appendChild(inp); card.appendChild(row);
    return inp;
  };

  const enableCb = rowTog('Enable BGP', 'Render and run a <code>router bgp</code> speaker on this host. Off leaves no BGP block in FRR\u2019s config and switches bgpd off.', !!b.enabled);
  const asnInp = rowInput('Local AS number', 'This node\u2019s autonomous-system number, e.g. 65001. Required to enable BGP.', b.asn||'', 'e.g. 65001', 180);
  const ridInp = rowInput('Router-id', 'BGP router-id (an IPv4-style id), e.g. 10.0.0.1. Optional \u2014 FRR picks one if left blank.', b.router_id||'', 'e.g. 10.0.0.1', 180);
  // isNewCfg still drives the session-timer defaults below; BFD itself has no
  // global toggle (see nbrAddRow's own default for how a brand-new neighbor
  // row gets BFD on).
  const isNewCfg = !imported && !b.enabled && !b.asn && !(b.neighbors && b.neighbors.length);
  const rcCb = rowTog('Redistribute connected', 'Advertise directly-connected routes into BGP.', !!b.redistribute_connected);
  const rsCb = rowTog('Redistribute static', 'Advertise static routes into BGP.', !!b.redistribute_static);
  // Session timers. A new config defaults to a fast 4s/12s (FRR's own default is
  // a sluggish 60s/180s); an existing/imported config shows its actual values,
  // blank meaning "FRR default".
  const kaDefault = isNewCfg ? 4 : (b.keepalive_time || '');
  const holdDefault = isNewCfg ? 12 : (b.hold_time || '');
  const kaInp = rowInput('Keepalive timer (seconds)', 'How often to send BGP keepalives. Default 4s.', kaDefault, 'e.g. 4', 100);
  const holdInp = rowInput('Hold timer (seconds)', 'Silence before a session is declared down; must exceed keepalive (3\u00d7 is conventional). Default 12s.', holdDefault, 'e.g. 12', 100);


  // ---- neighbors ----
  // Table styling matches every other list-editing section in the app
  // (Networks/Keys/Seeds/Firewall/NAT/QoS): a selcol checkbox column, +/- on
  // the toolbar enhanceTable renders from table._rowAdd/_rowRemove, and
  // double-click a field to edit a row in place with save/cancel — rather
  // than the old model of every cell being a permanently-live, per-keystroke
  // autosaving input. BFD is a separate double-click-to-toggle tag (same
  // pattern as NAT/QoS's rule state column), not part of the edit form.
  //
  // The MD5 password is masked by default with a show/hide toggle, right in
  // the row — no round trip needed to reveal it, unlike Keys' masked keys:
  // the plaintext is already sitting in this array (the GET that populated
  // it sent it in cleartext), the mask is purely a display choice.
  const nbrSec = $('<div style="margin-top:16px;border-top:1px solid var(--line);padding-top:12px"></div>');
  nbrSec.appendChild($('<div class="settings-label" style="margin-bottom:8px">Neighbors</div>'));
  const nbrBody = $('<div></div>'); nbrSec.appendChild(nbrBody);
  card.appendChild(nbrSec);

  const nbrPwCell = (n) => n.password
    ? '<span class="kval masked nbr-pw-val">\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022</span> <button class="ghost sm nbr-pw-toggle" title="show this neighbor\u2019s MD5 password">\ud83d\udc41\ufe0f</button>'
    : '<span class="hint">none</span>';

  function renderNbrs(){
    nbrBody.innerHTML = '';
    let h = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>peer address</th><th>remote AS</th><th>description</th>'
      + '<th title="MD5 session password">MD5 password</th><th title="Bidirectional Forwarding Detection for this peer">BFD</th>'
      + '<th title="administrative state of this session">state</th></tr>';
    if (!neighbors.length) h += '<tr><td colspan="7" class="empty">No neighbors \u2014 click + to define a BGP peer.</td></tr>';
    else neighbors.forEach((n, i) => {
      const bfdOn = !!n.bfd;
      const shutdown = !!n.shutdown;
      h += '<tr class="nbrrow" data-idx="'+i+'" data-peer="'+esc(n.peer||'')+'">'
        + '<td class="selcol"><input type="checkbox" class="selbox"></td>'
        + '<td class="nbr-field nbr-peer-cell">'+esc(n.peer||'')+'</td>'
        + '<td class="nbr-field nbr-as-cell">'+esc(String(n.remote_as||''))+'</td>'
        + '<td class="nbr-field nbr-desc-cell">'+esc(n.description||'')+'</td>'
        + '<td class="nbr-pw-cell">'+nbrPwCell(n)+'</td>'
        + '<td><span class="tag-toggle '+(bfdOn?'on':'off')+'" data-nbrbfd="1" title="double-click to '+(bfdOn?'disable':'enable')+' BFD for this peer">'+(bfdOn?'on':'off')+'</span></td>'
        + '<td><span class="tag-toggle '+(shutdown?'off':'on')+'" data-nbrstate="1" title="double-click to '+(shutdown?'enable':'disable')+' this neighbor">'+(shutdown?'disabled':'enabled')+'</span></td></tr>';
    });
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; nbrBody.appendChild(t);
    const table = t.querySelector('table');

    t.querySelectorAll('tr.nbrrow').forEach(tr => {
      tr.querySelectorAll('.nbr-field').forEach(td => { td.title = 'double-click to edit'; td.ondblclick = () => startNbrEdit(tr); });
    });
    t.querySelectorAll('[data-nbrbfd]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        const i = parseInt(tag.closest('tr').dataset.idx, 10);
        const on = !neighbors[i].bfd;
        neighbors[i].bfd = on;
        tag.className = 'tag-toggle ' + (on?'on':'off'); tag.textContent = on?'on':'off';
        tag.title = 'double-click to '+(on?'disable':'enable')+' BFD for this peer';
        scheduleSave(true);
      };
    });
    // Administrative state — disabling emits 'neighbor <peer> shutdown' in
    // FRR's config (the session stays fully configured, just held down),
    // enabling removes that line. Same double-click-to-toggle, immediate-save
    // shape as BFD above and as every other table's state column in the app
    // (NAT/QoS rule state, etc.) — the state field the user pointed to as the
    // pattern to follow.
    t.querySelectorAll('[data-nbrstate]').forEach(tag => {
      tag.ondblclick = (e) => {
        e.stopPropagation();
        const i = parseInt(tag.closest('tr').dataset.idx, 10);
        const shuttingDown = !neighbors[i].shutdown;
        neighbors[i].shutdown = shuttingDown;
        tag.className = 'tag-toggle ' + (shuttingDown?'off':'on'); tag.textContent = shuttingDown?'disabled':'enabled';
        tag.title = 'double-click to '+(shuttingDown?'enable':'disable')+' this neighbor';
        scheduleSave(true);
      };
    });
    // Purely client-side reveal — see nbrPwCell's comment on why no fetch is
    // needed here — just swaps the masked span for the real value and back.
    t.querySelectorAll('.nbr-pw-toggle').forEach(btn => {
      btn.onclick = (e) => {
        e.stopPropagation();
        const i = parseInt(btn.closest('tr').dataset.idx, 10);
        const n = neighbors[i]; if (!n) return;
        const span = btn.previousElementSibling;
        if (span.classList.contains('masked')){
          span.textContent = n.password; span.classList.remove('masked');
          btn.title = 'hide this neighbor\u2019s MD5 password';
        } else {
          span.textContent = '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022'; span.classList.add('masked');
          btn.title = 'show this neighbor\u2019s MD5 password';
        }
      };
    });
    selAllWire(t);
    table._rowAdd = () => nbrAddRow(table);
    table._rowRemove = () => {
      const rows = selCheckedRows(table);
      if (!rows.length){ alert('tick one or more rows to remove'); return; }
      const idxs = rows.map(tr => parseInt(tr.dataset.idx, 10)).sort((a,b) => b-a);
      for (const idx of idxs) neighbors.splice(idx, 1);
      renderNbrs(); scheduleSave(true);
    };
    enhanceTable(table); // secBgp's tables are built async, outside renderSection()'s own blanket enhanceTable pass, so this has to happen here explicitly.
  }

  // wireNbrForm wires the shared peer/AS/description/password edit form —
  // used by both a brand-new row (idx null, appended on save) and an
  // existing row being edited in place (idx is its position in neighbors).
  function wireNbrForm(tr, idx){
    const pwInp = tr.querySelector('.nbre-pw'), pwToggle = tr.querySelector('.nbre-pw-toggle');
    pwToggle.onclick = (e) => {
      e.stopPropagation();
      const showing = pwInp.type === 'text';
      pwInp.type = showing ? 'password' : 'text';
      pwToggle.title = showing ? 'show while editing' : 'hide while editing';
    };
    tr.querySelector('.nbre-cancel').onclick = () => renderNbrs();
    tr.querySelector('.nbre-save').onclick = () => {
      const peer = tr.querySelector('.nbre-peer').value.trim();
      const remote_as = parseInt(tr.querySelector('.nbre-as').value, 10) || 0;
      if (!peer || remote_as <= 0){ alert('peer address and remote AS are required'); return; }
      const entry = {
        peer, remote_as,
        description: tr.querySelector('.nbre-desc').value.trim(),
        password: pwInp.value,
        // New neighbors default to BFD on (sub-second failure detection is
        // the better baseline, and there's no global toggle to inherit it
        // from any more — see BGPConfig's doc comment); an existing
        // neighbor being edited keeps whatever it actually has.
        bfd: idx != null ? neighbors[idx].bfd : true,
        shutdown: idx != null ? neighbors[idx].shutdown : false,
      };
      if (idx != null) neighbors[idx] = entry; else neighbors.push(entry);
      renderNbrs(); scheduleSave(true);
    };
  }
  function startNbrEdit(tr){
    if (tr.querySelector('.nbre-peer')) return; // already editing
    const idx = parseInt(tr.dataset.idx, 10), n = neighbors[idx];
    tr.querySelector('.nbr-peer-cell').innerHTML = '<input class="nbre-peer" style="width:130px" placeholder="10.0.0.2" autocomplete="off" value="'+esc(n.peer||'')+'">';
    tr.querySelector('.nbr-as-cell').innerHTML = '<input class="nbre-as" style="width:80px" placeholder="65002" autocomplete="off" value="'+esc(String(n.remote_as||''))+'">';
    tr.querySelector('.nbr-desc-cell').innerHTML = '<input class="nbre-desc" style="width:150px" placeholder="optional" autocomplete="off" value="'+esc(n.description||'')+'">';
    tr.querySelector('.nbr-pw-cell').innerHTML = '<input class="nbre-pw" type="password" style="width:90px" placeholder="optional" autocomplete="off" value="'+esc(n.password||'')+'"> '
      + '<button class="ghost sm nbre-pw-toggle" title="show while editing">\ud83d\udc41\ufe0f</button> <button class="sm nbre-save">save</button> <button class="ghost sm nbre-cancel">cancel</button>';
    wireNbrForm(tr, idx);
  }
  function nbrAddRow(table){
    const tr = document.createElement('tr');
    tr.innerHTML = '<td class="selcol"></td>'
      + '<td><input class="nbre-peer" style="width:130px" placeholder="10.0.0.2" autocomplete="off"></td>'
      + '<td><input class="nbre-as" style="width:80px" placeholder="65002" autocomplete="off"></td>'
      + '<td><input class="nbre-desc" style="width:150px" placeholder="optional" autocomplete="off"></td>'
      + '<td><input class="nbre-pw" type="password" style="width:90px" placeholder="optional" autocomplete="off"> <button class="ghost sm nbre-pw-toggle" title="show while editing">\ud83d\udc41\ufe0f</button> <button class="sm nbre-save">save</button> <button class="ghost sm nbre-cancel">cancel</button></td>'
      + '<td><span class="hint">on</span></td>'
      + '<td><span class="hint">enabled</span></td>';
    if (!insertNewRow(table, tr)) return;
    wireNbrForm(tr, null);
  }
  renderNbrs();

  // ---- advertised networks ----
  const netSec = $('<div style="margin-top:16px;border-top:1px solid var(--line);padding-top:12px"></div>');
  netSec.appendChild($('<div class="settings-label" style="margin-bottom:8px">Advertised networks</div>'));
  const netBody = $('<div></div>'); netSec.appendChild(netBody);
  card.appendChild(netSec);

  function renderNets(){
    netBody.innerHTML = '';
    let h = '<table><tr><th class="selcol"><input type="checkbox" class="selall"></th><th>network prefix</th></tr>';
    if (!networks.length) h += '<tr><td colspan="2" class="empty">No advertised networks \u2014 click + to add a prefix (e.g. 10.0.0.0/24) to originate into BGP.</td></tr>';
    else networks.forEach((net, i) => {
      h += '<tr data-idx="'+i+'"><td class="selcol"><input type="checkbox" class="selbox"></td>'
        + '<td class="netg-cell" title="double-click to edit">'+esc(net||'')+'</td></tr>';
    });
    const t = $('<div></div>'); t.innerHTML = h+'</table>'; netBody.appendChild(t);
    const table = t.querySelector('table');
    t.querySelectorAll('td.netg-cell').forEach(td => td.ondblclick = () =>
      inlineCellEdit(td, td.textContent, 'e.g. 10.0.0.0/24', (v, prev) => {
        const idx = parseInt(td.closest('tr').dataset.idx, 10);
        if (v === prev){ renderNets(); return; }
        if (!v) networks.splice(idx, 1); else networks[idx] = v;
        renderNets(); scheduleSave(true);
      }));
    selAllWire(t);
    table._rowAdd = () => netAddRow(table);
    table._rowRemove = () => {
      const rows = selCheckedRows(table);
      if (!rows.length){ alert('tick one or more rows to remove'); return; }
      const idxs = rows.map(tr => parseInt(tr.dataset.idx, 10)).sort((a,b) => b-a);
      for (const idx of idxs) networks.splice(idx, 1);
      renderNets(); scheduleSave(true);
    };
    enhanceTable(table);
  }
  function netAddRow(table){
    const tr = document.createElement('tr');
    tr.innerHTML = '<td class="selcol"></td><td><input class="nete-val" style="width:220px" placeholder="e.g. 10.0.0.0/24"> <button class="sm nete-save">save</button> <button class="ghost sm nete-cancel">cancel</button></td>';
    if (!insertNewRow(table, tr)) return;
    tr.querySelector('.nete-cancel').onclick = () => renderNets();
    tr.querySelector('.nete-save').onclick = () => {
      const v = tr.querySelector('.nete-val').value.trim();
      if (!v){ alert('enter a network prefix'); return; }
      networks.push(v);
      renderNets(); scheduleSave(true);
    };
  }
  renderNets();

  // ---- autosave ----
  // No Save button: like every other form in the app, edits persist — and apply
  // to FRR — automatically. The scalar fields above debounce so we don't POST
  // on every keystroke; toggles and neighbor/network add/edit/remove save at
  // once. An intermediate invalid state (BGP enabled with no AS, or hold <=
  // keepalive) is held back with an inline hint instead of POSTed, and the
  // next valid edit saves it — so autosave never pushes a config FRR would
  // reject.
  const status = $('<div class="hint" style="margin-top:16px;border-top:1px solid var(--line);padding-top:12px"></div>');
  card.appendChild(status);

  let saveTimer = null, saveSeq = 0;
  function scheduleSave(immediate){
    if (saveTimer){ clearTimeout(saveTimer); saveTimer = null; }
    if (immediate){ doSave(); return; }
    saveTimer = setTimeout(doSave, 700);
  }
  async function doSave(){
    saveTimer = null;
    const asn = parseInt(asnInp.value, 10) || 0;
    const ka = parseInt(kaInp.value, 10) || 0;
    const hold = parseInt(holdInp.value, 10) || 0;
    // Hold back invalid intermediate states rather than POSTing a guaranteed error.
    if (enableCb.checked && asn <= 0){
      status.style.color = ''; status.textContent = 'Enter a local AS number to enable BGP.'; return;
    }
    if (enableCb.checked && hold > 0 && hold <= ka){
      status.style.color = ''; status.textContent = 'Hold timer must be greater than the keepalive timer (e.g. 4 and 12).'; return;
    }
    const payload = {
      enabled: enableCb.checked,
      asn: asn,
      router_id: ridInp.value.trim(),
      redistribute_connected: rcCb.checked,
      redistribute_static: rsCb.checked,
      keepalive_time: ka,
      hold_time: hold,
      // Drop blank rows so what's stored matches what FRR would accept.
      neighbors: neighbors.filter(n => n.peer && n.remote_as > 0)
        .map(n => ({ peer:n.peer, remote_as:n.remote_as, description:n.description||'', password:n.password||'', bfd:!!n.bfd, shutdown:!!n.shutdown })),
      networks: networks.map(s => (s||'').trim()).filter(s => s.length),
    };
    const seq = ++saveSeq;
    status.style.color = ''; status.textContent = 'saving\u2026';
    const r = await api('/api/bgp/config', { method:'POST', body: JSON.stringify(payload) });
    if (seq !== saveSeq) return; // a newer edit already superseded this save
    if (!r.ok){ status.style.color = 'var(--danger)'; status.textContent = (r.body && r.body.error) || 'Save failed.'; return; }
    status.style.color = 'var(--ok)';
    // Saving applies the config the same as every other form here — there is
    // no separate "apply" step. The server's note describing what reaching
    // FRR did (daemon count, reload vs restart, or that FRR is absent) isn't
    // shown here — it's routine detail on every single save, and the one
    // case actually worth surfacing (FRR not installed) already has its own
    // persistent banner elsewhere in this card (see the !installed check
    // above).
    status.textContent = 'Saved.';
  }

  // Toggles and structural changes apply at once; the four text fields debounce.

  enableCb.onchange = () => scheduleSave(true);
  rcCb.onchange = () => scheduleSave(true);
  rsCb.onchange = () => scheduleSave(true);
  [asnInp, ridInp, kaInp, holdInp].forEach(inp => { inp.oninput = () => scheduleSave(false); });

  // Attach the finished editor, replacing the "loading…" placeholder. This is
  // the line whose loss in v485 left the BGP section stuck on "loading
  // configuration…" — the whole form was built but never inserted.
  host.innerHTML = ''; host.appendChild(card);
  applyPendingBgpHighlight(card);
}

// infoHosts shows the local hosts file contents, read live from disk.
function infoHosts(c){
  const card = $('<div class="card"></div>');
  card.appendChild($('<h3>Local hosts file</h3>'));
  card.appendChild($('<div class="hint" style="margin:-4px 0 10px">This host\'s hosts file, including the gravinet-managed block (peer hostnames and advertised records). Read live from disk.</div>'));
  const body = $('<div></div>'); body.innerHTML = '<div class="hint">loading\u2026</div>'; card.appendChild(body); c.appendChild(card);
  (async () => {
    const r = await api('/api/localhosts');
    if (!r.ok || !r.body){ body.innerHTML = '<div class="hint">could not read the hosts file.</div>'; return; }
    if (r.body.error){ body.innerHTML = '<div class="hint">could not read '+esc(r.body.path||'')+': '+esc(r.body.error)+'</div>'; return; }
    body.innerHTML='';
    // Blank lines add nothing to read here and just add scroll — strip them
    // for display only; /api/localhosts still returns the file verbatim.
    const text = (r.body.text||'').split('\n').filter(l => l.trim()!=='').join('\n');
    const pre = $('<pre class="mono-block"></pre>'); pre.textContent = text || '(empty)'; body.appendChild(pre);
    if (text) addLineFilter(body, pre, text);
    if (r.body.path){ body.appendChild($('<div class="hint" style="margin:14px 0 0;padding-top:10px;border-top:1px solid var(--line)"><span class="net-id">'+esc(r.body.path)+'</span></div>')); }
  })();
}

// infoDNS shows what's actually registered with this host's OS resolver right
// now, per network — read live (resolvectl on Linux, /etc/resolver on macOS,
// NRPT on Windows), not from anything gravinet remembers applying, so it
// reflects reality even if a sync silently failed. This is the direct way to
// answer "is conditional forwarding actually working" without a shell.
function infoDNS(c){
  const card = $('<div class="card"></div>');
  card.appendChild($('<h3>Conditional DNS forwarding: live state</h3>'));
  card.appendChild($('<div class="hint" style="margin:-4px 0 10px">What\'s actually registered with this host\'s OS resolver right now, per network. Read live from the OS, not from gravinet\'s own records; if this is empty or missing an entry you expect, the last sync failed or hasn\'t happened yet; check the DNS section\'s advertise/reject lists and this node\'s logs.</div>'));
  const body = $('<div></div>'); body.innerHTML = '<div class="hint">loading\u2026</div>'; card.appendChild(body); c.appendChild(card);
  (async () => {
    const r = await api('/api/localdns');
    if (!r.ok || !r.body){ body.innerHTML = '<div class="hint">could not read DNS resolver state.</div>'; return; }
    const nets = r.body.networks||[];
    if (!nets.length){ body.innerHTML = '<div class="hint">no networks up.</div>'; return; }
    body.innerHTML = '';
    for (const n of nets){
      const sub = $('<div class="subcard"></div>');
      sub.appendChild($('<h4>'+esc(n.name)+' <span class="net-id">'+esc(n.iface||'\u2013')+'</span></h4>'));
      if (n.error){
        sub.appendChild($('<div class="hint">'+esc(n.error)+'</div>'));
      } else {
        const pre = $('<pre class="mono-block"></pre>'); pre.textContent = n.text || '(empty)'; sub.appendChild(pre);
      }
      body.appendChild(sub);
    }
  })();
}

// infoLatency pings every other peer on every up network over the overlay
// (measuring the mesh path itself, not the underlay) and shows the RTT. It
// self-refreshes every 10s and stops when the tab/section is left, the same
// lifecycle infoMetrics uses.
//
// latencyHistory keeps a short rolling window of recent readings per peer
// (keyed by network+node, since the same node_id could in principle appear
// on more than one network) plus when its current up/down state began. It's
// module-level, like metricsMinutes, so the trend survives navigating away
// and back — deliberately not reset on every infoLatency() call the way the
// timer is, since throwing away history on every tab visit would defeat the
// point of keeping it. A single-reading snapshot made a peer that blipped to
// 400ms for one cycle indistinguishable from one that had been rock-solid;
// the sparkline (a full-height red bar for a miss, unmistakable at a glance)
// makes that visible without having to be staring at the screen at the exact
// moment it happens. The up/down streak itself isn't printed permanently —
// it's a hover tooltip on the chart — since the bars already show downtime
// within the window; the streak only adds "and it may go back further than
// what's visible here."
let latencyTimer = null;
let latencyHistory = {};
const LATENCY_POLL_MS = 10000;
const LATENCY_WINDOW_MS = 180000; // how much history the trend sparkline covers
const LATENCY_HIST_LEN = Math.round(LATENCY_WINDOW_MS / LATENCY_POLL_MS);

// latencySparkline renders a short history as a small inline SVG bar chart,
// scaled to that peer's own min/max in the window (not a fixed ms scale) so
// small variations are visible even for a peer whose RTT never leaves a
// narrow band; an absolute scale would flatten "10.1, 10.4, 10.2ms" to
// three identical-looking full-height bars. This used to be text block
// characters (\u2581\u2582...\u2588), but those only have 8 discrete heights tied to
// font-size, which made "taller" not really an option and made genuine
// variation hard to read at a glance. A miss renders as a full-height red
// bar rather than a small glyph mixed in with the others, since a down
// reading isn't "a low value"; it's a different kind of thing, and it
// needs to be impossible to miss without reading anything.
// PX_PER_SLOT sets a fixed per-bar allotment (bar + its gap) rather than a
// fixed total chart width, so latencySparkline's bars stay a legible,
// comfortably hoverable width even if LATENCY_HIST_LEN changes later; a
// fixed-width chart would just squeeze each bar thinner as more of them get
// packed in (12 bars at a 104px-wide chart is a comfortable ~9px per bar;
// naively keeping that same 104px for 18 bars would shrink them to ~3px,
// hurting both legibility and how easy a bar is to hover for its tooltip).
const PX_PER_SLOT = 104/12;
function latencySparkline(hist){
  const n = hist.length, w = Math.round(n * PX_PER_SLOT), h = 32, pad = 2;
  const slot = n ? (w - pad*2) / n : 0;
  const barW = Math.max(2, slot*0.55);
  const okVals = hist.filter(x => x.ok && x.rtt!=null).map(x => x.rtt);
  const max = okVals.length ? Math.max(...okVals) : 0;
  const min = okVals.length ? Math.min(...okVals) : 0;
  const range = max - min;
  const minBarH = 3; // floor so a low-but-ok reading is still a visible sliver
  let bars = '';
  hist.forEach((x, i) => {
    const cx = pad + slot*i + slot/2;
    if (!x.ok){
      bars += '<rect x="'+(cx-barW/2).toFixed(1)+'" y="0" width="'+barW.toFixed(1)+'" height="'+h+'" rx="1" fill="var(--danger)"><title>unreachable</title></rect>';
      return;
    }
    const frac = range > 0 ? (x.rtt - min) / range : 0.5;
    const barH = Math.max(minBarH, frac*(h-minBarH));
    bars += '<rect x="'+(cx-barW/2).toFixed(1)+'" y="'+(h-barH).toFixed(1)+'" width="'+barW.toFixed(1)+'" height="'+barH.toFixed(1)+'" rx="1" fill="var(--acc)"><title>'+(Math.round(x.rtt*10)/10)+' ms</title></rect>';
  });
  return '<svg width="'+w+'" height="'+h+'" viewBox="0 0 '+w+' '+h+'" style="display:block">'+bars+'</svg>';
}

function infoLatency(c){
  if (latencyTimer){ clearInterval(latencyTimer); latencyTimer = null; }
  const card = $('<div class="card"></div>');
  const head = $('<h3>Latency to mesh peers</h3>');
  card.appendChild(head);
  card.appendChild($('<div class="hint" style="margin:-4px 0 10px">Round-trip time from this host to every other peer on each up network, pinged over the overlay (so it reflects the mesh path, not just the underlay). A couple of probes per peer, run concurrently; this can take a few seconds. Refreshes automatically every '+(LATENCY_POLL_MS/1000)+'s; <b>trend</b> covers the last '+(LATENCY_WINDOW_MS/1000)+'s; blue bars scale to that peer\u2019s own range, red is a miss; hover a bar for the exact reading, or the chart for how long it\u2019s held its current state.</div>'));
  const body = $('<div></div>'); body.innerHTML = '<div class="hint">pinging\u2026</div>'; card.appendChild(body); c.appendChild(card);

  const load = async () => {
    if (state.section!=='latency'){ if(latencyTimer){clearInterval(latencyTimer); latencyTimer=null;} return; }
    const r = await api('/api/latency');
    if (!r.ok || !r.body){ body.innerHTML = '<div class="hint">could not measure latency.</div>'; return; }
    const nets = r.body.networks||[];
    if (!nets.length){ body.innerHTML = '<div class="hint">no networks up.</div>'; return; }
    body.innerHTML = '';
    for (const n of nets){
      const sub = $('<div class="subcard"></div>');
      sub.appendChild($('<h4>'+esc(n.name)+'</h4>'));
      const peers = n.peers||[];
      // Resolve the network id once (the /api/latency response is name-keyed
      // only); Monitor > mesh peers is keyed by id, so the peer-name links
      // below need it to land on the right card. Empty if the name isn't in cfg.
      const netId = (state.cfg.find(x => x.name === n.name)||{}).id || '';
      if (!peers.length){ sub.appendChild($('<div class="hint">no other peers on this network.</div>')); body.appendChild(sub); continue; }
      // Alphabetical by the same name shown in the table, and nothing else —
      // sorting by rtt/reachability (as this used to) meant rows reshuffled
      // on every poll as values fluctuated or a peer flipped state, which
      // made the table hard to scan since a peer's position wasn't stable
      // from one refresh to the next. Reachability is already visible via
      // the rtt cell, the trend chart's red bars, and the flash on a state
      // change, so grouping by it here was redundant as well as disruptive.
      const peerName = (x) => (x.hostname || x.node_id || '').toLowerCase();
      const sorted = [...peers].sort((a,b) => peerName(a).localeCompare(peerName(b)));
      let h = '<table><tr><th>peer</th><th>overlay</th><th>rtt</th><th>trend</th></tr>';
      for (const p of sorted){
        const key = n.name+'|'+p.node_id;
        let e = latencyHistory[key];
        const prevOk = e ? e.sinceOk : null;
        if (!e) e = latencyHistory[key] = { hist: [] };
        const changed = prevOk !== null && prevOk !== p.ok;
        if (changed || prevOk === null) e.since = Date.now();
        e.sinceOk = p.ok;
        e.hist.push({ ok: p.ok, rtt: p.ok ? p.rtt_ms : null });
        if (e.hist.length > LATENCY_HIST_LEN) e.hist.shift();

        const rtt = p.ok ? (Math.round(p.rtt_ms*10)/10)+' ms' : '<span class="hint">'+esc(p.error||'unreachable')+'</span>';
        const streak = fmtElapsed(e.since*1e6);
        const trend = '<div title="'+(p.ok?'up ':'down ')+streak+'">'+latencySparkline(e.hist)+'</div>';
        const rowClass = changed ? (' class="'+(p.ok?'lat-flash-up':'lat-flash-down')+'"') : '';
        const nameLabel = esc(p.hostname||p.node_id.slice(0,8));
        const nameTitle = notesTitleForNetName(n.name, p.node_id);
        const nameCell = netId
          ? '<span class="peer-link" data-node="'+esc(p.node_id)+'" title="'+(nameTitle||'show this peer in Monitor \u2192 mesh peers')+'">'+nameLabel+'</span>'
          : (nameTitle ? '<span title="'+nameTitle+'">'+nameLabel+'</span>' : nameLabel);
        h += '<tr'+rowClass+'><td>'+nameCell+'</td><td>'+esc(p.overlay||'\u2013')+'</td><td>'+rtt+'</td><td>'+trend+'</td></tr>';
      }
      sub.innerHTML += h+'</table>';
      body.appendChild(sub);
      const tbl = sub.querySelector('table');
      enhanceTable(tbl);
      // Clicking a peer name jumps to that peer in Monitor > mesh peers. Wired
      // after enhanceTable (which only reorders existing nodes, so the handlers
      // survive a sort) and re-wired on every poll, since load() rebuilds the
      // table wholesale each refresh.
      tbl.querySelectorAll('.peer-link').forEach(el => {
        el.onclick = (e) => { e.stopPropagation(); gotoMeshPeer(netId, el.dataset.node); };
      });
    }
  };
  load();
  latencyTimer = setInterval(load, LATENCY_POLL_MS);
}

// infoAbout shows build and host identity.
function infoAbout(c){
  const card = $('<div class="card"></div>');
  const body = $('<div></div>'); body.innerHTML = '<div class="hint">loading\u2026</div>'; card.appendChild(body); c.appendChild(card);
  (async () => {
    const r = await api('/api/about');
    if (!r.ok || !r.body){ body.innerHTML = '<div class="hint">could not load build info.</div>'; return; }
    const b = r.body;
    const ver = esc(b.gravinet_version||'') + (b.gravinet_commit && b.gravinet_commit!=='none' ? ' ('+esc(b.gravinet_commit)+')' : '');
    const rows = [
      ['gravinet version', ver],
      ['operating system', esc(b.os||'')],
      ['OS version', esc(b.os_version||'')],
      ['architecture', esc(b.arch||'')],
      ['Go runtime', esc(b.go_version||'')],
    ];
    let h = '<div class="info-kv">';
    for (const [k,v] of rows) h += '<div class="k">'+k+'</div><div class="v">'+(v||'\u2013')+'</div>';
    body.innerHTML = h+'</div>';
  })();
}

// logLinkTokens builds the set of clickable tokens for the log view from
// current state: every network's id and name (-> Mesh > networks), every
// peer's id and hostname (-> Monitor > mesh peers), and every configured seed
// address (-> the mesh peer answering on that address, resolved by endpoint
// host at click time). Returned entries are { text, kind, netId, nodeId,
// seedHost } and are matched literally against raw log text, so this only ever
// links strings that correspond to a real entity the UI can navigate to —
// rather than trying to parse the many, changing log line formats. Tokens are
// de-duplicated (a name shared by peer and network, say) keeping the first
// seen, and the caller sorts by length so the most specific match wins.
function logLinkTokens(){
  const toks = [];
  const seen = new Set();
  const push = (text, ent) => {
    text = (text||'').trim();
    // Minimum length guards the degenerate matches: a 1-char network name
    // would otherwise light up half the prose in every line.
    if (text.length < 2 || seen.has(text)) return;
    seen.add(text);
    toks.push(Object.assign({ text }, ent));
  };
  for (const cf of (state.cfg||[])){
    if (cf.id) push(cf.id, { kind:'net', netId:cf.id });
    if (cf.name) push(cf.name, { kind:'net', netId:cf.id });
    for (const s of (cf.seeds||[])){
      const addr = stripScheme(s.address||s.Address||'');
      if (!addr) continue;
      const host = splitHostPort(addr).host || addr;
      // Link both the full seed token as written (host:port) and the bare
      // host, since a log line may print either. Both resolve to a peer by
      // endpoint host at click time (see gotoSeedPeer).
      push(addr, { kind:'seed', seedHost:host });
      push(host, { kind:'seed', seedHost:host });
    }
  }
  for (const n of (state.status||[])){
    for (const p of peerRowsForNet(n)){
      if (p.self) continue;
      if (p.id)   push(p.id,   { kind:'peer', netId:n.id, nodeId:p.id });
      if (p.host) push(p.host, { kind:'peer', netId:n.id, nodeId:p.id });
    }
  }
  return toks;
}

// gotoSeedPeer resolves a seed address (matched by underlay endpoint host) to
// the mesh peer currently answering on it and ticks that peer, so clicking a
// seed ip in the log lands on the peer it belongs to. If no live peer matches
// (the seed is configured but nobody's connected on it right now) it still
// honors the "go to mesh peers" intent, landing there with the selection
// cleared rather than ticking an unrelated row.
async function gotoSeedPeer(seedHost){
  const want = (seedHost||'').toLowerCase();
  for (const n of (state.status||[])){
    for (const p of peerRowsForNet(n)){
      if (p.self || !p.endpoint) continue;
      const h = (splitHostPort(p.endpoint).host||'').toLowerCase();
      if (h && h === want){ await gotoMeshPeer(n.id, p.id); return; }
    }
  }
  state.section = 'mesh-peers';
  setActiveRailTab('mesh-peers');
  selection.mpeers.clear();
  await refresh();
}

// linkifyLog turns a raw log message into HTML: known entity tokens become
// clickable spans, everything else is escaped verbatim. It scans the raw
// string (not the escaped one) so escaping and token boundaries can't
// interfere, matching the longest token that starts at each position and sits
// on non-alphanumeric boundaries (so a hostname isn't matched mid-word, and an
// id isn't matched inside a longer hex run). Non-matches are escaped a char at
// a time. The links carry only data-* attributes here; secLogs wires their
// click handlers after the rows are in the DOM, the same deferred-wiring the
// latency table uses.
function linkifyLog(msg, toks){
  msg = String(msg==null ? '' : msg);
  if (!toks || !toks.length) return esc(msg);
  const isWord = ch => ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z';
  let out = '', i = 0, n = msg.length;
  outer:
  while (i < n){
    const prevOk = i === 0 || !isWord(msg[i-1]);
    if (prevOk){
      for (const t of toks){
        const L = t.text.length;
        if (i + L > n) continue;
        if (msg.substr(i, L) !== t.text) continue;
        const nextOk = i + L === n || !isWord(msg[i+L]);
        if (!nextOk) continue;
        const attrs = t.kind === 'net'
          ? 'data-log-kind="net" data-net="'+esc(t.netId)+'"'
          : (t.kind === 'peer'
              ? 'data-log-kind="peer" data-net="'+esc(t.netId)+'" data-node="'+esc(t.nodeId)+'"'
              : 'data-log-kind="seed" data-seedhost="'+esc(t.seedHost)+'"');
        const title = t.kind === 'net' ? 'show this network in Mesh \u2192 networks' : 'show this peer in Monitor \u2192 mesh peers';
        out += '<span class="peer-link" '+attrs+' title="'+title+'">'+esc(t.text)+'</span>';
        i += L;
        continue outer;
      }
    }
    out += esc(msg[i]);
    i++;
  }
  return out;
}

// secLogs shows the daemon log (everything written to the log file), newest
// first, in a filterable/sortable table — the filter box comes from enhanceTable
// like every other list. Setting _rowButtons makes the toolbar (filter +
// Refresh) render immediately, before the async tail load populates the rows.
function secLogs(c){
  const card = $('<div class="card"></div>');
  const hint = $('<div class="hint" style="margin:0 0 10px">Everything the daemon logs, newest first. Filter narrows by text across all columns (try a network id, <b>ERROR</b>, or <b>mesh</b>). Refresh reloads the tail of the log file.</div>');
  card.appendChild(hint);
  const t = $('<div></div>');
  t.innerHTML = '<table><tr><th>time</th><th>level</th><th>message</th></tr></table>';
  const table = t.querySelector('table');
  table._rowButtons = [
    { label:'Refresh', cls:'ghost', title:'reload the log', onclick: () => renderSection() },
    { label:'Download', cls:'ghost', title:'download the log file', onclick: async () => {
        const r = await api('/api/logs?download=1');
        if (!r.ok || !r.body) { alert((r.body && r.body.error) || 'could not fetch log'); return; }
        const text = (r.body.text != null) ? r.body.text : (r.body.lines||[]).join('\n');
        const blob = new Blob([text], { type:'text/plain' });
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a'); a.href = url; a.download = 'gravinet.log'; a.click();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
      } },
    { label:'Clear', cls:'ghost', title:'clear the log file', onclick: async () => {
        if (!confirm('Clear the log file? This cannot be undone.')) return;
        const r = await api('/api/logs/clear', { method:'POST' });
        if (!r.ok) { alert((r.body && r.body.error) || 'could not clear log'); return; }
        renderSection();
      } }
  ];
  card.appendChild(t);
  c.appendChild(card);

  (async () => {
    const tb = table.tBodies[0] || table;
    const r = await api('/api/logs');
    if (!r.ok || !r.body) return;
    if (r.body.enabled === false){
      const tr=document.createElement('tr'); tr.innerHTML='<td colspan="3" class="empty">File logging is disabled; set "log_file" in the config to enable it.</td>'; tb.appendChild(tr); return;
    }
    const lines = (r.body.lines||[]).slice().reverse(); // newest first
    if (!lines.length){ const tr=document.createElement('tr'); tr.innerHTML='<td colspan="3" class="empty">log is empty</td>'; tb.appendChild(tr); return; }
    // Build the token set once for the whole tail, and sort longest-first so
    // the most specific token wins where several could match at one position
    // (a full host:port seed over its bare host, a hostname over a shorter
    // network name it contains).
    const toks = logLinkTokens().sort((a,b) => b.text.length - a.text.length);
    const frag = document.createDocumentFragment();
    for (const line of lines){
      const m = line.match(/^(\d{4}\/\d\d\/\d\d \d\d:\d\d:\d\d) \[(\w+)\] ([\s\S]*)$/);
      let time='', lvl='', msg=line;
      if (m){ time=m[1]; lvl=m[2]; msg=m[3]; }
      const lc = lvl ? lvl.toLowerCase() : '';
      const col = lc==='error' ? 'var(--danger)' : (lc==='warn' ? '#d29922' : 'var(--fg)');
      const tr=document.createElement('tr');
      tr.innerHTML = '<td style="white-space:nowrap;color:var(--mut);font-size:12.5px">'+esc(time)+'</td>'
        + '<td style="font-weight:600;font-size:12.5px;color:'+col+'">'+esc(lvl)+'</td>'
        + '<td style="word-break:break-word;font-size:12.5px">'+linkifyLog(msg, toks)+'</td>';
      frag.appendChild(tr);
    }
    tb.appendChild(frag);
    // Wire the entity links after the rows are in the DOM (deferred like the
    // latency table's peer links). enhanceTable's filter only hides/shows
    // rows, so these handlers survive filtering; a full Refresh rebuilds the
    // table and re-wires from scratch.
    tb.querySelectorAll('.peer-link[data-log-kind]').forEach(el => {
      el.onclick = (e) => {
        e.stopPropagation();
        const k = el.dataset.logKind;
        if (k === 'net') gotoNetwork(el.dataset.net);
        else if (k === 'peer') gotoMeshPeer(el.dataset.net, el.dataset.node);
        else if (k === 'seed') gotoSeedPeer(el.dataset.seedhost);
      };
    });
  })();
}

// mdRender turns the README markdown into HTML styled to match the rest of the
// admin (same palette, monospace, card-consistent code blocks). Deliberately
// small: handles the constructs this README uses — headings, fenced code,
// bullet lists, bold, inline code, links — and escapes everything first. Uses
// String.fromCharCode(96) for the backtick so this stays inside the Go raw
// string (which is itself backtick-delimited).
function mdRender(src){
  const BT = String.fromCharCode(96), BT3 = BT+BT+BT;
  const esc1 = s => s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
  const codeRe = new RegExp(BT+'([^'+BT+']+)'+BT, 'g');
  const codeStyle = 'background:var(--bg);border:1px solid var(--line);border-radius:4px;padding:1px 5px;font-size:12.5px';
  const inline = s => {
    const codes = [];
    s = s.replace(codeRe, (m,cc)=>{ codes.push(cc); return '\u0001'+(codes.length-1)+'\u0001'; });
    s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    // Italic uses _underscore_, not *single asterisk*, so it can't be confused
    // with **bold** above regardless of run length or nesting.
    s = s.replace(/_([^_]+)_/g, '<em>$1</em>');
    s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (m,t,u)=>'<a href="'+u+'" target="_blank" rel="noopener" style="color:var(--acc)">'+t+'</a>');
    s = s.replace(/\u0001(\d+)\u0001/g, (m,n)=>'<code style="'+codeStyle+'">'+codes[+n]+'</code>');
    return s;
  };
  const lines = src.replace(/\r\n/g,'\n').split('\n');
  let html='', i=0, listType=null; // null | 'ul' | 'ol'
  const closeList = ()=>{ if(listType){ html += listType==='ul' ? '</ul>' : '</ol>'; listType=null; } };
  while (i < lines.length){
    const line = lines[i];
    if (line.slice(0,3) === BT3){
      closeList(); i++;
      let code='';
      while (i < lines.length && lines[i].trim() !== BT3){ code += lines[i] + '\n'; i++; }
      i++;
      html += '<pre style="background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:12px 14px;overflow:auto;margin:10px 0"><code style="font-size:12.5px;white-space:pre">'+esc1(code.replace(/\n$/,''))+'</code></pre>';
      continue;
    }
    if (/^-{3,}\s*$/.test(line)){
      closeList();
      html += '<hr style="border:none;border-top:1px solid var(--line);margin:24px 0">';
      i++; continue;
    }
    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h){
      closeList();
      const lvl=h[1].length, txt=inline(esc1(h[2]));
      let st='text-transform:none;letter-spacing:normal;color:var(--fg);font-weight:700;';
      if (lvl===1) st+='font-size:20px;margin:16px 0 10px';
      else if (lvl===2) st+='font-size:16px;margin:18px 0 8px;padding-bottom:5px;border-bottom:1px solid var(--line)';
      else st+='font-size:13.5px;margin:14px 0 6px';
      html += '<h'+lvl+' style="'+st+'">'+txt+'</h'+lvl+'>';
      i++; continue;
    }
    const li = line.match(/^[-*]\s+(.*)$/);
    if (li){
      if (listType!=='ul'){ closeList(); html += '<ul style="margin:8px 0;padding-left:22px">'; listType='ul'; }
      html += '<li style="margin:3px 0">'+inline(esc1(li[1]))+'</li>';
      i++; continue;
    }
    const oli = line.match(/^\d+\.\s+(.*)$/);
    if (oli){
      if (listType!=='ol'){ closeList(); html += '<ol style="margin:8px 0;padding-left:22px">'; listType='ol'; }
      html += '<li style="margin:3px 0">'+inline(esc1(oli[1]))+'</li>';
      i++; continue;
    }
    if (/^\s*$/.test(line)){ closeList(); i++; continue; }
    closeList();
    let para=line; i++;
    while (i<lines.length && !/^\s*$/.test(lines[i]) && lines[i].slice(0,3)!==BT3 && !/^#{1,6}\s/.test(lines[i]) && !/^[-*]\s/.test(lines[i]) && !/^\d+\.\s/.test(lines[i]) && !/^-{3,}\s*$/.test(lines[i])){ para += ' ' + lines[i]; i++; }
    html += '<p style="margin:8px 0;line-height:1.65">'+inline(esc1(para))+'</p>';
  }
  closeList();
  return html;
}

// secReadme renders the on-disk README inside a normal card, matching the look
// of every other page. The file is installed alongside the binary and read by
// the daemon; this just fetches and renders it.
function secReadme(c){
  const card = $('<div class="card"></div>');
  const body = $('<div></div>');
  body.innerHTML = '<div class="hint">loading\u2026</div>';
  card.appendChild(body);
  c.appendChild(card);
  (async () => {
    const r = await api('/api/readme');
    if (!r.ok || !r.body){ body.innerHTML = '<div class="hint">could not load the readme.</div>'; return; }
    if (r.body.available === false){
      body.innerHTML = '<div class="hint">README is not installed on disk. Install it with the installer, or set <b>readme_path</b> in the config to point at it.</div>';
      return;
    }
    body.innerHTML = mdRender(r.body.text || '');
    if (r.body.path){ body.appendChild($('<div class="hint" style="margin:16px 0 0;padding-top:10px;border-top:1px solid var(--line)"><span class="net-id">'+esc(r.body.path)+'</span></div>')); }
  })();
}

// secGettingStarted renders getting-started.md — the markdown source,
// rendered through mdRender exactly like secReadme renders README — so it
// matches the rest of the app's own styling. (A separate getting-started.html,
// shown in an iframe, existed briefly; removed once it was clear native
// styling was what was actually wanted — one file to keep current, not two.)
function secGettingStarted(c){
  const card = $('<div class="card"></div>');
  const body = $('<div></div>');
  body.innerHTML = '<div class="hint">loading\u2026</div>';
  card.appendChild(body);
  c.appendChild(card);
  (async () => {
    const r = await api('/api/getting-started');
    if (!r.ok || !r.body){ body.innerHTML = '<div class="hint">could not load the getting-started guide.</div>'; return; }
    if (r.body.available === false){
      body.innerHTML = '<div class="hint">getting-started.md is not installed on disk. Install it with the installer, or set <b>getting_started_path</b> in the config to point at it.</div>';
      return;
    }
    body.innerHTML = mdRender(r.body.text || '');
    if (r.body.path){ body.appendChild($('<div class="hint" style="margin:16px 0 0;padding-top:10px;border-top:1px solid var(--line)"><span class="net-id">'+esc(r.body.path)+'</span></div>')); }
  })();
}

// secLicense renders the on-disk LICENSE inside a normal card, matching the look
// of every other page. The LICENSE is plain text (e.g. the GPL), so it's shown
// verbatim in a <pre> with its original alignment preserved — not run through the
// markdown renderer. textContent is used so the text can't inject markup.
function secLicense(c){
  const card = $('<div class="card"></div>');
  const body = $('<div></div>');
  body.innerHTML = '<div class="hint">loading\u2026</div>';
  card.appendChild(body);
  c.appendChild(card);
  (async () => {
    const r = await api('/api/license');
    if (!r.ok || !r.body){ body.innerHTML = '<div class="hint">could not load the license.</div>'; return; }
    if (r.body.available === false){
      body.innerHTML = '<div class="hint">LICENSE is not installed on disk. Install it with the installer, or set <b>license_path</b> in the config to point at it.</div>';
      return;
    }
    body.innerHTML = '';
    const pre = $('<pre style="white-space:pre-wrap;word-break:break-word;margin:0;font-size:12.5px;line-height:1.5;color:var(--fg)"></pre>');
    pre.textContent = r.body.text || '';
    body.appendChild(pre);
    if (r.body.path){ body.appendChild($('<div class="hint" style="margin:16px 0 0;padding-top:10px;border-top:1px solid var(--line)"><span class="net-id">'+esc(r.body.path)+'</span></div>')); }
  })();
}

// --- remote shell terminal --------------------------------------------------
//
// The actual terminal emulation (parsing PTY output — SGR colors, cursor
// movement, the alternate screen buffer, scroll regions, and the rest of
// VT100/VT220/xterm — and encoding keyboard input back into the right
// escape sequences) is @xterm/xterm (vendored, MIT licensed — see
// internal/webadmin/vendor/xterm/VENDORED.md), not hand-rolled here. An
// earlier version of this feature had a small hand-rolled parser in this
// file; it covered plain shell output well enough but broke on real
// interactive use (see docs/changelog.md's v301 entry for the specific bug
// that prompted replacing it) and, more fundamentally, could only ever be
// as complete as whatever applications happened to get tested against it.
// xterm.js is the actual, exhaustively-tested implementation of that
// surface — the same one VS Code, GitHub Codespaces, and Google Cloud
// Shell use — so this is that, not another reimplementation of pieces of
// it.
//
// This code is just the glue: open a Terminal into the modal, wire its
// output to the WebSocket's binary frames and its input (term.onData
// already encodes keystrokes, pastes, and every special key correctly) to
// the same channel, and relay the small JSON control channel (resize/exit/
// error) exactly as before. Fixed at 100x30 for this pass, like the
// hand-rolled version was — live-resizing the terminal (and the PTY window
// size that has to track it) is a reasonable follow-up but a separate,
// additive change, not part of replacing the emulator itself.

// openShellModal opens a terminal on node (empty/self = this node) with the
// given display label.
function openShellModal(node, label) {
  const cols = 100, rows = 30;
  const term = new Terminal({
    cols, rows,
    cursorBlink: true,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
    fontSize: 13,
    theme: { background: '#0b0e12', foreground: '#d8dee4' },
  });
  const bg = $('<div class="modal-backdrop"></div>');
  const panel = $('<div class="modal-panel term-panel"></div>');
  const head = $('<div class="modal-head"><h3></h3></div>');
  head.querySelector('h3').textContent = 'Shell: ' + label;
  const closeBtn = $('<button class="modal-close" title="close">\u2715</button>');
  head.appendChild(closeBtn);
  const screen = $('<div class="term-screen"></div>');
  const status = $('<div class="term-status">connecting\u2026</div>');
  panel.appendChild(head); panel.appendChild(screen); panel.appendChild(status);
  bg.appendChild(panel);
  document.body.appendChild(bg);
  term.open(screen);
  term.focus();

  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    try { ws.close(); } catch (e) {}
    try { term.dispose(); } catch (e) {}
    bg.remove();
    document.removeEventListener('keydown', onEscClose);
  };
  // Escape closes the modal unless the terminal itself has focus (where
  // Escape is a real, meaningful keystroke to send to the shell — e.g.
  // leaving insert mode in vim — not a request to close the window).
  const onEscClose = (e) => { if (e.key === 'Escape' && !screen.contains(document.activeElement)) close(); };
  closeBtn.onclick = close;
  bg.onclick = (e) => { if (e.target === bg) close(); };
  document.addEventListener('keydown', onEscClose);

  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = proto+'//'+location.host+'/api/shell/ws?node='+encodeURIComponent(node||'')+'&rows='+rows+'&cols='+cols;
  const ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => { status.textContent = 'connected'; term.focus(); };
  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') {
      let msg = null;
      try { msg = JSON.parse(ev.data); } catch (e) {}
      if (msg && msg.type === 'exit') {
        status.textContent = 'session ended (exit '+msg.code+')';
        // A brief pause so the exit status is actually readable rather than
        // the window just vanishing the instant "exit" is typed, then close
        // automatically — there's nothing left to interact with once the
        // shell itself has exited. Not done from ws.onclose below: that
        // also fires on an abrupt/unexpected disconnect (network drop,
        // proxy hop failing mid-session), where silently closing the
        // window would read as the session having vanished rather than
        // ended, so that path leaves the modal up showing "disconnected".
        setTimeout(close, 900);
      } else if (msg && msg.type === 'error') {
        status.textContent = msg.message || 'error';
        status.classList.add('err');
      }
      return;
    }
    // xterm.js's own parser wants raw bytes and does its own UTF-8 decoding
    // (correctly handling a multi-byte character split across two WS
    // messages, which a plain TextDecoder.decode() per message would not) —
    // hand it the ArrayBuffer directly rather than decoding it ourselves.
    term.write(new Uint8Array(ev.data));
  };
  ws.onerror = () => { status.textContent = 'connection error'; status.classList.add('err'); };
  ws.onclose = () => { if (status.textContent === 'connected') status.textContent = 'disconnected'; };

  term.onData((data) => {
    if (ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(data));
  });
}

dashboard();
</script>
</body>
</html>`
