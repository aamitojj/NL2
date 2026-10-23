"use strict";
const $ = (s) => document.querySelector(s);
let state = {};
let sites = [];
let current = null; // selected site slug
let page = 0;
const PAGE = 25;
let dnsTimer = null;
let filled = false;

function toast(msg, isErr) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = isErr ? "error" : "";
  clearTimeout(t._h);
  t._h = setTimeout(() => t.classList.add("hidden"), isErr ? 9000 : 4000);
}

async function api(method, path, body) {
  const opt = { method, headers: { "X-HostPanel": "1" } };
  if (body instanceof FormData) opt.body = body;
  else if (body !== undefined) { opt.body = JSON.stringify(body); opt.headers["Content-Type"] = "application/json"; }
  const r = await fetch(path, opt);
  let data = {};
  try { data = await r.json(); } catch (e) {}
  if (r.status === 401) throw new Error("Session expired. Open the link shown in the HostPanel window (or restart HostPanel).");
  if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
  return data;
}

function esc(s) { return String(s ?? "").replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c])); }

// Polls a background job and streams its log into <pre>.
function followJob(id, pre, onDone) {
  pre.classList.remove("hidden");
  pre.textContent = "";
  let from = 0;
  const tick = async () => {
    try {
      const j = await api("GET", `/api/jobs/${id}?from=${from}`);
      if (j.lines.length) {
        pre.textContent += j.lines.join("\n") + "\n";
        pre.scrollTop = pre.scrollHeight;
      }
      from = j.next;
      if (j.done) { onDone && onDone(j); return; }
    } catch (e) { pre.textContent += "\n[" + e.message + "]\n"; }
    setTimeout(tick, 1000);
  };
  tick();
}

async function withBtn(btn, fn) {
  btn.disabled = true;
  try { await fn(); } catch (e) { toast(e.message, true); } finally { btn.disabled = false; }
}

// ---------------- tabs ----------------
function showTab() {
  let t = (location.hash || "#setup").slice(1);
  let anchor = null;
  if (t.startsWith("guide-")) { anchor = t; t = "guide"; }
  document.querySelector("main").classList.toggle("wide", t === "testing");
  document.querySelectorAll(".tab").forEach(s => s.classList.toggle("hidden", s.id !== "tab-" + t));
  document.querySelectorAll("nav a").forEach(a => a.classList.toggle("active", a.dataset.tab === t));
  if (t === "sites") loadSites();
  if (t === "testing") testTabShown();
  if (anchor) { const el = document.getElementById(anchor); if (el) setTimeout(() => el.scrollIntoView(), 0); }
}
window.addEventListener("hashchange", showTab);

// ---------------- state ----------------
async function loadState() {
  state = await api("GET", "/api/state");
  $("#ver").textContent = "v" + state.version;
  $("#curVer").textContent = state.version + " (" + state.os + ")";
  $("#cfgPath").textContent = state.config_path;
  $("#manifestUrl").value = state.manifest_url || "";
  if (!filled) {
    filled = true;
    $("#ip").value = state.server_ip || "";
    $("#port").value = state.ssh_port || 22;
    $("#user").value = state.ssh_user || "root";
    $("#keypath").value = state.ssh_key_path || "";
    $("#curl").value = state.coolify_url || "";
    $("#viassh").checked = !!state.api_via_ssh;
  }
  $("#tokenNote").textContent = state.token_set ? "A token is saved (" + state.token_storage + ")." : "No token saved yet.";
  $("#hostkey").textContent = state.host_key_fp ? "Pinned server host key: " + state.host_key_fp : "";
  const b = $("#sshBadge");
  b.textContent = state.ssh_connected ? "SSH: connected" : "SSH: off";
  b.className = "badge " + (state.ssh_connected ? "ok" : "");
  const link = $("#coolifyLink");
  if (state.server_ip) { link.href = "http://" + state.server_ip + ":8000"; link.textContent = link.href; }
}

