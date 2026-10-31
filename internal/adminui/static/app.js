"use strict";
// ZAKS Drive admin UI. No framework, no external requests. Every element is
// built with DOM calls and text nodes, never by parsing markup, so text that comes
// from the server (repository names, error messages, log lines) can only ever
// be shown as text.

// All user-visible strings live here so a translation only touches this table.
const T = {
  login: "Log in", token: "Admin token", tokenHint: "The ADMIN_TOKEN from the server's .env file. It is exchanged once for a short session and is not stored in this browser.",
  overview: "Overview", repositories: "Repositories", certificates: "SAP certificates", diagnostics: "Diagnostics", logs: "Logs", audit: "Audit", settings: "Settings",
  pending: "Repository changes are waiting. Applying restarts the server: new requests get 503 with Retry-After (SAP retries), running requests finish first.",
  afterRestart: "The server restarted to apply your change. Sessions end with a restart, so please log in again.",
  apply: "Apply and restart", restarting: "Restarting... this page reconnects by itself.", applied: "Applied.",
  fallback: "The server is running the LAST WORKING repository state because the current one is invalid:",
  confirmApply: "Restart the server to apply the pending changes?\nNew requests get 503 with Retry-After while running requests finish.",
  confirmDetach: "Detach this repository? It stops being served. Documents are NOT deleted.",
  confirmDetachSAP: "This repository is exposed to SAP. SAP keeps references to its documents and gets errors until it is served again. Detach anyway?",
  secretHint: "Write-only. Leave empty to keep the stored secret.",
  purgeNote: "Deleting documents (purge) is not available here, by design. It exists only in the zaks-drive command on the Docker host.",
};

const $ = (id) => document.getElementById(id);
let csrf = "";
let currentView = "overview";

// el builds an element: el("p", {class: "x", onclick: fn}, "text", child, ...)
function el(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k === "class") e.className = v;
    else if (k === "value") e.value = v;
    else if (k === "checked") e.checked = !!v;
    else e.setAttribute(k, v === true ? "" : String(v));
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    e.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return e;
}

function badge(text, cls) { return el("span", { class: "badge " + (cls || "") }, text); }

async function api(method, path, body) {
  const headers = { "Accept": "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") headers["X-CSRF-Token"] = csrf;
  let resp;
  try {
    resp = await fetch("/admin/api/v1" + path, { method, headers, credentials: "same-origin", body: body === undefined ? undefined : JSON.stringify(body) });
  } catch (e) {
    throw { code: "NETWORK", detail: "The server does not answer." };
  }
  const text = await resp.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (e) { data = null; }
  if (resp.status === 401 && path !== "/session") { showLogin(); throw { code: "LOGIN", detail: "Please log in again." }; }
  if (!resp.ok) {
    const er = (data && data.error) || { code: "HTTP_" + resp.status, detail: resp.statusText };
    throw er;
  }
  return path.startsWith("/audit") || path.startsWith("/logs") ? text : data;
}

function errBox(e) {
  return el("div", { class: "err", role: "alert" }, [e.code ? e.code + ": " : "", e.detail || e.title || "failed", e.fix ? "\nFix: " + e.fix : ""]);
}

// ---- login ----

function showLogin(message) {
  csrf = "";
  $("nav").hidden = true; $("logout").hidden = true; $("banner").hidden = true;
  const input = el("input", { id: "tok", type: "password", autocomplete: "off", required: true, "aria-describedby": "tokhint" });
  const msg = el("div");
  const form = el("form", { class: "card login", onsubmit: async (ev) => {
    ev.preventDefault();
    msg.replaceChildren();
    try {
      const r = await api("POST", "/session", { token: input.value });
      input.value = "";
      csrf = r.csrf;
      start();
    } catch (e) { input.value = ""; msg.replaceChildren(errBox(e)); }
  } },
  el("h2", {}, T.login), el("label", { for: "tok" }, T.token), input, el("p", { class: "hint", id: "tokhint" }, T.tokenHint),
  msg, el("p", {}, el("button", { class: "primary", type: "submit" }, T.login)));
  if (message) msg.append(el("div", { class: "err" }, message));
  $("view").replaceChildren(form);
  input.focus();
}

// ---- shell ----

const VIEWS = { overview: viewOverview, repositories: viewRepositories, certificates: viewCertificates, diagnostics: viewDiagnostics, logs: viewLogs, audit: viewAudit, settings: viewSettings };

