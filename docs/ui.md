# The admin UI

A small web page, served by the ZAKS Drive server itself, for the server-side parts of administration: repositories, SAP certificates, diagnostics, logs and the audit trail. It is a client of the same [admin API](#the-admin-api) that the `zaks-drive` command uses. The design is in [RFC 0002](rfcs/0002-server-admin-ui.md).

> **Status.** Built and tested (unit tests, and a browser run against the real server and a Docker container). **Not yet reviewed independently**: the project's release criteria require an independent review of the authentication and the admin endpoints before the UI is called production-ready. Until then, keep it on `127.0.0.1` behind an SSH tunnel, as shipped.

## Open it

The Compose file publishes the admin port on the host's loopback only:

```text
http://127.0.0.1:9090/admin/ui/        (the port is ADMIN_PORT in .env)
```

From another machine use an SSH tunnel (`ssh -L 9090:127.0.0.1:9090 <host>`) or a TLS reverse proxy that you operate. Do **not** publish the admin port on a public address: it also serves metrics and the certificate endpoints.

### Logging in

The UI is **on by default**. The login is the user **`admin`**:

1. **First login:** `admin` / `admin`. This well-known pair works **only to choose a new password**: the page shows nothing but the "choose a new password" form, and the server refuses every other call from that session. Pick at least 12 characters (a few unrelated words make a good passphrase; no special characters are required, and password managers work).
2. **After that** only your password works. Changing it ends every other session.
3. **Forgot it?** `zaks-drive admin reset-password` on the Docker host (or create an empty file `NO_ADMIN` in the state volume and restart): the login becomes `admin` / `admin` again to be changed at once, and the server deletes `NO_ADMIN` when the new password is set. Details: [the command guide](cli.md#admin-ui-password).

The password is stored only as a salted Argon2id hash in the state volume (`admin.json`, mode 600), so the UI needs a state directory; without one nobody can log in. It is separate from the `ADMIN_TOKEN` in `.env`, which is the Bearer credential of the `zaks-drive` command and of scripts (the UI does not accept it as a password). Because `admin` / `admin` is public knowledge, **choose the password as soon as the server starts**, keep the admin port on `127.0.0.1`, and watch for the `admin-password` warning in `zaks-drive doctor`. If you do not want the page at all, set `server.adminUI: false`.

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
| Password | Argon2id (19 MiB, 2 passes) with a random salt, stored in a mode-600 file; at most two hash computations at a time; minimum 12 characters, no composition rules, obvious choices (`admin`, `password1234`, a repeated character, the old password) refused; verified in constant time with the same work for a wrong user name |
| Default password | `admin` / `admin` opens a session that may **only** change the password (every other call answers `ADMIN_PASSWORD_CHANGE_REQUIRED`); after the change all other sessions end and a fresh one starts; a corrupt `admin.json` locks the login instead of falling back to the default |
| Reset | needs write access to the state volume (the host); `NO_ADMIN` is removed by the server once a password is set, and a leftover file older than the last change is ignored |
| Session | cookie `HttpOnly`, `SameSite=Strict`, scoped to `/admin`, `Secure` when served over TLS; 30 minutes idle, 8 hours at most; held in memory (a restart ends it); at most 20 at a time |
| Guessing | after 10 wrong passwords or tokens the admin API answers `429` for a minute, and the lock **doubles with each repeated lockout** (up to 15 minutes) until a correct secret resets it; login, password change and Bearer calls share the counter; failed and successful logins and password changes are audited with the source address, never a value |
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
| `GET/POST/DELETE session`, `POST session/password` | the browser session (login with user name and password, probe, logout) and the password change (needs the session, the CSRF token and the current password) |

## Testing the page yourself

`scripts/ui-smoke.mjs` drives a headless Chromium-based browser over the DevTools protocol with Node only: it logs in (a wrong token first), adds a repository through the real form, tests access, visits every view, edits, detaches and logs out, and fails on console errors, CSP violations, exceptions or any request to another origin. Run it against a disposable server:

```bash
ZD_TOKEN=<ADMIN_TOKEN> ZD_BASE=http://127.0.0.1:9090 ZD_BROWSER=/usr/bin/google-chrome node scripts/ui-smoke.mjs
```

It is not wired into CI yet. A pass against a real Docker host behind a TLS reverse proxy, and the independent security review, are still to be done.
