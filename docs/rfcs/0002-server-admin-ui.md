# RFC 0002: Admin UI built into the server

- **Status:** draft
- **Author:** @Lamiv (with Claude)
- **Created:** 2026-10-30
- **Discussion:** the pull request that adds this file
- **Depends on:** [RFC 0001: multiple repositories with local or SharePoint storage](0001-repositories-and-storage-backends.md)

> An RFC is a proposal for discussion. It is not a promise that the change will be built, scheduled or released. See [docs/rfcs/README.md](README.md).

## Summary

Add a web **admin UI served by the server itself** on its existing admin listener, so an administrator can manage repositories, SAP certificates and diagnostics from a browser, with the same operations the `zaks-drive` command offers. The UI is a thin client of a documented **admin API** that the command also uses. It ships inside the server binary (no separate service, no Node build), is bound to `127.0.0.1` by default, and never shows a secret.

## Motivation

- SAP Basis and platform administrators often prefer a UI for routine work: adding the repository `ZH`, activating a certificate that SAP just sent, checking why a repository is not ready.
- The command-line tool needs shell access to the Docker host. A UI served by the server can be reached through an SSH tunnel or a TLS reverse proxy by people who should not have shell access.
- Today the admin port serves health, metrics and a few certificate endpoints. There is no consolidated way to see the state of every repository.

## Decisions so far

From the maintainer's answers (2026-10-30):