function setApiBadge(ok, text) {
  const b = $("#apiBadge");
  b.textContent = "API: " + text;
  b.className = "badge " + (ok ? "ok" : "bad");
}

// ---------------- setup ----------------
$("#saveServer").onclick = (e) => withBtn(e.target, async () => {
  await api("POST", "/api/server", { ip: $("#ip").value, port: +$("#port").value, user: $("#user").value, key_path: $("#keypath").value });
  toast("Server saved");
  await loadState();
});

$("#connect").onclick = (e) => withBtn(e.target, async () => {
  await api("POST", "/api/server", { ip: $("#ip").value, port: +$("#port").value, user: $("#user").value, key_path: $("#keypath").value });
  const r = await api("POST", "/api/ssh/connect", { password: $("#pw").value, passphrase: $("#pp").value });
  $("#pp").value = "";
  const pre = $("#serverInfo");
  pre.classList.remove("hidden");
  pre.textContent = (r.first_time ? "First connection: host key " + r.fingerprint + " is now pinned.\n" : "") + r.info;
  toast("Connected");
  await loadState();
});

$("#disconnect").onclick = (e) => withBtn(e.target, async () => { await api("POST", "/api/ssh/disconnect"); $("#pw").value = ""; await loadState(); });
$("#forget").onclick = (e) => withBtn(e.target, async () => {
  if (!confirm("Forget the stored host key? Only do this if you reinstalled or replaced the server.")) return;
  await api("POST", "/api/ssh/forget-hostkey"); await loadState();
});

$("#install").onclick = (e) => withBtn(e.target, async () => {
  if (!confirm("Install Coolify on " + (state.server_ip || "the server") + "? Use a fresh server.")) return;
  const r = await api("POST", "/api/install");
  e.target.disabled = true;
  followJob(r.job, $("#installLog"), (j) => {
    e.target.disabled = false;
    if (!j.error) $("#afterInstall").classList.remove("hidden");
  });
});

$("#saveCoolify").onclick = (e) => withBtn(e.target, async () => {
  const out = $("#coolifyResult");
  out.innerHTML = "";
  try {
    const r = await api("POST", "/api/coolify", { url: $("#curl").value, token: $("#ctoken").value, api_via_ssh: $("#viassh").checked });
    $("#ctoken").value = "";
    out.innerHTML = `<p class="ok">Connected to Coolify ${esc(r.version)}. Servers: ${r.servers.map(s => esc(s.name) + " (" + esc(s.ip) + ")").join(", ")}</p><p>Setup is done. Go to <a href="#sites">Sites</a>.</p>`;
    setApiBadge(true, "ok");
  } catch (err) {
    out.innerHTML = `<div class="err">${esc(err.message)}</div>`;
    setApiBadge(false, "error");
  }
  await loadState();
});

// ---------------- sites ----------------
$("#newName").oninput = () => {
  $("#newSlug").value = $("#newName").value.toLowerCase().normalize("NFKD").replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 40);
};

$("#createSite").onclick = (e) => withBtn(e.target, async () => {
  const r = await api("POST", "/api/sites", { name: $("#newName").value, slug: $("#newSlug").value });
  followJob(r.job, $("#createLog"), (j) => { loadSites().then(() => { if (!j.error) selectSite(r.slug); }); });
  $("#newName").value = ""; $("#newSlug").value = "";
});

async function loadSites() {
  try {
    const r = await api("GET", "/api/sites");
    sites = r.sites || [];
    const err = $("#sitesErr");
    err.classList.toggle("hidden", !r.api_error);
    err.textContent = r.api_error ? "Coolify status unavailable: " + r.api_error : "";
    setApiBadge(!r.api_error, r.api_error ? "error" : "ok");
    renderSites();
  } catch (e) { toast(e.message, true); }
}