async function start() {
  $("nav").hidden = false; $("logout").hidden = false;
  $("nav").replaceChildren(...Object.keys(VIEWS).map((k) => el("button", { type: "button", "data-view": k, onclick: () => show(k) }, T[k])));
  await show(currentView);
}

async function show(name) {
  currentView = name;
  for (const b of $("nav").children) {
    if (b.getAttribute("data-view") === name) b.setAttribute("aria-current", "page"); else b.removeAttribute("aria-current");
  }
  $("view").replaceChildren(el("p", { class: "muted" }, "Loading..."));
  try {
    await refreshBanner();
    $("view").replaceChildren(await VIEWS[name]());
  } catch (e) { if (e.code !== "LOGIN") $("view").replaceChildren(errBox(e)); }
  $("view").focus();
}

async function refreshBanner() {
  const st = await api("GET", "/status");
  const b = $("banner");
  b.replaceChildren(); b.classList.remove("bad");
  if (st.fallback) {
    b.classList.add("bad"); b.hidden = false;
    b.append(el("span", {}, [T.fallback, " ", st.fallback.split("\n")[0]]));
  } else if (st.pendingRestart) {
    b.hidden = false;
    b.append(el("span", {}, T.pending), el("button", { type: "button", class: "primary", onclick: () => applyChanges(st) }, T.apply));
  } else { b.hidden = true; }
  return st;
}

async function applyChanges(before) {
  if (!confirm(T.confirmApply)) return;
  const b = $("banner");
  try { await api("POST", "/apply", {}); } catch (e) { b.replaceChildren(errBox(e)); return; }
  b.hidden = false; b.classList.remove("bad"); b.replaceChildren(el("span", {}, T.restarting));
  const until = Date.now() + 5 * 60 * 1000;
  while (Date.now() < until) {
    await new Promise((r) => setTimeout(r, 1000));
    try {
      const st = await api("GET", "/status");
      if (st.startedAt !== before.startedAt && st.ready) { b.hidden = true; await show(currentView); alertIf(st); return; }
    } catch (e) {
      if (e.code === "LOGIN") { showLogin(T.afterRestart); return; } // the new process knows no sessions
      /* otherwise the server is still restarting */
    }
  }
  b.replaceChildren(errBox({ code: "TIMEOUT", detail: "The server did not become ready again within 5 minutes. Check: zaks-drive logs" }));
}

function alertIf(st) { if (st.fallback) alert(T.fallback + "\n" + st.fallback); }

// ---- views ----

async function viewOverview() {
  const [st, list] = await Promise.all([api("GET", "/status"), api("GET", "/repositories")]);
  const serving = new Set(st.serving || []);
  return el("div", {},
    el("div", { class: "card" }, el("h2", {}, T.overview),
      el("table", {}, el("tbody", {},
        row("Version", st.version), row("Ready", st.ready ? "yes" : "NO"), row("Started", new Date(st.startedAt).toLocaleString()),
        row("Pending changes", st.pendingRestart ? "yes, waiting for a restart" : "no"), row("Running requests", String(st.inFlight))))),
    el("div", { class: "card" }, el("h2", {}, T.repositories), repoTable(list.value || [], serving, false)),
    el("p", { class: "hint" }, T.purgeNote));
}

function row(k, v) { return el("tr", {}, el("th", { scope: "row" }, k), el("td", { class: "wrap" }, v)); }

function placeOf(r) {
  if (r.backend === "local") return "local folder " + ((r.local && r.local.path) || "") + " (keeps " + ((r.local && r.local.versions != null) ? r.local.versions : 5) + " versions)";
  let l = r.siteUrl || "";
  if (r.driveId) l = "drive " + r.driveId; else if (r.userId) l = "OneDrive of " + r.userId; else if (r.siteId) l = "site " + r.siteId;
  if (r.driveName) l += " / " + r.driveName;
  if (r.rootPath) l += " / " + r.rootPath;
  return l + (r.tenantId ? " (own application)" : "");
}

