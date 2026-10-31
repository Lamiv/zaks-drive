# The admin UI

A small web page, served by the ZAKS Drive server itself, for the server-side parts of administration: repositories, SAP certificates, diagnostics, logs and the audit trail. It is a client of the same [admin API](#the-admin-api) that the `zaks-drive` command uses. The design is in [RFC 0002](rfcs/0002-server-admin-ui.md).

> **Status.** Built and tested (unit tests, and a browser run against the real server and a Docker container). **Not yet reviewed independently**: the project's release criteria require an independent review of the authentication and the admin endpoints before the UI is called production-ready. Until then, keep it on `127.0.0.1` behind an SSH tunnel, as shipped.

## Open it

The Compose file publishes the admin port on the host's loopback only:

```text
http://127.0.0.1:9090/admin/ui/        (the port is ADMIN_PORT in .env)
```

From another machine use an SSH tunnel (`ssh -L 9090:127.0.0.1:9090 <host>`) or a TLS reverse proxy that you operate. Do **not** publish the admin port on a public address: it also serves metrics and the certificate endpoints.

Log in with the **admin token** (`ADMIN_TOKEN` in `.env`). If `ADMIN_TOKEN` is empty nobody can log in and every changing admin call is refused; `zaks-drive setup` generates a token.

## What it does

| View | What you can do |
|---|---|
| Overview | version, readiness, start time, running requests, repositories and whether they are served; a banner when changes wait for a restart |
| Repositories | list, **add**, **edit**, **test access**, **detach** (local folder or SharePoint, optional own Microsoft application with a write-only secret field, SAP content repository and signature mode) |
| SAP certificates | see what SAP sent, compare the fingerprint, activate and deactivate |
| Diagnostics | the checks the server can make about itself (definitions, pending changes, access per repository, certificates) with the fix |
| Logs | the server's recent log lines, filter by level or request ID, copy |
| Audit | who changed what and when |
| Settings | the effective configuration, **read-only and without secrets** |

Changes take effect through **Apply and restart**: the server refuses new requests with `503` and `Retry-After` (SAP retries those), lets running requests finish for up to `server.drainTimeout`, then ends, and Docker starts it again with the new repositories. The page tells you how this works before you confirm, and shows an explanation when it comes back; **sessions end with a restart**, so you log in again. If the new definitions are invalid, the server keeps serving the last working state and the page shows a red banner with the reason.

## What it does not do

- **No purge.** Deleting the documents of a repository is not available in the UI or the admin API, by design. It exists only in `zaks-drive repo purge` on the Docker host ([details](cli.md#purging-a-repository)).
- **No host operations.** Install, start, stop, update, rollback, the support bundle and host checks (Docker, ports, disk) stay in the `zaks-drive` command. A server inside a container cannot do them without the Docker socket, which this project will not hand to a network-facing service.
- **No document browsing.** ZAKS Drive is a content storage gateway, not a DMS.
- **One role.** There is only the administrator. Sign-in with Microsoft Entra ID is planned as a later option; the session layer is the only part that would change.

## How it is protected

| Concern | Control |
|---|---|
| Login | the admin token is compared in constant time and exchanged once for a random session; it is never stored in the browser |
| Session | cookie `HttpOnly`, `SameSite=Strict`, scoped to `/admin`, `Secure` when served over TLS; 30 minutes idle, 8 hours at most; held in memory (a restart ends it); at most 20 at a time |
| Guessing | after 10 wrong tokens the admin API answers `429` for a minute (login and Bearer calls share the counter); failed and successful logins are audited with the source address, never the value |
| CSRF | every change needs the session's CSRF token in a header **and** an `Origin` that names this server; a login or logout from another origin is refused |
| XSS, clickjacking | strict `Content-Security-Policy` (`default-src 'none'`, scripts, styles and requests only to the page's own origin, no inline code, no framing); the script builds the page with DOM calls and never parses markup; a test fails if the assets use a markup-parsing API or reference another site |
| Secrets | the client secret is a write-only field of a password type; it is cleared after submit, stored as its own mode-600 file, and never returned by any endpoint or written to the log view, the audit log or the settings view (tests inject synthetic secrets to prove it) |
| No outside loads | no CDN, font or script from elsewhere; a browser test fails on any request to another origin |
| Disable it | `server.adminUI: false` removes the page and the session endpoints; the Bearer API used by the command stays |

The admin listener is bound to the loopback of the **host** by the shipped Compose file (`127.0.0.1:${ADMIN_PORT}:9090`). Inside the container it listens on all interfaces, as the published port requires; the server cannot see how you publish it, so keeping it on loopback is your responsibility.

## The admin API

All under `/admin/api/v1/`, JSON, `Authorization: Bearer <ADMIN_TOKEN>` (or the session cookie for the UI). Errors are `{"error": {"code", "title", "detail", "fix", "more"}}` with the [catalogue](error-codes.md) code.

| Call | Purpose |
|---|---|
| `GET status` | version, readiness, start time, pending changes, fallback reason, repositories being served |
| `GET repositories`, `GET repositories/{id}` | repositories (no secrets) |
| `POST repositories`, `PUT repositories/{id}` | validate and save; the body may carry `clientSecret` (write-only) |
| `DELETE repositories/{id}[?allowInUse=true]` | detach; documents are kept |
| `POST repositories/{id}/check` | access test of one repository |
| `GET detached` | detached repositories (documents kept) |
| `POST apply`, `POST apply/cancel` | drain and restart; cancel during the drain |
| `GET certificates`, `POST certificates/activate`, `POST certificates/deactivate` | SAP certificates (the older `/admin/contentserver/certificates` endpoints keep working) |
| `GET diagnostics`, `GET logs?tail=&level=&requestId=`, `GET audit?lines=`, `GET settings` | the read-only views |
| `GET/POST/DELETE session` | the browser session (login, probe, logout) |

## Testing the page yourself

`scripts/ui-smoke.mjs` drives a headless Chromium-based browser over the DevTools protocol with Node only: it logs in (a wrong token first), adds a repository through the real form, tests access, visits every view, edits, detaches and logs out, and fails on console errors, CSP violations, exceptions or any request to another origin. Run it against a disposable server:

```bash
ZD_TOKEN=<ADMIN_TOKEN> ZD_BASE=http://127.0.0.1:9090 ZD_BROWSER=/usr/bin/google-chrome node scripts/ui-smoke.mjs
```

It is not wired into CI yet. A pass against a real Docker host behind a TLS reverse proxy, and the independent security review, are still to be done.