- **Login stays simple** for now: the admin token. **Sign-in with Microsoft Entra ID** is a planned later option and the design leaves room for it.
- **Only one role exists: administrator.** No read-only or other roles.
- **Restart traffic:** applying changes always goes through a maintenance drain (see [Applying changes](#applying-changes)).
- **Purge is not available in the UI or the admin API** (see [RFC 0001](0001-repositories-and-storage-backends.md#removing-and-purging-a-repository)).

## What the UI can and cannot do

The maintainer chose a UI **in the server**. That has a consequence that this RFC states plainly: a server running in a container **cannot** start, stop, update or roll back its own container, install itself, or collect host information, without being given the Docker socket, and handing the Docker socket to a network-facing service is a serious privilege escalation that this project will not do.

So "the same features as the CLI" means the **server-side** features:

| CLI feature | In the server UI | Why |
|---|---|---|
| `status`, `version` | Yes | the server knows its own state |
| `repo list/add/update/remove/check/show` | **Yes** (RFC 0001) | repositories are owned by the server |
| SAP certificates: list, activate, deactivate | **Yes** (endpoints exist today) | owned by the server |
| `doctor` (server-side checks: configuration, repository readiness, Graph token roles, certificates, memory and spool) | **Yes**, a server-side subset | Docker, ports, files, disk of the host are not visible from inside |
| `logs` | **Yes**, the server's own recent log lines (sanitized ring buffer, filter by level, interface, request ID) | the server cannot read Docker's log store |
| `check` (SharePoint access test) | **Yes**, per repository | the server does it itself |
| Apply changes (restart) | **Yes**: the server exits cleanly and the container policy restarts it | restart-based, per RFC 0001 |
| `repo purge` (delete documents) | **No, by design** | too dangerous to reach through a browser session or a stolen token; CLI on the host only (RFC 0001) |
| `setup`, `start`, `stop`, `restart` of the container, `update`, `rollback`, `support bundle`, host `doctor` checks | **No** | need the host and Docker; they stay in the command |

A later RFC can add a **host-side** mode (`zaks-drive ui`, running on the administrator's machine with the same code as the command) for the host-level operations. This RFC keeps the two designs compatible: both use the same admin API and the same UI assets.

## Design

### Admin API

A versioned JSON API under `/admin/api/v1/` on the **admin listener** (the same listener as `/healthz` and `/metrics`; localhost-only in the shipped Compose file). It is documented in `docs/api/admin-openapi.yaml`, tested against the routes like the REST API, and used by both the command and the UI.

```text
GET    /admin/api/v1/status                        version, readiness, uptime, pending changes
GET    /admin/api/v1/repositories                  list (no secrets)
POST   /admin/api/v1/repositories                  validate + create (body may carry a write-only secret)
GET    /admin/api/v1/repositories/{id}
PUT    /admin/api/v1/repositories/{id}
DELETE /admin/api/v1/repositories/{id}             detach; documents are kept
POST   /admin/api/v1/repositories/{id}/check       access test of that repository
POST   /admin/api/v1/validate                      dry run of a proposed change, nothing written
POST   /admin/api/v1/apply                         drain and exit so the container restarts with the new state
GET    /admin/api/v1/certificates                  (today's /admin/contentserver/certificates, kept as an alias)
POST   /admin/api/v1/certificates/{activate|deactivate}
GET    /admin/api/v1/diagnostics                   the server-side findings, PASS/WARN/FAIL/SKIPPED
GET    /admin/api/v1/logs?tail=&level=&iface=&requestId=
GET    /admin/api/v1/audit
```

The existing `/admin/contentserver/certificates...` endpoints keep working unchanged.

### Authentication and session

- **Login with `ADMIN_TOKEN`** (the same secret the admin API uses today). The browser exchanges it once for a **session cookie** (`HttpOnly`, `SameSite=Strict`, `Secure` when served over TLS, short idle and absolute lifetime). The token itself is never stored in the browser.
- **CSRF:** every state-changing request needs a per-session CSRF token header in addition to the cookie, and the server rejects cross-origin requests (`Origin` / `Host` check).
- **Login throttling:** failed logins are rate limited per source address, with a constant-time token comparison.
- **One role: administrator.** That is the decision for this RFC. Several accounts or roles would need an identity store. An **Entra ID sign-in** can replace the token login later without changing the API or the pages: the session layer is the only part that changes, and it will get its own RFC.
- If `ADMIN_TOKEN` is empty, the UI and every mutating admin endpoint are **disabled** (the server logs why).

### Exposure

- The admin listener stays **bound to `127.0.0.1`** in the shipped Compose file. Remote use is by an SSH tunnel or a TLS-terminating reverse proxy that the administrator operates.
- Serving the admin listener on a non-loopback address requires an explicit setting (`server.admin.allowRemote: true`) **and** TLS on that listener; otherwise the server refuses to start with a catalogued error. The UI is never reachable on the REST or SAP ports.
- `doctor` and `status` warn when the admin listener is reachable from other hosts.

### UI

Embedded static assets (`go:embed`), plain HTML, CSS and a small amount of JavaScript. **No Node toolchain, no external scripts, fonts or images**: the page loads nothing from a CDN, and a strict `Content-Security-Policy` (`default-src 'self'`, no inline scripts) is sent.

Views in the first version:

1. **Overview:** version, readiness and why, uptime, repositories with state, pending changes, a banner "changes pending: apply and restart".
2. **Repositories:** table; add and edit forms per backend (local: path and versions; SharePoint: site, library, root folder, optional own tenant, client and **write-only** secret field), SAP content repository ID and signature mode, test access, detach. The forms show the effect before applying ("this restarts the server; SAP retries").
3. **SAP certificates:** list per content repository with fingerprint and state, activate and deactivate with confirmation; guidance for the SAP side (OAC0, STRUST, `RSCMSTH0`).
4. **Diagnostics:** the server-side findings with fixes, and per-repository access tests.
5. **Logs:** recent sanitized lines, filters, copy to clipboard.
6. **Audit:** who changed what, when.
7. **Settings (read-only):** the effective non-secret configuration.

Design rules: no document browsing (ZAKS Drive is a content storage gateway, not a DMS); every destructive or restarting action asks for confirmation and states its effect; secrets are write-only fields that are cleared after submit; accessible (keyboard, labels, contrast) and usable at laptop and tablet widths; English first, strings kept in one table so translation is possible.

### Logs view without Docker

The server keeps a bounded in-memory ring buffer (default 5,000 lines) of the log events it already emits. The events pass through the existing logging policy (approved fields only), so the buffer cannot contain more than the log itself. It is lost on restart, which the UI says. Longer history stays in Docker's log store and is read with `zaks-drive logs`.

### Audit

Every mutating admin call, from the UI or the command, appends a line to the `audit.log` in the state directory: time, source address, action, repository ID, outcome. No values and no secrets. The file is rotated by size.

### Applying changes

The UI's "Apply and restart" calls `/apply`, which runs the **maintenance drain** defined in [RFC 0001](0001-repositories-and-storage-backends.md#applying-changes-restart-through-a-maintenance-drain): the server validates the pending state once more, enters maintenance mode (new SAP and REST requests get `503` with `Retry-After`; SAP retries), waits for in-flight requests up to the drain time, then exits with status 0 so the container policy (`restart: unless-stopped`) starts it again with the new repositories.

The UI shows how many requests are still running before the administrator confirms, offers wait, extend or cancel, then shows "restarting" and polls `/readyz`. If the new state fails validation at start-up the server keeps the **last working state**: it records the failure in the audit log and the diagnostics, so a bad repository cannot take the whole service down.

## Alternatives

- **A host-side UI only (`zaks-drive ui`).** Reuses the command's code and can offer start, stop and update. But it needs the administrator's host and cannot be used by someone without shell access. Deferred as a complementary mode (see above).
- **Give the server the Docker socket** so the UI can do everything. Rejected: it makes any compromise of the server a compromise of the host.
- **A separate management container** with its own port and Docker access. More moving parts and the same privilege problem; rejected for now.
- **A single-page application framework with a Node build.** Richer UI but a large dependency tree and a second toolchain in a security-sensitive component. Rejected for the first version.
- **OIDC login (Entra ID) from day one.** Better for teams; needs identity configuration and an app registration. Listed as a future RFC.

## Compatibility

- **No change** to the REST API, the SAP Content Server interface, ports, configuration keys or existing admin endpoints.
- The admin listener gains `/ui/` and `/admin/api/v1/`; `/healthz`, `/readyz`, `/metrics` and `/version` are unchanged and remain unauthenticated.
- Behaviour with an empty `ADMIN_TOKEN` is unchanged (certificate admin already needs it when set; the UI and new mutating endpoints require it).
- The image grows by the size of the embedded assets (kilobytes).
- This is **not** a breaking change.

## Security

The UI is a new, privileged, browser-facing surface on a component that holds Microsoft credentials. The controls above are the design; the risks to review hardest:

| Risk | Control |
|---|---|
| Brute force or stolen token | constant-time comparison, per-address rate limiting, short sessions, token never stored in the browser, audit of failed and successful logins |
| CSRF, clickjacking, XSS | per-session CSRF token and `Origin` check; `SameSite=Strict`; `X-Frame-Options: DENY` and `frame-ancestors 'none'`; strict CSP with no inline script; all output escaped, untrusted text (repository names, error text) rendered as text only |
| Secret exposure | secrets are write-only fields, never returned by any endpoint, never logged, cleared from the form after submit, excluded from the audit log |
| Server-side request forgery through "test access" or a SharePoint URL | the check uses only the configured Graph endpoints; site URLs are validated by the existing loader; no arbitrary URL fetching |
| Local backend path abuse | validated paths, `os.Root` confinement, no links (RFC 0001) |
| Exposure beyond localhost | default binding, `allowRemote` plus TLS required, `doctor` warns |
| Dependency and supply chain | no front-end dependencies in the first version; assets reviewed in the repository |
| Denial of service by the UI | the admin listener has its own request limits and is separate from the SAP and REST listeners |

An independent review of the authentication and the new endpoints is a **precondition for calling the UI production-ready**, in line with the project's release criteria.

## Testing

- Admin API: contract tests against `docs/api/admin-openapi.yaml`; table tests for authentication, CSRF, rate limiting, disabled-without-token, `allowRemote` refusal.
- **Secret handling:** tests inject synthetic secrets through the create and update calls and assert they never appear in any response, log line, audit entry or the ring buffer.
- Policy tests: the logging-policy tests extended to the new packages; a test that no endpoint serializes a secret field.
- UI: a headless browser smoke test (create a local repository, test access, apply) in CI using only the Graph emulator and local storage; accessibility checks (axe-style) in the same job; a test that the page makes no request outside its own origin.
- Restart path: apply with a valid and with an invalid pending state (the last working state keeps serving).

Needs real systems: a browser pass against a real Docker host, behind a TLS reverse proxy, and an SAP certificate activation from the UI after a real OAC0 "Send certificate". Reported, not assumed.

## Migration

None. The UI is off unless `ADMIN_TOKEN` is set, which the shipped setup already generates. Existing deployments get the UI after an update; nothing is migrated.

## Phasing

1. Admin API (status, repositories, validate, apply, certificates, diagnostics, audit) with the command using it for `repo` operations (RFC 0001 step 5).
2. Authentication, session, CSRF, rate limiting, audit log.
3. Log ring buffer and the logs endpoint.
4. UI assets: overview, repositories, certificates; then diagnostics, logs, audit, settings.
5. Hardening and the independent review; documentation in `docs/ui.md`.

## Open questions

Answered by the maintainer and moved to [Decisions so far](#decisions-so-far): the login method for now, roles, restart traffic. Still open:

- Should the UI be **off by default** (`server.admin.ui: false`) and enabled by `zaks-drive setup`, or on whenever `ADMIN_TOKEN` is set?
- Which languages besides English?
- The drain length and whether requests during maintenance should be queued instead of refused (shared with RFC 0001).
- When the Entra sign-in RFC is written: which Entra roles or groups map to the administrator role?

## Outcome (filled in by a maintainer)

Decision: _pending_.
