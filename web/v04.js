"use strict";
// Dilreet 0.4: dashboard, organised projects list (groups, tags, pins,
// filters, bulk actions), templates, visitors & logs, backups & alerts.
// Loaded after app.js and reuses its helpers (api, route, modal, ...).
// Every action that changes the server or DNS asks first (confirmBox).

let orgCache = { groups: [], tags: [], palette: [], ungrouped: 0 };
async function loadOrg() {
  try { orgCache = await api("GET", "/api/org"); } catch (e) { /* keep the old one */ }
  drawNavGroups();
  return orgCache;
}
const tagColor = (t) => ((orgCache.tags || []).find(x => x.name === t) || {}).color || "gray";
function tagChips(tags, linkable) {
  return (tags || []).map(t => linkable ? `<a class="chip c-${esc(tagColor(t))}" href="#/projects?tag=${encodeURIComponent(t)}">${esc(t)}</a>` : `<span class="chip c-${esc(tagColor(t))}">${esc(t)}</span>`).join(" ");
}
function groupName(id) { const g = (orgCache.groups || []).find(x => x.id === id); return g ? g.name : ""; }
function drawNavGroups() {
  const el = $("#navGroups"); if (!el) return;
  const gs = orgCache.groups || [];
  el.innerHTML = gs.map(g => `<a href="#/projects/g/${esc(g.id)}" data-route="group-${esc(g.id)}"><span class="gdot c-${esc(g.color)}" aria-hidden="true"></span><span>${esc(g.name)}</span><small>${g.count || ""}</small></a>`).join("");
}
// Plain-language glossary: <abbr class="tip" title="..."> shown as a dotted word.
const GLOSSARY = {
  "deploy": "Putting a new version of your files online.",
  "DNS": "The internet's address book. It tells browsers which server a web address belongs to.",
  "HTTPS": "The padlock in the browser. It keeps visits private. Dilreet sets it up for you for free.",
  "server": "The computer on the internet (your VPS) that shows your sites to visitors.",
  "rollback": "Going back to an older version of your site with one click.",
  "framework": "The tool the site was built with, like React or Vite. Plain HTML needs none.",
};
function tip(word, label) { return `<abbr class="tip" tabindex="0" title="${esc(GLOSSARY[word] || "")}">${esc(label || word)}</abbr>`; }

// Fix-it buttons from /api/dashboard. Server-changing fixes ask first.
async function runFix(a, btn) {
  const f = a.fix; if (!f) return;
  if (f.kind === "href") return go(f.href);
  if (f.kind === "reconnect") return go("#/settings/server");
  if (f.confirm && !await confirmBox(f.label + "?", `<p><b>${esc(a.name || "")}</b>: ${esc(a.text)}</p><p class="small muted">This changes something on your server. Nothing happens until you click the button below.</p>`, f.label)) return;
  return busy(btn, async () => {
    if (f.kind === "restart") { await api("POST", `/api/sites/${a.slug}/restart`); toast("Restart started for " + (a.name || a.slug), "success"); }
    else if (f.kind === "health") { await api("POST", "/api/health/run", { slugs: a.slug ? [a.slug] : [] }); toast("Checked again", "success"); }
    else if (f.kind === "backup") { const j = await api("POST", "/api/backup/run"); toast("Backup started", "success"); void j; }
    else if (f.kind === "pull") { await pullDeploy(a.slug, false, $("#dJob")); }
    later(rerender, 900);
  });
}

