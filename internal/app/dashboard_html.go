package app

// dashboardHTML — embedded single-page dashboard (GitHub-dark style, ported
// from the qoder2api dashboard). __AUTH_TOKEN__ is injected by handleRoot and
// used for the /api/* calls, which sit behind withAuth.
const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Verdent 2API</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0d1117; color: #c9d1d9; min-height: 100vh; padding: 24px; }
.header { display: flex; align-items: center; gap: 12px; margin-bottom: 24px; }
.header h1 { font-size: 20px; font-weight: 600; color: #e6edf3; }
.header .sub { font-size: 13px; color: #8b949e; }
.dot { width: 10px; height: 10px; border-radius: 50%; background: #56d364; box-shadow: 0 0 8px #56d364; animation: pulse 2s infinite; }
@keyframes pulse { 0%,100%{opacity:1} 50%{opacity:.5} }
.grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 16px; }
.card { background: #161b22; border: 1px solid #21262d; border-radius: 8px; padding: 18px; }
.card-title { font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: .05em; color: #8b949e; margin-bottom: 14px; }
.row { display: flex; justify-content: space-between; align-items: center; padding: 6px 0; border-bottom: 1px solid #21262d; gap: 8px; }
.row:last-child { border-bottom: none; }
.row-label { font-size: 13px; color: #8b949e; }
.row-value { font-size: 13px; color: #e6edf3; font-weight: 500; text-align: right; word-break: break-all; }
.badge { padding: 2px 8px; border-radius: 12px; font-size: 11px; font-weight: 600; user-select: none; }
.badge-green { background: #1a3a1a; color: #56d364; border: 1px solid #238636; }
.badge-red   { background: #3a1a1a; color: #f85149; border: 1px solid #da3633; }
.badge-gray  { background: #21262d; color: #8b949e; border: 1px solid #30363d; }
.badge-blue  { background: #1a2a3a; color: #79c0ff; border: 1px solid #1f6feb; }
button { background: #21262d; color: #c9d1d9; border: 1px solid #30363d; border-radius: 6px; padding: 4px 10px; font-size: 12px; cursor: pointer; }
button:hover { background: #30363d; }
button.primary { background: #1f6feb; border-color: #1f6feb; color: #fff; }
input { background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9; padding: 6px 8px; font-size: 12px; width: 100%; }
select { background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9; padding: 4px 6px; font-size: 12px; }
.endpoint { font-family: monospace; font-size: 12px; color: #56d364; }
.method { display: inline-block; padding: 2px 6px; border-radius: 4px; font-size: 10px; font-weight: 600; margin-right: 6px; }
.method-post { background: #1a2a3a; color: #79c0ff; border: 1px solid #1f6feb; }
.method-get  { background: #1a3a1a; color: #56d364; border: 1px solid #238636; }
.path { font-family: monospace; color: #e6edf3; font-size: 13px; }
.desc { font-size: 12px; color: #8b949e; margin-top: 2px; }
.ep-row { padding: 8px 0; border-bottom: 1px solid #21262d; }
.ep-row:last-child { border-bottom: none; }
.loading { color: #484f58; font-style: italic; font-size: 13px; }
.model { padding: 6px 8px; border-radius: 6px; cursor: pointer; display: flex; justify-content: space-between; align-items: center; }
.model:hover { background: #21262d; }
.model.selected { background: #1a2a3a; border: 1px solid #1f6feb; }
.account { border: 1px solid #21262d; border-radius: 6px; padding: 10px; margin-bottom: 10px; }
.account.active { border-color: #238636; }
.account .top { display: flex; justify-content: space-between; align-items: center; margin-bottom: 6px; gap: 6px; flex-wrap: wrap; }
.muted { color: #8b949e; font-size: 12px; }
.actions { display: flex; gap: 6px; flex-wrap: wrap; align-items: center; }
#modelsList { max-height: 340px; overflow-y: auto; scrollbar-width: thin; scrollbar-color: #30363d transparent; }
#modelsList::-webkit-scrollbar { width: 8px; }
#modelsList::-webkit-scrollbar-track { background: transparent; }
#modelsList::-webkit-scrollbar-thumb { background: #30363d; border-radius: 4px; }
#modelsList::-webkit-scrollbar-thumb:hover { background: #484f58; }
.err { color: #f85149; font-size: 11px; margin-top: 4px; word-break: break-all; }
.bar { height: 8px; background: #21262d; border-radius: 4px; overflow: hidden; margin: 4px 0 8px; }
.bar > div { height: 100%; background: #56d364; border-radius: 4px; transition: width .3s; }
.bar > div.low { background: #d29922; }
.bar > div.empty { background: #f85149; }
</style>
</head>
<body>

<div class="header">
  <div class="dot" id="statusDot"></div>
  <div>
    <h1>Verdent 2API</h1>
    <div class="sub" id="modeSub">OpenAI/Anthropic bridge · loading…</div>
  </div>
</div>

<div class="grid">

  <div class="card">
    <div class="card-title">Status</div>
    <div class="row"><span class="row-label">Proxy</span><span class="badge badge-green" id="proxyBadge">running</span></div>
    <div class="row"><span class="row-label">Endpoint</span><span class="endpoint" id="endpointVal">—</span></div>
    <div class="row"><span class="row-label">Accounts</span><span class="row-value" id="accCountVal">—</span></div>
    <div class="row"><span class="row-label">Active JWT</span><span class="row-value" id="activeJwtVal">—</span></div>
    <div class="row"><span class="row-label">User</span><span class="row-value" id="userVal">—</span></div>
    <div class="row"><span class="row-label">Default model</span><span class="row-value" id="defModelVal">—</span></div>
    <div class="row"><span class="row-label">Context</span>
      <select id="ctxSel" onchange="setRuntime()"></select></div>
    <div class="row"><span class="row-label">Thinking</span>
      <select id="thinkSel" onchange="setRuntime()"></select></div>
  </div>

  <div class="card">
    <div class="card-title">Usage <span class="muted">(active account)</span>
      <button style="float:right" onclick="refreshQuota()">Refresh</button>
    </div>
    <div id="usageCard"><span class="loading">loading…</span></div>
  </div>

  <div class="card">
    <div class="card-title">Models <span class="muted">(live · click = default model)</span></div>
    <div id="modelsList"><span class="loading">loading…</span></div>
  </div>

  <div class="card" style="grid-column: 1 / -1;">
    <div class="card-title">Accounts
      <button style="float:right" onclick="refreshQuota()">Refresh quota</button>
    </div>
    <div id="accountsList"><span class="loading">loading…</span></div>
    <div class="row" style="border-top:1px solid #21262d; padding-top:10px">
      <input id="newJwt" placeholder="eyJhbGciOi… or click Login via Verdent" style="flex:1">
      <button class="primary" onclick="verdentLogin()">Login via Verdent</button>
      <button onclick="addJwt()">Add JWT</button>
    </div>
  </div>

  <div class="card" style="grid-column: 1 / -1;">
    <div class="card-title">API Endpoints</div>
    <div class="ep-row"><span class="method method-post">POST</span><span class="path">/v1/chat/completions</span><div class="desc">OpenAI-compatible chat (stream / non-stream, tool calling); automatic account and model failover</div></div>
    <div class="ep-row"><span class="method method-post">POST</span><span class="path">/v1/messages</span><div class="desc">Anthropic-compatible messages (stream / non-stream, tool use)</div></div>
    <div class="ep-row"><span class="method method-post">POST</span><span class="path">/v1/responses</span><div class="desc">OpenAI Responses API (stream / non-stream)</div></div>
    <div class="ep-row"><span class="method method-get">GET</span><span class="path">/v1/models</span><div class="desc">Verdent free model list</div></div>
    <div class="ep-row"><span class="method method-post">POST</span><span class="path">/prompt</span><div class="desc">Plain prompt → {success, response}</div></div>
  </div>

</div>

<script>
var AUTH = '__AUTH_TOKEN__';
function j(url, opts) {
  opts = opts || {};
  opts.headers = Object.assign({'Content-Type':'application/json'}, opts.headers || {}, AUTH ? {Authorization: 'Bearer ' + AUTH} : {});
  return fetch(url, opts).then(function(res) { return res.json(); });
}
function post(url, body) {
  return j(url, { method: 'POST', body: JSON.stringify(body || {}) });
}

var lastStatus = null;

async function refreshStatus() {
  try {
    var d = await j('/api/status');
    lastStatus = d;
    document.getElementById('modeSub').textContent = 'OpenAI/Anthropic bridge · port ' + d.port;
    document.getElementById('endpointVal').textContent = location.origin + '/v1';
    document.getElementById('accCountVal').textContent = d.accounts;
    document.getElementById('activeJwtVal').textContent = d.active_jwt || '—';
    document.getElementById('userVal').textContent = d.active_user || '—';
    document.getElementById('defModelVal').textContent = d.default_model || '—';
  } catch(e) {
    document.getElementById('proxyBadge').className = 'badge badge-red';
    document.getElementById('proxyBadge').textContent = 'offline';
  }
}

// ── Models: live list from /v1/models (click = default model) ──

var modelCfg = {};

function setRuntime() {
  var m = lastDefault || '';
  var th = document.getElementById('thinkSel').value;
  var thinking = th, effort = '';
  if (['', 'off', 'on'].indexOf(th) === -1) { effort = th; thinking = 'on'; }
  post('/api/settings', {
    model: m,
    context: document.getElementById('ctxSel').value || 'default',
    thinking: thinking || 'default',
    effort: effort || 'default'
  }).then(function(r) { if (r.error) alert(r.error); refreshStatus(); });
}

function applyRuntimeSelects(st) {
  var m = modelCfg[st.default_model] || {};
  var set = (st.model_settings || {})[st.default_model] || {};
  var ctxHtml = '<option value="">default</option>';
  Object.keys(m.ctx || {}).forEach(function(k) { ctxHtml += '<option value="' + k + '">' + k + '</option>'; });
  var ctxSel = document.getElementById('ctxSel'); ctxSel.innerHTML = ctxHtml; ctxSel.value = set.context || '';
  var thHtml = '<option value="">model default</option><option value="off">off</option><option value="on">on</option>';
  (m.efforts || []).forEach(function(l) { thHtml += '<option value="' + l + '">' + l + '</option>'; });
  var thSel = document.getElementById('thinkSel'); thSel.innerHTML = thHtml;
  thSel.value = set.effort || (set.thinking === 'enabled' ? 'on' : (set.thinking === 'disabled' ? 'off' : ''));
}

var lastDefault = '';

async function refreshModels() {
  var el = document.getElementById('modelsList');
  try {
    var st = await j('/api/status');
    var d = await j('/v1/models');
    lastDefault = st.default_model || '';
    (d.data || []).forEach(function(m) { modelCfg[m.id] = { ctx: m.context_config, efforts: m.effort_levels }; });
    applyRuntimeSelects(st);
    el.innerHTML = '';
    (d.data || []).forEach(function(m) {
      var cool = (st.cooldowns || {})[m.id];
      var div = document.createElement('div');
      div.className = 'model' + (m.id === st.default_model ? ' selected' : '');
      div.title = m.description || '';
      var label = m.display_name || m.id;
      var right = (label !== m.id ? '<span class="muted">' + m.id + '</span>' : '') +
        (cool ? ' <span class="badge badge-red">' + Math.ceil(cool/1000) + 's</span>' : '');
      div.innerHTML = '<span>' + label + '</span>' + right;
      div.onclick = function() {
        post('/api/settings', { default_model: m.id }).then(function(r) {
          if (r.error) { alert(r.error); return; }
          refreshModels(); refreshStatus();
        });
      };
      el.appendChild(div);
    });
    if (!(d.data || []).length) el.innerHTML = '<span class="loading">no models</span>';
  } catch(e) { el.innerHTML = '<span class="loading">failed to load</span>'; }
}

// ── Usage: server quotas for the active account ──

function usedPct(v) { return (v == null || v < 0) ? null : Math.round(v * 100); }
function bar(v) { // v = used fraction
  if (v == null || v < 0) return '<div class="muted">no data</div>';
  var cls = v >= 0.95 ? 'empty' : (v >= 0.75 ? 'low' : '');
  return '<div class="bar"><div class="' + cls + '" style="width:' + Math.round(v * 100) + '%"></div></div>';
}
function fmtReset(ms) {
  if (!ms) return '';
  var d = new Date(ms < 1e12 ? ms * 1000 : ms);
  var mins = Math.round((d.getTime() - Date.now()) / 60000);
  var when = mins > 90 ? Math.round(mins / 60) + 'h' : Math.max(0, mins) + 'm';
  return ' · resets in ' + when;
}

function renderUsageCard(acc) {
  var el = document.getElementById('usageCard');
  var u = acc && acc.usage;
  if (!u) { el.innerHTML = '<span class="loading">no quota data yet</span>'; return; }
  var html = '';
  html += '<div class="row"><span class="row-label">Account</span><span class="row-value">' + (u.email || acc.user || acc.jwt) + '</span></div>';
  if (u.free_credits != null) html += '<div class="row"><span class="row-label">Credits</span><span class="row-value">' + u.free_credits + (u.is_subscribe ? ' · subscribed' : ' · free plan') + '</span></div>';
  var f5 = usedPct(u.free_5h_used), f7 = usedPct(u.free_7d_used), e5 = usedPct(u.eco_5h_used);
  html += '<div class="row"><span class="row-label">Free 5h used</span><span class="row-value">' + (f5 == null ? '—' : f5 + '%') + fmtReset(u.reset_5h_ms) + '</span></div>' + bar(u.free_5h_used);
  html += '<div class="row"><span class="row-label">Free 7d used</span><span class="row-value">' + (f7 == null ? '—' : f7 + '%') + fmtReset(u.reset_7d_ms) + '</span></div>' + bar(u.free_7d_used);
  if (u.eco_available) {
    html += '<div class="row"><span class="row-label">Eco mode 5h</span><span class="row-value">' + (e5 == null ? '—' : e5 + '%') + '</span></div>' + bar(u.eco_5h_used);
  }
  if (u.error) html += '<div class="err">' + u.error + '</div>';
  html += '<div class="muted" style="margin-top:6px">fetched ' + (u.fetched_at || '').replace('T', ' ').slice(0, 19) + '</div>';
  el.innerHTML = html;
}

async function refreshUsageCard() {
  try {
    var d = await j('/api/accounts');
    var acc = (d.accounts || []).filter(function(a) { return a.active; })[0] || (d.accounts || [])[0];
    renderUsageCard(acc);
  } catch(e) { /* keep previous */ }
}

function verdentLogin() {
  post('/api/auth/start').then(function(d) {
    if (d.error) { alert(d.error); return; }
    window.open(d.url, '_blank');
  });
}

// ── Accounts ──

async function refreshAccounts() {
  var el = document.getElementById('accountsList');
  try {
    var d = await j('/api/accounts');
    var st = {};
    el.innerHTML = '';
    (d.accounts || []).forEach(function(acc) {
      var s = st[acc.jwt] || {};
      var div = document.createElement('div');
      div.className = 'account' + (acc.active ? ' active' : '');
      var badges = acc.active ? '<span class="badge badge-green">active</span>' : '<span class="badge badge-gray">standby</span>';
      if (acc.disabled) badges += ' <span class="badge badge-red">disabled (401)</span>';
      var quota = quotaLine(acc.usage);
      var usage = '';
      div.innerHTML = '<div class="top"><div><b>' + acc.jwt + '</b> <span class="muted">' + (acc.user || '') + '</span></div>' +
        '<div class="actions">' + badges +
        (acc.active ? '' : '<button onclick="selectAcc(' + acc.index + ')">Use</button>') +
        '<button onclick="removeAcc(' + acc.index + ')">Remove</button></div></div>' + quota + usage;
      el.appendChild(div);
    });
    if (!(d.accounts || []).length) el.innerHTML = '<span class="loading">no accounts — add a JWT below or set VERDENT_TOKEN</span>';
  } catch(e) { el.innerHTML = '<span class="loading">failed to load</span>'; }
}

// quotaLine renders the upstream usage windows (free/eco % left + resets).
function quotaLine(u) {
  if (!u) return '';
  function pct(v) { return v == null ? '—' : v + '%'; }
  function rst(ms) {
    if (!ms) return '';
    var d = new Date(ms < 1e12 ? ms * 1000 : ms);
    return ' resets ' + d.toTimeString().slice(0,5);
  }
  var html = '<div class="muted">free used: 5h ' + pct(usedPct(u.free_5h_used)) + ' · 7d ' + pct(usedPct(u.free_7d_used)) + rst(u.reset_5h_ms) + '</div>';
  if (u.free_credits != null && u.free_credits > 0) html += '<div class="muted">credits: ' + u.free_credits + (u.is_subscribe ? ' · subscribed' : '') + '</div>';
  if (u.error) html += '<div class="muted" style="color:#f85149">' + u.error + '</div>';
  return html;
}

function refreshQuota() {
  post('/api/quota/refresh').then(function(d) {
    if (d.error) { alert(d.error); return; }
    refreshAccounts(); refreshStatus(); refreshUsageCard();
  });
}

function addJwt() {
  var v = document.getElementById('newJwt').value.trim();
  if (!v) return;
  post('/api/accounts/add', { jwt: v }).then(function(d) {
    if (d.error) { alert(d.error); return; }
    document.getElementById('newJwt').value = '';
    refreshAccounts(); refreshStatus();
  });
}
function removeAcc(i) { if (confirm('Remove this account?')) post('/api/accounts/remove', { index: i }).then(function(d){ if (d.error) alert(d.error); refreshAccounts(); refreshStatus(); }); }
function selectAcc(i) { post('/api/accounts/select', { index: i }).then(function(d){ if (d.error) alert(d.error); refreshAccounts(); refreshStatus(); }); }

refreshStatus(); refreshAccounts(); refreshModels(); refreshUsageCard();
setInterval(function() { refreshStatus(); refreshAccounts(); refreshUsageCard(); }, 5000);
</script>
</body>
</html>`