function repoTable(repos, serving, withActions) {
  const head = ["ID", "Backend", "Source", "SAP", "State", "Location"];
  if (withActions) head.push("Actions");
  const body = repos.map((r) => el("tr", {},
    el("td", {}, el("strong", {}, r.id)), el("td", {}, r.backend), el("td", {}, r.source),
    el("td", {}, r.sap ? (r.sap.contRep || r.id) : "-"),
    el("td", {}, serving.has(r.id) ? badge("serving", "serving") : badge("pending restart", "pending")),
    el("td", { class: "wrap" }, placeOf(r)),
    withActions ? el("td", {}, el("div", { class: "row" },
      r.source === "managed" ? el("button", { type: "button", onclick: () => repoForm(r) }, "Edit") : badge("config.yaml"),
      el("button", { type: "button", onclick: () => checkRepo(r.id) }, "Test access"),
      r.source === "managed" ? el("button", { type: "button", class: "danger", onclick: () => detach(r) }, "Detach") : null)) : null));
  if (!repos.length) return el("p", { class: "muted" }, "No repositories yet.");
  return el("div", { class: "scroll" }, el("table", {}, el("thead", {}, el("tr", {}, head.map((h) => el("th", { scope: "col" }, h)))), el("tbody", {}, body)));
}

async function viewRepositories() {
  const [st, list] = await Promise.all([api("GET", "/status"), api("GET", "/repositories")]);
  const det = await api("GET", "/detached").catch(() => ({ value: [] }));
  const out = el("div", {}, el("div", { class: "card" }, el("div", { class: "row" }, el("h2", {}, T.repositories),
    el("button", { type: "button", class: "primary", onclick: () => repoForm(null) }, "Add repository")),
  repoTable(list.value || [], new Set(st.serving || []), true), el("div", { id: "result" })),
  el("div", { id: "formhost" }));
  if ((det.value || []).length) out.append(el("div", { class: "card" }, el("h2", {}, "Detached (documents are kept)"),
    el("p", { class: "muted" }, (det.value || []).map((d) => d.id + " (" + placeOf(d) + ")").join(", ")), el("p", { class: "hint" }, T.purgeNote)));
  return out;
}

function field(label, input, hint) {
  const id = "f_" + label.replace(/\W+/g, "_");
  input.id = id;
  return el("div", {}, el("label", { for: id }, label), input, hint ? el("p", { class: "hint" }, hint) : null);
}

function repoForm(existing) {
  const r = existing || {};
  const isNew = !existing;
  const backend = el("select", { disabled: !isNew }, ["local", "sharepoint"].map((b) => el("option", { value: b, selected: (r.backend || "local") === b }, b)));
  const f = {
    id: el("input", { value: r.id || "", maxlength: 2, pattern: "[A-Z0-9]{2}", required: true, disabled: !isNew, autocomplete: "off" }),
    description: el("input", { value: r.description || "" }),
    path: el("input", { value: (r.local && r.local.path) || "" }),
    versions: el("input", { type: "number", min: 0, max: 99, value: (r.local && r.local.versions != null) ? r.local.versions : 5 }),
    siteUrl: el("input", { value: r.siteUrl || "", placeholder: "https://contoso.sharepoint.com/sites/hr" }),
    driveName: el("input", { value: r.driveName || "" }), rootPath: el("input", { value: r.rootPath || "" }),
    userId: el("input", { value: r.userId || "" }), driveId: el("input", { value: r.driveId || "" }), siteId: el("input", { value: r.siteId || "" }),
    tenantId: el("input", { value: r.tenantId || "", autocomplete: "off" }), clientId: el("input", { value: r.clientId || "", autocomplete: "off" }),
    secret: el("input", { type: "password", autocomplete: "new-password" }),
    readOnly: el("input", { type: "checkbox", checked: r.readOnly }),
    sap: el("input", { type: "checkbox", checked: !!r.sap }),
    contRep: el("input", { value: (r.sap && r.sap.contRep) || "", maxlength: 2 }),
    signature: el("select", {}, ["required", "optional", "none"].map((s) => el("option", { value: s, selected: ((r.sap && r.sap.signature) || "required") === s }, s))),
  };
  const localBox = el("div", { class: "grid" }, field("Folder (below the data directory)", f.path, "Default: the repository ID."), field("Versions kept (0-99)", f.versions));
  const spBox = el("div", { class: "grid" }, field("Site URL", f.siteUrl), field("Library", f.driveName), field("Folder in the library", f.rootPath),
    field("OneDrive user (instead of a site)", f.userId), field("Drive ID", f.driveId), field("Site ID", f.siteId),
    field("Own tenant ID (optional)", f.tenantId), field("Own client ID (optional)", f.clientId),
    field("Client secret", f.secret, T.secretHint));
  const sync = () => { localBox.hidden = backend.value !== "local"; spBox.hidden = backend.value !== "sharepoint"; };
  backend.addEventListener("change", sync); sync();
  const msg = el("div");
  const form = el("form", { class: "card", onsubmit: async (ev) => {
    ev.preventDefault(); msg.replaceChildren();
    const body = { id: f.id.value.trim(), backend: backend.value, description: f.description.value || undefined, readOnly: f.readOnly.checked || undefined };
    if (backend.value === "local") {
      body.local = { path: f.path.value.trim() || body.id, versions: Number(f.versions.value) };
    } else {
      for (const k of ["siteUrl", "driveName", "rootPath", "userId", "driveId", "siteId", "tenantId", "clientId"]) if (f[k].value.trim()) body[k] = f[k].value.trim();
      if (f.secret.value) body.clientSecret = f.secret.value;
    }
    if (f.sap.checked) body.sap = { contRep: f.contRep.value.trim() || undefined, signature: f.signature.value };
    try {
      await api(isNew ? "POST" : "PUT", "/repositories" + (isNew ? "" : "/" + encodeURIComponent(body.id)), body);
      f.secret.value = "";
      await show("repositories");
      $("result").replaceChildren(el("p", { class: "toast", role: "status" }, "Saved. The change is pending: use \"" + T.apply + "\" above."));
    } catch (e) { f.secret.value = ""; msg.replaceChildren(errBox(e)); }
  } },
  el("h2", {}, isNew ? "Add repository" : "Edit repository " + r.id),
  el("div", { class: "grid" }, field("ID (exactly 2 characters, A-Z or 0-9)", f.id), field("Backend", backend), field("Description", f.description)),
  localBox, spBox,
  el("div", { class: "row" }, el("label", {}, f.readOnly, " Read-only"), el("label", {}, f.sap, " Expose to SAP")),
  el("div", { class: "grid" }, field("SAP content repository ID (default: the ID)", f.contRep), field("Signature", f.signature)),
  msg,
  el("div", { class: "row" }, el("button", { type: "submit", class: "primary" }, "Save"), el("button", { type: "button", onclick: () => show("repositories") }, "Cancel")));
  $("formhost").replaceChildren(form);
  form.scrollIntoView();
}