// ---------- dashboard ----------
route(/^\/dashboard$/, async () => {
  await refreshOverview();
  await loadOrg();
  const v = view();
  v.innerHTML = `<div class="head"><h1>Dashboard</h1><span class="grow"></span><a class="btn primary" href="#/new">+ Add new project</a></div><div class="muted">Loading&hellip;</div>`;
  let d;
  try { d = await api("GET", "/api/dashboard"); } catch (e) { v.innerHTML = `<div class="alert bad">${esc(e.message)}</div>`; return; }
  const c = d.counts || {};
  const done = (d.checklist || []).filter(x => x.done).length, all = (d.checklist || []).length;
  const showList = !d.setup_hidden && done < all;
  const lvl = { fail: "bad", warn: "warn", info: "info" };
  v.innerHTML = `<div class="head"><div><h1 style="margin:0">Dashboard</h1><p class="muted" style="margin:4px 0 0">Everything about your sites, in one place.</p></div><span class="grow"></span><a class="btn" href="#/new/template">Start from a template</a><a class="btn primary" href="#/new">+ Add new project</a></div>
    <div class="stats">
      <a class="stat" href="#/projects"><b>${c.total || 0}</b><span>Projects</span></a>
      <a class="stat ok" href="#/projects?status=live"><b>${c.live || 0}</b><span>Online</span></a>
      <a class="stat ${c.down ? "bad" : ""}" href="#/projects?status=down"><b>${c.down || 0}</b><span>Need help</span></a>
      <a class="stat" href="#/domains"><b>${c.domains || 0}</b><span>Own web addresses</span></a>
    </div>
    ${showList ? `<div class="card checklist"><div class="card-head"><h2>Getting started</h2><span class="badge accent">${done} of ${all} done</span><span class="grow"></span><button class="ghost sm" id="hideSetup">Hide this</button></div>
      <div class="progress"><div style="width:${Math.round(done / Math.max(all, 1) * 100)}%"></div></div>
      <ol class="todo">${d.checklist.map(x => `<li class="${x.done ? "done" : ""}"><span class="tick" aria-hidden="true">${x.done ? "&#10004;" : ""}</span><span class="grow"><b>${esc(x.title)}</b>${x.optional && !/optional/i.test(x.title) ? ` <span class="muted small">(optional)</span>` : ""}<div class="small muted">${esc(x.text)}</div></span>${x.done ? "" : `<a class="btn sm" href="${esc(x.href)}">Do it</a>`}</li>`).join("")}</ol></div>` : ""}
    <div class="grid2">
      <div class="card"><div class="card-head"><h2>Needs your attention</h2><span class="grow"></span><button class="sm ghost" id="checkAll" title="Open every site now and check it answers">Check all sites now</button></div>
        ${(d.attention || []).length ? `<ul class="attn">${d.attention.map((a, i) => `<li class="lv-${esc(a.level)}"><span class="badge ${lvl[a.level] || ""}">${a.level === "fail" ? "Problem" : a.level === "warn" ? "Check" : "Tip"}</span><span class="grow"><b>${a.name ? `<a href="#/project/${esc(a.slug)}">${esc(a.name)}</a>: ` : ""}${esc(a.title)}</b><div class="small muted">${esc(a.text)}</div></span>${a.fix ? `<button class="sm ${a.level === "fail" ? "primary" : ""}" data-fix="${i}">${esc(a.fix.label)}</button>` : ""}</li>`).join("")}</ul>` : `<div class="empty small">&#127881; All good. Nothing needs your attention.</div>`}
        <div id="dJob"></div></div>
      <div class="card"><div class="card-head"><h2>&#9733; Pinned projects</h2></div>
        ${(d.pinned || []).length ? `<ul class="deploys">${d.pinned.map(p => `<li>${thumb(p.name)}<span class="grow"><a href="#/project/${esc(p.slug)}"><b>${esc(p.name)}</b></a><div class="small muted">${esc(p.domain || (p.instant_url || "").replace(/^https?:\/\//, "") || "no address yet")}</div></span>${statusBadge(p)}</li>`).join("")}</ul>` : `<div class="empty small">Click the &#9734; star on a project to keep it here.</div>`}</div>
    </div>
    <div class="card"><div class="card-head"><h2>Recent deploys</h2><span class="grow"></span><a class="small" href="#/deploys">See all &rarr;</a></div>
      ${(d.recent || []).length ? `<ul class="deploys">${d.recent.map(r => `<li><span class="grow"><a href="#/project/${esc(r.slug)}/deploys"><b>${esc(r.name)}</b></a> ${r.live ? `<span class="badge ok">Live</span>` : ""}${r.status === "failed" ? `<span class="badge bad">failed</span>` : ""}<div class="small muted">${esc(ago(r.time))} &middot; ${esc(srcLabel[r.source] || r.source || "")} &middot; ${r.files || 0} files</div></span></li>`).join("")}</ul>` : `<div class="empty small">No deploys yet. <a href="#/new">Publish your first site</a>.</div>`}</div>
    <div class="grid2">
      <div class="card"><h2>Backups</h2>${(d.backup || {}).every && d.backup.every !== "off" ? `<p>Automatic backup: <b>${esc(d.backup.every)}</b>. Last: ${esc(ago(d.backup.last))}</p>` : `<p class="muted">Automatic backups are off.</p>`}<a class="btn sm" href="#/settings/backups">Backup settings</a></div>
      <div class="card"><h2>Help</h2><p class="small">New here? Read the short <a href="#/guide">guide</a>, or ask <a href="#/jiv">Jiv</a>, the built-in assistant, in plain words.</p></div>
    </div>`;
  if ($("#hideSetup")) $("#hideSetup").onclick = async () => { await api("POST", "/api/setup/hide", { hidden: true }).catch(err); rerender(); };
  $("#checkAll").onclick = (e) => busy(e.target, async () => { await api("POST", "/api/health/run", {}); toast("All sites checked", "success"); rerender(); });
  $$("[data-fix]", v).forEach(b => b.onclick = () => runFix(d.attention[+b.dataset.fix], b));
}, "dashboard");

// ---------- projects list ----------
function projState(s) {
  if (!s.last_deploy) return "new";
  return s.state || "unknown";
}
const STATE_LABEL = { live: "Online", down: "Needs help", busy: "Working", unknown: "Unknown", new: "Not online yet" };
route(/^\/projects(?:\/g\/([^/?]+))?(?:\?(.*))?$/, async (gid, qs) => {
  await refreshOverview();
  await Promise.all([loadSites(), loadOrg()]);
  const q = new URLSearchParams(qs || "");
  const pref = store("plist") || {};
  const f = { text: q.get("q") || "", status: q.get("status") || "", tag: q.get("tag") || "", fw: "", sort: pref.sort || "pinned", view: pref.view || "grid", archived: q.get("archived") === "1", group: gid || "" };
  const sel = new Set();
  const v = view();
  const g = gid ? (orgCache.groups || []).find(x => x.id === gid) : null;
  const fws = [...new Set(sites.map(s => s.framework || "Plain HTML"))].sort();
  v.innerHTML = `<div class="crumbs">${g ? `<a href="#/projects">Projects</a> /` : ""}</div>
    <div class="head"><h1>${g ? `<span class="gdot c-${esc(g.color)}"></span> ${esc(g.name)}` : "Projects"}</h1><span class="grow"></span>
      <a class="btn" href="#/new/template">Use a template</a><a class="btn primary" href="#/new">+ Add new project</a></div>
    ${sitesErr ? `<div class="alert warn small">Live status unavailable: ${esc(sitesErr)}</div>` : ""}
    <div class="gtabs" role="tablist" aria-label="Groups"><a href="#/projects" ${!gid ? 'aria-current="page"' : ""}>All</a>${(orgCache.groups || []).map(x => `<a href="#/projects/g/${esc(x.id)}" ${x.id === gid ? 'aria-current="page"' : ""}><span class="gdot c-${esc(x.color)}"></span>${esc(x.name)} <small>${x.count}</small></a>`).join("")}<a href="#/projects/g/none" ${gid === "none" ? 'aria-current="page"' : ""}>No group <small>${orgCache.ungrouped || 0}</small></a><button class="linkish small" id="newGroup">+ New group</button></div>
    <div class="toolbar">
      <input type="search" id="fq" placeholder="Search by name, address, tag or note" aria-label="Search projects" value="${esc(f.text)}">
      <select id="fs" aria-label="Status"><option value="">Any status</option>${Object.entries(STATE_LABEL).map(([k, l]) => `<option value="${k}" ${f.status === k ? "selected" : ""}>${l}</option>`).join("")}</select>
      <select id="ft" aria-label="Tag"><option value="">Any tag</option>${(orgCache.tags || []).map(t => `<option ${f.tag === t.name ? "selected" : ""}>${esc(t.name)}</option>`).join("")}</select>
      <select id="ff" aria-label="Built with"><option value="">Built with: any</option>${fws.map(x => `<option>${esc(x)}</option>`).join("")}</select>
      <select id="fo" aria-label="Sort"><option value="pinned">Sort: pinned first</option><option value="name">Sort: name A&ndash;Z</option><option value="recent">Sort: last deploy</option><option value="status">Sort: problems first</option></select>
      <label class="check small" style="margin:0"><input type="checkbox" id="fa" ${f.archived ? "checked" : ""}> Show archived</label>
      <span class="grow"></span>
      <div class="seg" role="group" aria-label="View"><button data-v="grid" aria-pressed="${f.view === "grid"}" title="Cards">&#9638;</button><button data-v="list" aria-pressed="${f.view === "list"}" title="List">&#9776;</button></div>
    </div>
    <div id="bulk" class="bulkbar hidden" role="region" aria-label="Selected projects"></div>
    <div id="plist"></div>`;
  $("#fo").value = f.sort;
  const filtered = () => {
    const t = f.text.toLowerCase();
    let l = sites.filter(s => (f.archived ? s.archived : !s.archived)
      && (!f.group || (f.group === "none" ? !s.group : s.group === f.group))
      && (!f.status || projState(s) === f.status)
      && (!f.tag || (s.tags || []).includes(f.tag))
      && (!f.fw || (s.framework || "Plain HTML") === f.fw)
      && (!t || [s.name, s.slug, s.domain, s.instant_url, s.notes, ...(s.tags || [])].join(" ").toLowerCase().includes(t)));
    const rank = { down: 0, unknown: 1, busy: 2, new: 3, live: 4 };
    const by = { name: (a, b) => a.name.localeCompare(b.name), recent: (a, b) => String(b.last_deploy || "").localeCompare(String(a.last_deploy || "")), status: (a, b) => rank[projState(a)] - rank[projState(b)] || a.name.localeCompare(b.name), pinned: (a, b) => (b.pinned ? 1 : 0) - (a.pinned ? 1 : 0) || a.name.localeCompare(b.name) };
    return l.sort(by[f.sort] || by.pinned);
  };
  const card = (s) => {
    const url = siteURL(s), st = projState(s);
    return `<div class="pcard ${sel.has(s.slug) ? "sel" : ""}" data-slug="${esc(s.slug)}">
      <label class="pick" title="Select"><input type="checkbox" data-sel="${esc(s.slug)}" ${sel.has(s.slug) ? "checked" : ""} aria-label="Select ${esc(s.name)}"></label>
      <button class="star ${s.pinned ? "on" : ""}" data-pin="${esc(s.slug)}" title="${s.pinned ? "Unpin" : "Pin"}" aria-pressed="${!!s.pinned}">${s.pinned ? "&#9733;" : "&#9734;"}</button>
      <a class="pbody" href="#/project/${esc(s.slug)}">${thumb(s.name)}<span class="grow" style="min-width:0"><b class="pname">${esc(s.name)}</b><span class="small muted ell">${esc(url ? url.replace(/^https?:\/\//, "") : "no address yet")}</span></span></a>
      <div class="pmeta"><span class="sdot st-${st}" aria-hidden="true"></span><span class="small">${STATE_LABEL[st]}</span>${s.group && !f.group ? `<span class="small muted">&middot; ${esc(groupName(s.group))}</span>` : ""}<span class="grow"></span><span class="small muted">${s.last_deploy ? "updated " + esc(ago(s.last_deploy)) : ""}</span></div>
      <div class="ptags">${tagChips(s.tags, true)}${s.framework ? `<span class="chip ghost">${esc(s.framework)}</span>` : ""}</div></div>`;
  };
  const draw = () => {
    const l = filtered();
    const el = $("#plist");
    if (!sites.length) { el.innerHTML = `<div class="empty big-empty"><div class="emoji">&#128640;</div><h2>No projects yet</h2><p>Drag a folder with your website onto Dilreet, pick a ready-made template, or import from GitHub or Netlify.</p><div class="row center"><a class="btn primary lg" href="#/new">Add your first project</a><a class="btn lg" href="#/new/template">Start from a template</a></div></div>`; return; }
    if (!l.length) { el.innerHTML = `<div class="empty">No projects match. <button class="linkish" id="clearF">Clear the filters</button></div>`; $("#clearF").onclick = () => go("#/projects"); return; }
    el.className = f.view === "list" ? "plist list" : "plist grid";
    el.innerHTML = l.map(card).join("");
    drawBulk();
  };
  const drawBulk = () => {
    const b = $("#bulk");
    b.classList.toggle("hidden", !sel.size);
    if (!sel.size) return;
    b.innerHTML = `<b>${sel.size} selected</b><button class="sm" data-b="pin">&#9733; Pin</button><button class="sm" data-b="unpin">Unpin</button>
      <select class="sm" data-b="group" aria-label="Move to group"><option value="">Move to group&hellip;</option>${(orgCache.groups || []).map(x => `<option value="${esc(x.id)}">${esc(x.name)}</option>`).join("")}<option value="-">No group</option></select>
      <button class="sm" data-b="tag">+ Add tag</button><button class="sm" data-b="health">Check now</button><button class="sm primary" data-b="redeploy">Redeploy</button>
      <button class="sm" data-b="${f.archived ? "unarchive" : "archive"}">${f.archived ? "Restore" : "Archive"}</button><span class="grow"></span><button class="sm ghost" data-b="none">Clear</button><div id="bJobs" style="flex-basis:100%"></div>`;
  };
  const bulk = async (action, value, btn) => {
    const slugs = [...sel];
    if (action === "redeploy" && !await confirmBox(`Redeploy ${slugs.length} project(s)?`, `<p>The files saved on this PC for each project are published again to your server. Sites stay online while this runs.</p>`, "Redeploy")) return;
    return busy(btn, async () => {
      const r = await api("POST", "/api/bulk", { action, slugs, value: value || "" });
      if (r.job) { await jobPanel(r.job, $("#bJobs"), { title: "Redeploying", doneTitle: "All redeployed" }); }
      else if (action === "health") toast(`Checked ${slugs.length} site(s)`, "success");
      else toast(`Updated ${r.changed ?? slugs.length} project(s)`, "success");
      if (!r.job) { sel.clear(); await Promise.all([loadSites(), loadOrg()]); draw(); }
    });
  };
  $("#bulk").onclick = async (e) => {
    const b = e.target.closest("button[data-b]"); if (!b) return;
    const a = b.dataset.b;
    if (a === "none") { sel.clear(); draw(); return; }
    if (a === "tag") { const t = await askText("Add a tag", "For example: client, shop, personal.", ""); if (t) bulk("tag", t, b); return; }
    bulk(a, "", b);
  };
  $("#bulk").onchange = (e) => { if (e.target.dataset.b === "group" && e.target.value) bulk("group", e.target.value === "-" ? "" : e.target.value, null); };
  $("#plist").onchange = (e) => { const s = e.target.dataset.sel; if (!s) return; e.target.checked ? sel.add(s) : sel.delete(s); e.target.closest(".pcard").classList.toggle("sel", e.target.checked); drawBulk(); };
  $("#fq").oninput = (e) => { f.text = e.target.value; draw(); };
  $("#fs").onchange = (e) => { f.status = e.target.value; draw(); };
  $("#ft").onchange = (e) => { f.tag = e.target.value; draw(); };
  $("#ff").onchange = (e) => { f.fw = e.target.value; draw(); };
  $("#fo").onchange = (e) => { f.sort = e.target.value; store("plist", { sort: f.sort, view: f.view }); draw(); };
  $("#fa").onchange = (e) => { f.archived = e.target.checked; sel.clear(); draw(); };
  $$(".seg button", v).forEach(b => b.onclick = () => { f.view = b.dataset.v; store("plist", { sort: f.sort, view: f.view }); $$(".seg button", v).forEach(x => x.setAttribute("aria-pressed", x === b)); draw(); });
  $("#newGroup").onclick = () => editGroup(null);
  draw();
}, "projects");

// Pin star works everywhere (list and project header).
document.addEventListener("click", async (e) => {
  const b = e.target.closest("[data-pin]"); if (!b) return;
  e.preventDefault();
  const s = sites.find(x => x.slug === b.dataset.pin); if (!s) return;
  try { await api("POST", `/api/sites/${s.slug}/meta`, { pinned: !s.pinned }); s.pinned = !s.pinned; b.classList.toggle("on", s.pinned); b.innerHTML = s.pinned ? "&#9733;" : "&#9734;"; b.setAttribute("aria-pressed", s.pinned); toast(s.pinned ? "Pinned" : "Unpinned", "success"); } catch (x) { err(x); }
});
document.addEventListener("click", (e) => { const a = e.target.closest("[data-testsite]"); if (a) store("testSite", a.dataset.testsite); });

async function editGroup(g) {
  const pal = orgCache.palette && orgCache.palette.length ? orgCache.palette : ["blue", "green", "orange", "purple", "pink", "teal", "red", "gray"];
  const r = await modal(`<h2>${g ? "Edit group" : "New group"}</h2><p class="small muted">Groups keep projects apart, for example one group per client or per business.</p>
    <div class="field"><label for="gN">Name</label><input id="gN" value="${esc(g ? g.name : "")}" maxlength="40" autofocus placeholder="e.g. Clients"></div>
    <div class="field"><label>Colour</label><div class="row">${pal.map((c, i) => `<label class="swatch c-${c}" title="${c}"><input type="radio" name="gC" value="${c}" ${(g ? g.color === c : i === 0) ? "checked" : ""}><span></span></label>`).join("")}</div></div>
    <div class="row end">${g ? `<button class="danger" id="gDel">Delete group</button><span class="grow"></span>` : ""}<button data-close="">Cancel</button><button class="primary" id="gOk">Save</button></div>`, (m, close) => {
    $("#gOk", m).onclick = () => close({ name: $("#gN", m).value.trim(), color: ($("input[name=gC]:checked", m) || {}).value });
    if ($("#gDel", m)) $("#gDel", m).onclick = () => close({ delete: true });
  });
  if (!r) return;
  if (r.delete && !await confirmBox("Delete group " + g.name + "?", "<p>The projects stay. They just move to “No group”.</p>", "Delete group", true)) return;
  if (!r.delete && !r.name) return toast("Give the group a name", "warn");
  try { await api("POST", "/api/org/groups", Object.assign({ id: g ? g.id : "" }, r)); toast("Saved", "success"); await loadOrg(); rerender(); } catch (x) { err(x); }
}

// ---------- project: organise card (in Settings) ----------
function projectOrganise(s, el) {
  el.innerHTML = `<div class="card"><h2>Organise</h2>
    <div class="grid2"><div class="field"><label for="oG">Group</label><select id="oG"><option value="">No group</option>${(orgCache.groups || []).map(g => `<option value="${esc(g.id)}" ${s.group === g.id ? "selected" : ""}>${esc(g.name)}</option>`).join("")}</select></div>
      <div class="field"><label for="oT">Tags <span class="muted small">(comma separated)</span></label><input id="oT" value="${esc((s.tags || []).join(", "))}" placeholder="client, shop" list="oTL"><datalist id="oTL">${(orgCache.tags || []).map(t => `<option value="${esc(t.name)}">`).join("")}</datalist></div></div>
    <div class="field"><label for="oN">Notes <span class="muted small">(only you see these)</span></label><textarea id="oN" rows="2" maxlength="2000" placeholder="Who is the client, login hints, what to change next…">${esc(s.notes || "")}</textarea></div>
    <div class="row"><button id="oSave" class="primary">Save</button><button id="oPin">${s.pinned ? "Unpin" : "&#9733; Pin to dashboard"}</button><button id="oDup" title="Make a new project with a copy of these files">Duplicate</button><button id="oArc">${s.archived ? "Restore from archive" : "Archive"}</button></div>
    <p class="hint">Archive hides a project from your lists. The site keeps running.</p></div>`;
  const meta = (body, msg) => api("POST", `/api/sites/${s.slug}/meta`, body).then(async () => { toast(msg || "Saved", "success"); await loadSites(); await loadOrg(); rerender(); }).catch(err);
  $("#oSave").onclick = (e) => busy(e.target, () => meta({ group: $("#oG").value, tags: $("#oT").value.split(",").map(x => x.trim()).filter(Boolean), notes: $("#oN").value }));
  $("#oPin").onclick = (e) => busy(e.target, () => meta({ pinned: !s.pinned }, s.pinned ? "Unpinned" : "Pinned"));
  $("#oArc").onclick = (e) => busy(e.target, () => meta({ archived: !s.archived }, s.archived ? "Restored" : "Archived. The site keeps running."));
  $("#oDup").onclick = (e) => busy(e.target, async () => {
    const r = await api("POST", `/api/sites/${s.slug}/duplicate`);
    toast("Copy ready. Choose a name and publish it.", "success");
    openReview(r, {});
  });
}
function reviewOrgFields(s) {
  const gs = orgCache.groups || [];
  return `${s.template ? `<div class="alert info small">Made from the <b>${esc(s.template)}</b> template. You can change the text later in the Files tab.</div>` : ""}${s.copy_of ? `<div class="alert info small">This is a copy of <b>${esc(s.copy_of)}</b>.</div>` : ""}
    ${gs.length ? `<div class="field"><label for="pGroup">Group</label><select id="pGroup"><option value="">No group</option>${gs.map(g => `<option value="${esc(g.id)}" ${s.group === g.id ? "selected" : ""}>${esc(g.name)}</option>`).join("")}</select></div>` : ""}`;
}

// ---------- project: health card (Overview) ----------
async function projectHealthCard(s, el) {
  const r = await api("GET", "/api/health").catch(() => ({ results: {} }));
  const h = (r.results || {})[s.slug];
  const draw = (h) => {
    el.innerHTML = `<div class="card hcard"><div class="row"><span class="sdot st-${h ? (h.ok ? "live" : "down") : "unknown"}"></span><b>${h ? (h.ok ? "Your site is answering" : "Your site is not answering") : "Not checked yet"}</b>
      ${h ? `<span class="small muted">${h.ok ? `${h.ms} ms` : esc(h.error || "")} &middot; checked ${esc(ago(h.at))}</span>` : ""}
      ${h && h.cert_days != null ? `<span class="badge ${h.cert_days <= 14 ? "warn" : "ok"}">&#128274; ${tip("HTTPS", "padlock")} valid ${h.cert_days} more days</span>` : ""}<span class="grow"></span><button class="sm" id="hNow">Check now</button></div></div>`;
    $("#hNow").onclick = (e) => busy(e.target, async () => { const x = await api("POST", "/api/health/run", { slugs: [s.slug] }); draw((x.results || {})[s.slug]); });
  };
  draw(h);
}

// ---------- project: visitors & logs ----------
async function projectLogs(s, b) {
  b.innerHTML = `<div class="card"><div class="card-head"><h2>Visitors &amp; logs</h2><span class="grow"></span><button id="lR" class="sm">Refresh</button></div><p class="small muted">Read from your site's web server on the ${tip("server")} (last 14 days, up to 20,000 visits). No tracking code and no cookies are added to your site.</p><div id="lBody" class="muted">Loading&hellip;</div></div>`;
  const load = async () => {
    const el = $("#lBody");
    el.innerHTML = `<span class="spin"></span> Reading the log&hellip;`;
    let t;
    try { t = await api("GET", `/api/sites/${s.slug}/logs`); } catch (e) { el.innerHTML = `<div class="alert warn">${esc(e.message)}</div>`; return; }
    const days = t.days || [], mx = Math.max(1, ...days.map(d => d.n));
    const list = (title, arr, empty) => `<div class="card flat"><h3>${title}</h3>${(arr || []).length ? `<ol class="top">${arr.map(x => `<li><span class="grow ell"><code>${esc(x.key)}</code></span><b>${x.n}</b></li>`).join("")}</ol>` : `<p class="small muted">${empty}</p>`}</div>`;
    const st = t.status || {};
    el.innerHTML = `<div class="stats"><div class="stat"><b>${t.page_views}</b><span>Page views</span></div><div class="stat"><b>${t.visitors}</b><span>Visitors (about)</span></div><div class="stat ${st["5xx"] ? "bad" : ""}"><b>${(st["4xx"] || 0) + (st["5xx"] || 0)}</b><span>Errors</span></div><div class="stat"><b>${t.bots}</b><span>Bots</span></div></div>
      <div class="card flat"><h3>Page views per day</h3><div class="bars" role="img" aria-label="Page views per day">${days.map(d => `<div class="bar" title="${esc(d.key)}: ${d.n}"><i style="height:${Math.round(d.n / mx * 100)}%"></i><small>${esc(d.key.slice(5))}</small></div>`).join("")}</div></div>
      <div class="grid3">${list("Top pages", t.top_pages, "No visits yet.")}${list("Missing pages (404)", t.not_found, "No broken links found. &#127881;")}${list("Visitors came from", t.referrers, "No other websites sent visitors yet.")}</div>
      <details class="card flat"><summary><b>Raw log</b> <span class="small muted">(newest last)</span></summary><pre class="code log">${esc((t.recent || []).join("\n"))}</pre></details>
      ${(t.errors || []).length ? `<details class="card flat"><summary><b>Server error messages</b></summary><pre class="code log">${esc(t.errors.join("\n"))}</pre></details>` : ""}`;
  };
  $("#lR").onclick = (e) => busy(e.target, load);
  load();
}

// ---------- new project: templates ----------
route(/^\/new\/template$/, async () => {
  await refreshOverview(); await loadOrg();
  const v = view();
  v.innerHTML = `<div class="crumbs"><a href="#/projects">Projects</a> / <a href="#/new">Add new project</a> /</div><div class="head"><h1>Start from a template</h1></div>
    <p class="muted">Pick a ready-made site. Type your name, look at the preview, and publish. You can change the text later.</p>
    <div class="field" style="max-width:420px"><label for="tName">Site name</label><input id="tName" value="${esc(store("tplName") || "")}" placeholder="e.g. Sunny Bakery" maxlength="60"></div>
    <div id="tpls" class="tgrid"><p class="muted">Loading&hellip;</p></div>`;
  let list = [];
  try { list = (await api("GET", "/api/templates")).templates || []; } catch (e) { $("#tpls").innerHTML = `<div class="alert bad">${esc(e.message)}</div>`; return; }
  const nm = () => $("#tName").value.trim() || "My Site";
  const draw = () => {
    $("#tpls").innerHTML = list.map(t => `<div class="tcard"><div class="tframe"><iframe loading="lazy" sandbox tabindex="-1" title="Preview of ${esc(t.name)}" src="/api/templates/${esc(t.id)}/preview/index.html?name=${encodeURIComponent(nm())}"></iframe></div>
      <div class="tinfo"><b>${esc(t.name)}</b><p class="small muted">${esc(t.desc)}</p><div class="row"><button class="primary sm" data-use="${esc(t.id)}">Use this</button><a class="btn sm ghost" target="_blank" rel="noopener" href="/api/templates/${esc(t.id)}/preview/index.html?name=${encodeURIComponent(nm())}">Full preview</a></div></div></div>`).join("");
  };
  draw();
  let tmr; $("#tName").oninput = () => { store("tplName", $("#tName").value); clearTimeout(tmr); tmr = setTimeout(draw, 500); };
  $("#tpls").onclick = (e) => { const b = e.target.closest("[data-use]"); if (!b) return; busy(b, async () => { const r = await api("POST", `/api/templates/${b.dataset.use}/use`, { name: nm() }); r.suggested_name = r.suggested_name || nm(); openReview(r, {}); }); };
}, "projects");
// Add the Templates choice to the "Add new project" page.
const _origNew = routes.find(r => String(r.re).startsWith("/^\\/new"));
if (_origNew) {
  const fn = _origNew.fn;
  _origNew.fn = async (tab) => {
    await fn(tab);
    const ch = $(".choices", view());
    if (ch) ch.insertAdjacentHTML("afterbegin", `<a class="choice" href="#/new/template"><span class="ic" aria-hidden="true">&#10024;</span><b>Start from a template</b><small>A ready-made site: business, portfolio, link page, menu, event&hellip;</small></a>`);
  };
}

// ---------- settings: groups & tags ----------
async function setOrganise(el) {
  await loadOrg();
  const gs = orgCache.groups || [], ts = orgCache.tags || [];
  el.innerHTML = `<div class="card"><div class="card-head"><h2>Groups</h2><span class="grow"></span><button id="ogNew" class="primary sm">+ New group</button></div><p class="small muted">Use groups for clients, businesses or anything else. Each group shows in the left menu.</p>
    ${gs.length ? `<ul class="deploys">${gs.map(g => `<li><span class="gdot c-${esc(g.color)}"></span><span class="grow"><b>${esc(g.name)}</b> <span class="small muted">${g.count} project(s)</span></span><a class="btn sm ghost" href="#/projects/g/${esc(g.id)}">Open</a><button class="sm" data-g="${esc(g.id)}">Edit</button></li>`).join("")}</ul>` : `<div class="empty small">No groups yet.</div>`}</div>
    <div class="card"><h2>Tags</h2><p class="small muted">Tags are small labels like “client” or “shop”. Add them in a project's Settings, or select many projects and click “Add tag”.</p>
    ${ts.length ? `<ul class="deploys">${ts.map(t => `<li><span class="chip c-${esc(t.color)}">${esc(t.name)}</span><span class="grow small muted">${t.count} project(s)</span><select class="sm" data-tc="${esc(t.name)}" aria-label="Colour">${(orgCache.palette || []).map(c => `<option ${c === t.color ? "selected" : ""}>${c}</option>`).join("")}</select><button class="sm" data-tr="${esc(t.name)}">Rename</button><button class="sm ghost danger" data-td="${esc(t.name)}">Remove</button></li>`).join("")}</ul>` : `<div class="empty small">No tags yet.</div>`}</div>`;
  $("#ogNew").onclick = () => editGroup(null);
  el.onclick = async (e) => {
    const b = e.target.closest("button"); if (!b) return;
    if (b.dataset.g) return editGroup(gs.find(x => x.id === b.dataset.g));
    if (b.dataset.tr) { const n = await askText("Rename tag", "New name", b.dataset.tr); if (n) api("POST", "/api/org/tags", { name: b.dataset.tr, rename: n }).then(rerender, err); }
    if (b.dataset.td && await confirmBox("Remove tag " + b.dataset.td + "?", "<p>The tag is taken off every project. The projects stay.</p>", "Remove", true)) api("POST", "/api/org/tags", { name: b.dataset.td, delete: true }).then(rerender, err);
  };
  el.onchange = (e) => { const t = e.target.dataset.tc; if (t) api("POST", "/api/org/tags", { name: t, color: e.target.value }).then(() => toast("Saved", "success"), err); };
}

// ---------- settings: backups & alerts ----------
async function setBackups(el) {
  let r;
  try { r = await api("GET", "/api/backup"); } catch (e) { el.innerHTML = `<div class="alert bad">${esc(e.message)}</div>`; return; }
  const b = r.settings || {};
  el.innerHTML = `<div class="card"><h2>Automatic backups</h2><p class="small muted">Dilreet saves a .zip of all your sites' files on this PC. Passwords and keys are never put in a backup.</p>
    <div class="grid2"><div class="field"><label for="bE">How often</label><select id="bE">${[["off", "Off"], ["daily", "Every day"], ["weekly", "Every week"]].map(([k, l]) => `<option value="${k}" ${b.every === k ? "selected" : ""}>${l}</option>`).join("")}</select></div>
    <div class="field"><label for="bK">Keep the last</label><input id="bK" type="number" min="1" max="100" value="${esc(b.keep || 8)}"></div></div>
    <div class="field"><label for="bD">Save in this folder</label><input id="bD" value="${esc(b.dir || "")}"></div>
    <label class="check"><input type="checkbox" id="bI" ${b.include_deploys ? "checked" : ""}> Also include every saved deploy <span class="muted small">(bigger files)</span></label>
    <p class="small">Last backup: <b>${esc(ago(b.last))}</b>${b.last_file ? ` &middot; <code>${esc(b.last_file)}</code> (${esc(humanSize(b.last_size || 0))})` : ""}</p>${b.last_error ? `<div class="alert bad small">${esc(b.last_error)}</div>` : ""}
    <div class="row"><button id="bS" class="primary">Save</button><button id="bN">Back up now</button></div><div id="bJob"></div>
    ${(r.backups || []).length ? `<h3>Saved backups</h3><ul class="deploys">${r.backups.map(x => `<li><span class="grow"><code>${esc(x.name)}</code><div class="small muted">${esc(fmtTime(x.time))} &middot; ${esc(humanSize(x.size))}</div></span></li>`).join("")}</ul>` : ""}</div>
    <div class="card"><h2>Alerts</h2><p class="small muted">Every 10 minutes (when background checks are on) Dilreet opens each site and tells you in Notifications if it does not answer.</p>
    <div class="field" style="max-width:320px"><label for="bW">Warn me when the ${tip("HTTPS", "padlock certificate")} ends within</label><div class="row"><input id="bW" type="number" min="1" max="60" value="${esc(r.ssl_warn_days || 14)}" style="width:90px"> days</div></div>
    <button id="bWS">Save</button></div>`;
  const save = () => api("POST", "/api/backup/settings", { every: $("#bE").value, dir: $("#bD").value.trim(), keep: +$("#bK").value, include_deploys: $("#bI").checked, ssl_warn_days: +$("#bW").value });
  $("#bS").onclick = (e) => busy(e.target, async () => { await save(); toast("Saved", "success"); });
  $("#bWS").onclick = (e) => busy(e.target, async () => { await save(); toast("Saved", "success"); });
  $("#bN").onclick = (e) => busy(e.target, async () => { const j = await api("POST", "/api/backup/run"); const x = await jobPanel(j.job, $("#bJob"), { title: "Backing up", doneTitle: "Backup saved" }); if (!x.error) later(rerender, 1200); });
}

loadOrg();
