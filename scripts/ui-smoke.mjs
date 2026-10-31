// Browser smoke test of the admin UI (docs/ui.md). Drives a headless Chromium-based browser over the
// DevTools protocol with Node 22+ only (no packages). It logs in (a wrong token first), adds a local
// repository through the real form, tests access, visits every view, edits, detaches and logs out, and
// fails on console errors, CSP violations, exceptions or any request to another origin.
//
//   ZD_TOKEN=<ADMIN_TOKEN> [ZD_BASE=http://127.0.0.1:9090] [ZD_BROWSER=/path/to/chrome|msedge] node scripts/ui-smoke.mjs [outdir]
//
// Run it against a disposable server: it creates and detaches a repository "AD" and expects no repository
// with that ID to exist. It never restarts the server.
import { spawn } from "node:child_process";
import { writeFileSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const BASE = process.env.ZD_BASE || "http://127.0.0.1:9090";
const TOKEN = process.env.ZD_TOKEN;
if (!TOKEN) { console.error("set ZD_TOKEN to the server's ADMIN_TOKEN"); process.exit(2); }
const OUT = process.argv[2] || ".";
const edge = process.env.ZD_BROWSER || (process.platform === "win32" ? "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe" : "google-chrome");
const profile = mkdtempSync(join(tmpdir(), "zd-edge-"));
const proc = spawn(edge, ["--headless=new", "--remote-debugging-port=9333", `--user-data-dir=${profile}`, "--no-first-run", "--disable-gpu", "--window-size=1280,900", "about:blank"], { stdio: "ignore" });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function getJSON(u, tries = 50) { for (let i = 0; i < tries; i++) { try { return await (await fetch(u)).json(); } catch { await sleep(200); } } throw new Error("no browser"); }

const targets = await getJSON("http://127.0.0.1:9333/json/list");
const page = targets.find((t) => t.type === "page");
const ws = new WebSocket(page.webSocketDebuggerUrl);
await new Promise((r) => (ws.onopen = r));
let id = 0; const pending = new Map(); const events = [];
ws.onmessage = (m) => { const d = JSON.parse(m.data); if (d.id && pending.has(d.id)) { pending.get(d.id)(d); pending.delete(d.id); } else if (d.method) events.push(d); };
const send = (method, params = {}) => new Promise((res) => { const i = ++id; pending.set(i, res); ws.send(JSON.stringify({ id: i, method, params })); });
const ev = async (expr) => { const r = await send("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true }); if (r.result.exceptionDetails) throw new Error("eval: " + JSON.stringify(r.result.exceptionDetails.exception?.description || r.result.exceptionDetails.text)); return r.result.result.value; };
async function waitFor(expr, what, ms = 8000) { const end = Date.now() + ms; while (Date.now() < end) { try { if (await ev(expr)) return; } catch {} await sleep(100); } throw new Error("timeout waiting for " + what + "\nPAGE TEXT: " + (await ev("document.body.innerText").catch(() => "?")).slice(0, 600)); }

await send("Page.enable"); await send("Runtime.enable"); await send("Network.enable"); await send("Log.enable");
const results = []; const ok = (name, cond, detail = "") => { results.push({ name, pass: !!cond, detail }); if (!cond) console.log("FAIL", name, detail); };

try {
  await send("Page.navigate", { url: BASE + "/admin/ui/" });
  await waitFor("!!document.querySelector('form.login')", "login form");
  ok("login form shown before any data", await ev("!document.querySelector('nav') || document.getElementById('nav').hidden"));
  ok("page title", (await ev("document.title")) === "ZAKS Drive admin");

  // wrong token
  await ev("document.getElementById('tok').value='wrong-token'; document.querySelector('form.login').requestSubmit()");
  await waitFor("!!document.querySelector('.err')", "login error");
  ok("wrong token shows a coded error", (await ev("document.querySelector('.err').textContent")).includes("AUTH_ADMIN_UNAUTHORIZED"));
  ok("token field is cleared after a failed login", (await ev("document.getElementById('tok').value")) === "");

  // right token
  await ev(`document.getElementById('tok').value=${JSON.stringify(TOKEN)}; document.querySelector('form.login').requestSubmit()`);
  await waitFor("document.querySelectorAll('#nav button').length === 7", "navigation");
  ok("seven views in the navigation", true);
  ok("token is not left in the page", !(await ev("document.documentElement.outerHTML")).includes(TOKEN));
  ok("document.cookie cannot see the session (HttpOnly)", (await ev("document.cookie")) === "");
  await waitFor("document.body.innerText.includes('Overview') && document.body.innerText.includes('Version')", "overview");

  // repositories: add
  await ev("[...document.querySelectorAll('#nav button')].find(b=>b.textContent==='Repositories').click()");
  await waitFor("[...document.querySelectorAll('button')].some(b=>b.textContent==='Add repository')", "add button");
  await ev("[...document.querySelectorAll('button')].find(b=>b.textContent==='Add repository').click()");
  await waitFor("!!document.querySelector('form input[maxlength=\"2\"][pattern]')", "form");
  await ev("const i=document.querySelector('form input[maxlength=\"2\"][pattern]'); i.value='AD'; i.dispatchEvent(new Event('input'));");
  await ev("document.querySelector('#formhost form').requestSubmit()");
  await waitFor("document.body.innerText.includes('Saved. The change is pending')", "saved message");
  ok("repository AD listed after save", (await ev("document.querySelector('table').innerText")).includes("AD"));
  await waitFor("!document.getElementById('banner').hidden", "pending banner");
  ok("pending banner offers apply and restart", (await ev("document.getElementById('banner').innerText")).includes("Apply and restart"));
  await send("Page.captureScreenshot", { format: "png" }).then((r) => writeFileSync(OUT + "/repositories.png", Buffer.from(r.result.data, "base64")));

  // invalid input is reported by the server, not swallowed
  await ev("[...document.querySelectorAll('button')].find(b=>b.textContent==='Add repository').click()");
  await waitFor("!!document.querySelector('#formhost form')", "form 2");
  await ev("const f=document.querySelector('#formhost form'); f.querySelector('input[maxlength=\"2\"][pattern]').value='AD'; f.requestSubmit()");
  await waitFor("!!document.querySelector('#formhost .err')", "server-side error");
  ok("a rejected form shows the server's coded reason (duplicate ID)", (await ev("(document.querySelector('#formhost .err')||{textContent:'NONE: '+document.getElementById('formhost').innerText+' // '+document.getElementById('view').innerText.slice(0,300)}).textContent")).includes("ADMIN_REPO_EXISTS"));
  await ev("[...document.querySelectorAll('#formhost button')].find(b=>b.textContent==='Cancel').click()");

  // test access
  await waitFor("[...document.querySelectorAll('button')].some(b=>b.textContent==='Test access')", "test access button");
  await ev("[...document.querySelectorAll('button')].find(b=>b.textContent==='Test access').click()");
  await waitFor("!!document.querySelector('#result pre')", "access report");
  ok("access test passed for the local repository", (await ev("document.querySelector('#result pre').textContent")).includes("[PASS] [AD] Write"));

  // every view renders
  for (const [name, needle] of [["Overview", "Version"], ["SAP certificates", "No certificate received"], ["Diagnostics", "Repository definitions"], ["Logs", "Refresh"], ["Audit", "add"], ["Settings", "interfaces"]]) {
    await ev(`[...document.querySelectorAll('#nav button')].find(b=>b.textContent===${JSON.stringify(name)}).click()`);
    await waitFor(`document.getElementById('view').innerText.includes(${JSON.stringify(needle)})`, name + " view");
    ok("view " + name, true);
    if (name === "Logs") ok("logs view shows server lines", (await ev("document.querySelector('#view pre').textContent")).length > 5);
    if (name === "Settings") ok("settings hide secrets", !(await ev("document.getElementById('view').innerText")).includes(TOKEN));
  }

  // edit then detach
  await ev("[...document.querySelectorAll('#nav button')].find(b=>b.textContent==='Repositories').click()");
  await waitFor("[...document.querySelectorAll('button')].some(b=>b.textContent==='Edit')", "edit button");
  await ev("[...document.querySelectorAll('button')].find(b=>b.textContent==='Edit').click()");
  await waitFor("!!document.querySelector('#formhost form')", "edit form");
  ok("the ID cannot be changed when editing", await ev("document.querySelector('#formhost input[maxlength=\"2\"]').disabled"));
  await ev("[...document.querySelectorAll('#formhost button')].find(b=>b.textContent==='Cancel').click()");
  await ev("window.confirm = () => true");
  await waitFor("[...document.querySelectorAll('button')].some(b=>b.textContent==='Detach')", "detach button");
  await ev("[...document.querySelectorAll('button')].find(b=>b.textContent==='Detach').click()");
  await waitFor("document.getElementById('view').innerText.includes('Detached (documents are kept)')", "detached list");
  ok("detached repository is listed with the purge note", (await ev("document.getElementById('view').innerText")).includes("not available here, by design"));

  // logout
  await ev("document.getElementById('logout').click()");
  await waitFor("!!document.querySelector('form.login')", "login after logout");
  ok("logged out", true);
  await ev("fetch('/admin/api/v1/repositories').then(r=>r.status)").then((s) => ok("API refuses after logout", s === 401, String(s)));
} catch (e) { console.log("ERROR", e.message); results.push({ name: "run", pass: false, detail: e.message }); }

const requests = events.filter((e) => e.method === "Network.requestWillBeSent").map((e) => e.params.request.url);
const foreign = requests.filter((u) => !u.startsWith(BASE) && !u.startsWith("data:") && !u.startsWith("about:"));
ok("every request stays on the admin origin", foreign.length === 0, foreign.join(", "));
const logs401 = events.filter((e) => e.method === "Log.entryAdded" && /status of 401/.test(e.params.entry.text));
ok("only the two deliberate 401s (wrong token, call after logout)", logs401.length === 2, String(logs401.length));
const bad = events.filter((e) => (e.method === "Log.entryAdded" && ["error", "warning"].includes(e.params.entry.level) && !/favicon|status of 401|status of 409/i.test(e.params.entry.text + (e.params.entry.url || ""))) || e.method === "Runtime.exceptionThrown");
ok("no console errors, CSP violations or exceptions", bad.length === 0, JSON.stringify(bad.map((b) => b.params.entry?.text || b.params.exceptionDetails?.exception?.description || b.params.exceptionDetails?.text)).slice(0, 900));
writeFileSync(OUT + "/result.json", JSON.stringify({ results, requests: [...new Set(requests.map((u) => u.replace(BASE, "")))] }, null, 1));
console.log(results.filter((r) => r.pass).length + " passed, " + results.filter((r) => !r.pass).length + " failed");
ws.close(); proc.kill(); process.exit(results.every((r) => r.pass) ? 0 : 1);