function renderSites() {
  const q = $("#filter").value.toLowerCase();
  const list = sites.filter(s => !q || (s.name + " " + s.slug + " " + (s.domain || "")).toLowerCase().includes(q));
  const pages = Math.max(1, Math.ceil(list.length / PAGE));
  if (page >= pages) page = pages - 1;
  const rows = list.slice(page * PAGE, page * PAGE + PAGE);
  $("#siteCount").textContent = `${list.length} of ${sites.length} sites`;
  $("#sitesTable tbody").innerHTML = rows.map(s => {
    const addr = s.domain ? (s.domain_live ? `<a href="https://${esc(s.domain)}" target="_blank" rel="noopener">${esc(s.domain)}</a>` : esc(s.domain) + ' <span class="muted">(waiting for DNS)</span>')
      : (s.fqdn ? `<a href="${esc(s.fqdn.split(",")[0])}" target="_blank" rel="noopener">temporary address</a>` : '<span class="muted">none</span>');
    return `<tr data-slug="${esc(s.slug)}" class="${s.slug === current ? "sel" : ""}"><td><b>${esc(s.name)}</b><br><span class="muted small">${esc(s.slug)}</span></td>
      <td>${esc(s.status)}</td><td>${addr}</td><td class="small">${s.last_deploy ? new Date(s.last_deploy).toLocaleString() : '<span class="muted">never</span>'}</td>
      <td><button data-open="${esc(s.slug)}">Manage</button></td></tr>`;
  }).join("") || `<tr><td colspan="5" class="muted">No sites yet. Create one above.</td></tr>`;
  $("#pager").innerHTML = pages > 1 ? Array.from({ length: pages }, (_, i) => `<button data-page="${i}" ${i === page ? "class='primary'" : ""}>${i + 1}</button>`).join("") : "";
}
$("#filter").oninput = () => { page = 0; renderSites(); };
$("#refreshSites").onclick = loadSites;
$("#pager").onclick = (e) => { if (e.target.dataset.page) { page = +e.target.dataset.page; renderSites(); } };
$("#sitesTable").onclick = (e) => { const s = e.target.dataset.open; if (s) selectSite(s); };

function selectSite(slug) {
  current = slug;
  const s = sites.find(x => x.slug === slug);
  if (!s) return;
  renderSites();
  $("#siteDetail").classList.remove("hidden");
  $("#sdTitle").textContent = s.name + " (" + s.slug + ")";
  $("#domain").value = s.domain || "";
  $("#www").checked = s.domain ? !!s.include_www : true;
  ["#deployLog", "#otherLog"].forEach(id => $(id).classList.add("hidden"));
  $("#releases").innerHTML = "";
  $("#dnsBox").classList.toggle("hidden", !s.domain);
  clearInterval(dnsTimer);
  if (s.domain) { checkDns(); if (!s.domain_live) dnsTimer = setInterval(checkDns, 15000); }
  $("#siteDetail").scrollIntoView({ behavior: "smooth" });
}

$("#deployBtn").onclick = (e) => withBtn(e.target, async () => {
  const fd = new FormData();
  const zip = $("#zipFile").files[0];
  const dir = $("#dirFiles").files;
  const lp = $("#localPath").value.trim();
  if (zip) fd.append("zip", zip);
  else if (dir.length) { for (const f of dir) { fd.append("files", f); fd.append("paths", f.webkitRelativePath || f.name); } }
  else if (lp) fd.append("local_path", lp);
  else throw new Error("Choose a .zip, a folder, or enter a path");
  const r = await api("POST", `/api/sites/${current}/deploy`, fd);
  $("#zipFile").value = ""; $("#dirFiles").value = "";
  followJob(r.job, $("#deployLog"), () => loadSites());
});

$("#loadReleases").onclick = (e) => withBtn(e.target, async () => {
  const r = await api("GET", `/api/sites/${current}/releases`);
  $("#releases").innerHTML = r.releases.map(x => `<li>${esc(x)} ${x === r.current ? "<b>(live)</b>" : `<button data-rel="${esc(x)}">Make live</button>`}</li>`).join("");
});
$("#releases").onclick = (e) => {
  const rel = e.target.dataset.rel;
  if (!rel) return;
  withBtn(e.target, async () => { await api("POST", `/api/sites/${current}/rollback`, { release: rel }); toast("Release " + rel + " is live"); $("#loadReleases").click(); });
};