async function checkRepo(id) {
  $("result").replaceChildren(el("p", { class: "muted" }, "Testing " + id + " ... (SharePoint repositories contact Microsoft)"));
  try {
    const r = await api("POST", "/repositories/" + encodeURIComponent(id) + "/check", {});
    $("result").replaceChildren(el("h2", {}, "Access test of " + id + ": " + (r.ok ? "passed" : "FAILED")), el("pre", {}, r.report));
  } catch (e) { $("result").replaceChildren(errBox(e)); }
}

async function detach(r) {
  if (!confirm(T.confirmDetach)) return;
  const sap = !!r.sap;
  if (sap && !confirm(T.confirmDetachSAP)) return;
  try { await api("DELETE", "/repositories/" + encodeURIComponent(r.id) + (sap ? "?allowInUse=true" : "")); await show("repositories"); }
  catch (e) { $("result").replaceChildren(errBox(e)); }
}

async function viewCertificates() {
  const r = await api("GET", "/certificates");
  const rows = (r.value || []).map((c) => el("tr", {},
    el("td", {}, c.contRep), el("td", { class: "wrap" }, c.authId), el("td", { class: "wrap" }, c.sha256Fingerprint),
    el("td", {}, new Date(c.notAfter).toLocaleDateString()), el("td", {}, c.active ? badge("active", "pass") : badge("inactive", "warn")),
    el("td", {}, el("button", { type: "button", class: c.active ? "danger" : "primary", onclick: () => setCert(c) }, c.active ? "Deactivate" : "Activate"))));
  return el("div", { class: "card" }, el("h2", {}, T.certificates),
    el("p", { class: "hint" }, "In SAP: OAC0, choose Send certificate; then compare the fingerprint here with the one SAP shows before you activate. Test with RSCMSTH0."),
    rows.length ? el("div", { class: "scroll" }, el("table", {}, el("thead", {}, el("tr", {}, ["Content repo", "Authorization ID", "SHA-256 fingerprint", "Expires", "State", ""].map((h) => el("th", { scope: "col" }, h)))), el("tbody", {}, rows)))
      : el("p", { class: "muted" }, "No certificate received from SAP yet."), el("div", { id: "result" }));
}

