"use strict";
// Dilreet 0.3 UI. Same API and security model as 0.2: every request sends
// X-Dilreet: 1, the session cookie is HttpOnly, and nothing here talks to
// any host other than this local server (CSP connect-src 'self').
const $ = (s, el) => (el || document).querySelector(s);
const $$ = (s, el) => Array.from((el || document).querySelectorAll(s));
const view = () => $("#view");
let ov = {};          // /api/overview
let state = {};       // /api/state
let sites = [];
let sitesErr = "";
let lastNotice = "";
const timers = new Set();

function esc(s) { return String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }
function h(html) { const t = document.createElement("template"); t.innerHTML = html.trim(); return t.content.firstElementChild; }
function later(fn, ms) { const id = setTimeout(() => { timers.delete(id); fn(); }, ms); timers.add(id); return id; }
function clearTimers() { timers.forEach(clearTimeout); timers.clear(); }
function humanSize(n) { if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB"; if (n >= 1024) return Math.round(n / 1024) + " KB"; return n + " bytes"; }
function ago(t) {
  if (!t) return "never";
  const d = (Date.now() - new Date(t).getTime()) / 1000;
  if (isNaN(d)) return t;
  if (d < 60) return "just now";
  if (d < 3600) return Math.floor(d / 60) + " min ago";
  if (d < 86400) return Math.floor(d / 3600) + " h ago";
  if (d < 86400 * 7) return Math.floor(d / 86400) + " days ago";
  return new Date(t).toLocaleDateString();
}
function store(k, v) { try { if (v === undefined) return JSON.parse(localStorage.getItem("hp-" + k)); localStorage.setItem("hp-" + k, JSON.stringify(v)); } catch (e) { return null; } }

async function api(method, path, body, opts) {
  const opt = { method, headers: { "X-Dilreet": "1" }, signal: opts && opts.signal };
  if (body instanceof FormData) opt.body = body;
  else if (body !== undefined) { opt.body = JSON.stringify(body); opt.headers["Content-Type"] = "application/json"; }
  const r = await fetch(path, opt);
  let data = {};
  try { data = await r.json(); } catch (e) {}
  if (r.status === 401) throw new Error("Session expired. Close and reopen Dilreet.");
  if (!r.ok) { const e = new Error(data.error || ("HTTP " + r.status)); e.status = r.status; e.data = data; throw e; }
  return data;
}

// ---------- toasts, modals ----------
function toast(msg, kind, opts) {
  const t = h(`<div class="toast ${kind === true ? "error" : esc(kind || "")}" role="${kind === true || kind === "error" ? "alert" : "status"}"><div>${esc(msg)}</div><button class="x" aria-label="Dismiss">&times;</button></div>`);
  if (opts && opts.ask) {
    const b = h(`<button class="sm ask" style="margin-left:8px">Ask assistant</button>`);
    b.onclick = () => askAbout(msg);
    t.firstElementChild.appendChild(b);
  }
  t.querySelector(".x").onclick = () => t.remove();
  $("#toasts").appendChild(t);
  setTimeout(() => t.remove(), kind === true || kind === "error" ? 12000 : 5000);
}
function err(e) { toast(e.message || String(e), "error", { ask: true }); }

function modal(html, setup) {
  return new Promise(resolve => {
    const prev = document.activeElement;
    const back = h(`<div class="modal-back"><div class="modal" role="dialog" aria-modal="true">${html}</div></div>`);
    const m = back.firstElementChild;
    const close = (v) => { back.remove(); document.removeEventListener("keydown", key); if (prev && prev.focus) prev.focus(); resolve(v); };
    const key = (e) => {
      if (e.key === "Escape") close(null);
      if (e.key === "Tab") { const f = $$("button,input,select,textarea,a[href]", m).filter(x => !x.disabled && x.offsetParent); if (!f.length) return; const a = f[0], z = f[f.length - 1]; if (e.shiftKey && document.activeElement === a) { z.focus(); e.preventDefault(); } else if (!e.shiftKey && document.activeElement === z) { a.focus(); e.preventDefault(); } }
    };
    document.addEventListener("keydown", key);
    back.addEventListener("mousedown", e => { if (e.target === back) close(null); });
    $$("[data-close]", m).forEach(b => b.onclick = () => close(b.dataset.close === "ok" ? true : null));
    $("#modalRoot").appendChild(back);
    if (setup) setup(m, close);
    const f = $("[autofocus]", m) || $(".primary,.danger.solid", m) || $("button", m);
    if (f) f.focus();
  });
}
function askText(title, label, value) {
  return modal(`<h2>${esc(title)}</h2><label class="small muted" for="askT">${esc(label)}</label><input id="askT" value="${esc(value || "")}" maxlength="500" autofocus><div class="row" style="margin-top:12px"><button class="primary" id="askOk">Save</button><button data-close>Cancel</button></div>`, (m, close) => {
    const ok = () => close($("#askT", m).value);
    $("#askOk", m).onclick = ok; $("#askT", m).onkeydown = (e) => { if (e.key === "Enter") ok(); };
  });
}
function confirmBox(title, body, okText, danger) {
  return modal(`<h2>${esc(title)}</h2><div>${body}</div><div class="row end"><button data-close="">Cancel</button><button data-close="ok" class="${danger ? "danger solid" : "primary"}">${esc(okText || "Continue")}</button></div>`).then(v => v === true);
}
function typedConfirm(title, body, word, okText) {
  return modal(`<h2>${esc(title)}</h2><div>${body}</div><div class="field" style="margin-top:12px"><label for="tc">Type <code>${esc(word)}</code> to confirm</label><input id="tc" autocomplete="off" autofocus></div><div class="row end"><button data-close="">Cancel</button><button id="tcOk" class="danger solid" disabled>${esc(okText)}</button></div>`,
    (m, close) => { const i = $("#tc", m), b = $("#tcOk", m); i.oninput = () => b.disabled = i.value.trim() !== word; b.onclick = () => close(true); i.onkeydown = e => { if (e.key === "Enter" && !b.disabled) close(true); }; }).then(v => v === true);
}

async function busy(btn, fn) {
  if (!btn) return fn();
  const html = btn.innerHTML;
  btn.disabled = true; btn.setAttribute("aria-busy", "true");
  btn.innerHTML = `<span class="spin" aria-hidden="true"></span> ${html}`;
  try { return await fn(); } catch (e) { err(e); } finally { btn.disabled = false; btn.removeAttribute("aria-busy"); btn.innerHTML = html; }
}

// ---------- jobs ----------
// jobPanel renders a friendly progress view (steps + bar) with the raw log
// tucked into a <details>. Resolves with the final job snapshot.
function jobPanel(id, el, opt) {
  opt = opt || {};
  el.innerHTML = `<div class="jobview"><div class="row"><span class="spin" aria-hidden="true"></span><b class="jstage">${esc(opt.title || "Working")}&hellip;</b></div>
    <ul class="joblist hidden"></ul><div class="progress indet"><div style="width:0"></div></div>
    <details class="log" ${opt.openLog ? "open" : ""}><summary>Show details</summary><pre class="log" aria-live="off"></pre></details><div class="jres"></div></div>`;
  const pre = $("pre", el), list = $(".joblist", el), bar = $(".progress", el), stageEl = $(".jstage", el);
  let from = 0;
  return new Promise(resolve => {
    const tick = async () => {
      let j;
      try { j = await api("GET", `/api/jobs/${id}?from=${from}`); } catch (e) { pre.textContent += "\n[" + e.message + "]\n"; setTimeout(tick, 2000); return; }
      if (j.lines.length) { pre.textContent += j.lines.join("\n") + "\n"; pre.scrollTop = pre.scrollHeight; }
      from = j.next;
      if (j.steps && j.steps.length) {
        list.classList.remove("hidden");
        list.innerHTML = j.steps.map((s, i) => `<li class="${j.done && !j.error ? "done" : i < j.step ? "done" : i === j.step ? (j.error ? "fail" : "cur") : ""}">${i < j.step || (j.done && !j.error) ? "&#10004;" : i === j.step ? (j.error ? "&#10006;" : "&#9654;") : "&#9675;"} ${esc(s)}</li>`).join("");
      }
      if (j.stage) stageEl.textContent = j.stage;
      if (j.progress > 0) { bar.classList.remove("indet"); bar.firstElementChild.style.width = Math.round(j.progress * 100) + "%"; }
      if (j.done) {
        $(".spin", el) && $(".spin", el).remove();
        bar.classList.add("hidden");
        if (j.error) {
          stageEl.innerHTML = `<span style="color:var(--bad)">&#10006; ${esc(opt.failTitle || "Something went wrong")}</span>`;
          const box = h(`<div class="alert bad"><div>${esc(j.error)}</div><div class="row"><button class="sm ask">Ask assistant about this</button></div></div>`);
          $(".ask", box).onclick = () => askAbout(j.error + "\n\nLast log lines:\n" + pre.textContent.split("\n").slice(-25).join("\n"));
          $(".jres", el).appendChild(box);
        } else stageEl.innerHTML = `<span style="color:var(--ok)">&#10004; ${esc(opt.doneTitle || "Done")}</span>`;
        resolve(j); return;
      }
      setTimeout(tick, 800);
    };
    tick();
  });
}

// ---------- router ----------
const routes = [];
function route(re, fn, nav) { routes.push({ re, fn, nav }); }
let curRoute = "";
async function render() {
  const hash = location.hash || "#/projects";
  const path = hash.slice(1).split("#")[0];
  const anchor = hash.slice(1).split("#")[1];
  if (path === curRoute && !render.force) return;
  render.force = false;
  curRoute = path;
  clearTimers();
  for (const r of routes) {
    const m = path.match(r.re);
    if (!m) continue;
    $$("#nav a").forEach(a => { if (a.dataset.route === r.nav) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current"); });
    const isTesting = r.nav === "testing";
    $("#view-testing").classList.toggle("hidden", !isTesting);
    view().className = "page" + (isTesting ? " hidden" : "");
    if (view().contains($("#jiv"))) mountJiv(false);
    if (!isTesting) view().innerHTML = "";
    try { await r.fn(...m.slice(1).map(x => x == null ? x : decodeURIComponent(x))); } catch (e) { view().innerHTML = `<div class="alert bad">${esc(e.message)}</div>`; }
    if (anchor) { const el = document.getElementById(anchor); if (el) el.scrollIntoView(); } else $("#main").scrollTop = 0;
    return;
  }
  location.hash = "#/projects";
}
window.addEventListener("hashchange", render);
function go(hash) { if (location.hash === hash) { render.force = true; render(); } else location.hash = hash; }
function rerender() { render.force = true; render(); }

// ---------- shell: status, notifications, theme ----------
async function refreshOverview() {
  try { ov = await api("GET", "/api/overview"); } catch (e) { return; }
  $("#ver").textContent = "v" + ov.version + (ov.app_mode === "browser" ? " · browser mode" : "");
  const dot = $("#connDot"), txt = $("#connText");
  const st = { no_server: ["", "No server yet"], no_engine: ["warn", "Server not set up"], disconnected: ["bad", "Disconnected"], ready: ["ok", "Connected"] }[ov.stage] || ["", ov.stage];
  dot.className = "dot " + st[0]; txt.textContent = st[1];
  const u = $("#unread");
  u.textContent = ov.unread; u.classList.toggle("hidden", !ov.unread);
  const b = $("#banner");
  if (ov.stage === "disconnected") {
    b.innerHTML = `<span>&#9888;</span><span class="grow">Not connected to your server (${esc(ov.server_ip)}). Deploys and domain changes need the connection.</span><button class="sm primary" id="reconnect">Reconnect</button>`;
    b.classList.remove("hidden");
    $("#reconnect").onclick = (e) => reconnect(e.target);
  } else b.classList.add("hidden");
  if (ov.unread) {
    try {
      const n = (await api("GET", "/api/notices")).notices || [];
      const fresh = n.filter(x => !x.read);
      if (fresh.length && fresh[0].id !== lastNotice) {
        if (lastNotice) fresh.slice(0, 3).forEach(x => toast(x.text, x.level === "warn" ? "warn" : x.level === "success" ? "success" : ""));
        lastNotice = fresh[0].id;
      }
    } catch (e) {}
  }
}

// reconnect: managed key first (no password needed), otherwise ask.
async function reconnect(btn) {
  if (ov.managed_key || ov.uses_key_file) {
    try { await busy(btn, async () => { await api("POST", "/api/ssh/connect", { password: "", passphrase: "" }); }); await refreshOverview(); if (ov.ssh_connected) { toast("Connected", "success"); rerender(); return; } } catch (e) {}
  }
  const ok = await modal(`<h2>Connect to ${esc(ov.server_ip)}</h2><p class="muted small">Your password is used for this session only. It is never saved.</p>
    <div class="field"><label for="rpw">SSH password <span class="muted">(or sudo password)</span></label><input id="rpw" type="password" autocomplete="off" autofocus></div>
    <div class="field"><label for="rpp">Key passphrase <span class="muted">(only if your key has one)</span></label><input id="rpp" type="password" autocomplete="off"></div>
    <div id="rerr"></div><div class="row end"><button data-close="">Cancel</button><button id="rgo" class="primary">Connect</button></div>`, (m, close) => {
    const goBtn = $("#rgo", m);
    const doit = () => busy(goBtn, async () => {
      try { await api("POST", "/api/ssh/connect", { password: $("#rpw", m).value, passphrase: $("#rpp", m).value }); close(true); }
      catch (e) { $("#rerr", m).innerHTML = `<div class="alert bad">${esc(e.message)}</div>`; }
    });
    goBtn.onclick = doit; $("#rpw", m).onkeydown = e => { if (e.key === "Enter") doit(); };
  });
  if (ok) { toast("Connected", "success"); await refreshOverview(); rerender(); }
}

$("#themeBtn").onclick = () => {
  const t = document.documentElement.getAttribute("data-theme") === "dark" ? "light" : "dark";
  document.documentElement.setAttribute("data-theme", t);
  try { localStorage.setItem("hp-theme", t); } catch (e) {}
};
$("#bell").onclick = async () => {
  const p = $("#noticePanel"), b = $("#bell");
  if (!p.classList.contains("hidden")) { p.classList.add("hidden"); b.setAttribute("aria-expanded", "false"); return; }
  b.setAttribute("aria-expanded", "true");
  const n = (await api("GET", "/api/notices").catch(() => ({ notices: [] }))).notices || [];
  p.innerHTML = `<div class="row"><b class="grow">Notifications</b><button class="sm ghost" id="nClear">Clear all</button><button class="sm ghost" id="nClose" aria-label="Close">&times;</button></div>` +
    (n.map(x => `<div class="notice"><span class="badge ${x.level === "warn" ? "warn" : x.level === "success" ? "ok" : "info"}">${esc(x.level)}</span> ${x.site ? `<a href="#/project/${esc(x.site)}">${esc(x.text)}</a>` : esc(x.text)}<time>${esc(ago(x.time))}</time></div>`).join("") || `<p class="muted small">Nothing yet. Dilreet tells you here when a domain goes live, HTTPS is ready, a deploy happens automatically, or an update is available.</p>`);
  p.classList.remove("hidden");
  $("#nClose").onclick = () => $("#bell").click();
  $("#nClear").onclick = async () => { await api("DELETE", "/api/notices"); $("#bell").click(); refreshOverview(); };
  p.onclick = (e) => { if (e.target.closest("a")) $("#bell").click(); };
  api("POST", "/api/notices/read").then(refreshOverview).catch(() => {});
};

async function loadSites() {
  try { const r = await api("GET", "/api/sites"); sites = r.sites || []; sitesErr = r.api_error || ""; } catch (e) { sitesErr = e.message; }
  return sites;
}
function siteURL(s) {
  if (s.domain && s.domain_live) return "https://" + s.domain;
  if (s.instant_url) return s.instant_url;
  if (s.fqdn) return s.fqdn.split(",")[0];
  return "";
}
function statusBadge(s) {
  const st = (s.status || "").toLowerCase();
  if (!s.last_deploy) return `<span class="badge">Not deployed</span>`;
  if (st.startsWith("running")) return `<span class="badge ok">&#9679; Published</span>`;
  if (st.includes("exited") || st.includes("degraded") || st.includes("missing")) return `<span class="badge bad">${esc(s.status)}</span>`;
  if (st === "unknown") return `<span class="badge">Status unknown</span>`;
  return `<span class="badge warn">${esc(s.status)}</span>`;
}
const colors = ["#0a8f8c", "#6467f2", "#d97757", "#2f7fe0", "#b5458f", "#3c9a4c", "#c1862c", "#5b6b7a"];
function thumb(name) { let n = 0; for (const c of name) n = (n * 31 + c.charCodeAt(0)) >>> 0; return `<div class="thumb" aria-hidden="true" style="background:${colors[n % colors.length]}">${esc((name || "?").trim().slice(0, 1).toUpperCase())}</div>`; }

// ---------- onboarding (3 steps) ----------
function stepsBar(cur) {
  const names = ["Connect a server", "Set up hosting", "Deploy your first project"];
  return `<ol class="steps" aria-label="Setup progress">${names.map((n, i) => `<li class="${i < cur ? "done" : i === cur ? "cur" : ""}" ${i === cur ? 'aria-current="step"' : ""}>${n}</li>`).join("")}</ol>`;
}
route(/^\/setup$/, async () => {
  await refreshOverview();
  const v = view();
  const cur = ov.stage === "no_server" ? 0 : ov.stage === "no_engine" ? 1 : ov.stage === "disconnected" ? 0 : 2;
  v.innerHTML = `<div class="head"><h1>Welcome to Dilreet</h1></div><p class="muted">Host your websites on your own server, as easily as Netlify. Three steps, about 10 minutes.</p>${stepsBar(cur)}<div id="ob"></div>`;
  const ob = $("#ob");
  if (cur === 0) return obServer(ob);
  if (cur === 1) return obEngine(ob);
  ob.innerHTML = `<div class="card"><h2>&#127881; Your server is ready</h2><p>Drag and drop a site folder, or import one from GitHub, GitLab or Netlify. You get a free https address right away.</p><div class="row"><a class="btn primary lg" href="#/new">Deploy your first project</a><a class="btn" href="#/projects">Go to Projects</a></div></div>`;
}, "projects");

function obServer(el) {
  el.innerHTML = `<div class="grid2">
    <button class="choice" id="cHave" aria-pressed="true"><span class="ico">&#128273;</span><span><b>I already have a server</b><span class="muted small">Any Ubuntu 22.04/24.04 VPS. You need its IP address and root password or SSH key.</span></span></button>
    <button class="choice" id="cNew" aria-pressed="false"><span class="ico">&#10024;</span><span><b>Create one for me</b><span class="muted small">Hetzner or DigitalOcean. Paste an API token and pick a size; Dilreet does the rest.</span></span></button>
  </div><div id="obForm" style="margin-top:16px"></div>`;
  const pick = (which) => { $("#cHave").setAttribute("aria-pressed", which === "have"); $("#cNew").setAttribute("aria-pressed", which === "new"); which === "have" ? haveServerForm($("#obForm")) : cloudServerForm($("#obForm")); };
  $("#cHave").onclick = () => pick("have"); $("#cNew").onclick = () => pick("new");
  pick("have");
}

async function haveServerForm(el, opts) {
  opts = opts || {};
  state = await api("GET", "/api/state").catch(() => ({}));
  el.innerHTML = `<div class="card"><h2>Connect your server</h2>
    <div class="grid2"><div class="field"><label for="sIp">Server IP address</label><input id="sIp" placeholder="203.0.113.10" value="${esc(state.server_ip || "")}" autocomplete="off"></div>
    <div class="field"><label for="sPw">Root password <span class="muted">(used for this session only, never saved)</span></label><input id="sPw" type="password" autocomplete="off"></div></div>
    <details ${state.ssh_key_path || (state.ssh_user && state.ssh_user !== "root") ? "open" : ""}><summary class="small muted" style="cursor:pointer">Advanced: user, port, SSH key</summary>
      <div class="grid2" style="margin-top:10px"><div class="field"><label for="sUser">SSH user</label><input id="sUser" value="${esc(state.ssh_user || "root")}"></div>
      <div class="field"><label for="sPort">SSH port</label><input id="sPort" type="number" value="${esc(state.ssh_port || 22)}"></div>
      <div class="field"><label for="sKey">Private key file <span class="muted">(optional, OpenSSH format)</span></label><input id="sKey" placeholder="C:\\Users\\you\\.ssh\\id_ed25519" value="${esc(state.ssh_key_path || "")}"></div>
      <div class="field"><label for="sPp">Key passphrase</label><input id="sPp" type="password" autocomplete="off"></div></div></details>
    <p class="hint">After setup Dilreet installs its own SSH key on the server, so you won't need the password again.</p>
    <div id="sErr"></div><div class="row"><button id="sGo" class="primary lg">Connect</button>${state.ssh_connected ? `<button id="sOff">Disconnect</button>` : ""}${state.host_key_fp ? `<button id="sForget" class="ghost sm">Forget host key</button>` : ""}</div>
    ${state.host_key_fp ? `<p class="hint">Pinned server fingerprint: <code>${esc(state.host_key_fp)}</code></p>` : ""}<div id="sInfo"></div></div>`;
  const doConnect = () => busy($("#sGo"), async () => {
    $("#sErr").innerHTML = "";
    try {
      await api("POST", "/api/server", { ip: $("#sIp").value.trim(), port: +$("#sPort").value || 22, user: $("#sUser").value.trim() || "root", key_path: $("#sKey").value.trim() });
      const r = await api("POST", "/api/ssh/connect", { password: $("#sPw").value, passphrase: $("#sPp").value });
      $("#sPp").value = "";
      if (r.first_time) toast("First connection: the server fingerprint " + r.fingerprint + " is now pinned.", "info");
      toast("Connected to your server", "success");
      await refreshOverview();
      if (opts.onDone) opts.onDone(r); else go("#/setup");
    } catch (e) {
      $("#sErr").innerHTML = `<div class="alert bad">${esc(e.message)}<div class="row"><button class="sm ask">Ask assistant about this</button></div></div>`;
      $("#sErr .ask").onclick = () => askAbout("SSH connection failed: " + e.message);
    }
  });
  $("#sGo").onclick = doConnect;
  $("#sPw").onkeydown = e => { if (e.key === "Enter") doConnect(); };
  if ($("#sOff")) $("#sOff").onclick = (e) => busy(e.target, async () => { await api("POST", "/api/ssh/disconnect"); await refreshOverview(); haveServerForm(el, opts); });
  if ($("#sForget")) $("#sForget").onclick = async () => {
    if (!await confirmBox("Forget the server fingerprint?", "Only do this if you reinstalled or replaced the server. If you did not, a changed fingerprint can mean someone is intercepting the connection.", "Forget it", true)) return;
    await api("POST", "/api/ssh/forget-hostkey"); haveServerForm(el, opts);
  };
}

function cloudServerForm(el) {
  el.innerHTML = `<div class="card"><h2>Create a server</h2>
    <div class="row" role="radiogroup" aria-label="Provider"><label class="check"><input type="radio" name="cp" value="hetzner" checked> Hetzner Cloud <span class="muted">(Germany/Finland/US/Singapore, from about &euro;4/month)</span></label>
    <label class="check"><input type="radio" name="cp" value="digitalocean"> DigitalOcean <span class="muted">(worldwide, from about $6/month)</span></label></div>
    <div class="field"><label for="cTok">API token <span class="muted">(read &amp; write; used once to create the server, not saved)</span></label><input id="cTok" type="password" autocomplete="off"><div class="hint"><a id="cTokLink" target="_blank" rel="noopener">Create a token</a> &middot; <span id="cTokHint"></span></div></div>
    <button id="cPlans">Show server sizes</button><div id="cPlanBox"></div></div>`;
  const links = { hetzner: ["https://console.hetzner.cloud/", "Hetzner Console &rarr; your project &rarr; Security &rarr; API tokens &rarr; Generate (Read &amp; Write)."], digitalocean: ["https://cloud.digitalocean.com/account/api/tokens", "API &rarr; Generate New Token, with write scope."] };
  const upd = () => { const k = $("input[name=cp]:checked").value; $("#cTokLink").href = links[k][0]; $("#cTokHint").innerHTML = links[k][1]; $("#cPlanBox").innerHTML = ""; };
  $$("input[name=cp]").forEach(r => r.onchange = upd); upd();
  $("#cPlans").onclick = (e) => busy(e.target, async () => {
    const kind = $("input[name=cp]:checked").value;
    const r = await api("POST", "/api/cloud/plans", { kind, token: $("#cTok").value });
    const plans = r.plans || [];
    $("#cPlanBox").innerHTML = `<div class="field" style="margin-top:14px"><label for="cPlan">Size</label><select id="cPlan">${plans.map(p => `<option value="${esc(p.id)}">${esc(p.label)} &middot; ${p.cores} CPU, ${p.memory_gb} GB RAM, ${p.disk_gb} GB disk &middot; ${p.monthly ? p.monthly.toFixed(2) + " " + esc(p.currency) + "/month" : ""}</option>`).join("")}</select><div class="hint">4 GB RAM or more is recommended; 8 GB comfortably hosts about 100 static sites.</div></div>
      <div class="field"><label for="cRegion">Location</label><select id="cRegion"></select></div>
      <div class="field"><label for="cMail">Your e-mail <span class="muted">(for the hosting dashboard login and certificate notices)</span></label><input id="cMail" type="email" autocomplete="email"></div>
      <label class="switch"><input type="checkbox" id="cAuto" checked> Install security updates automatically</label>
      <div class="alert info small">This creates a paid server in your ${kind === "hetzner" ? "Hetzner" : "DigitalOcean"} account. You can delete it there any time.</div>
      <button id="cCreate" class="primary lg">Create server and set everything up</button><div id="cJob"></div>`;
    const regions = () => { const p = plans.find(x => x.id === $("#cPlan").value); $("#cRegion").innerHTML = ((p && p.regions) || []).map(x => `<option>${esc(x)}</option>`).join(""); };
    const best = plans.find(p => p.memory_gb >= 4) || plans[0];
    if (best) $("#cPlan").value = best.id;
    $("#cPlan").onchange = regions; regions();
    $("#cCreate").onclick = (ev) => busy(ev.target, async () => {
      const p = plans.find(x => x.id === $("#cPlan").value);
      if (!await confirmBox("Create this server?", `<p>${esc(p ? p.label : "")} in ${esc($("#cRegion").value)}${p && p.monthly ? `, about <b>${p.monthly.toFixed(2)} ${esc(p.currency)}/month</b>` : ""}. Dilreet then installs the hosting engine and connects to it (about 10 minutes).</p>`, "Create server")) return;
      const j = await api("POST", "/api/cloud/create", { kind, token: $("#cTok").value, plan: $("#cPlan").value, region: $("#cRegion").value, admin_email: $("#cMail").value, auto_updates: $("#cAuto").checked });
      $("#cTok").value = "";
      ev.target.classList.add("hidden");
      const res = await jobPanel(j.job, $("#cJob"), { title: "Creating your server", doneTitle: "Your server is ready" });
      await refreshOverview();
      if (!res.error) later(() => go("#/setup"), 1200);
    });
  });
}

function obEngine(el) {
  el.innerHTML = `<div class="card"><h2>Set up hosting on ${esc(ov.server_ip)}</h2>
    <p>Dilreet installs <a href="https://coolify.io" target="_blank" rel="noopener">Coolify</a> (free, open source) with its official installer, creates the admin login and an API token, and connects to it. No copy and paste needed. Takes about 5&ndash;10 minutes.</p>
    <div class="field"><label for="eMail">Your e-mail <span class="muted">(admin login for the Coolify dashboard; optional)</span></label><input id="eMail" type="email" value="${esc(ov.admin_email || "")}"></div>
    <label class="switch"><input type="checkbox" id="eAuto" checked> Install security updates automatically</label>
    <div class="row"><button id="eGo" class="primary lg">Set up automatically</button><button id="eHave" class="ghost">Coolify is already installed</button></div><div id="eJob"></div><div id="eHaveBox"></div></div>`;
  $("#eGo").onclick = (e) => busy(e.target, async () => {
    if (!ov.ssh_connected) { await reconnect(); if (!ov.ssh_connected) return; }
    const j = await api("POST", "/api/install", { admin_email: $("#eMail").value, auto_updates: $("#eAuto").checked, remember_key: true });
    e.target.classList.add("hidden");
    const res = await jobPanel(j.job, $("#eJob"), { title: "Setting up", doneTitle: "Hosting is ready" });
    await refreshOverview();
    if (!res.error) later(() => go("#/setup"), 1200);
  });
  $("#eHave").onclick = () => coolifyForm($("#eHaveBox"), () => go("#/setup"));
}

async function coolifyForm(el, onDone) {
  state = await api("GET", "/api/state").catch(() => ({}));
  el.innerHTML = `<div class="card flat" style="margin-top:14px"><h3 style="margin-top:0">Use an existing Coolify</h3>
    <div class="grid2"><div class="field"><label for="kUrl">Coolify URL</label><input id="kUrl" placeholder="http://203.0.113.10:8000" value="${esc(state.coolify_url || (state.server_ip ? "http://" + state.server_ip + ":8000" : ""))}"></div>
    <div class="field"><label for="kTok">API token <span class="muted">${state.token_set ? "(leave empty to keep the saved one)" : "(Coolify &rarr; Keys &amp; Tokens &rarr; API tokens, permission root)"}</span></label><input id="kTok" type="password" autocomplete="off"></div></div>
    <label class="check"><input type="checkbox" id="kSsh" ${state.api_via_ssh ? "checked" : ""}> Send API calls through the SSH connection (recommended while Coolify has no HTTPS domain)</label>
    <p class="hint">${state.token_set ? "A token is saved (" + esc(state.token_storage) + ")." : "No token saved yet."}</p>
    <button id="kGo" class="primary">Save &amp; test</button><div id="kRes"></div></div>`;
  $("#kGo").onclick = (e) => busy(e.target, async () => {
    try {
      const r = await api("POST", "/api/coolify", { url: $("#kUrl").value, token: $("#kTok").value, api_via_ssh: $("#kSsh").checked });
      $("#kTok").value = "";
      $("#kRes").innerHTML = `<div class="alert ok">Connected to Coolify ${esc(r.version)}. Servers: ${(r.servers || []).map(s => esc(s.name) + " (" + esc(s.ip) + ")").join(", ")}</div>`;
      await refreshOverview();
      if (onDone) later(onDone, 1000);
    } catch (x) { $("#kRes").innerHTML = `<div class="alert bad">${esc(x.message)}</div>`; }
  });
}

// ---------- sites list ----------
route(/^\/projects$/, async () => {
  await refreshOverview();
  const v = view();
  v.innerHTML = `<div class="head"><h1>Projects</h1><span class="grow"></span><a class="btn primary" href="#/new">Add new project</a></div><div id="sl"><p class="muted">Loading&hellip;</p></div>`;
  if (ov.stage === "no_server" || ov.stage === "no_engine") {
    if (!ov.site_count) { go("#/setup"); return; }
  }
  await loadSites();
  const sl = $("#sl");
  if (!sites.length) {
    sl.innerHTML = `<div class="card empty-hero"><div class="hero-ic" aria-hidden="true">&#128640;</div><h2>Put your first project online</h2>
      <p class="muted">Drag in a folder or .zip, import from GitHub, GitLab or a link, or move a site from Netlify. It is live in about a minute on a free https address.</p>
      <p><a class="btn primary lg" href="#/new">Add new project</a></p>
      <p class="small muted"><a href="#/new/git">Import from Git</a> &middot; <a href="#/new/netlify">Import from Netlify</a> &middot; <button class="linkish" type="button" id="eAsk">Ask Jiv how it works</button></p></div>`;
    $("#eAsk").onclick = () => jivAsk("How do I put my first website online with Dilreet?");
    return;
  }
  sl.innerHTML = `${sitesErr ? `<div class="alert warn">Live status is unavailable: ${esc(sitesErr)}</div>` : ""}
    <div class="toolbar"><input type="search" id="q" placeholder="Search projects" aria-label="Search projects"><span class="grow"></span><span class="muted small" id="cnt"></span></div>
    <div class="sitelist" id="list"></div>`;
  const draw = () => {
    const q = $("#q").value.toLowerCase();
    const list = sites.filter(s => !q || (s.name + " " + s.slug + " " + (s.domain || "")).toLowerCase().includes(q));
    $("#cnt").textContent = `${list.length} of ${sites.length} projects`;
    $("#list").innerHTML = list.map(s => `<a class="siterow" href="#/project/${esc(s.slug)}">${thumb(s.name)}<div class="t"><b>${esc(s.name)}</b><div class="u">${esc((siteURL(s) || "no address yet").replace(/^https?:\/\//, ""))}${s.domain && !s.domain_live ? ` &middot; <span class="badge warn">${esc(s.domain)}: waiting for DNS</span>` : ""}</div></div>
      <div class="meta">${statusBadge(s)}${s.git ? ` <span class="badge info" title="${esc(s.git.display)}">&#128279; ${esc(s.git.branch || "git")}${s.git.auto_deploy ? " · auto" : ""}</span>` : ""}${s.watch ? ` <span class="badge info">watching folder</span>` : ""}<div>Deployed ${esc(ago(s.last_deploy))}</div></div></a>`).join("") || `<div class="empty">No project matches.</div>`;
  };
  $("#q").oninput = draw; draw();
}, "projects");
route(/^\/sites(?:\/new)?$/, async () => { location.replace(location.hash.includes("new") ? "#/new" : "#/projects"); }, "projects");
route(/^\/site\/([^/]+)(\/domain)?$/, async (slug, d) => { location.replace(`#/project/${encodeURIComponent(slug)}${d || ""}`); }, "projects");
route(/^\/assistant$/, async () => { location.replace("#/jiv"); }, "assistant");

// ---------- drop zone ----------
const SKIP_DIRS = /(^|\/)(node_modules|\.git|\.svn|\.hg)(\/|$)/;
async function readDropped(dt) {
  const items = Array.from(dt.items || []).filter(i => i.kind === "file");
  const entries = items.map(i => i.webkitGetAsEntry && i.webkitGetAsEntry()).filter(Boolean);
  if (entries.length === 1 && entries[0].isFile && /\.zip$/i.test(entries[0].name)) return { zip: dt.files[0] };
  const files = [];
  const walk = (entry, prefix) => new Promise(resolve => {
    if (entry.isFile) { entry.file(f => { files.push([f, prefix + entry.name]); resolve(); }, () => resolve()); return; }
    if (!entry.isDirectory) return resolve();
    const p = prefix + entry.name + "/";
    if (SKIP_DIRS.test(p)) return resolve();
    const reader = entry.createReader();
    const all = [];
    const more = () => reader.readEntries(async batch => { if (!batch.length) { for (const e of all) await walk(e, p); resolve(); } else { all.push(...batch); more(); } }, () => resolve());
    more();
  });
  for (const e of entries) await walk(e, "");
  if (!entries.length && dt.files && dt.files.length === 1 && /\.zip$/i.test(dt.files[0].name)) return { zip: dt.files[0] };
  return { files };
}
function formFrom(sel) {
  const fd = new FormData();
  if (sel.zip) fd.append("zip", sel.zip);
  else if (sel.files && sel.files.length) sel.files.forEach(([f, p]) => { fd.append("files", f); fd.append("paths", p); });
  else if (sel.path) fd.append("local_path", sel.path);
  else throw new Error("Choose a folder, a .zip file, or enter a path");
  return fd;
}
function dropZone(el, opt) {
  opt = opt || {};
  el.innerHTML = `<div class="drop ${opt.compact ? "compact" : ""}" tabindex="0" role="button" aria-label="Drag and drop your site folder or zip here, or press Enter to browse">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><path d="M12 16V4M7 9l5-5 5 5"/><path d="M4 16v3a1 1 0 001 1h14a1 1 0 001-1v-3"/></svg>
      <div class="big">Drag and drop your site folder or zip here</div>
      <div class="muted small">or <button type="button" class="sm" data-b="dir">browse for a folder</button> <button type="button" class="sm" data-b="zip">choose a .zip</button></div>
      <input type="file" class="hidden" data-i="dir" webkitdirectory multiple><input type="file" class="hidden" data-i="zip" accept=".zip">
    </div>
    <details style="margin-top:8px"><summary class="small muted" style="cursor:pointer">Use a folder path on this PC instead (lets Dilreet redeploy automatically when the folder changes)</summary>
      <div class="row" style="margin-top:8px"><input class="grow" data-i="path" placeholder="C:\\Sites\\bakery\\dist" style="flex:1"><button type="button" data-b="path">Use this folder</button></div></details>
    <div class="dzStatus" aria-live="polite"></div>`;
  const d = $(".drop", el), st = $(".dzStatus", el);
  const send = async (sel) => {
    st.innerHTML = `<div class="alert info"><span class="spin"></span> Reading and checking your files&hellip;</div>`;
    try {
      const fd = formFrom(sel);
      const r = await api("POST", "/api/import/upload", fd);
      st.innerHTML = "";
      openReview(r, { slug: opt.slug });
    } catch (e) { st.innerHTML = `<div class="alert bad">${esc(e.message)}</div>`; }
  };
  ["dragenter", "dragover"].forEach(ev => d.addEventListener(ev, e => { e.preventDefault(); d.classList.add("over"); }));
  ["dragleave", "drop"].forEach(ev => d.addEventListener(ev, e => { e.preventDefault(); d.classList.remove("over"); }));
  d.addEventListener("drop", async e => { const sel = await readDropped(e.dataTransfer); if (!sel.zip && !(sel.files || []).length) { st.innerHTML = `<div class="alert bad">Drop a folder or a single .zip file.</div>`; return; } send(sel); });
  d.addEventListener("click", e => { if (!e.target.closest("button")) $("[data-i=dir]", el).click(); });
  d.addEventListener("keydown", e => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); $("[data-i=dir]", el).click(); } });
  $("[data-b=dir]", el).onclick = () => $("[data-i=dir]", el).click();
  $("[data-b=zip]", el).onclick = () => $("[data-i=zip]", el).click();
  $("[data-i=dir]", el).onchange = (e) => { const files = Array.from(e.target.files).map(f => [f, f.webkitRelativePath || f.name]).filter(([, p]) => !SKIP_DIRS.test(p)); e.target.value = ""; if (files.length) send({ files }); };
  $("[data-i=zip]", el).onchange = (e) => { const f = e.target.files[0]; e.target.value = ""; if (f) send({ zip: f }); };
  $("[data-b=path]", el).onclick = () => { const p = $("[data-i=path]", el).value.trim(); if (p) send({ path: p }); };
}

// ---------- add new project ----------
route(/^\/new(?:\/(git|netlify))?$/, async (tab) => {
  await refreshOverview();
  tab = tab || "drop";
  const v = view();
  const opts = [["drop", "&#128193;", "Deploy manually", "Drag and drop a folder or .zip from this PC"], ["git", "&#128279;", "Import from Git or a link", "GitHub, GitLab, Bitbucket, Codeberg, Azure, any .git or .zip link"], ["netlify", "&#9729;", "Import from Netlify", "Copy a Netlify site with its rules and domain"]];
  v.innerHTML = `<div class="crumbs"><a href="#/projects">Projects</a> /</div><div class="head"><h1>Add new project</h1></div>
    ${ov.stage !== "ready" ? `<div class="alert warn">${ov.stage === "disconnected" ? "Not connected to your server. You can prepare the project now; publishing needs the connection." : `Your server is not set up yet. <a href="#/setup">Finish setup</a> first.`}</div>` : ""}
    <div class="choices" role="tablist">${opts.map(([k, ic, t, sub]) => `<button role="tab" class="choice" data-t="${k}" aria-selected="${tab === k}"><span class="ic" aria-hidden="true">${ic}</span><b>${t}</b><small>${esc(sub)}</small></button>`).join("")}</div><div id="tabBody" role="tabpanel"></div>`;
  $$(".choices button", v).forEach(b => b.onclick = () => go(b.dataset.t === "drop" ? "#/new" : "#/new/" + b.dataset.t));
  const body = $("#tabBody");
  if (tab === "drop") {
    body.innerHTML = `<div class="card"><div id="dz"></div><p class="hint" style="margin-top:12px">A plain site needs an <code>index.html</code>. For React, Vue, Vite, Astro, Next (static export) and similar, drop the project folder and Dilreet offers to build it, or drop the built <code>dist</code>/<code>build</code> folder. A copy is kept in <code>Documents\\Dilreet\\Projects</code>.</p></div>`;
    dropZone($("#dz"), {});
  } else if (tab === "git") gitImport(body);
  else netlifyImport(body);
}, "projects");

function gitImport(el) {
  el.innerHTML = `<div class="card"><h2>Import from Git or a link</h2>
    <p class="muted small">Paste a repository or download link: GitHub, GitLab (also self-hosted), Bitbucket, Codeberg/Gitea, Azure DevOps, any <code>.git</code> URL, or a <code>.zip</code>/<code>.tar.gz</code> link. Links to a branch or folder (<code>/tree/main/docs</code>) work too.</p>
    <div class="row"><input id="gUrl" class="grow" style="flex:1" placeholder="https://github.com/you/your-site" aria-label="Repository or download link" autofocus><button id="gGo" class="primary">Continue</button></div>
    <div id="gDet"></div></div><div id="gRepos"></div>`;
  const run = () => busy($("#gGo"), () => detectRepo($("#gUrl").value.trim(), $("#gDet")));
  $("#gGo").onclick = run;
  $("#gUrl").onkeydown = e => { if (e.key === "Enter") run(); };
  myRepos($("#gRepos"), (url) => { $("#gUrl").value = url; run(); });
}

async function myRepos(el, pick) {
  const a = await api("GET", "/api/git/accounts").catch(() => ({ accounts: [] }));
  const accs = (a.accounts || []).filter(x => x.kind !== "netlify");
  if (!accs.length) { el.innerHTML = `<p class="small muted">Private repositories: paste the link above and Dilreet helps you connect your account with a read-only token.</p>`; return; }
  el.innerHTML = `<div class="card"><div class="card-head"><h2>Your repositories</h2><select id="rAcc" style="width:auto">${accs.map(x => `<option value="${esc(x.key)}">${esc(x.login || x.user || x.kind)} &middot; ${esc(x.host)}</option>`).join("")}</select></div><input type="search" id="rQ" placeholder="Filter" aria-label="Filter repositories" style="margin-bottom:8px"><div id="rList" class="muted small">Loading&hellip;</div></div>`;
  let repos = [];
  const draw = () => { const q = $("#rQ").value.toLowerCase(); $("#rList").innerHTML = `<ul class="deploys">${repos.filter(r => !q || r.name.toLowerCase().includes(q)).slice(0, 100).map(r => `<li><span class="grow"><b>${esc(r.name)}</b> ${r.private ? `<span class="badge">private</span>` : ""}</span><button class="sm" data-u="${esc(r.url)}">Import</button></li>`).join("")}</ul>` || "No repositories."; };
  const load = async () => { $("#rList").textContent = "Loading…"; try { repos = (await api("GET", "/api/git/repos?key=" + encodeURIComponent($("#rAcc").value))).repos || []; draw(); } catch (e) { $("#rList").innerHTML = `<div class="alert bad">${esc(e.message)}</div>`; } };
  $("#rAcc").onchange = load; $("#rQ").oninput = draw;
  $("#rList").onclick = (e) => { const u = e.target.dataset.u; if (u) { pick(u); window.scrollTo(0, 0); $("#main").scrollTop = 0; } };
  load();
}

async function detectRepo(url, el) {
  if (!url) { el.innerHTML = `<div class="alert bad" style="margin-top:12px">Paste a link first.</div>`; return; }
  el.innerHTML = "";
  let d;
  try { d = await api("POST", "/api/import/detect", { url }); } catch (e) { el.innerHTML = `<div class="alert bad" style="margin-top:12px">${esc(e.message)}</div>`; return; }
  const ref = d.ref || {};
  if (ref.kind === "netlify") { el.innerHTML = `<div class="alert info" style="margin-top:12px">This is a Netlify site. <a href="#/new/netlify">Import it from Netlify</a> to copy the published files, rules and domain.</div>`; return; }
  const repoish = !["archive", "git"].includes(ref.kind);
  const branches = d.branches || [];
  el.innerHTML = `<div style="margin-top:16px"><div class="row"><span class="badge accent">${esc(d.label)}</span><b>${esc(ref.display || url)}</b>${d.connected ? `<span class="badge ok">account connected</span>` : ""}</div>
    ${d.warning && !d.needs_token ? `<div class="alert warn small">${esc(d.warning)}</div>` : ""}
    ${d.needs_token ? `<div class="alert warn">This repository is private or could not be found. Connect your ${esc(d.label)} account to import it.</div><div id="gTok"></div>` : ""}
    <div class="grid2" style="margin-top:10px">
      ${repoish || ref.kind === "git" ? `<div class="field"><label for="gBr">Branch</label>${branches.length ? `<select id="gBr">${branches.map(b => `<option ${b === (ref.branch || d.default_branch) ? "selected" : ""}>${esc(b)}</option>`).join("")}</select>` : `<input id="gBr" value="${esc(ref.branch || d.default_branch || "")}" placeholder="default branch">`}</div>` : ""}
      <div class="field"><label for="gSub">Base directory <span class="muted">(optional, e.g. <code>site</code> or <code>apps/web</code>)</span></label><input id="gSub" value="${esc(ref.subdir || ref.tree_path || "")}" placeholder="top folder"></div>
    </div>
    ${ref.kind === "git" && !d.git_available ? `<div class="alert warn small">This host has no download API that Dilreet knows, and Git is not installed on this PC. Install Git for Windows (git-scm.com) or use a GitHub/GitLab/Bitbucket/Gitea/Azure link.</div>` : ""}
    <div class="row"><button id="gFetch" class="primary">Import files</button>${!d.needs_token && !d.connected && repoish ? `<button id="gConn" class="ghost sm">Connect account (for private repositories)</button>` : ""}</div><div id="gJob"></div><div id="gTok2"></div></div>`;
  const tokenCard = (box) => connectAccountCard(box, { kind: ref.kind, host: ref.host, org: ref.org, label: d.label, token_url: d.token_url, hint: d.token_hint, onDone: () => detectRepo(url, el) });
  if (d.needs_token) tokenCard($("#gTok"));
  if ($("#gConn")) $("#gConn").onclick = () => tokenCard($("#gTok2"));
  $("#gFetch").onclick = (e) => busy(e.target, async () => {
    const j = await api("POST", "/api/import/fetch", { url, branch: $("#gBr") ? $("#gBr").value : "", subdir: $("#gSub").value });
    const res = await jobPanel(j.job, $("#gJob"), { title: "Downloading", doneTitle: "Files downloaded", failTitle: "Import failed" });
    if (res.error) { if (res.result && res.result.needs_token) tokenCard($("#gTok2")); return; }
    openReview(res.result, {});
  });
}

function connectAccountCard(el, o) {
  const k = o.kind;
  el.innerHTML = `<div class="card flat" style="margin-top:12px"><h3 style="margin-top:0">Connect ${esc(o.label || k)}</h3>
    <ol class="small"><li><a href="${esc(o.token_url || "#")}" target="_blank" rel="noopener">Create a token</a> (opens ${esc(o.label || k)} with read-only permissions pre-selected where the site supports it). ${esc(o.hint || "")}</li><li>Paste it here. It is stored encrypted on this PC and only sent to ${esc(o.label || k)}.</li></ol>
    ${k === "gitlab" || k === "gitea" ? `<div class="field"><label for="aHost">Server</label><input id="aHost" value="${esc(o.host || "")}"></div>` : ""}
    ${k === "azure" ? `<div class="field"><label for="aOrg">Organisation</label><input id="aOrg" value="${esc(o.org || "")}"></div>` : ""}
    ${k === "bitbucket" ? `<div class="field"><label for="aUser">Atlassian account e-mail</label><input id="aUser" type="email"></div>` : ""}
    <div class="field"><label for="aTok">Token</label><input id="aTok" type="password" autocomplete="off"></div><div class="aErr"></div><button class="primary aGo">Connect</button></div>`;
  $(".aGo", el).onclick = (e) => busy(e.target, async () => {
    try {
      const r = await api("POST", "/api/git/accounts", { kind: k, host: $("#aHost", el) ? $("#aHost", el).value : o.host || "", org: $("#aOrg", el) ? $("#aOrg", el).value : o.org || "", user: $("#aUser", el) ? $("#aUser", el).value : "", token: $("#aTok", el).value });
      toast(`Connected${r.login ? " as " + r.login : ""}`, "success");
      if (o.onDone) o.onDone();
    } catch (x) { $(".aErr", el).innerHTML = `<div class="alert bad">${esc(x.message)}</div>`; }
  });
}

async function netlifyImport(el) {
  el.innerHTML = `<div class="card"><h2>Import from Netlify</h2><p class="muted small">Copies the files of each site's published deploy, carries over <code>_redirects</code>, <code>_headers</code> and <code>netlify.toml</code> rules, and offers to move the custom domain. Your Netlify sites are not changed.</p><div id="nBody" class="muted">Loading&hellip;</div></div>`;
  const b = $("#nBody");
  try {
    const r = await api("GET", "/api/import/netlify/sites");
    const list = r.sites || [];
    b.innerHTML = list.length ? `<ul class="deploys">${list.map(s => `<li>${thumb(s.name)}<span class="grow"><b>${esc(s.name)}</b><div class="small muted">${esc(s.custom_domain || (s.ssl_url || s.url || "").replace(/^https?:\/\//, ""))} &middot; updated ${esc(ago(s.updated_at))}</div></span>${s.published_deploy ? `<button class="sm primary" data-id="${esc(s.id)}">Import</button>` : `<span class="badge">nothing published</span>`}</li>`).join("")}</ul><div id="nJob"></div>` : `<p>No sites in this Netlify account.</p>`;
    b.onclick = (e) => {
      const id = e.target.dataset.id;
      if (!id) return;
      busy(e.target, async () => {
        const j = await api("POST", "/api/import/netlify/fetch", { site_id: id });
        const res = await jobPanel(j.job, $("#nJob"), { title: "Copying files from Netlify", doneTitle: "Files copied" });
        if (!res.error) openReview(res.result, {});
      });
    };
  } catch (e) {
    if (e.data && e.data.needs_token) {
      b.innerHTML = "";
      connectAccountCard(b, { kind: "netlify", label: "Netlify", token_url: e.data.token_url, hint: e.data.token_hint, onDone: () => netlifyImport(el) });
    } else b.innerHTML = `<div class="alert bad">${esc(e.message)}</div>`;
  }
}

// ---------- review & publish (shared by drop, Git and Netlify) ----------
let staged = null;     // {sum, slug}
let pendingImport = null; // import waiting in App Testing
function openReview(sum, opt) { staged = { sum, slug: (opt && opt.slug) || "" }; go("#/review"); }

route(/^\/review$/, async () => {
  const v = view();
  if (!staged) { v.innerHTML = `<div class="empty">Nothing to review. <a href="#/new">Add new project</a></div>`; return; }
  await refreshOverview();
  if (!sites.length) await loadSites();
  const s = staged.sum, b = s.build || {};
  const repoKind = !["upload", "netlify", "archive", "git"].includes(s.kind);
  const target = staged.slug ? sites.find(x => x.slug === staged.slug) : null;
  const issues = (s.issue_list || []);
  v.innerHTML = `<div class="crumbs"><a href="#/projects">Projects</a> / ${target ? `<a href="#/project/${esc(target.slug)}">${esc(target.name)}</a> / ` : ""}</div>
    <div class="head"><h1>${target ? "Deploy a new version" : "Review and publish"}</h1></div>
    <div class="card"><div class="kv">
      <div>Source</div><div><b>${esc(s.display)}</b>${s.branch ? ` &middot; branch <code>${esc(s.branch)}</code>` : ""}${s.sha ? ` &middot; commit <code>${esc(s.sha.slice(0, 7))}</code>` : ""}</div>
      <div>Framework</div><div>${esc(s.framework || "Plain HTML")}</div>
      <div>Files</div><div>${s.files} files, ${esc(humanSize(s.bytes))}</div>
      <div>Publish directory</div><div><code>${esc(s.publish_dir || "/")}</code> <span class="muted small">${esc(s.publish_note || "")}</span></div>
      ${s.carried_rules && s.carried_rules.length ? `<div>Netlify rules</div><div>${s.carried_rules.map(x => `<code>${esc(x)}</code>`).join(" ")} carried over</div>` : ""}
      ${s.custom_domain ? `<div>Custom domain</div><div>${esc(s.custom_domain)} <span class="muted small">(you can move it after publishing)</span></div>` : ""}
    </div>
    ${issues.length ? `<details ${s.fails ? "open" : ""} style="margin-top:10px"><summary>${s.fails ? `<span class="badge bad">${s.fails} problem(s)</span>` : ""} ${issues.length - s.fails ? `<span class="badge warn">${issues.length - s.fails} note(s)</span>` : ""}</summary><ul class="small">${issues.map(i => `<li><b>${esc(i.level)}</b> ${esc(i.msg)}${i.fix ? ` <span class="muted">&mdash; ${esc(i.fix)}</span>` : ""}</li>`).join("")}</ul></details>` : ""}
    </div>
    <div id="buildBox"></div>
    <div class="card" id="pubCard"><h2>Publish</h2>
      ${target ? `<p>This replaces the live version of <b>${esc(target.name)}</b>. The old version stays available for rollback.</p>` : `
      <div class="field"><label for="pName">Project name</label><input id="pName" value="${esc(s.suggested_name || "")}"><div class="hint">Used for the free address: <code id="pPrev"></code></div></div>
      ${sites.length ? `<details><summary class="small muted" style="cursor:pointer">Or update an existing project instead</summary><div class="field" style="margin-top:8px"><select id="pExisting"><option value="">Create a new project</option>${sites.map(x => `<option value="${esc(x.slug)}">${esc(x.name)}</option>`).join("")}</select></div></details>` : ""}`}
      <label class="switch"><input type="checkbox" id="pTest" ${store("testFirst") ? "checked" : ""}> Test before publishing <span class="muted small">(opens App Testing with a preview on this PC first)</span></label>
      ${repoKind ? `<label class="switch"><input type="checkbox" id="pAuto" checked> Deploy automatically when the repository changes</label>
        <div class="row small" id="pAutoOpts" style="margin-left:52px">Check every <input type="number" id="pInt" value="5" min="1" max="1440" style="width:80px"> minutes</div>` : ""}
      ${s.kind === "upload" && s.local_path ? `<label class="switch"><input type="checkbox" id="pWatch" checked> Watch <code>${esc(s.local_path)}</code> and redeploy when files change</label>` : ""}
      <div class="row" style="margin-top:12px"><button id="pGo" class="primary lg">Publish</button><button id="pCancel" class="ghost">Cancel</button><span id="pNote" class="small muted"></span></div>
      <div id="pJob"></div></div>`;
  const nameEl = $("#pName");
  if (nameEl) { const upd = () => $("#pPrev").textContent = (nameEl.value.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "") || "site") + ".<server-ip>.sslip.io"; nameEl.oninput = upd; upd(); }
  if ($("#pAuto")) $("#pAuto").onchange = () => $("#pAutoOpts").classList.toggle("hidden", !$("#pAuto").checked);
  $("#pTest").onchange = () => store("testFirst", $("#pTest").checked);
  $("#pCancel").onclick = async () => { await api("DELETE", "/api/import/" + s.import_id).catch(() => {}); staged = null; go(target ? "#/project/" + target.slug : "#/projects"); };
  const needsBuild = b.needed && !s.built;
  const pubReady = () => { $("#pGo").disabled = !!(staged.sum.build || {}).needed && !staged.sum.built || ov.stage === "disconnected"; $("#pNote").textContent = $("#pGo").disabled ? (ov.stage === "disconnected" ? "Reconnect to your server to publish." : "Build the project first (above).") : (staged.sum.has_index ? "" : "No index.html found yet."); };
  if (needsBuild) {
    $("#buildBox").innerHTML = `<div class="card" style="border-color:var(--accent)"><h2>This project needs to be built</h2>
      <p>${esc(b.reason || "It is a source project, not a finished website.")}</p>
      <p class="small">Dilreet will run these commands <b>on this PC</b> in a temporary copy of the files: <code>${esc(b.install || "npm install")}</code> then <code>${esc(b.command || "npm run build")}</code>. Packages are downloaded from the npm registry and the project's build scripts run with your user rights. Only do this for projects you trust.</p>
      ${b.node_ok ? "" : `<div class="alert warn">${esc(b.node_hint || "Node.js is not installed.")} <a href="https://nodejs.org/en/download" target="_blank" rel="noopener">Download Node.js LTS</a>, install it, then click Build again.</div>`}
      <div class="row"><button id="bGoBuild" class="primary">Build it for me</button><span class="small muted">or build it yourself and drop the <code>dist</code>/<code>build</code> folder instead.</span></div><div id="bJob"></div></div>`;
    $("#bGoBuild").onclick = (e) => busy(e.target, async () => {
      const j = await api("POST", `/api/import/${s.import_id}/build`);
      e.target.classList.add("hidden");
      const res = await jobPanel(j.job, $("#bJob"), { title: "Building", doneTitle: "Build finished", failTitle: "Build failed" });
      if (res.error) { e.target.classList.remove("hidden"); return; }
      staged.sum = res.result; toast("Build finished", "success"); rerender();
    });
  }
  pubReady();
  $("#pGo").onclick = (e) => busy(e.target, async () => {
    const slug = target ? target.slug : ($("#pExisting") && $("#pExisting").value) || "";
    const opts = { slug, name: nameEl ? nameEl.value.trim() : "", auto_deploy: !!($("#pAuto") && $("#pAuto").checked), interval_min: $("#pInt") ? +$("#pInt").value : 5, test_first: $("#pTest").checked, watch: !!($("#pWatch") && $("#pWatch").checked) };
    if ($("#pTest").checked) {
      const t = await api("POST", `/api/import/${s.import_id}/test`);
      pendingImport = { id: s.import_id, opts, sum: s, label: target ? target.name : (opts.name || s.suggested_name) };
      test = null;
      go("#/testing");
      renderTest(t);
      return;
    }
    if (slug && !target) { const ex = sites.find(x => x.slug === slug); if (!await confirmBox("Replace the live version?", `<p>This publishes the files to <b>${esc(ex ? ex.name : slug)}</b>. The old version stays available for rollback.</p>`, "Publish")) return; }
    await publishImport(s.import_id, opts, $("#pJob"), s.fails);
  });
}, "projects");

async function publishImport(id, opts, el, fails) {
  if (fails && !opts.accept_fails) {
    if (!await confirmBox("Publish with problems?", `<p>${fails} check(s) failed. Visitors may see broken pages.</p>`, "Publish anyway", true)) return;
    opts.accept_fails = true;
  }
  let r;
  try { r = await api("POST", `/api/import/${id}/deploy`, opts); }
  catch (e) { if (e.data && e.data.needs_build) { toast("Build the project first", "warn"); return; } throw e; }
  const res = await jobPanel(r.job, el, { title: "Publishing", doneTitle: "Published", failTitle: "Publishing failed" });
  staged = null;
  if (res.error) { el.appendChild(h(`<p class="small muted">Start again from <a href="#/new">Add new site</a>.</p>`)); return false; }
  await loadSites();
  showPublished(res.result, el);
  return true;
}
function showPublished(r, el) {
  const s = sites.find(x => x.slug === r.slug) || {};
  const url = r.instant_url || siteURL(s);
  if (r.custom_domain) store("prefillDomain-" + r.slug, r.custom_domain);
  el.innerHTML = `<div class="card" style="border-color:var(--ok)"><h2>&#127881; Your site is live</h2>
    ${url ? `<p class="big"><a href="${esc(url)}" target="_blank" rel="noopener">${esc(url.replace(/^https?:\/\//, ""))}</a> <button class="sm copy" data-copy="${esc(url)}">Copy</button></p><p class="hint">The free https address can take a minute until its certificate is ready.</p>` : ""}
    <div class="row"><a class="btn primary" href="#/project/${esc(r.slug)}/domain">${r.custom_domain ? "Move " + esc(r.custom_domain) + " here" : "Set up a custom domain"}</a><a class="btn" href="#/project/${esc(r.slug)}">Open project</a></div></div>`;
  toast("Your site is live", "success");
}
document.addEventListener("click", (e) => {
  const c = e.target.closest("[data-copy]");
  if (!c) return;
  navigator.clipboard.writeText(c.dataset.copy).then(() => { const t = c.textContent; c.textContent = "Copied"; setTimeout(() => c.textContent = t, 1200); }, () => toast("Copy failed; select the text instead", "warn"));
});

// ---------- App Testing (ported from 0.2; preview server is a separate localhost origin without API access) ----------
let test = null;
let previewOrigin = "";
let logFrom = 0, logItems = [], logTimer = null;
let vp = { w: 1280, h: 800, full: false };
let lastNav = "";
const onTesting = () => (location.hash || "").startsWith("#/testing");

route(/^\/testing$/, async () => {
  await loadSites();
  fillSiteSelect();
  renderPending();
  const t = await api("GET", "/api/test").catch(e => { err(e); return { active: false }; });
  if (!test || test.id !== t.id || !t.active) renderTest(t); else { renderTest(t); }
}, "testing");

function renderPending() {
  const p = $("#tPending");
  const deployCard = $("#tDeploy").closest(".card");
  if (!pendingImport) { p.classList.add("hidden"); if (deployCard) deployCard.classList.remove("hidden"); return; }
  p.innerHTML = `<div class="row"><span class="grow">Testing <b>${esc(pendingImport.label || pendingImport.sum.display)}</b> before publishing. Click around the preview below; when it looks right, publish it.</span><button id="pendPub" class="primary">Publish this version</button><button id="pendCancel" class="ghost sm">Cancel</button></div><div id="pendJob"></div>`;
  p.classList.remove("hidden");
  if (deployCard) deployCard.classList.add("hidden");
  $("#pendCancel").onclick = async () => { await api("DELETE", "/api/import/" + pendingImport.id).catch(() => {}); pendingImport = null; renderPending(); };
  $("#pendPub").onclick = (e) => busy(e.target, async () => {
    const fails = test && test.active ? (test.report.counts.fail || 0) : pendingImport.sum.fails;
    const pi = pendingImport;
    if (!fails && !await confirmBox("Publish this version?", `<p>Publish <b>${esc(pi.label)}</b> now?</p>`, "Publish")) return;
    const ok = await publishImport(pi.id, Object.assign({}, pi.opts, fails ? {} : { accept_fails: false }), $("#pendJob"), fails);
    if (ok) { pendingImport = null; e.target.classList.add("hidden"); $("#pendCancel").classList.add("hidden"); }
  });
}

function fillSiteSelect() {
  const sel = $("#tSite");
  const prev = sel.value || store("testSite") || "";
  sel.innerHTML = `<option value="">Choose a site&hellip;</option>` + sites.map(s => `<option value="${esc(s.slug)}">${esc(s.name)}${s.domain ? " - " + esc(s.domain) : ""}</option>`).join("");
  if (prev && sites.some(s => s.slug === prev)) sel.value = prev;
  updateDeployBtn();
}
$("#tSite").onchange = () => { store("testSite", $("#tSite").value); updateDeployBtn(); };

function updateDeployBtn() {
  const ok = !!(test && test.active && $("#tSite").value);
  $("#tDeploy").disabled = !ok;
  const n = test && test.active ? (test.report.counts.fail || 0) : 0;
  $("#tDeployNote").textContent = !test || !test.active ? "Load and test a version first." :
    !$("#tSite").value ? "Choose the project to deploy to." :
    n ? `${n} check(s) failed: you will be asked to confirm.` : "All checks passed or only warnings.";
}

function renderTest(t) {
  const first = !test || !t.active || !test.active || test.id !== t.id;
  test = t;
  const active = !!t.active;
  $("#tMain").classList.toggle("hidden", !active);
  $("#tStop").classList.toggle("hidden", !active);
  $("#tReload").classList.toggle("hidden", !(active && t.reloadable));
  clearTimeout(logTimer);
  if (!active) { $("#tInfo").innerHTML = ""; previewOrigin = ""; updateDeployBtn(); return; }
  const ws = t.workspace, rep = t.report;
  previewOrigin = new URL(t.preview_url).origin;
  $("#tInfo").innerHTML = `<p><b>Testing:</b> ${esc(ws.source)} &middot; ${rep.files} files, ${esc(humanSize(rep.bytes))} &middot; published folder: <code>${esc(ws.publish_dir || "/")}</code> <span class="muted">(${esc(ws.publish_note)})</span><br>
    <span class="muted">Framework: ${esc(rep.project.framework)} &middot; rules: ${rep.redirect_rules} redirect, ${rep.header_rules} header &middot; fingerprint ${esc(rep.hash.slice(0, 12))} &middot; preview <code>${esc(t.preview_url)}</code></span></p>`;
  renderReport();
  setTimeout(applyViewport, 0);
  if (first) {
    logFrom = 0; logItems = []; $("#tLog").innerHTML = "";
    $("#bExternal").classList.add("hidden");
    navigate("/");
  }
  pollLog();
  updateDeployBtn();
}

const order = { fail: 0, warn: 1, info: 2, pass: 3 };
function renderReport() {
  const rep = test.report;
  const c = rep.counts;
  $("#tSummary").innerHTML = `<span class="pill fail">${c.fail || 0} fail</span> <span class="pill warn">${c.warn || 0} warn</span> <span class="pill pass">${c.pass || 0} pass</span>` + (c.info ? ` <span class="pill info">${c.info} info</span>` : "");
  const showPass = $("#rShowPass").checked;
  const list = rep.checks.slice().sort((a, b) => order[a.status] - order[b.status]).filter(x => showPass || x.status !== "pass");
  $("#tReport").innerHTML = list.map(ch => `<div class="check-item ${esc(ch.status)}">
      <h3><span class="pill ${esc(ch.status)}">${esc(ch.status)}</span>${esc(ch.title)}</h3>
      <div>${esc(ch.summary)}</div>
      ${ch.status !== "pass" && ch.explain ? `<div class="small muted">${esc(ch.explain)}</div>` : ""}
      ${ch.items && ch.items.length ? `<ul>${ch.items.map(it => `<li>${it.file ? `<code>${esc(it.file)}${it.line ? ":" + it.line : ""}</code> ` : ""}${esc(it.detail)}${it.file && /\.html?$/i.test(it.file) ? ` <a href="#" data-page="${esc(it.file)}">view</a>` : ""}</li>`).join("")}${ch.more ? `<li class="muted">&hellip;and ${ch.more} more</li>` : ""}</ul>` : ""}
      ${ch.status !== "pass" && ch.fix ? `<div class="fix"><b>How to fix:</b> ${esc(ch.fix)}</div>` : ""}
      ${ch.status === "fail" ? `<button class="sm ghost" data-ask="${esc(ch.title + ": " + ch.summary + (ch.fix ? "\nSuggested fix: " + ch.fix : ""))}">Ask assistant about this</button>` : ""}
    </div>`).join("") || `<p class="ok">Everything passed.</p>`;
}
$("#rShowPass").onchange = () => test && test.active && renderReport();
$("#tReport").onclick = (e) => {
  if (e.target.dataset.ask) { askAbout("App Testing check failed: " + e.target.dataset.ask); return; }
  const f = e.target.dataset.page;
  if (!f) return;
  e.preventDefault();
  let p = "/" + f;
  if (p.endsWith("/index.html")) p = p.slice(0, -10);
  navigate(p);
  $("#frameWrap").scrollIntoView({ behavior: "smooth" });
};

async function loadTest(sel) {
  $("#tLoading").classList.remove("hidden");
  try {
    const t = await api("POST", "/api/test/load", formFrom(sel));
    $("#tZip").value = ""; $("#tDir").value = "";
    test = null;
    renderTest(t);
    toast("Site loaded: " + (t.report.counts.fail ? t.report.counts.fail + " check(s) failed" : "no failed checks"), t.report.counts.fail ? "warn" : "success");
  } finally { $("#tLoading").classList.add("hidden"); }
}
$("#tLoad").onclick = (e) => busy(e.target, async () => {
  const zip = $("#tZip").files[0], dir = $("#tDir").files, lp = $("#tPath").value.trim();
  await loadTest(zip ? { zip } : dir.length ? { files: Array.from(dir).map(f => [f, f.webkitRelativePath || f.name]).filter(([, p]) => !SKIP_DIRS.test(p)) } : { path: lp });
});
(() => {
  const d = $("#tDrop");
  ["dragenter", "dragover"].forEach(ev => d.addEventListener(ev, e => { e.preventDefault(); d.classList.add("over"); }));
  ["dragleave", "drop"].forEach(ev => d.addEventListener(ev, e => { e.preventDefault(); d.classList.remove("over"); }));
  d.addEventListener("drop", async e => { const sel = await readDropped(e.dataTransfer); loadTest(sel).catch(err); });
  d.addEventListener("click", () => $("#tDir").click());
  d.addEventListener("keydown", e => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); $("#tDir").click(); } });
})();
$("#tReload").onclick = (e) => busy(e.target, async () => {
  const path = currentPath();
  const t = await api("POST", "/api/test/reload");
  test = null;
  renderTest(t);
  navigate(path);
  toast("Reloaded and re-tested");
});
$("#tStop").onclick = (e) => busy(e.target, async () => {
  await api("POST", "/api/test/stop");
  $("#pFrame").src = "about:blank";
  renderTest({ active: false });
});

function currentPath() { return lastNav || "/"; }
function navigate(path) {
  if (!previewOrigin) return;
  let p = String(path || "/").trim();
  try {
    const u = new URL(p, previewOrigin + "/");
    if (u.origin !== previewOrigin) {
      if (/^https?:$/.test(u.protocol)) { showExternal(u.href); return; }
      p = "/";
    } else p = u.pathname + u.search + u.hash;
  } catch (e) { p = "/"; }
  lastNav = p;
  $("#bAddr").value = p;
  $("#pFrame").src = previewOrigin + p;
}
function showExternal(href) {
  const box = $("#bExternal");
  box.classList.remove("hidden");
  box.innerHTML = `This page wants to open an outside address: <code>${esc(href)}</code> <a href="${esc(href)}" target="_blank" rel="noopener noreferrer">Open it in a new tab</a> <button class="sm ghost" id="bExtClose">dismiss</button>`;
  $("#bExtClose").onclick = () => box.classList.add("hidden");
}
window.addEventListener("message", (e) => {
  const fr = $("#pFrame");
  if (!previewOrigin || e.origin !== previewOrigin || e.source !== fr.contentWindow) return;
  const d = e.data || {};
  if (d.type === "hp-nav" && typeof d.href === "string") {
    try { const u = new URL(d.href); if (u.origin === previewOrigin) { lastNav = u.pathname + u.search + u.hash; if (document.activeElement !== $("#bAddr")) $("#bAddr").value = lastNav; } } catch (x) {}
  } else if (d.type === "hp-external" && typeof d.href === "string" && /^https?:\/\//i.test(d.href)) showExternal(d.href);
});
function frameCmd(cmd) { const w = $("#pFrame").contentWindow; if (w && previewOrigin) w.postMessage({ hpCmd: cmd }, previewOrigin); }
$("#bBack").onclick = () => frameCmd("back");
$("#bFwd").onclick = () => frameCmd("forward");
$("#bReload").onclick = () => navigate(currentPath());
$("#bHome").onclick = () => navigate("/");
$("#bGo").onclick = () => navigate($("#bAddr").value);
$("#bAddr").onkeydown = (e) => { if (e.key === "Enter") navigate($("#bAddr").value); };
$("#bOpen").onclick = () => { if (previewOrigin) window.open(previewOrigin + currentPath(), "_blank", "noopener"); };

function applyViewport() {
  const box = $("#frameBox"), wrap = $("#frameWrap");
  if (vp.full) {
    box.classList.add("full");
    box.style.transform = ""; box.style.width = ""; box.style.height = "75vh";
    box.style.marginRight = ""; box.style.marginBottom = "";
    $("#vpLabel").textContent = "full width";
    return;
  }
  box.classList.remove("full");
  if (!wrap.clientWidth) return;
  const avail = wrap.clientWidth - 24;
  const s = $("#vpFit").checked && vp.w > avail ? avail / vp.w : 1;
  box.style.width = vp.w + "px";
  box.style.height = vp.h + "px";
  box.style.transform = s < 1 ? `scale(${s})` : "";
  box.style.marginRight = s < 1 ? (vp.w * s - vp.w) + "px" : "";
  box.style.marginBottom = s < 1 ? (vp.h * s - vp.h) + "px" : "";
  $("#vpLabel").textContent = `${vp.w} × ${vp.h}` + (s < 1 ? ` (shown at ${Math.round(s * 100)}%)` : "");
  $("#vpW").value = vp.w; $("#vpH").value = vp.h;
}
$$("[data-vp]").forEach(b => b.onclick = () => {
  $$("[data-vp]").forEach(x => x.classList.toggle("primary", x === b));
  if (b.dataset.vp === "full") vp.full = true;
  else { const [w, hh] = b.dataset.vp.split("x").map(Number); vp = { w, h: hh, full: false }; }
  applyViewport();
});
$("#vpSet").onclick = () => {
  const w = Math.min(3840, Math.max(200, +$("#vpW").value || 1280)), hh = Math.min(3000, Math.max(200, +$("#vpH").value || 800));
  vp = { w, h: hh, full: false };
  $$("[data-vp]").forEach(x => x.classList.remove("primary"));
  applyViewport();
};
$("#vpRotate").onclick = () => { if (!vp.full) { vp = { w: vp.h, h: vp.w, full: false }; applyViewport(); } };
$("#vpFit").onchange = applyViewport;
window.addEventListener("resize", () => { if (onTesting()) applyViewport(); });

async function pollLog() {
  clearTimeout(logTimer);
  if (!test || !test.active) return;
  const id = test.id;
  try {
    const r = await api("GET", "/api/test/events?from=" + logFrom);
    if (!r.active || !test || r.id !== id) return;
    if (r.events.length) { logItems = logItems.concat(r.events).slice(-1000); logFrom = r.last; renderLog(); }
    $("#logCounts").textContent = `${r.errors} error(s), ${r.warnings} warning(s)`;
  } catch (e) {}
  if (onTesting()) logTimer = setTimeout(pollLog, 1000);
}
function logFilter() { return $("input[name=logf]:checked").value; }
$$("input[name=logf]").forEach(r => r.onchange = renderLog);
function renderLog() {
  const all = logFilter() === "all";
  const box = $("#tLog");
  const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 20;
  const rows = logItems.filter(e => all || e.kind !== "request" || e.level !== "info").slice(-500);
  box.innerHTML = rows.map(e => {
    const time = new Date(e.time).toLocaleTimeString();
    const text = e.kind === "request" ? `${e.method} ${e.url} → ${e.status}${e.msg ? " " + e.msg : ""}` : `[${e.kind}] ${e.msg}` + (e.url && e.kind !== "nav" ? `  @ ${e.url.replace(previewOrigin, "")}` : "");
    return `<div class="${esc(e.level)}">${esc(time)} ${esc(text)}${e.via ? `<span class="via">  (${esc(e.via)})</span>` : ""}${e.stack ? `<br><span class="via">${esc(e.stack)}</span>` : ""}</div>`;
  }).join("") || `<div class="info">No problems logged yet. Click around the site; errors appear here.</div>`;
  if (atBottom) box.scrollTop = box.scrollHeight;
}
$("#logClear").onclick = async () => { await api("POST", "/api/test/events/clear").catch(() => {}); logItems = []; renderLog(); };
$("#rNginx").onclick = async () => {
  const pre = $("#tNginx");
  if (!pre.classList.contains("hidden")) { pre.classList.add("hidden"); return; }
  const r = await fetch("/api/test/nginx");
  pre.textContent = await r.text();
  pre.classList.remove("hidden");
};
$("#tDeploy").onclick = (e) => busy(e.target, async () => {
  const slug = $("#tSite").value;
  const s = sites.find(x => x.slug === slug);
  if (!test || !test.active || !s) return;
  const fails = test.report.checks.filter(c => c.status === "fail");
  let accept = false;
  if (fails.length) {
    if (!await confirmBox(`${fails.length} check(s) failed`, `<ul>${fails.map(f => `<li><b>${esc(f.title)}</b>: ${esc(f.summary)}</li>`).join("")}</ul><p>Visitors may see broken pages. Deploy to <b>${esc(s.name)}</b> anyway?</p>`, "Deploy anyway", true)) return;
    accept = true;
  } else if (!await confirmBox("Deploy the tested version?", `<p>${test.report.files} files (fingerprint <code>${esc(test.report.hash.slice(0, 12))}</code>) to <b>${esc(s.name)}</b>${s.domain ? " (" + esc(s.domain) + ")" : ""}.</p>`, "Deploy")) return;
  const r = await api("POST", "/api/test/deploy", { slug, test_id: test.id, accept_fails: accept });
  const j = await jobPanel(r.job, $("#tDeployLog"), { title: "Deploying", doneTitle: "Deployed to " + s.name });
  if (!j.error) toast("Deployed to " + s.name, "success");
  loadSites();
});

// ---------- project pages (Overview / Files / Deploys / Domain / Settings) ----------
async function getSite(slug) { await loadSites(); const s = sites.find(x => x.slug === slug); if (!s) throw new Error("Project not found: " + slug); return s; }
const projTabs = [["", "Overview"], ["files", "Files"], ["deploys", "Deploys"], ["domain", "Domain"], ["settings", "Settings"]];
function projectHead(s, tab) {
  const url = siteURL(s);
  return `<div class="crumbs"><a href="#/projects">Projects</a> /</div>
    <div class="head">${thumb(s.name)}<div><h1 style="margin:0">${esc(s.name)}</h1>${url ? `<a href="${esc(url)}" target="_blank" rel="noopener" class="small">${esc(url.replace(/^https?:\/\//, ""))} &#8599;</a>` : `<span class="muted small">no address yet</span>`}</div><span class="grow"></span>${statusBadge(s)}${url ? `<a class="btn" href="${esc(url)}" target="_blank" rel="noopener">Open site</a>` : ""}</div>
    <nav class="ptabs" aria-label="Project">${projTabs.map(([k, l]) => `<a href="#/project/${esc(s.slug)}${k ? "/" + k : ""}" ${k === (tab || "") ? 'aria-current="page"' : ""}>${l}</a>`).join("")}</nav>`;
}
function fmtTime(t) { if (!t) return ""; const d = new Date(t); return isNaN(d) ? t : d.toLocaleString(); }
const srcLabel = { drop: "Drag & drop", folder: "Project folder", git: "Git", netlify: "Netlify", watch: "Watched folder", archive: "Re-published" };
function checksBadges(c) { if (!c) return ""; return `${c.fail ? `<span class="badge bad">${c.fail} failed</span>` : ""}${c.warn ? `<span class="badge warn">${c.warn} warnings</span>` : ""}${!c.fail && !c.warn ? `<span class="badge ok">checks passed</span>` : ""}`; }

route(/^\/project\/([^/]+)(?:\/(files|deploys|settings))?$/, async (slug, tab) => {
  await refreshOverview();
  const s = await getSite(slug);
  const v = view();
  v.innerHTML = projectHead(s, tab) + `${sitesErr ? `<div class="alert warn small">Live status unavailable: ${esc(sitesErr)}</div>` : ""}<div id="pBody"></div>`;
  const b = $("#pBody");
  if (tab === "files") return projectFiles(s, b);
  if (tab === "deploys") return projectDeploys(s, b);
  if (tab === "settings") return projectSettings(s, b);
  return projectOverview(s, b);
}, "projects");

function projectOverview(s, b) {
  const g = s.git;
  b.innerHTML = `<div class="grid2">
      <div class="card"><h2>Deploy a new version</h2><div id="sdz"></div></div>
      <div class="card"><h2>Domain</h2>${s.domain ? `<p><b>${esc(s.domain)}</b>${s.include_www ? " + www" : ""} ${s.domain_live ? (s.https_active ? `<span class="badge ok">HTTPS active</span>` : `<span class="badge warn">connected, HTTPS pending</span>`) : `<span class="badge warn">waiting for DNS</span>`}${s.dns_mode === "cloudflare" ? ` <span class="badge info">Cloudflare DNS</span>` : ""}${s.dns_provider ? ` <span class="badge info">${esc(s.dns_provider)} API</span>` : ""}</p>` : `<p class="muted">No custom domain yet.</p>`}
        ${(s.pending_ns || []).length ? `<div class="alert info small">Waiting for the nameserver change: ${s.pending_ns.map(n => `<code>${esc(n)}</code>`).join(" ")}</div>` : ""}
        ${s.instant_url ? `<p class="small">Free address: <a href="${esc(s.instant_url)}" target="_blank" rel="noopener">${esc(s.instant_url.replace(/^https?:\/\//, ""))}</a></p>` : ""}
        <a class="btn ${s.domain ? "" : "primary"}" href="#/project/${esc(s.slug)}/domain">${s.domain ? "Manage domain" : "Add a custom domain"}</a></div>
    </div>
    ${g ? `<div class="card"><div class="card-head"><h2>Repository</h2><span class="grow"></span><button id="gPull" class="primary sm">Pull latest and deploy</button></div>
      <p><span class="badge accent">${esc(g.kind)}</span> <b>${esc(g.display)}</b> &middot; branch <code>${esc(g.branch || "default")}</code>${g.subdir ? ` &middot; folder <code>${esc(g.subdir)}</code>` : ""}${g.last_sha ? ` &middot; deployed commit <code>${esc(g.last_sha.slice(0, 7))}</code>` : ""}${g.build ? ` &middot; <span class="badge">built on this PC</span>` : ""}</p>
      <label class="switch"><input type="checkbox" id="gAuto" ${g.auto_deploy ? "checked" : ""} ${["archive", "git"].includes(g.kind) ? "disabled" : ""}> Deploy automatically when the repository changes</label>
      <div class="row small" style="margin-left:52px">Check every <input type="number" id="gInt" value="${esc(g.interval_min || 5)}" min="1" max="1440" style="width:80px"> minutes &middot; last checked ${esc(ago(g.last_check))}</div>
      <label class="switch"><input type="checkbox" id="gTest" ${g.test_first ? "checked" : ""}> Test before publishing automatic deploys <span class="muted small">(new commits wait for you in App Testing if checks fail)</span></label>
      <div class="row"><button id="gUnlink" class="ghost sm">Unlink repository</button></div><div id="gJob"></div></div>` : ""}
    <div class="grid2">
      <div class="card"><div class="card-head"><h2>Latest deploys</h2><span class="grow"></span><a class="small" href="#/project/${esc(s.slug)}/deploys">All deploys &rarr;</a></div><div id="latest" class="muted small">Loading&hellip;</div></div>
      <div class="card"><h2>Project folder on this PC</h2><p class="small muted">Every deploy is saved here. Edit the files in <b>current</b> and deploy them again.</p><p class="small"><code id="pDir">&hellip;</code></p>
        <div class="row"><button id="pOpen" class="sm">Open in File Explorer</button><a class="btn sm ghost" href="#/project/${esc(s.slug)}/files">Browse files</a></div></div>
    </div>`;
  dropZone($("#sdz"), { compact: true, slug: s.slug });
  api("GET", `/api/projects/${s.slug}/deploys`).then(r => {
    $("#pDir").textContent = r.dir;
    const l = (r.deploys || []).slice(0, 3);
    $("#latest").innerHTML = l.length ? `<ul class="deploys">${l.map(d => `<li><span class="grow"><b>${esc(fmtTime(d.time))}</b> ${d.live ? `<span class="badge ok">Live</span>` : ""}<div class="small muted">${esc(srcLabel[d.source] || d.source)} &middot; ${d.files} files &middot; ${esc(humanSize(d.bytes))}</div></span>${d.status === "failed" ? `<span class="badge bad">failed</span>` : checksBadges(d.checks)}</li>`).join("")}</ul>` : "No deploys saved on this PC yet. The next deploy is saved automatically.";
  }).catch(e => { $("#latest").innerHTML = `<div class="alert warn">${esc(e.message)}</div>`; });
  $("#pOpen").onclick = (e) => busy(e.target, () => api("POST", `/api/projects/${s.slug}/open`, { what: "folder" }));
  if (g) {
    $("#gPull").onclick = (e) => busy(e.target, async () => pullDeploy(s.slug, false, $("#gJob")));
    const save = (body) => api("POST", `/api/sites/${s.slug}/git`, body).then(() => toast("Saved", "success")).catch(err);
    $("#gAuto").onchange = () => save({ auto_deploy: $("#gAuto").checked });
    $("#gTest").onchange = () => save({ test_first: $("#gTest").checked });
    $("#gInt").onchange = () => save({ interval_min: +$("#gInt").value });
    $("#gUnlink").onclick = async () => { if (await confirmBox("Unlink the repository?", "The site stays online. Automatic deploys from the repository stop.", "Unlink")) { await save({ unlink: true }); rerender(); } };
  }
}

// Files tab: the project's current\ folder on this PC.
const textExt = /\.(html?|css|js|mjs|json|txt|md|xml|svg|toml|ya?ml|csv|map|webmanifest|ts|jsx|tsx|vue|astro|ini|conf|htaccess|txt)$|(^|\/)(_redirects|_headers)$/i;
const imgExt = /\.(png|jpe?g|gif|webp|avif|ico|bmp)$/i;
async function projectFiles(s, b) {
  b.innerHTML = `<div class="card"><div class="card-head"><h2>Files</h2><span class="grow"></span>
      <button id="fOpen" class="sm">Open folder</button><a class="btn sm" href="/api/projects/${esc(s.slug)}/folder.zip" download>Download as .zip</a><button id="fDeploy" class="sm primary">Deploy from this folder</button></div>
      <p class="small muted">The files of the latest deploy, on this PC: <code id="fDir">&hellip;</code>. Change them with any editor, then click <b>Deploy from this folder</b>.</p>
      <div class="files"><div class="ftree" id="fTree" role="tree" aria-label="Project files"><p class="muted small">Loading&hellip;</p></div><div class="fprev" id="fPrev"><div class="empty small">Select a file to preview it.</div></div></div><div id="fJob"></div></div>`;
  let r;
  try { r = await api("GET", `/api/projects/${s.slug}/files`); } catch (e) { $("#fTree").innerHTML = `<div class="alert bad">${esc(e.message)}</div>`; return; }
  $("#fDir").textContent = r.current;
  const items = r.items || [];
  const files = items.filter(x => !x.dir);
  if (!files.length) $("#fTree").innerHTML = `<div class="empty small">No files yet. They appear here after the next deploy, or copy files into the folder.</div>`;
  else {
    const rows = items.slice().sort((a, b) => a.path.localeCompare(b.path)).map(x => {
      const depth = x.path.split("/").length - 1, name = x.path.split("/").pop();
      return `<div class="frow ${x.dir ? "dir" : ""}" role="treeitem" tabindex="0" data-p="${esc(x.path)}" ${x.dir ? 'data-dir="1"' : ""} style="padding-left:${8 + depth * 16}px"><span class="fname">${x.dir ? "&#128193;" : "&#128196;"} ${esc(name)}</span>${x.dir ? "" : `<span class="fmeta">${esc(humanSize(x.size))}</span><span class="fmeta">${esc(fmtTime(x.mtime))}</span>`}</div>`;
    });
    $("#fTree").innerHTML = rows.join("") + `<div class="small muted" style="padding:8px">${files.length} files &middot; ${esc(humanSize(r.bytes))}${r.truncated ? " (list shortened)" : ""}</div>`;
  }
  const raw = (p) => `/api/projects/${encodeURIComponent(s.slug)}/raw/${p.split("/").map(encodeURIComponent).join("/")}`;
  const show = async (p, isDir) => {
    $$(".frow", $("#fTree")).forEach(x => x.classList.toggle("sel", x.dataset.p === p));
    const pv = $("#fPrev");
    const tools = `<div class="row" style="margin-bottom:8px"><b class="grow" style="word-break:break-all">${esc(p)}</b>${isDir ? "" : `<button class="sm" data-a="editor">Open in editor</button><a class="btn sm ghost" href="${raw(p)}?download=1" download>Download</a>`}<button class="sm ghost" data-a="reveal">Show in Explorer</button></div>`;
    if (isDir) { pv.innerHTML = tools; }
    else if (imgExt.test(p)) pv.innerHTML = tools + `<div class="imgprev"><img src="${raw(p)}" alt="${esc(p)}"></div>`;
    else if (/\.html?$/i.test(p)) pv.innerHTML = tools + `<div class="row small" style="margin-bottom:6px"><label class="check" style="margin:0"><input type="radio" name="hv" value="page" checked> Page</label><label class="check" style="margin:0"><input type="radio" name="hv" value="code"> Code</label><span class="muted">Scripts are off in this preview. For a full preview use <a href="#/testing">App Testing</a>.</span></div><div id="hvBox"><iframe class="fframe" src="${raw(p)}" sandbox title="Preview of ${esc(p)}"></iframe></div>`;
    else if (textExt.test(p)) { pv.innerHTML = tools + `<pre class="code fcode">Loading&hellip;</pre>`; loadText(p); }
    else pv.innerHTML = tools + `<div class="empty small">No preview for this file type.</div>`;
    $$("input[name=hv]", pv).forEach(x => x.onchange = () => { $("#hvBox").innerHTML = x.value === "code" ? `<pre class="code fcode">Loading&hellip;</pre>` : `<iframe class="fframe" src="${raw(p)}" sandbox title="Preview of ${esc(p)}"></iframe>`; if (x.value === "code") loadText(p); });
    $$("[data-a]", pv).forEach(btn => btn.onclick = (e) => busy(e.target, () => api("POST", `/api/projects/${s.slug}/open`, { what: btn.dataset.a, path: p })));
  };
  const loadText = async (p) => {
    try {
      const res = await fetch(raw(p) + "?text=1");
      let t = await res.text();
      if (t.length > 200000) t = t.slice(0, 200000) + "\n… (shortened)";
      const el = $("#fPrev .fcode"); if (el) el.textContent = t;
    } catch (e) { const el = $("#fPrev .fcode"); if (el) el.textContent = e.message; }
  };
  $("#fTree").onclick = (e) => { const r = e.target.closest(".frow"); if (r) show(r.dataset.p, !!r.dataset.dir); };
  $("#fTree").onkeydown = (e) => { const r = e.target.closest(".frow"); if (r && (e.key === "Enter" || e.key === " ")) { e.preventDefault(); show(r.dataset.p, !!r.dataset.dir); } };
  $("#fOpen").onclick = (e) => busy(e.target, () => api("POST", `/api/projects/${s.slug}/open`, { what: "folder" }));
  $("#fDeploy").onclick = async (e) => {
    if (!files.length) { toast("The folder is empty.", "warn"); return; }
    if (!await confirmBox("Deploy from this folder?", `<p>Dilreet checks the files in <code>${esc(r.current)}</code> and publishes them as a new deploy of <b>${esc(s.name)}</b>. The current version is kept and you can publish it again from the Deploys tab.</p>`, "Deploy now")) return;
    busy(e.target, async () => {
      const fd = new FormData(); fd.append("local_path", r.current);
      const j = await api("POST", `/api/sites/${s.slug}/deploy`, fd);
      const res = await jobPanel(j.job, $("#fJob"), { title: "Deploying from the project folder", doneTitle: "Deployed", failTitle: "Deploy failed" });
      if (!res.error) toast(s.name + " is updated", "success");
    });
  };
}

// Deploys tab: every deploy, kept as a zip on this PC.
async function projectDeploys(s, b) {
  b.innerHTML = `<div class="card"><div class="card-head"><h2>Deploys</h2><span class="grow"></span><span id="dUse" class="small muted"></span><button id="dOpen" class="sm ghost">Open deploys folder</button></div><div id="dList" class="muted small">Loading&hellip;</div><div id="dJob"></div></div>
    <details class="card"><summary><b>Server releases</b> <span class="small muted">&mdash; the last 5 copies kept on the server (instant switch)</span></summary><div id="rels" class="muted small" style="margin-top:10px">Loading&hellip;</div></details>`;
  $("#dOpen").onclick = (e) => busy(e.target, () => api("POST", `/api/projects/${s.slug}/open`, { what: "deploys" }));
  let r;
  try { r = await api("GET", `/api/projects/${s.slug}/deploys`); } catch (e) { $("#dList").innerHTML = `<div class="alert bad">${esc(e.message)}</div>`; return; }
  const list = r.deploys || [];
  $("#dUse").textContent = `${list.length} saved · ${humanSize(r.bytes || 0)}${r.keep ? ` · keeping the last ${r.keep}` : " · keeping all"}`;
  $("#dList").innerHTML = list.length ? `<ul class="deploys dlist">${list.map((d, i) => `<li data-id="${esc(d.id)}">
      <span class="grow"><b>${esc(fmtTime(d.time))}</b> ${d.live ? `<span class="badge ok">Live</span>` : ""} ${d.status === "failed" ? `<span class="badge bad" title="${esc(d.error || "")}">failed</span>` : d.status === "deploying" ? `<span class="badge info">deploying</span>` : ""} ${checksBadges(d.checks)}
        <div class="small muted">${esc(srcLabel[d.source] || d.source)}${d.detail ? " &middot; " + esc(d.detail) : ""}${d.commit ? " &middot; commit <code>" + esc(d.commit.slice(0, 7)) + "</code>" : ""} &middot; ${d.files} files &middot; ${esc(humanSize(d.bytes))} (zip ${esc(humanSize(d.size))})${d.restored ? " &middot; published again " + esc(ago(d.restored)) : ""}</div>
        ${d.notes ? `<div class="small">&#128221; ${esc(d.notes)}</div>` : ""}${d.missing ? `<div class="alert warn small">The zip is missing from the deploys folder.</div>` : ""}</span>
      <span class="row" style="gap:6px;flex-wrap:nowrap">${d.live || d.missing ? "" : `<button class="sm primary" data-a="publish">Publish</button>`}${i + 1 < list.length ? `<button class="sm" data-a="compare">Compare</button>` : ""}${d.missing ? "" : `<a class="btn sm ghost" href="/api/projects/${esc(s.slug)}/deploys/${esc(d.id)}/zip" download title="Download this deploy as .zip">&#8681;</a>`}<button class="sm ghost" data-a="notes" title="Add a note">&#128221;</button>${d.live ? "" : `<button class="sm ghost danger" data-a="delete" title="Delete this saved deploy">&#128465;</button>`}</span></li>`).join("")}</ul>`
    : `<div class="empty">No deploys saved yet. From now on every deploy is kept here as a .zip, so you can download it or publish it again.</div>`;
  $("#dList").onclick = async (e) => {
    const btn = e.target.closest("button[data-a]"); if (!btn) return;
    const id = btn.closest("li").dataset.id, d = list.find(x => x.id === id);
    const a = btn.dataset.a;
    if (a === "publish") {
      if (!await confirmBox("Publish this deploy?", `<p>Make the deploy from <b>${esc(fmtTime(d.time))}</b> the live version of ${esc(s.name)}. If the server still has it, the switch is instant; otherwise Dilreet uploads it again from the copy on this PC.</p>`, "Publish")) return;
      busy(btn, async () => { const j = await api("POST", `/api/projects/${s.slug}/deploys/${id}/publish`); const res = await jobPanel(j.job, $("#dJob"), { title: "Publishing", doneTitle: "Published", failTitle: "Publishing failed" }); if (!res.error) { toast("Published", "success"); later(rerender, 600); } });
    } else if (a === "compare") {
      busy(btn, async () => {
        const c = await api("GET", `/api/projects/${s.slug}/deploys/${id}/compare`);
        const sec = (t, arr, cls) => arr.length ? `<h3>${t} (${arr.length})</h3><ul class="small diff ${cls}">${arr.slice(0, 300).map(x => `<li><code>${esc(x)}</code></li>`).join("")}</ul>` : "";
        modal(`<h2>Changes in this deploy</h2><p class="small muted">Compared with the deploy before it${c.base ? ` (${esc(c.base)})` : ""}. ${c.unchanged} files unchanged.</p>${sec("Added", c.added, "add")}${sec("Changed", c.changed, "chg")}${sec("Removed", c.removed, "del")}${!c.added.length && !c.changed.length && !c.removed.length ? "<p>No file changes.</p>" : ""}<div class="row"><button class="primary" data-close>Close</button></div>`);
      });
    } else if (a === "notes") {
      const n = await askText("Note for this deploy", "For example “new prices” or “before redesign”.", d.notes || "");
      if (n === null) return;
      busy(btn, async () => { await api("POST", `/api/projects/${s.slug}/deploys/${id}/notes`, { notes: n }); rerender(); });
    } else if (a === "delete") {
      if (!await confirmBox("Delete this saved deploy?", `<p>The zip from <b>${esc(fmtTime(d.time))}</b> is deleted from this PC. The live site is not affected.</p>`, "Delete", true)) return;
      busy(btn, async () => { await api("DELETE", `/api/projects/${s.slug}/deploys/${id}`); toast("Deleted", "success"); rerender(); });
    }
  };
  api("GET", `/api/sites/${s.slug}/releases`).then(r => {
    const rel = r.releases || [];
    $("#rels").innerHTML = rel.length ? `<ul class="deploys">${rel.map(x => `<li><span class="grow"><code>${esc(x)}</code> ${x === r.current ? `<span class="badge ok">Live</span>` : ""}<div class="small muted">${esc(relTime(x))}</div></span>${x === r.current ? "" : `<button class="sm" data-rel="${esc(x)}">Switch to this</button>`}</li>`).join("")}</ul>` : "No releases on the server yet.";
  }).catch(e => $("#rels").innerHTML = `<div class="alert warn">${esc(e.message)}</div>`);
  $("#rels").onclick = async (e) => {
    const rel = e.target.dataset.rel;
    if (!rel) return;
    if (!await confirmBox("Roll back?", `<p>Make release <code>${esc(rel)}</code> the live version of ${esc(s.name)}? It switches instantly; you can switch back any time.</p>`, "Switch")) return;
    busy(e.target, async () => { await api("POST", `/api/sites/${s.slug}/rollback`, { release: rel }); toast("Release " + rel + " is live", "success"); rerender(); });
  };
}

function projectSettings(s, b) {
  b.innerHTML = `<div class="card"><h2>Project settings</h2>
      <div class="field"><label for="sName">Name</label><div class="row"><input id="sName" value="${esc(s.name)}" style="flex:1"><button id="sRen">Rename</button></div></div>
      <label class="switch"><input type="checkbox" id="sWatch" ${s.watch ? "checked" : ""}> Watch a folder on this PC and redeploy when it changes</label>
      <div class="row" style="margin-left:52px"><input id="sWPath" value="${esc(s.watch_path || s.last_source || "")}" placeholder="C:\\Sites\\${esc(s.slug)}\\dist" style="flex:1"></div>
      <div class="row" style="margin-top:12px"><button id="sRestart">Restart container</button><a class="btn ghost" href="#/testing">Test a version first</a><a class="btn ghost" href="#/settings/storage">Storage settings</a></div>
      <div class="kv small muted" style="margin-top:12px"><div>Project id</div><div><code>${esc(s.slug)}</code></div><div>Folder</div><div><code>${esc(s.folder || "")}</code></div><div>Created</div><div>${esc(ago(s.created_at))}</div><div>Coolify app</div><div><code>${esc(s.app_uuid || "")}</code></div></div>
    </div>
    <div class="card danger-zone"><h2>Danger zone</h2><label class="check"><input type="checkbox" id="sKeep"> Keep the files on the server</label><p class="small muted">The project folder on this PC (with all saved deploys) is never deleted by Dilreet.</p><div class="row"><button id="sDel" class="danger">Delete project</button></div><div id="sJob"></div></div>`;
  $("#sRen").onclick = (e) => busy(e.target, async () => { await api("POST", `/api/sites/${s.slug}/rename`, { name: $("#sName").value }); toast("Renamed", "success"); rerender(); });
  $("#sWatch").onchange = (e) => api("POST", `/api/sites/${s.slug}/watch`, { enabled: e.target.checked, path: $("#sWPath").value.trim() }).then(() => toast(e.target.checked ? "Watching the folder: changes deploy automatically" : "Stopped watching", "success")).catch(x => { e.target.checked = !e.target.checked; err(x); });
  $("#sRestart").onclick = (e) => busy(e.target, async () => { if (!await confirmBox("Restart the container?", "Coolify restarts the web server for this site. It is offline for a few seconds.", "Restart")) return; await api("POST", `/api/sites/${s.slug}/restart`); toast("Restart queued", "success"); });
  $("#sDel").onclick = async (e) => {
    const keep = $("#sKeep").checked;
    if (!await typedConfirm("Delete " + s.name + "?", `<p>This removes the project from your server${keep ? " (the files stay in /data/hostpanel/sites/" + esc(s.slug) + ")" : " including all its files and server releases"}. ${s.dns_mode === "cloudflare" ? "Its Cloudflare DNS records are removed too." : ""} The copies on this PC stay. This cannot be undone.</p>`, s.slug, "Delete project")) return;
    busy(e.target, async () => {
      const r = await api("DELETE", `/api/sites/${s.slug}` + (keep ? "?keep_files=1" : ""));
      const j = await jobPanel(r.job, $("#sJob"), { title: "Deleting", doneTitle: "Deleted" });
      if (!j.error) { toast("Project deleted", "success"); go("#/projects"); }
    });
  };
}
function relTime(rel) { const m = /^(\d{4})(\d\d)(\d\d)-?(\d\d)(\d\d)(\d\d)/.exec(rel); if (!m) return ""; return ago(new Date(Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6])).toISOString()); }

async function pullDeploy(slug, allowBuild, el) {
  const r = await api("POST", `/api/sites/${slug}/pull`, { allow_build: allowBuild });
  const j = await jobPanel(r.job, el, { title: "Pulling the latest commit", doneTitle: "Deployed", failTitle: "Deploy failed" });
  if (j.error && /build/i.test(j.error) && !allowBuild) {
    if (await confirmBox("Build on this PC?", `<p>The repository needs to be built (npm). Dilreet runs <code>npm install</code> and the build script on this PC, in a temporary folder. Only allow this for projects you trust. Future automatic deploys of this site will build too.</p>`, "Allow build and deploy")) return pullDeploy(slug, true, el);
  }
  if (!j.error) { toast("Deployed the latest commit", "success"); later(rerender, 800); }
  return j;
}

// ---------- domain wizard ----------
route(/^\/project\/([^/]+)\/domain$/, async (slug) => {
  await refreshOverview();
  const s = await getSite(slug);
  const v = view();
  const pre = s.domain || store("prefillDomain-" + slug) || "";
  v.innerHTML = projectHead(s, "domain") + `
    ${stepsBar0(s)}
    <div class="card"><h2>1. Your domain</h2><div class="row"><input id="dDom" value="${esc(pre)}" placeholder="example.com" style="flex:1" aria-label="Domain name" autofocus><label class="check"><input type="checkbox" id="dWww" ${s.domain ? (s.include_www ? "checked" : "") : "checked"}> also www</label><button id="dNext" class="primary">Continue</button></div>
      <p class="hint">Use a domain you own (GoDaddy, Namecheap, Porkbun, Hostinger, Squarespace, Cloudflare, &hellip;). Dilreet finds out where it is managed and shows you the easiest way. <button class="linkish" id="dAsk" type="button">Ask Jiv to walk me through it</button></p></div>
    ${(s.pending_ns || []).length ? `<div class="alert info">Waiting for the nameserver change to Cloudflare (${s.pending_ns.map(n => `<code>${esc(n)}</code>`).join(", ")}). This can take from a few minutes up to 24 hours; Dilreet keeps checking.</div>` : ""}
    <div id="dStep2"></div><div id="dStep3"></div>`;
  const dom = () => $("#dDom").value.trim().toLowerCase().replace(/^https?:\/\//, "").replace(/\/.*$/, "");
  $("#dNext").onclick = (e) => busy(e.target, () => domainStep2(s, dom(), $("#dWww").checked));
  $("#dDom").onkeydown = (e) => { if (e.key === "Enter") $("#dNext").click(); };
  $("#dAsk").onclick = () => jivAsk(`Walk me through connecting ${dom() || "my domain"} to my project ${s.name}, step by step.`);
  if (s.domain) domainStatus(s, $("#dStep3"));
}, "domains");
function stepsBar0(s) { const cur = !s.domain ? 0 : !s.domain_live ? 1 : 2; const n = ["Enter domain", "Point DNS to your server", "Live with HTTPS"]; return `<ol class="steps">${n.map((x, i) => `<li class="${i < cur || (s.https_active && i === 2) ? "done" : i === cur ? "cur" : ""}">${x}</li>`).join("")}</ol>`; }
function mdi(t) { return esc(t).replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>"); }
function copyBtn(v) { return `<button class="sm copy" data-copy="${esc(v)}" aria-label="Copy ${esc(v)}">Copy</button>`; }

async function domainStep2(s, domain, www) {
  if (!/^[a-z0-9.-]+\.[a-z]{2,}$/.test(domain)) { toast("Enter a domain like example.com", "warn"); return; }
  const el = $("#dStep2");
  const d = await api("GET", "/api/dns/host?domain=" + encodeURIComponent(domain));
  const host = d.host || {};
  if (d.cloudflare_connected && host.auto) {
    el.innerHTML = `<div class="card"><h2>2. Connect automatically</h2><p>${esc(domain)} uses Cloudflare, and your Cloudflare account is connected. Dilreet can create the DNS records for you.</p>
      <label class="check"><input type="checkbox" id="cfProxy"> Proxy through Cloudflare (orange cloud) <span class="muted small">&mdash; leave off unless you know you need it</span></label>
      <div class="row"><button id="cfGo" class="primary lg">Set up DNS automatically</button><button id="manual" class="ghost">Show the records instead</button></div></div>`;
    $("#cfGo").onclick = (e) => busy(e.target, () => autoDNS(s, domain, www, false));
    $("#manual").onclick = () => manualDNS(s, domain, www, host, d);
    return;
  }
  if (host.auto && !d.cloudflare_connected) {
    el.innerHTML = `<div class="card"><h2>2. Connect automatically with Cloudflare</h2><p>${esc(domain)} uses Cloudflare. Paste a Cloudflare API token and Dilreet sets up DNS for you, now and for future domains.</p>
      <ol class="small"><li><a href="${esc(d.cf_token_url)}" target="_blank" rel="noopener">Create a token</a> with the template <b>Edit zone DNS</b> (all zones, or just this one).</li><li>Paste it here. It is stored encrypted on this PC.</li></ol>
      <div class="row"><input id="cfTok" type="password" autocomplete="off" style="flex:1" aria-label="Cloudflare API token"><button id="cfSave" class="primary">Connect Cloudflare</button></div>
      <p><button id="manual" class="ghost sm">No thanks, show the DNS records</button></p></div>`;
    $("#cfSave").onclick = (e) => busy(e.target, async () => { await api("POST", "/api/cloudflare", { token: $("#cfTok").value }); toast("Cloudflare connected", "success"); domainStep2(s, domain, www); });
    $("#manual").onclick = () => manualDNS(s, domain, www, host, d);
    return;
  }
  // Any other registrar: guided (default), registrar API, or move DNS to Cloudflare.
  const g = d.guide || null;
  const regName = host.name || "your registrar";
  const modes = [["guided", "Guided steps", "Recommended. Dilreet shows exactly what to click and checks for you."]];
  if (g && g.api) modes.push(["api", `Automatic with ${regName} API`, "Dilreet creates the record with your API key."]);
  modes.push(["move", "Fully automatic (Cloudflare)", "Move DNS to a free Cloudflare account once; future changes are automatic."]);
  el.innerHTML = `<div class="card"><h2>2. Point ${esc(domain)} to your server</h2>
    <p>${host.name ? `This domain is managed at <b>${esc(host.name)}</b>.` : "Dilreet could not tell where this domain is managed; the steps below work at any registrar."} Choose how to connect it:</p>
    <div class="modes" role="radiogroup" aria-label="How to connect">${modes.map(([k, t, sub], i) => `<label class="mode"><input type="radio" name="dmode" value="${k}" ${i === 0 ? "checked" : ""}><span><b>${esc(t)}</b><small>${esc(sub)}</small></span></label>`).join("")}</div>
    <div id="dMode"></div></div>`;
  const pick = () => {
    const m = $("input[name=dmode]:checked", el).value;
    const box = $("#dMode");
    if (m === "guided") guidedDNS(s, domain, www, host, g, box);
    else if (m === "api") registrarAPI(s, domain, www, host, g, d.registrars || {}, box);
    else cloudflareMove(s, domain, www, host, d, box);
  };
  $$("input[name=dmode]", el).forEach(r => r.onchange = pick);
  pick();
}
function bumpSteps(s, domain) { const sb = $(".steps"); if (sb) sb.outerHTML = stepsBar0(Object.assign({}, s, { domain, domain_live: s.domain === domain && s.domain_live })); }
async function autoDNS(s, domain, www, replace) {
  try {
    const r = await api("POST", `/api/sites/${s.slug}/auto-dns`, { domain, www, replace, proxy: !!($("#cfProxy") && $("#cfProxy").checked) });
    toast(`Created ${r.records} DNS record(s) in ${r.zone}`, "success");
    bumpSteps(s, domain);
    domainStatus(Object.assign({}, s, { domain, include_www: www }), $("#dStep3"));
  } catch (e) {
    if (e.status === 409 && e.data && e.data.conflicts) {
      const list = e.data.conflicts.map(c => `<li><code>${esc(c.type)} ${esc(c.name)} &rarr; ${esc(c.content)}</code></li>`).join("");
      if (await confirmBox("Replace existing DNS records?", `<p>These records already exist and would be replaced:</p><ul>${list}</ul><p>If the domain currently points to another website (for example Netlify), that site stops receiving visitors for this domain.</p>`, "Replace records", true)) return autoDNS(s, domain, www, true);
      return;
    }
    throw e;
  }
}
function recordsTable(recs, g) {
  return `<table class="records"><thead><tr><th>Type</th><th>Name / Host</th><th>Value / Points to</th><th>TTL</th></tr></thead><tbody>${recs.map(x => {
    const www = /^www(\.|$)/.test(x.name);
    if (www && g && g.www_cname) return `<tr><td>A <span class="muted">or</span> CNAME</td><td><code>${esc(x.name)}</code> ${copyBtn(x.name)}</td><td><code>${esc(x.value)}</code> ${copyBtn(x.value)}<div class="small muted">or keep the existing CNAME <code>www &rarr; @</code></div></td><td>${esc(x.ttl)}</td></tr>`;
    return `<tr><td>${esc(x.type)}</td><td><code>${esc(x.name)}</code> ${copyBtn(x.name)}</td><td><code>${esc(x.value)}</code> ${copyBtn(x.value)}</td><td>${esc(x.ttl)}</td></tr>`;
  }).join("")}</tbody></table>`;
}
async function guidedDNS(s, domain, www, host, g, box) {
  const r = await api("POST", `/api/sites/${s.slug}/domain`, { domain, www });
  bumpSteps(s, domain);
  const recs = r.records || [];
  const steps = g ? g.steps : [`Sign in where you bought ${domain} and open its DNS settings.`, "Add (or edit) the records below. Delete other A, AAAA or CNAME records for the same names (for example old Netlify ones).", "Dilreet checks automatically and notifies you when the domain is live."];
  box.innerHTML = `<div class="guided">
    ${g && g.dns_url ? `<p><a class="btn primary" href="${esc(g.dns_url)}" target="_blank" rel="noopener">Open ${esc(host.name)} DNS &#8599;</a></p>` : host.url ? `<p><a class="btn primary" href="${esc(host.url)}" target="_blank" rel="noopener">Open ${esc(host.name)} &#8599;</a></p>` : ""}
    <ol class="gsteps">${steps.map(t => `<li>${mdi(t)}</li>`).join("")}</ol>
    <h3>The record${recs.length > 1 ? "s" : ""} to add</h3>${recordsTable(recs, g)}
    ${g && (g.warnings || []).length ? `<div class="alert warn small"><b>Watch out</b><ul>${g.warnings.map(w => `<li>${mdi(w)}</li>`).join("")}</ul></div>` : ""}
    <p class="hint">DNS changes usually take a few minutes, sometimes up to a few hours. You can close this page &mdash; you'll get a notification when it works.${(host.nameservers || []).length ? " Current nameservers: " + host.nameservers.map(esc).join(", ") : ""}</p></div>`;
  domainStatus(Object.assign({}, s, { domain: r.domain || domain, include_www: www }), $("#dStep3"));
}
async function manualDNS(s, domain, www, host, d) {
  $("#dStep2").innerHTML = `<div class="card"><h2>2. Add these DNS records${host.name ? " at " + esc(host.name) : ""}</h2><div id="dMode"></div></div>`;
  return guidedDNS(s, domain, www, host, d.guide || null, $("#dMode"));
}
function registrarAPI(s, domain, www, host, g, regs, box) {
  const kind = g.api, cred = regs[kind];
  if (cred) {
    box.innerHTML = `<p><span class="badge ok">${esc(host.name)} connected</span> <span class="small muted">key ${esc(cred.hint || "")}${cred.mode === "pat" ? " · Personal Access Token" : ""}</span> <button class="linkish small" id="rDisc" type="button">Disconnect</button></p>
      <p>Dilreet sets the <b>A</b> record for <code>${esc(domain)}</code>${www ? " and www" : ""} to your server at ${esc(host.name)}. ${g.www_cname ? "An existing <code>www &rarr; @</code> CNAME is kept." : ""} Mail records are not touched.</p>
      <div class="row"><button id="rGo" class="primary">Create the records at ${esc(host.name)}</button></div><div id="rOut"></div>`;
    $("#rDisc").onclick = async () => { if (await confirmBox(`Disconnect ${host.name}?`, "The saved key is deleted from this PC. DNS records stay as they are.", "Disconnect")) { await api("DELETE", "/api/registrars/" + kind); delete regs[kind]; registrarAPI(s, domain, www, host, g, regs, box); } };
    const run = async (replace) => {
      try {
        const r = await api("POST", `/api/sites/${s.slug}/registrar-dns`, { provider: kind, domain, www, replace });
        toast(`Set ${r.records} record(s) at ${host.name}`, "success");
        bumpSteps(s, domain);
        domainStatus(Object.assign({}, s, { domain, include_www: www }), $("#dStep3"));
      } catch (e) {
        if (e.status === 409 && e.data && e.data.conflicts) {
          const list = e.data.conflicts.map(c => `<li><code>${esc(c.type)} ${esc(c.name)} &rarr; ${esc(c.content)}</code></li>`).join("");
          if (await confirmBox("Replace existing DNS records?", `<p>These records at ${esc(host.name)} would be replaced:</p><ul>${list}</ul><p>If they belong to another website or a parking page, it stops receiving visitors for this domain.</p>`, "Replace records", true)) return run(true);
          return;
        }
        $("#rOut").innerHTML = `<div class="alert bad">${esc(e.message)}</div><p class="small">You can always use the <b>Guided steps</b> instead.</p>`;
      }
    };
    $("#rGo").onclick = (e) => busy(e.target, () => run(false));
    return;
  }
  const gd = kind === "godaddy";
  box.innerHTML = `<div class="alert info small">${mdi(g.api_note || "")}</div>
    <ol class="small"><li><a href="${esc(g.key_url)}" target="_blank" rel="noopener">Create an API key at ${esc(host.name)} &#8599;</a>${gd ? " &mdash; choose <b>Production</b> (not OTE)." : " and turn on <b>API Access</b> for this domain."}</li><li>Paste it here. It is encrypted on this PC and only sent to ${esc(host.name)}.</li></ol>
    ${gd ? `<div class="row small" role="radiogroup" aria-label="Key type"><label class="check"><input type="radio" name="gdm" value="pat" checked> Personal Access Token</label><label class="check"><input type="radio" name="gdm" value="key"> API key + secret</label></div>` : ""}
    <div class="grid2"><div class="field"><label for="rKey" id="rKeyL">${gd ? "Personal Access Token" : "API key"}</label><input id="rKey" type="password" autocomplete="off"></div>
      <div class="field ${gd ? "hidden" : ""}" id="rSecF"><label for="rSec">${gd ? "Secret" : "Secret API key"}</label><input id="rSec" type="password" autocomplete="off"></div></div>
    <div class="row"><button id="rSave" class="primary">Connect and test</button></div><div id="rOut"></div>`;
  if (gd) $$("input[name=gdm]", box).forEach(r => r.onchange = () => { const k = r.value === "key" && r.checked; $("#rSecF").classList.toggle("hidden", !k); $("#rKeyL").textContent = k ? "API key" : "Personal Access Token"; });
  $("#rSave").onclick = (e) => busy(e.target, async () => {
    const pat = gd && $("input[name=gdm]:checked", box).value === "pat";
    try {
      const r = await api("POST", "/api/registrars/" + kind, { key: $("#rKey").value.trim(), secret: pat ? "" : $("#rSec").value.trim(), domain });
      regs[kind] = { hint: r.hint || "", mode: r.mode || (pat ? "pat" : "key") };
      toast(`${host.name} connected`, "success");
      registrarAPI(s, domain, www, host, g, regs, box);
    } catch (x) { $("#rOut").innerHTML = `<div class="alert bad">${esc(x.message)}</div><p class="small">No problem &mdash; the <b>Guided steps</b> work for every account.</p>`; }
  });
}
function cloudflareMove(s, domain, www, host, d, box) {
  if (!d.cloudflare_connected) {
    box.innerHTML = `<p>Dilreet adds ${esc(domain)} to your free Cloudflare account, copies your e-mail records (MX/TXT), and points the site to your server. You then change the nameservers at ${esc(host.name || "your registrar")} once${host.id === "godaddy" ? " (or Dilreet does it with a GoDaddy token)" : ""}. After that every DNS change is automatic.</p>
      <ol class="small"><li><a href="https://dash.cloudflare.com/sign-up" target="_blank" rel="noopener">Create a free Cloudflare account</a> if you don't have one.</li>
        <li><a href="https://dash.cloudflare.com/profile/api-tokens" target="_blank" rel="noopener">Create a token</a>: <b>Create Custom Token</b> with permissions <b>Zone &rsaquo; Zone &rsaquo; Edit</b> and <b>Zone &rsaquo; DNS &rsaquo; Edit</b>, for all zones.</li><li>Paste it here. It is stored encrypted on this PC.</li></ol>
      <div class="row"><input id="cfT2" type="password" autocomplete="off" style="flex:1" aria-label="Cloudflare API token"><button id="cfS2" class="primary">Connect Cloudflare</button></div>`;
    $("#cfS2").onclick = (e) => busy(e.target, async () => { await api("POST", "/api/cloudflare", { token: $("#cfT2").value }); toast("Cloudflare connected", "success"); await refreshOverview(); d.cloudflare_connected = true; cloudflareMove(s, domain, www, host, d, box); });
    return;
  }
  box.innerHTML = `<p>Dilreet will add <b>${esc(domain)}</b> to your Cloudflare account, copy the e-mail records it can see (MX, SPF/TXT), and create the records for your project${www ? " (with www)" : ""}. Nothing changes for visitors until you switch the nameservers.</p>
    <details class="small"><summary>Advanced: Cloudflare account</summary><div class="field" style="margin-top:6px"><label for="cfAcc">Account ID <span class="muted">(only if your token can see several accounts)</span></label><input id="cfAcc" placeholder="leave empty to use the first account"></div></details>
    <div class="row"><button id="cfMove" class="primary">Move DNS to Cloudflare</button></div><div id="cfOut"></div>`;
  $("#cfMove").onclick = async (e) => {
    if (!await confirmBox("Move DNS to Cloudflare?", `<p>Dilreet creates the zone <b>${esc(domain)}</b> in your Cloudflare account and adds the records for your project. Check later in Cloudflare that every record you need (for example for e-mail) is there before switching nameservers.</p>`, "Move DNS")) return;
    busy(e.target, async () => {
      const r = await api("POST", `/api/sites/${s.slug}/cloudflare-move`, { domain, www, account_id: $("#cfAcc").value.trim() });
      bumpSteps(s, domain);
      const g = r.guide || {};
      $("#cfOut").innerHTML = `<div class="alert ok">${r.created ? "Added" : "Found"} <b>${esc(r.zone)}</b> in Cloudflare${(r.copied || []).length ? ` and copied ${r.copied.length} mail record(s)` : ""}.</div>
        <h3>Last step: change the nameservers at ${esc((r.registrar && r.registrar.name) || "your registrar")}</h3>
        <table class="records"><tbody>${(r.nameservers || []).map((n, i) => `<tr><td>Nameserver ${i + 1}</td><td><code>${esc(n)}</code> ${copyBtn(n)}</td></tr>`).join("")}</tbody></table>
        ${r.can_switch ? `<div class="row"><button id="cfSw" class="primary">Change them at GoDaddy for me</button><span class="small muted">or do it by hand:</span></div>` : ""}
        ${g.ns_url ? `<p><a class="btn" href="${esc(g.ns_url)}" target="_blank" rel="noopener">Open nameserver settings &#8599;</a></p>` : ""}
        <ol class="gsteps">${(g.ns_steps || ["Sign in where you bought the domain, find Nameservers, choose custom nameservers and paste the two above."]).map(t => `<li>${mdi(t)}</li>`).join("")}</ol>
        <p class="hint">The change takes from a few minutes up to 24 hours. Dilreet checks in the background and tells you when the domain is live.</p>`;
      if ($("#cfSw")) $("#cfSw").onclick = async (ev) => {
        if (!await confirmBox("Change nameservers at GoDaddy?", `<p>Dilreet replaces the nameservers of <b>${esc(r.zone)}</b> at GoDaddy with:</p><ul>${(r.nameservers || []).map(n => `<li><code>${esc(n)}</code></li>`).join("")}</ul><p>From then on Cloudflare answers for this domain. Make sure your e-mail records are in Cloudflare.</p>`, "Change nameservers", true)) return;
        busy(ev.target, async () => { await api("POST", `/api/sites/${s.slug}/switch-nameservers`); toast("Nameservers changed at GoDaddy", "success"); });
      };
      domainStatus(Object.assign({}, s, { domain, include_www: www }), $("#dStep3"));
    });
  };
}
function domainStatus(s, el) {
  el.innerHTML = `<div class="card"><h2>3. Going live</h2><div id="dChecks" class="muted">Checking DNS&hellip;</div><div class="row"><button id="dCheck" class="sm">Check now</button><span class="small muted">Dilreet checks every 10 seconds and connects the domain as soon as DNS is ready. You can leave this page; you'll get a notification.</span></div></div>`;
  let applying = false;
  const check = async () => {
    try {
      const r = await api("GET", `/api/sites/${s.slug}/dns`);
      $("#dChecks").innerHTML = (r.checks || []).map(c => `<p>${c.ok ? "&#10004;" : "&#8987;"} <b>${esc(c.host)}</b>: ${esc(c.message)}</p>`).join("") +
        (r.domain_live ? `<div class="alert ok">&#127881; <b>${esc(s.domain)}</b> is connected. HTTPS certificates are issued automatically within 1&ndash;2 minutes. <a href="https://${esc(s.domain)}" target="_blank" rel="noopener">Open it</a></div>` : "");
      if (r.ok && !r.domain_live && !applying) { applying = true; await applyDomain(s, false, false); applying = false; return; }
      if (!r.domain_live && onRoute(s)) later(check, 10000);
    } catch (e) { $("#dChecks").innerHTML = `<div class="alert warn">${esc(e.message)}</div>`; if (onRoute(s)) later(check, 15000); }
  };
  $("#dCheck").onclick = (e) => busy(e.target, check);
  check();
}
function onRoute(s) { return location.hash === `#/project/${s.slug}/domain`; }
async function applyDomain(s, force, skip) {
  try {
    await api("POST", `/api/sites/${s.slug}/domain/apply`, { force, skip_dns: skip });
    toast("Domain connected: " + s.domain, "success");
    rerender();
  } catch (e) {
    if (/^DNS not ready/.test(e.message)) { if (await confirmBox("DNS is not ready yet", `<p>${esc(e.message)}</p><p>Connect anyway? HTTPS starts working once DNS points to your server.</p>`, "Connect anyway")) return applyDomain(s, force, true); return; }
    if (/conflict/i.test(e.message)) { if (await confirmBox("Domain already in use", `<p>${esc(e.message)}</p>`, "Use it anyway", true)) return applyDomain(s, true, skip); return; }
    err(e);
  }
}

// ---------- domains, deploys ----------
route(/^\/domains$/, async () => {
  await refreshOverview(); await loadSites();
  const v = view();
  v.innerHTML = `<div class="head"><h1>Domains</h1></div>
    <div class="card"><div class="sitelist">${sites.map(s => `<a class="siterow" href="#/project/${esc(s.slug)}/domain">${thumb(s.name)}<div class="t"><b>${esc(s.domain || "No custom domain")}</b><div class="u">${esc(s.name)} &middot; ${esc((s.instant_url || "").replace(/^https?:\/\//, ""))}</div></div><div class="meta">${s.domain ? (s.domain_live ? (s.https_active ? `<span class="badge ok">HTTPS active</span>` : `<span class="badge warn">HTTPS pending</span>`) : `<span class="badge warn">waiting for DNS</span>`) : `<span class="btn sm">Add domain</span>`}</div></a>`).join("") || `<div class="empty">No projects yet. <a href="#/new">Add a project</a> first.</div>`}</div></div>
    <div class="grid2"><div class="card"><h2>Cloudflare</h2><div id="cfBox"></div></div><div class="card"><h2>Registrar API keys</h2><div id="regBox" class="small muted">Loading&hellip;</div></div></div>
    <div class="card"><h2>Where is my domain?</h2><p class="small">Dilreet detects the registrar and gives you exact steps for <b>GoDaddy</b>, <b>Namecheap</b>, <b>Porkbun</b>, <b>Hostinger</b>, <b>Squarespace</b> and <b>Cloudflare</b>, with a button that opens the right DNS page. Open a project's <b>Domain</b> tab to start, or <button class="linkish" type="button" id="dmAsk">ask Jiv</button>.</p></div>`;
  cloudflareCard($("#cfBox"));
  $("#dmAsk").onclick = () => jivAsk("How do I connect my domain? Which way is easiest for my registrar?");
  registrarsCard($("#regBox"));
}, "domains");
async function registrarsCard(el) {
  const r = await api("GET", "/api/registrars").catch(() => ({ connected: {} }));
  const c = r.connected || {};
  const row = (k, name, note) => `<li><span class="grow"><b>${name}</b> ${c[k] ? `<span class="badge ok">connected</span> <span class="small muted">${esc(c[k].hint || "")}${c[k].mode === "pat" ? " · token" : ""}</span>` : `<span class="small muted">${note}</span>`}</span>${c[k] ? `<button class="sm ghost danger" data-k="${k}">Disconnect</button>` : ""}</li>`;
  el.classList.remove("muted");
  el.innerHTML = `<ul class="deploys">${row("godaddy", "GoDaddy", "optional; connect from a project's Domain tab")}${row("porkbun", "Porkbun", "optional; connect from a project's Domain tab")}</ul><p class="hint">Namecheap, Hostinger and Squarespace use guided steps (their APIs need IP whitelisting or are not public).</p>`;
  el.onclick = async (e) => { const k = e.target.dataset.k; if (k && await confirmBox("Disconnect?", "The saved key is deleted from this PC. DNS records stay as they are.", "Disconnect")) { await api("DELETE", "/api/registrars/" + k); registrarsCard(el); } };
}
function cloudflareCard(el) {
  if (ov.cloudflare) {
    el.innerHTML = `<p><span class="badge ok">Connected</span> Dilreet creates DNS records automatically for domains in your Cloudflare account.</p><button id="cfDel" class="ghost sm">Disconnect</button>`;
    $("#cfDel", el).onclick = async () => { if (await confirmBox("Disconnect Cloudflare?", "Existing DNS records stay. Dilreet deletes the saved token.", "Disconnect")) { await api("DELETE", "/api/cloudflare"); await refreshOverview(); cloudflareCard(el); } };
  } else {
    el.innerHTML = `<p class="muted small">If your domains use Cloudflare, connect it once and new domains are set up without touching DNS settings.</p><ol class="small"><li><a href="https://dash.cloudflare.com/profile/api-tokens" target="_blank" rel="noopener">Create a token</a> with the <b>Edit zone DNS</b> template.</li><li>Paste it here.</li></ol><div class="row"><input id="cfT" type="password" autocomplete="off" style="flex:1" aria-label="Cloudflare API token"><button id="cfS" class="primary">Connect</button></div>`;
    $("#cfS", el).onclick = (e) => busy(e.target, async () => { await api("POST", "/api/cloudflare", { token: $("#cfT", el).value }); toast("Cloudflare connected", "success"); await refreshOverview(); cloudflareCard(el); });
  }
}
route(/^\/deploys$/, async () => {
  await refreshOverview(); await loadSites();
  const n = (await api("GET", "/api/notices").catch(() => ({ notices: [] }))).notices || [];
  const list = sites.filter(s => s.last_deploy).sort((a, b) => (b.last_deploy || "").localeCompare(a.last_deploy || ""));
  view().innerHTML = `<div class="head"><h1>Deploys</h1></div>
    <div class="card"><h2>Latest deploy per project</h2><ul class="deploys">${list.map(s => `<li>${thumb(s.name)}<span class="grow"><a href="#/project/${esc(s.slug)}"><b>${esc(s.name)}</b></a><div class="small muted">${esc(s.release || "")} ${s.git && s.git.last_sha ? "&middot; commit " + esc(s.git.last_sha.slice(0, 7)) : ""}</div></span>${statusBadge(s)}<span class="small muted">${esc(ago(s.last_deploy))}</span></li>`).join("") || `<li class="muted">No deploys yet.</li>`}</ul></div>
    <div class="card"><h2>Activity</h2><div class="activity">${n.slice(0, 50).map(x => `<div class="notice"><span class="badge ${x.level === "warn" ? "warn" : x.level === "success" ? "ok" : "info"}">${esc(x.level)}</span> ${esc(x.text)}<time>${esc(ago(x.time))}</time></div>`).join("") || `<p class="muted small">No activity yet.</p>`}</div></div>`;
}, "deploys");
route(/^\/guide$/, async () => { view().innerHTML = ""; view().appendChild(document.importNode($("#tpl-guide").content, true)); }, "settings");

// ---------- assistant ----------
let chat = store("chat") || [];
let aiInfo = null;
let chatAbort = null;
// Jiv lives in a dock at the bottom of every page; the Jiv page re-parents the same element.
function jivOpen(on) {
  const j = $("#jiv"); if (!j) return;
  j.classList.toggle("open", !!on); store("jivOpen", !!on);
  $("#cIn").setAttribute("aria-expanded", String(!!on || j.classList.contains("full")));
  if (on || j.classList.contains("full")) { ensureAI(); drawChat(); }
}
function mountJiv(full) {
  const j = $("#jiv"), dock = $("#jivDock"); if (!j || !dock) return;
  if (full) { const slot = $("#jivSlot"); if (slot) slot.appendChild(j); j.classList.add("full"); dock.classList.add("hidden"); }
  else if (j.parentElement !== dock) { dock.appendChild(j); j.classList.remove("full"); dock.classList.remove("hidden"); }
  drawChat();
}
let aiLoading = null;
function ensureAI() {
  if (aiInfo) return Promise.resolve(aiInfo);
  if (!aiLoading) aiLoading = api("GET", "/api/ai/providers").catch(() => ({ providers: [], local: [] })).then(r => { aiInfo = r; aiLoading = null; fillModels(); return r; });
  return aiLoading;
}
function jivAsk(text, about) { jivOpen(true); ensureAI().then(() => sendChat(text, about)); }
function askAbout(text) {
  const about = String(text || "").slice(0, 4000);
  jivOpen(true);
  ensureAI().then(() => {
    const t = curTarget();
    $("#cIn").value = "Help me fix this problem.";
    $("#cIn").dataset.about = about;
    $("#suggest").innerHTML = `<div class="alert info small">Attached: <code>${esc(about.slice(0, 160))}${about.length > 160 ? "&hellip;" : ""}</code></div>`;
    if (!t || !t.cloud) sendChat($("#cIn").value); else $("#cIn").focus();
  });
}
function initJiv() {
  const j = $("#jiv");
  $("#cForm").onsubmit = (e) => { e.preventDefault(); sendChat($("#cIn").value); };
  $("#cIn").onkeydown = (e) => {
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); sendChat($("#cIn").value); }
    if (e.key === "Escape" && j.classList.contains("open") && !j.classList.contains("full")) { e.stopPropagation(); jivOpen(false); $("#cIn").blur(); }
  };
  $("#cIn").addEventListener("focus", () => { if (!j.classList.contains("open") && !j.classList.contains("full")) jivOpen(true); });
  $("#jMin").onclick = () => jivOpen(false);
  $("#cStop").onclick = () => chatAbort && chatAbort.abort();
  $("#cClear").onclick = async () => { if (!chat.length || await confirmBox("Clear the chat?", "This deletes the conversation with Jiv on this PC.", "Clear")) { chat = []; store("chat", chat); $("#suggest").innerHTML = ""; drawChat(); if (aiInfo) fillModels(); } };
  if (store("jivOpen")) jivOpen(true);
}

function md(src) {
  const blocks = String(src || "").replace(/```action[\s\S]*?(```|$)/g, "").trim().split(/```/);
  return blocks.map((b, i) => {
    if (i % 2) { const nl = b.indexOf("\n"); return `<pre class="code"><code>${esc(nl >= 0 ? b.slice(nl + 1) : b)}</code></pre>`; }
    let t = esc(b)
      .replace(/`([^`\n]+)`/g, "<code>$1</code>")
      .replace(/\*\*([^*\n]+)\*\*/g, "<b>$1</b>")
      .replace(/\[([^\]\n]+)\]\((https?:\/\/[^)\s]+|#\/[^)\s]*)\)/g, (m, a, u) => `<a href="${u}" ${u[0] === "#" ? "" : 'target="_blank" rel="noopener"'}>${a}</a>`)
      .replace(/^#{1,4} (.+)$/gm, "<b>$1</b>");
    t = t.replace(/(^|\n)((?:\s*(?:[-*]|\d+\.) .+(?:\n|$))+)/g, (m, pre, list) => {
      const ordered = /^\s*\d+\./.test(list);
      const items = list.trim().split("\n").map(l => `<li>${l.replace(/^\s*(?:[-*]|\d+\.) /, "")}</li>`).join("");
      return `${pre}<${ordered ? "ol" : "ul"}>${items}</${ordered ? "ol" : "ul"}>`;
    });
    return t.split(/\n{2,}/).map(p => p.trim() ? (/^<(ul|ol)/.test(p.trim()) ? p : `<p>${p.replace(/\n/g, "<br>")}</p>`) : "").join("");
  }).join("");
}

function targets() {
  const out = [];
  if (!aiInfo) return out;
  (aiInfo.local || []).forEach(l => (l.models && l.models.length ? l.models : [""]).forEach(m => out.push({ v: `local:${l.id}|${m}`, label: `${l.name}${m ? " · " + m : ""}`, cloud: false })));
  (aiInfo.providers || []).filter(p => p.connected).forEach(p => out.push({ v: `${p.id}|${p.model || ""}`, label: `${p.custom ? (p.conn_name || p.name) : p.name}${p.model ? " · " + p.model : ""} (cloud)`, cloud: true, company: p.company }));
  return out;
}

route(/^\/jiv$/, async () => {
  const v = view();
  v.className = "page wide jivpage";
  v.innerHTML = `<div class="head"><span class="jiv-av lg" aria-hidden="true">J</span><div><h1>Jiv</h1><div class="small muted">Your helper for Dilreet, hosting and anything else. Uses AI on this PC first; cloud AI only if you connect it.</div></div><span class="grow"></span><a class="btn ghost sm" href="#/settings/ai">Connect AI</a></div><div id="jivSlot"></div>`;
  mountJiv(true);
  await ensureAI();
  fillModels();
  $("#cIn").focus();
}, "jiv");

function curTarget() { const sel = $("#cModel"); return sel ? targets().find(t => t.v === sel.value) : null; }
function fillModels() {
  const sel = $("#cModel"); if (!sel || !aiInfo) return;
  const ts = targets();
  $("#cCtx").checked = !aiInfo.no_context;
  if (!ts.length) {
    sel.innerHTML = `<option value="">Jiv offline (built-in guide)</option>`;
    $("#cWhere").innerHTML = "";
    $("#suggest").innerHTML = `<div class="card flat"><b>No AI app found on this PC.</b> Offline, Jiv answers questions about Dilreet from its built-in guide. For any other question, install a free local AI (runs entirely on this PC) or connect a provider.
      <div class="row" style="margin-top:8px"><button id="oInst" class="primary sm">Install Ollama (free, local)</button><a class="btn sm" href="#/settings/ai">Connect a cloud AI</a><button id="oRescan" class="ghost sm">Scan again</button></div><div id="oJob"></div>
      <p class="hint">Also detected automatically when running: LM Studio, llama.cpp server, Jan, LocalAI, GPT4All, text-generation-webui, vLLM.</p></div>`;
    $("#oInst").onclick = installOllama;
    $("#oRescan").onclick = (e) => busy(e.target, async () => { aiInfo = await api("GET", "/api/ai/providers?fresh=1"); fillModels(); });
    return;
  }
  sel.innerHTML = ts.map(t => `<option value="${esc(t.v)}">${esc(t.label)}</option>`).join("");
  const def = aiInfo.default && ts.find(t => t.v === aiInfo.default) ? aiInfo.default : (ts.find(t => !t.cloud) || ts[0]).v;
  sel.value = def;
  const where = () => { const t = curTarget(); $("#cWhere").innerHTML = t && t.cloud ? `<span class="badge warn">sent to ${esc(t.company)}</span>` : `<span class="badge ok">stays on this PC</span>`; };
  sel.onchange = () => { where(); api("POST", "/api/ai/settings", { default: sel.value }).catch(() => {}); };
  where();
  if (!chat.length && !$("#suggest").innerHTML) suggestions();
}
function suggestions() {
  const q = ["Connect my GoDaddy domain", "Import a site from GitHub", "Check my server and projects", "Where are my project files?", "How do I roll back a deploy?"];
  $("#suggest").innerHTML = q.map(x => `<button class="pill" type="button">${esc(x)}</button>`).join("");
  $("#suggest").onclick = (e) => { if (e.target.matches("button.pill")) sendChat(e.target.textContent); };
}

function drawChat() {
  const box = $("#msgs"); if (!box) return;
  box.innerHTML = chat.length ? "" : `<div class="empty"><b>Hi, I'm Jiv!</b><br>Ask me anything &mdash; about your projects, domains and hosting, or any other question when an AI is connected. When I can do something for you, I show a card first, and nothing changes until you click it.</div>`;
  chat.forEach((m, i) => box.appendChild(msgEl(m, i)));
  box.scrollTop = box.scrollHeight;
}
function msgEl(m, i) {
  const el = h(`<div class="msg ${m.role}"><div class="bubble">${m.role === "user" ? esc(m.content).replace(/\n/g, "<br>") : md(m.content) || (m.error ? "" : `<span class="spin"></span>`)}</div></div>`);
  if (m.error) el.firstElementChild.appendChild(h(`<div class="alert bad small">${esc(m.error)}</div>`));
  (m.actions || []).forEach((a, k) => el.appendChild(actionCard(a, i, k)));
  if (m.role === "assistant" && m.meta) el.appendChild(h(`<div class="small muted">${esc(m.meta)}${i === chat.length - 1 && !chatAbort ? ` &middot; <a href="#" class="regen">Regenerate</a>` : ""}</div>`));
  const r = $(".regen", el); if (r) r.onclick = (e) => { e.preventDefault(); regenerate(); };
  return el;
}
function actionCard(a, i, k) {
  const el = h(`<div class="action card flat" role="group" aria-label="Suggested action"><div><b>${esc(a.label || a.name)}</b></div>
    <div class="small muted">${a.state === "done" ? "&#10004; Done" : a.state === "dismissed" ? "Dismissed" : "Nothing happens until you click Run."}</div>
    ${a.state ? "" : `<div class="row"><button class="primary sm run">Run</button><button class="ghost sm dis">Dismiss</button></div>`}<div class="aj"></div></div>`);
  const mark = (s) => { chat[i].actions[k].state = s; store("chat", chat); };
  if (!a.state) {
    $(".dis", el).onclick = () => { mark("dismissed"); drawChat(); };
    $(".run", el).onclick = (e) => busy(e.target, async () => { const ok = await runAction(a, $(".aj", el)); if (ok !== false) { mark("done"); $(".row", el).remove(); } });
  }
  return el;
}
function siteArg(x) { x = String(x || "").toLowerCase().trim(); return sites.find(s => s.slug === x || s.name.toLowerCase() === x || (s.domain || "") === x); }
async function runAction(a, el) {
  const args = a.args || {};
  await loadSites();
  const needSite = () => { const s = siteArg(args.site); if (!s) throw new Error(`I couldn't find a project called "${args.site}".`); return s; };
  switch (a.name) {
    case "open_page": {
      const p = { projects: "#/projects", sites: "#/projects", "new-project": "#/new", "new-site": "#/new", deploys: "#/deploys", domains: "#/domains", testing: "#/testing", assistant: "#/jiv", jiv: "#/jiv", settings: "#/settings", "connect-ai": "#/settings/ai", storage: "#/settings/storage" }[args.page] || "#/projects";
      go(p); return;
    }
    case "open_project": {
      const s = needSite();
      const t = { files: "/files", deploys: "/deploys", domain: "/domain", settings: "/settings" }[args.tab] || "";
      go(`#/project/${s.slug}${t}`); return;
    }
    case "open_project_folder": { const s = needSite(); await api("POST", `/api/projects/${s.slug}/open`, { what: "folder" }); toast("Opened the folder of " + s.name, "success"); return; }
    case "setup_server": go("#/setup"); return;
    case "create_site": {
      const r = await api("POST", "/api/sites", { name: args.name || "my-site" });
      const j = await jobPanel(r.job, el, { title: "Creating project", doneTitle: "Project created" });
      if (j.error) return false;
      toast("Project created", "success"); return;
    }
    case "redeploy_site": { const s = needSite(); await api("POST", `/api/sites/${s.slug}/restart`); toast("Redeploy queued for " + s.name, "success"); return; }
    case "pull_and_deploy": { const s = needSite(); const j = await pullDeploy(s.slug, false, el); return j.error ? false : undefined; }
    case "rollback_site": {
      const s = needSite();
      const r = await api("GET", `/api/sites/${s.slug}/releases`);
      const rel = r.releases || []; // newest first
      const i = rel.indexOf(r.current);
      const prev = i >= 0 && i + 1 < rel.length ? rel[i + 1] : null;
      if (!prev) throw new Error("There is no earlier deploy to roll back to.");
      await api("POST", `/api/sites/${s.slug}/rollback`, { release: prev });
      toast(`${s.name}: deploy ${prev} is live again`, "success"); return;
    }
    case "set_domain": { const s = needSite(); if (args.domain) store("prefillDomain-" + s.slug, args.domain); go(`#/project/${s.slug}/domain`); return; }
    case "auto_dns": {
      const s = needSite();
      if (!s.domain) { go(`#/project/${s.slug}/domain`); return; }
      try { const r = await api("POST", `/api/sites/${s.slug}/auto-dns`, { domain: s.domain, www: s.include_www, replace: false, proxy: false }); toast(`DNS records created in ${r.zone}`, "success"); }
      catch (e) { if (e.status === 409) { go(`#/project/${s.slug}/domain`); toast("Some DNS records already exist. Review them on the domain page.", "warn"); return false; } throw e; }
      return;
    }
    case "check_dns": { const s = needSite(); go(`#/project/${s.slug}/domain`); return; }
    case "toggle_auto_deploy": { const s = needSite(); await api("POST", `/api/sites/${s.slug}/git`, { auto_deploy: !!args.on }); toast(`Auto-deploy ${args.on ? "on" : "off"} for ${s.name}`, "success"); return; }
    case "install_ollama": return installOllama();
    default: throw new Error("Unknown action " + a.name);
  }
}

async function installOllama() {
  const ok = await confirmBox("Install Ollama?", `<p>Dilreet downloads the official Ollama installer for Windows from <code>ollama.com</code> (about 1 GB) and starts it. Then it downloads the model <code>llama3.2:3b</code> (about 2 GB). Everything runs on this PC; your chats never leave it.</p>`, "Download and install");
  if (!ok) return false;
  const box = $("#oJob") || $("#msgs");
  const r = await api("POST", "/api/ai/ollama/install");
  if (r.manual) { box.appendChild(h(`<div class="alert info">Install Ollama from <a href="${esc(r.url)}" target="_blank" rel="noopener">${esc(r.url)}</a>, then click "Scan again".</div>`)); return false; }
  const d = h(`<div></div>`); box.appendChild(d);
  const j = await jobPanel(r.job, d, { title: "Installing Ollama", doneTitle: "Ollama is running" });
  if (j.error) return false;
  const p = await api("POST", "/api/ai/ollama/pull", { model: "llama3.2:3b" });
  const d2 = h(`<div></div>`); box.appendChild(d2);
  const j2 = await jobPanel(p.job, d2, { title: "Downloading the AI model", doneTitle: "Model ready" });
  aiInfo = await api("GET", "/api/ai/providers?fresh=1").catch(() => aiInfo);
  $("#suggest") && ($("#suggest").innerHTML = "");
  fillModels();
  return j2.error ? false : undefined;
}

function regenerate() {
  while (chat.length && chat[chat.length - 1].role === "assistant") chat.pop();
  const last = chat.pop();
  if (last) sendChat(last.content, last.about);
}
async function sendChat(text, aboutOverride) {
  text = String(text || "").trim();
  if (!text || chatAbort) return;
  await ensureAI();
  const about = aboutOverride || $("#cIn").dataset.about || "";
  $("#cIn").value = ""; delete $("#cIn").dataset.about;
  $("#suggest").innerHTML = "";
  chat.push({ role: "user", content: text, about });
  const t = curTarget();
  if (!t) {
    const r = await api("POST", "/api/ai/offline", { question: text + (about ? "\n" + about : "") }).catch(e => ({ answer: e.message }));
    chat.push({ role: "assistant", content: r.answer, meta: "Jiv offline · built-in guide" });
    store("chat", chat); drawChat(); return;
  }
  const [target, model] = t.v.split("|");
  const msg = { role: "assistant", content: "", actions: [] };
  chat.push(msg); drawChat();
  chatAbort = new AbortController();
  $("#cSend").classList.add("hidden"); $("#cStop").classList.remove("hidden");
  const history = chat.slice(0, -1).filter(m => m.content).map(m => ({ role: m.role, content: m.content })).slice(-30);
  try {
    let r = await chatFetch({ target, model, messages: history, include_context: $("#cCtx").checked, about });
    if (r.status === 409) {
      const d = await r.json();
      const ok = await confirmBox(`Send this chat to ${d.company}?`, `<p>This model runs in the cloud. Your messages${$("#cCtx").checked ? " and a summary of your Dilreet status (site names, domains, deploy states)" : ""} are sent to <b>${esc(d.company)}</b>, and their privacy terms apply.</p><p>Never sent: SSH passwords, API tokens or keys. Secrets and IP addresses are removed before sending.</p><p class="small muted">You can turn off "Include my Dilreet status" any time, or use a local AI instead.</p>`, "Allow and send");
      if (!ok) throw new Error("Not sent. Choose a local model, or allow this provider.");
      await api("POST", "/api/ai/consent/" + encodeURIComponent(d.need_consent));
      r = await chatFetch({ target, model, messages: history, include_context: $("#cCtx").checked, about });
    }
    if (!r.ok) { let m = "HTTP " + r.status; try { m = (await r.json()).error || m; } catch (e) {} throw new Error(m); }
    const reader = r.body.getReader(), dec = new TextDecoder();
    let buf = "";
    const bubble = () => { const all = $$("#msgs .msg"); return all[all.length - 1]; };
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).trim(); buf = buf.slice(nl + 1);
        if (!line) continue;
        let ev; try { ev = JSON.parse(line); } catch (e) { continue; }
        if (ev.t === "start") msg.meta = `${ev.label}${ev.model ? " · " + ev.model : ""}${ev.cloud ? " · cloud" : " · on this PC"}${ev.context ? " · with status" : ""}`;
        else if (ev.t === "text") { msg.content += ev.v; const b = bubble(); if (b) { b.firstElementChild.innerHTML = md(msg.content); $("#msgs").scrollTop = $("#msgs").scrollHeight; } }
        else if (ev.t === "action") msg.actions.push({ name: ev.name, args: ev.args, label: ev.label });
        else if (ev.t === "error") msg.error = ev.v;
      }
    }
  } catch (e) {
    if (e.name === "AbortError") msg.meta = (msg.meta || "") + " · stopped";
    else msg.error = e.message;
  } finally {
    chatAbort = null;
    if ($("#cSend")) { $("#cSend").classList.remove("hidden"); $("#cStop").classList.add("hidden"); }
    if (!msg.content && !msg.error && !msg.actions.length) msg.content = "_(no answer)_";
    store("chat", chat.slice(-100)); drawChat();
  }
}
function chatFetch(body) {
  return fetch("/api/ai/chat", { method: "POST", headers: { "X-Dilreet": "1", "Content-Type": "application/json" }, body: JSON.stringify(body), signal: chatAbort.signal });
}

// ---------- settings ----------
const settingsTabs = [["general", "General"], ["server", "Server"], ["storage", "Storage"], ["ai", "Connect AI"], ["git", "Git & Netlify"], ["updates", "Updates"]];
route(/^\/settings(?:\/(server|storage|ai|git|updates))?$/, async (tab) => {
  tab = tab || "general";
  await refreshOverview();
  state = await api("GET", "/api/state").catch(() => ({}));
  const v = view();
  v.innerHTML = `<div class="head"><h1>Settings</h1><span class="grow"></span><a class="btn ghost sm" href="#/guide">Guide &amp; help</a></div>
    <div class="tabs" role="tablist">${settingsTabs.map(([k, l]) => `<button role="tab" data-t="${k}" aria-selected="${k === tab}">${l}</button>`).join("")}</div><div id="sBody" role="tabpanel"></div>`;
  $$(".tabs button", v).forEach(b => b.onclick = () => go(b.dataset.t === "general" ? "#/settings" : "#/settings/" + b.dataset.t));
  const body = $("#sBody");
  ({ general: setGeneral, server: setServer, storage: setStorage, ai: setAI, git: setGit, updates: setUpdates })[tab](body);
}, "settings");

function setGeneral(el) {
  const theme = document.documentElement.getAttribute("data-theme");
  el.innerHTML = `<div class="card"><h2>Appearance</h2><div class="row" role="radiogroup" aria-label="Theme">${["light", "dark", "system"].map(t => `<label class="check"><input type="radio" name="th" value="${t}" ${(localStorage.getItem("hp-theme") || "system") === t ? "checked" : ""}> ${t[0].toUpperCase() + t.slice(1)}</label>`).join("")}</div></div>
    <div class="card"><h2>Automation</h2>
      <label class="switch"><input type="checkbox" id="gBg" ${ov.background_checks ? "checked" : ""}> Background checks <span class="muted small">&mdash; watch DNS and HTTPS, connect domains when DNS is ready, check site health, auto-deploy repositories and look for updates</span></label>
      <label class="switch"><input type="checkbox" id="gTf" ${store("testFirst") ? "checked" : ""}> Test before publishing by default</label></div>
    <div class="card"><h2>About</h2><div class="kv small"><div>Version</div><div>${esc(ov.version)} (${esc(ov.app_mode || "")}${ov.app_mode_reason ? ": " + esc(ov.app_mode_reason) : ""})</div><div>Settings file</div><div><code>${esc(state.config_path)}</code></div><div>Secrets</div><div>${esc(state.token_storage || "")}</div></div>
      <div class="row" style="margin-top:12px"><a class="btn" href="#/guide">Open the guide</a><button id="gQuit" class="danger">Quit Dilreet</button></div></div>`;
  $$("input[name=th]", el).forEach(r => r.onchange = () => {
    const t = r.value;
    if (t === "system") { localStorage.removeItem("hp-theme"); document.documentElement.setAttribute("data-theme", matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"); }
    else { localStorage.setItem("hp-theme", t); document.documentElement.setAttribute("data-theme", t); }
  });
  void theme;
  $("#gBg").onchange = (e) => api("POST", "/api/background", { enabled: e.target.checked }).then(() => toast("Saved", "success")).catch(err);
  $("#gTf").onchange = (e) => store("testFirst", e.target.checked);
  $("#gQuit").onclick = async () => { if (await confirmBox("Quit Dilreet?", "Background checks and automatic deploys stop until you start it again.", "Quit", true)) { await api("POST", "/api/quit"); document.body.innerHTML = `<main style="padding:40px"><h1>Dilreet has stopped.</h1><p>You can close this window.</p></main>`; } };
}

async function setStorage(el) {
  el.innerHTML = `<p class="muted">Loading&hellip;</p>`;
  const r = await api("GET", "/api/storage");
  const ps = r.projects || [];
  el.innerHTML = `<div class="card"><div class="card-head"><h2>Project folders</h2><span class="grow"></span><b>${esc(humanSize(r.bytes || 0))}</b>&nbsp;<span class="small muted">used</span></div>
      <p class="small muted">Each project has a folder with its current files and every deploy as a .zip.</p>
      <p><code>${esc(r.root)}</code> <button id="stOpen" class="sm ghost">Open</button></p>
      ${ps.length ? `<table class="records"><thead><tr><th>Project</th><th>Folder</th><th>Deploys</th><th>Current files</th><th>Total</th></tr></thead><tbody>${ps.map(p => `<tr><td><a href="#/project/${esc(p.slug)}/deploys">${esc(p.name)}</a></td><td><code>${esc(p.dir.split(/[\\/]/).pop())}</code></td><td>${p.count}</td><td>${esc(humanSize(p.current))}</td><td>${esc(humanSize(p.current + p.deploys))}</td></tr>`).join("")}</tbody></table>` : `<p class="muted">No projects yet.</p>`}</div>
    <div class="card"><h2>Keep deploys</h2><div class="row"><select id="stKeep" style="width:auto">${[[0, "All deploys"], [5, "Last 5"], [10, "Last 10"], [20, "Last 20"], [50, "Last 50"]].map(([n, l]) => `<option value="${n}" ${+r.keep === n ? "selected" : ""}>${l}</option>`).join("")}</select><span class="small muted">Older saved deploys are deleted after each new deploy. The live deploy is always kept.</span></div></div>
    <div class="card"><h2>Location</h2><div class="field"><label for="stRoot">Folder for projects</label><input id="stRoot" value="${esc(r.root)}" placeholder="${esc(r.default_root)}"></div>
      <label class="check"><input type="checkbox" id="stMove" checked> Move existing project folders there</label>
      <div class="row"><button id="stSave" class="primary sm">Change location</button><button id="stDef" class="ghost sm">Use the default (${esc(r.default_root)})</button></div></div>`;
  $("#stOpen").onclick = (e) => { const p = ps[0]; if (p) busy(e.target, () => api("POST", `/api/projects/${p.slug}/open`, { what: "reveal" })); else toast("No project folders yet.", "info"); };
  $("#stKeep").onchange = (e) => api("POST", "/api/storage", { keep: +e.target.value }).then(() => toast("Saved", "success")).catch(err);
  const move = async (root) => {
    const mv = $("#stMove").checked;
    if (!await confirmBox("Change the projects folder?", `<p>New location: <code>${esc(root || r.default_root)}</code>.</p><p>${mv ? "Dilreet moves the existing project folders there." : "Existing folders stay where they are; new deploys go to the new location."}</p>`, "Change")) return;
    await api("POST", "/api/storage", { root, move: mv }); toast("Saved", "success"); setStorage(el);
  };
  $("#stSave").onclick = (e) => busy(e.target, () => move($("#stRoot").value.trim()));
  $("#stDef").onclick = (e) => busy(e.target, () => move(""));
}

function setServer(el) {
  el.innerHTML = `<div id="srvForm"></div>
    <div class="card"><h2>Hosting engine (Coolify)</h2><div class="kv small"><div>URL</div><div>${state.coolify_url ? `<code>${esc(state.coolify_url)}</code>` : "not set"}</div><div>API token</div><div>${state.token_set ? "saved (" + esc(state.token_storage) + ")" : "not set"}</div><div>Version</div><div id="cVer">&ndash;</div></div>
      <div class="row" style="margin-top:10px"><button id="cTest" class="sm">Test connection</button>${ov.admin_password_set ? `<button id="cAdmin" class="sm">Show dashboard login</button>` : ""}<button id="cProj" class="sm ghost">List projects</button><button id="cEdit" class="sm ghost">Change URL or token</button></div><div id="cOut"></div><div id="cEditBox"></div></div>
    ${ov.stage !== "no_server" && ov.stage !== "ready" ? `<div class="card"><a class="btn primary" href="#/setup">Continue setup</a></div>` : ""}`;
  haveServerForm($("#srvForm"), { onDone: () => rerender() });
  $("#cTest").onclick = (e) => busy(e.target, async () => { const t = await api("GET", "/api/coolify/test"); $("#cVer").textContent = t.version; toast("Coolify " + t.version + " is reachable", "success"); });
  if ($("#cAdmin")) $("#cAdmin").onclick = async (e) => {
    if (!await confirmBox("Show the dashboard password?", "The Coolify admin password will be shown on screen.", "Show")) return;
    busy(e.target, async () => { const r = await api("POST", "/api/coolify/admin"); $("#cOut").innerHTML = `<div class="alert info"><div>Dashboard: <a href="${esc(r.url)}" target="_blank" rel="noopener">${esc(r.url)}</a></div><div>E-mail: <code>${esc(r.email)}</code></div><div>Password: <code>${esc(r.password)}</code> <button class="sm copy" data-copy="${esc(r.password)}">Copy</button></div></div>`; });
  };
  $("#cProj").onclick = (e) => busy(e.target, async () => { const p = await api("GET", "/api/coolify/projects"); $("#cOut").innerHTML = `<ul class="small">${(p || []).map(x => `<li>${esc(x.name)} <span class="muted">${esc(x.uuid)} ${esc(x.description || "")}</span></li>`).join("") || "<li>none</li>"}</ul>`; });
  $("#cEdit").onclick = () => coolifyForm($("#cEditBox"), () => rerender());
}

async function setAI(el) {
  el.innerHTML = `<p class="muted">Loading&hellip;</p>`;
  const info = await api("GET", "/api/ai/providers?fresh=1");
  aiInfo = info;
  fillModels();
  const local = info.local || [];
  el.innerHTML = `<div class="card"><h2>AI on this PC</h2>${local.length ? `<ul class="deploys">${local.map(l => `<li><span class="badge ok">running</span> <span class="grow"><b>${esc(l.name)}</b> <span class="small muted">${esc(l.base)}</span><div class="small muted">${(l.models || []).map(esc).join(", ") || "no models yet"}</div></span></li>`).join("")}</ul>` : `<p>No local AI app is running. Dilreet looks for Ollama, LM Studio, llama.cpp, Jan, LocalAI, GPT4All, text-generation-webui and vLLM on their usual ports.</p><div class="row"><button id="aiOl" class="primary sm">Install Ollama (free)</button><button id="aiRe" class="sm ghost">Scan again</button></div><div id="oJob"></div>`}
      <p class="hint">Local AI keeps everything on this PC. It is used by default when available.</p></div>
    <div class="card"><h2>Cloud AI providers</h2><p class="small muted">Optional. Paste your own API key. Keys are encrypted on this PC and only sent to that company. Before the first chat, Dilreet asks you once per provider.</p>
      <label class="switch"><input type="checkbox" id="aiNoCtx" ${info.no_context ? "checked" : ""}> Never include my Dilreet status when chatting with cloud AI</label>
      <div class="grid2" id="provs" style="margin-top:12px"></div></div>`;
  if ($("#aiOl")) { $("#aiOl").onclick = () => installOllama().then(() => setAI(el)); $("#aiRe").onclick = () => setAI(el); }
  $("#aiNoCtx").onchange = (e) => api("POST", "/api/ai/settings", { no_context: e.target.checked }).then(() => toast("Saved", "success")).catch(err);
  const pv = $("#provs");
  (info.providers || []).forEach(p => pv.appendChild(providerCard(p, () => setAI(el))));
}
function providerCard(p, refresh) {
  const el = h(`<div class="provider card flat"><div class="row"><span class="dot" style="background:${esc(p.color || "#888")}"></span><b class="grow">${esc(p.custom ? (p.conn_name || p.name) : p.name)}</b>${p.connected ? `<span class="badge ok">connected</span>` : ""}</div>
    <p class="small muted">${esc(p.company || "")}${p.hint ? " &middot; suggested model <code>" + esc(p.hint) + "</code>" : ""}</p>
    ${p.custom ? `<div class="field"><label>Name</label><input class="pn" value="${esc(p.conn_name || "")}" placeholder="My server"></div><div class="field"><label>Base URL <span class="muted">(OpenAI-compatible, ending in /v1)</span></label><input class="pb" value="${esc(p.conn_base || "")}" placeholder="http://localhost:8080/v1"></div>` : ""}
    <div class="field"><label>API key ${p.key_url ? `<a href="${esc(p.key_url)}" target="_blank" rel="noopener" class="small">Get an API key &#8599;</a>` : ""}</label><input class="pk" type="password" autocomplete="off" placeholder="${p.connected && p.key_hint ? "saved: " + esc(p.key_hint) : p.custom ? "optional" : "paste key"}"></div>
    <div class="field"><label>Default model</label><select class="pm">${p.model || p.hint ? `<option>${esc(p.model || p.hint)}</option>` : `<option value="">Test the key to list models</option>`}</select></div>
    <div class="row"><button class="sm pt">Test</button><button class="sm primary ps">Save</button>${p.connected ? `<button class="sm ghost danger pr">Remove</button>` : ""}</div><div class="pe"></div></div>`);
  const val = (c) => $(c, el) ? $(c, el).value.trim() : undefined;
  $(".pt", el).onclick = (e) => busy(e.target, async () => {
    $(".pe", el).innerHTML = "";
    try {
      const r = await api("POST", `/api/ai/providers/${p.id}/test`, { key: val(".pk"), base_url: val(".pb") || "" });
      const ms = r.models || [];
      $(".pm", el).innerHTML = ms.map(m => `<option ${m === p.model ? "selected" : ""}>${esc(m)}</option>`).join("") || `<option value="">(no models listed)</option>`;
      $(".pe", el).innerHTML = `<div class="alert ok small">Key works: ${ms.length} models.</div>`;
    } catch (x) { $(".pe", el).innerHTML = `<div class="alert bad small">${esc(x.message)}</div>`; }
  });
  $(".ps", el).onclick = (e) => busy(e.target, async () => {
    const body = { model: val(".pm") };
    if (val(".pk")) body.key = val(".pk");
    if (p.custom) { body.base_url = val(".pb"); body.name = val(".pn"); }
    try { await api("POST", `/api/ai/providers/${p.id}`, body); toast("Saved", "success"); refresh(); }
    catch (x) { $(".pe", el).innerHTML = `<div class="alert bad small">${esc(x.message)}</div>`; }
  });
  if ($(".pr", el)) $(".pr", el).onclick = async () => { if (await confirmBox("Remove " + p.name + "?", "The saved key is deleted from this PC.", "Remove", true)) { await api("DELETE", `/api/ai/providers/${p.id}`); refresh(); } };
  return el;
}

async function setGit(el) {
  const a = await api("GET", "/api/git/accounts").catch(() => ({ accounts: [], providers: {} }));
  const accs = a.accounts || [];
  el.innerHTML = `<div class="card"><h2>Connected accounts</h2>${accs.length ? `<ul class="deploys">${accs.map(x => `<li><span class="badge accent">${esc(x.kind)}</span><span class="grow"><b>${esc(x.login || x.user || "")}</b> <span class="small muted">${esc(x.host)}${x.org ? " / " + esc(x.org) : ""} &middot; token ${esc(x.hint)}</span></span><button class="sm ghost danger" data-k="${esc(x.key)}">Remove</button></li>`).join("")}</ul>` : `<p class="muted">None. Public repositories work without an account.</p>`}</div>
    <div class="card"><h2>Connect an account</h2><div class="row"><select id="gk" style="width:auto">${["github", "gitlab", "bitbucket", "gitea", "azure", "netlify"].filter(k => (a.providers || {})[k]).map(k => [k, a.providers[k]]).map(([k, v]) => `<option value="${esc(k)}">${esc(v.label)}</option>`).join("")}</select></div><div id="gkBox"></div></div>`;
  el.onclick = async (e) => {
    const k = e.target.dataset.k;
    if (!k) return;
    if (await confirmBox("Remove this account?", "The saved token is deleted from this PC. Projects linked to private repositories stop auto-deploying.", "Remove", true)) { await api("DELETE", "/api/git/accounts?key=" + encodeURIComponent(k)); setGit(el); }
  };
  const show = () => { const k = $("#gk").value, pr = a.providers[k] || {}; connectAccountCard($("#gkBox"), { kind: k, host: k === "gitlab" ? "gitlab.com" : k === "gitea" ? "codeberg.org" : "", label: pr.label, token_url: pr.token_url && pr.token_url.replace("{org}", ""), hint: pr.hint, onDone: () => setGit(el) }); };
  $("#gk").onchange = show; show();
}

function setUpdates(el) {
  el.innerHTML = `<div class="card"><h2>Dilreet</h2><p>You have version <b>${esc(ov.version)}</b>. <span id="uRes"></span></p>
      <div class="row"><button id="uCheck">Check for updates</button><button id="uApply" class="primary hidden">Update now</button></div>
      <details style="margin-top:10px"><summary class="small muted" style="cursor:pointer">Update source</summary><div class="row" style="margin-top:8px"><input id="uMan" value="${esc(state.manifest_url || "")}" style="flex:1" aria-label="Update manifest URL"><button id="uSave" class="sm">Save</button></div><p class="hint">An https link to a JSON manifest (GitHub Releases works). Updates are verified with SHA-256 before installing.</p></details><div id="uJob"></div></div>
    <div class="card"><h2>Hosting engine (Coolify)</h2><p>Installed: <b id="kVer">&ndash;</b> &middot; Latest: <b id="kLat">&ndash;</b></p><div class="row"><button id="kCheck">Check</button><button id="kUp" class="primary">Update Coolify</button></div><p class="hint">Websites keep running during the update; only the dashboard is unavailable for about a minute.</p><div id="kJob"></div></div>`;
  $("#uSave").onclick = (e) => busy(e.target, async () => { await api("POST", "/api/settings", { manifest_url: $("#uMan").value }); toast("Saved", "success"); });
  $("#uCheck").onclick = (e) => busy(e.target, async () => {
    const r = await api("GET", "/api/update/check");
    $("#uRes").innerHTML = r.newer ? `<span class="badge info">Version ${esc(r.manifest.version)} is available</span> ${esc(r.manifest.notes || "")}` : `<span class="badge ok">You're up to date</span>`;
    $("#uApply").classList.toggle("hidden", !r.newer);
  });
  $("#uApply").onclick = async (e) => {
    if (!await confirmBox("Update Dilreet now?", "Dilreet downloads the new version, verifies it, and restarts.", "Update")) return;
    busy(e.target, async () => { const j = await api("POST", "/api/update/apply"); const r = await jobPanel(j.job, $("#uJob"), { title: "Updating", doneTitle: "Updated, restarting" }); if (!r.error) toast("Dilreet is restarting", "success"); });
  };
  $("#kCheck").onclick = (e) => busy(e.target, async () => {
    try { $("#kVer").textContent = (await api("GET", "/api/coolify/test")).version; } catch (x) { $("#kVer").textContent = "unknown"; }
    try { $("#kLat").textContent = (await api("GET", "/api/coolify/latest")).latest; } catch (x) { $("#kLat").textContent = "unknown"; }
  });
  $("#kUp").onclick = async (e) => {
    if (!await confirmBox("Update Coolify?", "The Coolify dashboard is unavailable for about a minute; websites keep running.", "Update Coolify")) return;
    busy(e.target, async () => { const j = await api("POST", "/api/coolify/upgrade"); await jobPanel(j.job, $("#kJob"), { title: "Updating Coolify", doneTitle: "Coolify updated" }); });
  };
}

// ---------- init ----------
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && !$("#noticePanel").classList.contains("hidden")) $("#bell").click(); });
initJiv();
(async () => {
  await refreshOverview();
  if (!location.hash || location.hash === "#") location.hash = (ov.stage === "no_server" || ov.stage === "no_engine") && !ov.site_count ? "#/setup" : "#/projects";
  else render();
  setInterval(refreshOverview, 15000);
})();