$("#setDomain").onclick = (e) => withBtn(e.target, async () => {
  const r = await api("POST", `/api/sites/${current}/domain`, { domain: $("#domain").value, www: $("#www").checked });
  $("#domain").value = r.domain;
  await loadSites();
  selectSite(current);
});

function renderRecords(recs) {
  $("#dnsBox tbody").innerHTML = recs.map(r => `<tr><td>${esc(r.type)}</td><td>${esc(r.name)}<br><span class="muted small">${esc(r.fqdn)}</span></td><td>${esc(r.value)}</td><td>${esc(r.ttl)}</td></tr>`).join("");
}

async function checkDns() {
  const slug = current;
  try {
    const r = await api("GET", `/api/sites/${slug}/dns`);
    if (slug !== current) return;
    renderRecords(r.records);
    $("#dnsStatus").innerHTML = r.checks.map(c => `<p class="${c.ok ? "ok" : ""}"><b>${esc(c.host)}</b>: ${c.ok ? "&#10004; " : "&#8987; "}${esc(c.message)}<br><span class="muted small">Seen: ${Object.entries(c.found).map(([k, v]) => esc(k.replace(":53", "")) + " &rarr; " + esc(v.join(", ") || "nothing")).join("; ")}</span></p>`).join("")
      + (r.domain_live ? `<p class="ok">Domain is connected in Coolify. HTTPS is issued automatically (allow 1&ndash;2 minutes).</p>` : "");
    if (r.ok && !r.domain_live && $("#autoApply").checked) {
      clearInterval(dnsTimer);
      await applyDomain();
    }
  } catch (e) { $("#dnsStatus").innerHTML = `<div class="err">${esc(e.message)}</div>`; }
}
$("#checkDns").onclick = (e) => withBtn(e.target, checkDns);

async function applyDomain(force, skipDns) {
  try {
    const r = await api("POST", `/api/sites/${current}/domain/apply`, { force: !!force, skip_dns: !!skipDns });
    toast("Domain connected: " + r.domains);
    await loadSites();
    checkDns();
  } catch (e) {
    if (/^DNS not ready/.test(e.message)) {
      if (confirm(e.message + "\n\nConnect anyway? HTTPS will only work once DNS points to your server (Coolify keeps retrying).")) return applyDomain(force, true);
      return;
    }
    if (/conflict/i.test(e.message) && confirm(e.message + "\n\nUse this domain anyway?")) return applyDomain(true, skipDns);
    toast(e.message, true);
  }
}
$("#applyDomain").onclick = (e) => withBtn(e.target, () => applyDomain(false));

$("#restartSite").onclick = (e) => withBtn(e.target, async () => { await api("POST", `/api/sites/${current}/restart`); toast("Redeploy queued in Coolify"); });

$("#deleteSite").onclick = (e) => withBtn(e.target, async () => {
  const s = sites.find(x => x.slug === current);
  if (prompt(`Type the site id "${s.slug}" to delete it permanently:`) !== s.slug) return;
  const r = await api("DELETE", `/api/sites/${current}` + ($("#keepFiles").checked ? "?keep_files=1" : ""));
  followJob(r.job, $("#otherLog"), (j) => { if (!j.error) { $("#siteDetail").classList.add("hidden"); current = null; } loadSites(); });
});