async function setCert(c) {
  const verb = c.active ? "deactivate" : "activate";
  if (!confirm("Really " + verb + " the certificate " + c.authId + " for " + c.contRep + "?\nSAP signed requests " + (c.active ? "will be refused." : "with it will be accepted."))) return;
  try { await api("POST", "/certificates/" + verb, { contRep: c.contRep, authId: c.authId }); await show("certificates"); }
  catch (e) { $("result").replaceChildren(errBox(e)); }
}

async function viewDiagnostics() {
  const r = await api("GET", "/diagnostics");
  return el("div", { class: "card" }, el("h2", {}, T.diagnostics),
    el("p", { class: "hint" }, "These are the checks the server can make about itself. Host checks (Docker, ports, disk) are in: zaks-drive doctor."),
    el("div", { class: "scroll" }, el("table", {}, el("thead", {}, el("tr", {}, ["Check", "State", "Detail", "Fix"].map((h) => el("th", { scope: "col" }, h)))),
      el("tbody", {}, (r.value || []).map((f) => el("tr", {}, el("td", {}, f.title), el("td", {}, badge(f.status, f.status.toLowerCase())), el("td", { class: "wrap" }, f.detail || ""), el("td", { class: "wrap" }, f.fix || "")))))));
}

async function viewLogs() {
  const level = el("select", {}, ["DEBUG", "INFO", "WARN", "ERROR"].map((l) => el("option", { value: l, selected: l === "INFO" }, l)));
  const rid = el("input", { placeholder: "request ID" });
  const out = el("pre", {});
  const load = async () => {
    try {
      const t = await api("GET", "/logs?tail=300&level=" + level.value + (rid.value ? "&requestId=" + encodeURIComponent(rid.value) : ""));
      const lines = JSON.parse(t || "[]").map((e) => e.time.replace("T", " ").replace(/\..*/, "") + " " + e.level.padEnd(5) + " " + e.msg + (e.attrs ? "  " + e.attrs : ""));
      out.textContent = lines.join("\n") || "(no lines)";
    } catch (e) { out.textContent = (e.code || "") + " " + (e.detail || ""); }
  };
  await load();
  return el("div", { class: "card" }, el("h2", {}, T.logs),
    el("p", { class: "hint" }, "The server's recent log lines, with credentials masked. They are kept in memory only and are lost on restart; longer history: zaks-drive logs."),
    el("div", { class: "row" }, level, rid, el("button", { type: "button", onclick: load }, "Refresh"),
      el("button", { type: "button", onclick: () => navigator.clipboard && navigator.clipboard.writeText(out.textContent) }, "Copy")), out);
}

async function viewAudit() {
  const t = await api("GET", "/audit?lines=300");
  const lines = t.split("\n").filter(Boolean).map((l) => { try { const e = JSON.parse(l); return [e.time, e.actor, e.action, e.id, e.detail || ""]; } catch (x) { return null; } }).filter(Boolean).reverse();
  return el("div", { class: "card" }, el("h2", {}, T.audit), el("p", { class: "hint" }, "Who changed which repository and when. Never values or secrets."),
    lines.length ? el("div", { class: "scroll" }, el("table", {}, el("thead", {}, el("tr", {}, ["Time", "By", "Action", "Repository", "Detail"].map((h) => el("th", { scope: "col" }, h)))),
      el("tbody", {}, lines.map((c) => el("tr", {}, c.map((x) => el("td", { class: "wrap" }, x))))))) : el("p", { class: "muted" }, "Nothing recorded yet."));
}

async function viewSettings() {
  const s = await api("GET", "/settings");
  return el("div", { class: "card" }, el("h2", {}, T.settings), el("p", { class: "hint" }, "The effective configuration without secrets. Change it in config.yaml and recreate the container."),
    el("table", {}, el("tbody", {}, Object.entries(s).map(([k, v]) => row(k, typeof v === "object" ? JSON.stringify(v) : String(v))))));
}

// ---- boot ----

$("logout").addEventListener("click", async () => { try { await api("DELETE", "/session"); } catch (e) { /* already gone */ } showLogin(); });

(async function boot() {
  try {
    const s = await api("GET", "/session");
    if (s && s.authenticated) { csrf = s.csrf; start(); return; }
  } catch (e) { /* fall through to the login form */ }
  showLogin();
})();
