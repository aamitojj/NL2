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
  const t = (location.hash || "#setup").slice(1);
  document.querySelectorAll(".tab").forEach(s => s.classList.toggle("hidden", s.id !== "tab-" + t));
  document.querySelectorAll("nav a").forEach(a => a.classList.toggle("active", a.dataset.tab === t));
  if (t === "sites") loadSites();
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

// ---------------- init ----------------
loadState().then(() => {
  if (!location.hash) location.hash = state.token_set ? "#sites" : "#setup";
  showTab();
}).catch(e => toast(e.message, true));
setInterval(() => loadState().catch(() => {}), 15000);