// ---------------- settings ----------------
$("#saveManifest").onclick = (e) => withBtn(e.target, async () => { await api("POST", "/api/settings", { manifest_url: $("#manifestUrl").value }); toast("Saved"); });
$("#checkUpdate").onclick = (e) => withBtn(e.target, async () => {
  const box = $("#updateInfo");
  box.innerHTML = "";
  const r = await api("GET", "/api/update/check");
  if (!r.newer) { box.innerHTML = `<p class="ok">You have the latest version (${esc(r.current)}).</p>`; return; }
  box.innerHTML = `<div class="note"><b>Version ${esc(r.manifest.version)} is available.</b><pre>${esc(r.manifest.notes || "")}</pre><button id="doUpdate" class="primary">Download &amp; install</button></div>`;
  $("#doUpdate").onclick = (ev) => withBtn(ev.target, async () => {
    const j = await api("POST", "/api/update/apply");
    followJob(j.job, $("#updateLog"), (res) => { if (!res.error) toast("Updated - HostPanel is restarting; a new tab will open."); });
  });
});
$("#checkCoolify").onclick = (e) => withBtn(e.target, async () => {
  try { const t = await api("GET", "/api/coolify/test"); $("#coolifyVer").textContent = t.version; } catch (err) { $("#coolifyVer").textContent = "unknown (" + err.message + ")"; }
  try { const l = await api("GET", "/api/coolify/latest"); $("#coolifyLatest").textContent = l.latest; } catch (err) { $("#coolifyLatest").textContent = "unknown"; }
});
$("#upgradeCoolify").onclick = (e) => withBtn(e.target, async () => {
  if (!confirm("Update Coolify on the server now? The Coolify dashboard will be unavailable for about a minute; websites keep running.")) return;
  const r = await api("POST", "/api/coolify/upgrade");
  followJob(r.job, $("#upgradeLog"));
});
$("#listProjects").onclick = (e) => withBtn(e.target, async () => {
  const p = await api("GET", "/api/coolify/projects");
  $("#projects").innerHTML = p.map(x => `<li>${esc(x.name)} <span class="muted">${esc(x.uuid)} ${esc(x.description || "")}</span></li>`).join("") || "<li>none</li>";
});
$("#quit").onclick = async () => { if (confirm("Quit HostPanel?")) { await api("POST", "/api/quit"); document.body.innerHTML = "<main><h1>HostPanel has stopped.</h1><p>You can close this tab.</p></main>"; } };


// ---------------- app testing ----------------
let test = null;          // state from /api/test
let previewOrigin = "";
let logFrom = 0, logItems = [], logTimer = null;
let vp = { w: 1280, h: 800, full: false };
let lastNav = "";

function testTabShown() {
  loadSites().then(fillSiteSelect);
  api("GET", "/api/test").then(renderTest).catch(e => toast(e.message, true));
}

function fillSiteSelect() {
  const sel = $("#tSite");
  const prev = sel.value || current || "";
  sel.innerHTML = `<option value="">Choose a site&hellip;</option>` + sites.map(s => `<option value="${esc(s.slug)}">${esc(s.name)} (${esc(s.slug)})${s.domain ? " - " + esc(s.domain) : ""}</option>`).join("");
  if (prev && sites.some(s => s.slug === prev)) sel.value = prev;
  updateDeployBtn();
}
$("#tSite").onchange = updateDeployBtn;

function updateDeployBtn() {
  const ok = !!(test && test.active && $("#tSite").value);
  $("#tDeploy").disabled = !ok;
  const n = test && test.active ? (test.report.counts.fail || 0) : 0;
  $("#tDeployNote").textContent = !test || !test.active ? "Load and test a site first." :
    !$("#tSite").value ? "Choose the site to deploy to." :
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

function humanSize(n) {
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  if (n >= 1024) return Math.round(n / 1024) + " KB";
  return n + " bytes";
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
    </div>`).join("") || `<p class="ok">Everything passed.</p>`;
}
$("#rShowPass").onchange = () => test && test.active && renderReport();
$("#tReport").onclick = (e) => {
  const f = e.target.dataset.page;
  if (!f) return;
  e.preventDefault();
  let p = "/" + f;
  if (p.endsWith("/index.html")) p = p.slice(0, -10);
  navigate(p);
  $("#frameWrap").scrollIntoView({ behavior: "smooth" });
};

function uploadForm(zipEl, dirEl, pathEl) {
  const fd = new FormData();
  const zip = zipEl.files[0], dir = dirEl.files, lp = pathEl.value.trim();
  if (zip) fd.append("zip", zip);
  else if (dir.length) { for (const f of dir) { fd.append("files", f); fd.append("paths", f.webkitRelativePath || f.name); } }
  else if (lp) fd.append("local_path", lp);
  else throw new Error("Choose a .zip, a folder, or enter a path");
  return fd;
}

$("#tLoad").onclick = (e) => withBtn(e.target, async () => {
  const fd = uploadForm($("#tZip"), $("#tDir"), $("#tPath"));
  $("#tLoading").classList.remove("hidden");
  try {
    const t = await api("POST", "/api/test/load", fd);
    $("#tZip").value = ""; $("#tDir").value = "";
    test = null;
    renderTest(t);
    toast("Site loaded: " + (t.report.counts.fail ? t.report.counts.fail + " check(s) failed" : "no failed checks"));
  } finally { $("#tLoading").classList.add("hidden"); }
});
$("#tReload").onclick = (e) => withBtn(e.target, async () => {
  const path = currentPath();
  const t = await api("POST", "/api/test/reload");
  test = null;
  renderTest(t);
  navigate(path);
  toast("Reloaded and re-tested");
});
$("#tStop").onclick = (e) => withBtn(e.target, async () => {
  await api("POST", "/api/test/stop");
  $("#pFrame").src = "about:blank";
  renderTest({ active: false });
});

// --- built-in browser ---
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
  } catch (err) { p = "/"; }
  lastNav = p;
  $("#bAddr").value = p;
  $("#pFrame").src = previewOrigin + p;
}

function showExternal(href) {
  const box = $("#bExternal");
  box.classList.remove("hidden");
  box.innerHTML = `This page wants to open an outside address: <code>${esc(href)}</code> <a href="${esc(href)}" target="_blank" rel="noopener noreferrer">Open it in a new tab</a> <button class="subtle" id="bExtClose">dismiss</button>`;
  $("#bExtClose").onclick = () => box.classList.add("hidden");
}

window.addEventListener("message", (e) => {
  const fr = $("#pFrame");
  if (!previewOrigin || e.origin !== previewOrigin || e.source !== fr.contentWindow) return;
  const d = e.data || {};
  if (d.type === "hp-nav" && typeof d.href === "string") {
    try {
      const u = new URL(d.href);
      if (u.origin === previewOrigin) { lastNav = u.pathname + u.search + u.hash; if (document.activeElement !== $("#bAddr")) $("#bAddr").value = lastNav; }
    } catch (err) {}
  } else if (d.type === "hp-external" && typeof d.href === "string" && /^https?:\/\//i.test(d.href)) {
    showExternal(d.href);
  }
});

function frameCmd(cmd) {
  const w = $("#pFrame").contentWindow;
  if (w && previewOrigin) w.postMessage({ hpCmd: cmd }, previewOrigin);
}
$("#bBack").onclick = () => frameCmd("back");
$("#bFwd").onclick = () => frameCmd("forward");
$("#bReload").onclick = () => { navigate(currentPath()); };
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
document.querySelectorAll("[data-vp]").forEach(b => b.onclick = () => {
  document.querySelectorAll("[data-vp]").forEach(x => x.classList.toggle("primary", x === b));
  if (b.dataset.vp === "full") vp.full = true;
  else { const [w, h] = b.dataset.vp.split("x").map(Number); vp = { w, h, full: false }; }
  applyViewport();
});
$("#vpSet").onclick = () => {
  const w = Math.min(3840, Math.max(200, +$("#vpW").value || 1280)), h = Math.min(3000, Math.max(200, +$("#vpH").value || 800));
  vp = { w, h, full: false };
  document.querySelectorAll("[data-vp]").forEach(x => x.classList.remove("primary"));
  applyViewport();
};
$("#vpRotate").onclick = () => { if (!vp.full) { vp = { w: vp.h, h: vp.w, full: false }; applyViewport(); } };
$("#vpFit").onchange = applyViewport;
window.addEventListener("resize", () => { if (location.hash === "#testing") applyViewport(); });

// --- browser log ---
async function pollLog() {
  clearTimeout(logTimer);
  if (!test || !test.active) return;
  const id = test.id;
  try {
    const r = await api("GET", "/api/test/events?from=" + logFrom);
    if (!r.active || !test || r.id !== id) return;
    if (r.events.length) {
      logItems = logItems.concat(r.events).slice(-1000);
      logFrom = r.last;
      renderLog();
    }
    $("#logCounts").textContent = `${r.errors} error(s), ${r.warnings} warning(s)`;
  } catch (e) {}
  if (location.hash === "#testing") logTimer = setTimeout(pollLog, 1000);
}

function logFilter() { return document.querySelector("input[name=logf]:checked").value; }
document.querySelectorAll("input[name=logf]").forEach(r => r.onchange = renderLog);

function renderLog() {
  const all = logFilter() === "all";
  const box = $("#tLog");
  const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 20;
  const rows = logItems.filter(e => all || e.kind !== "request" || e.level !== "info").slice(-500);
  box.innerHTML = rows.map(e => {
    const time = new Date(e.time).toLocaleTimeString();
    let text;
    if (e.kind === "request") text = `${e.method} ${e.url} → ${e.status}${e.msg ? " " + e.msg : ""}`;
    else text = `[${e.kind}] ${e.msg}` + (e.url && e.kind !== "nav" ? `  @ ${e.url.replace(previewOrigin, "")}` : "");
    return `<div class="${esc(e.level)}">${esc(time)} ${esc(text)}${e.via ? `<span class="via">  (${esc(e.via)})</span>` : ""}${e.stack ? `<br><span class="via">${esc(e.stack)}</span>` : ""}</div>`;
  }).join("") || `<div class="info">No problems logged yet. Click around the site; errors appear here.</div>`;
  if (atBottom) box.scrollTop = box.scrollHeight;
}
$("#logClear").onclick = async () => { await api("POST", "/api/test/events/clear").catch(() => {}); logItems = []; renderLog(); };

$("#rNginx").onclick = (e) => withBtn(e.target, async () => {
  const pre = $("#tNginx");
  if (!pre.classList.contains("hidden")) { pre.classList.add("hidden"); return; }
  const r = await fetch("/api/test/nginx");
  pre.textContent = await r.text();
  pre.classList.remove("hidden");
});

// --- deploy the tested build ---
$("#tDeploy").onclick = (e) => withBtn(e.target, async () => {
  const slug = $("#tSite").value;
  const s = sites.find(x => x.slug === slug);
  if (!test || !test.active || !s) return;
  const fails = test.report.checks.filter(c => c.status === "fail");
  let accept = false;
  if (fails.length) {
    if (!confirm(`${fails.length} check(s) FAILED:\n\n- ${fails.map(f => f.title + ": " + f.summary).join("\n- ")}\n\nVisitors may see broken pages. Deploy to "${s.name}" anyway?`)) return;
    accept = true;
  } else if (!confirm(`Deploy the tested version (${test.report.files} files, fingerprint ${test.report.hash.slice(0, 12)}) to "${s.name}"${s.domain ? " (" + s.domain + ")" : ""}?`)) return;
  const r = await api("POST", "/api/test/deploy", { slug, test_id: test.id, accept_fails: accept });
  followJob(r.job, $("#tDeployLog"), (j) => { if (!j.error) toast("Deployed to " + s.name); loadSites(); });
});

window.addEventListener("load", applyViewport);

// ---------------- init ----------------
loadState().then(() => {
  if (!location.hash) location.hash = state.token_set ? "#sites" : "#setup";
  showTab();
}).catch(e => toast(e.message, true));
setInterval(() => loadState().catch(() => {}), 15000);
