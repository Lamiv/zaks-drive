# The zaks-drive command

`zaks-drive` installs, configures, starts, stops, updates and troubleshoots a ZAKS Drive deployment that runs under Docker Compose, without you editing Compose files or inspecting containers by hand. It is meant for SAP Basis administrators, Linux engineers and contributors, and it works in scripts.

> **Two programs, two names.** `zaks-drive` is the **management command** you run on the host. The **server** is a separate program, `zaks-drive-server`, that runs inside the container. Inside the image `/zaks-drive` and `/adapter` still exist as compatibility names of the *server*, so `docker compose exec adapter /zaks-drive check` keeps working. See [branding and compatibility](development/branding-and-compatibility.md#executable-names).

## Contents

[Install](#install) · [First-time setup](#first-time-setup) · [Start, stop, status](#start-stop-restart-and-status) · [Logs](#logs) · [Health checks: doctor](#health-checks-doctor) · [Repositories](#repositories-several-storage-locations) · [SAP troubleshooting](#sap-troubleshooting) · [Microsoft Graph connectivity](#microsoft-graph-connectivity) · [Updates and rollback](#updates-and-rollback) · [Support bundles](#support-bundles) · [Security](#security-considerations) · [Uninstall](#uninstall) · [Reference](#reference)

## Install

Requirements: Linux (amd64 or arm64), Docker Engine and the Docker Compose v2 plugin. **The installer does not install Docker and never uses sudo.**

```bash
curl -fsSLO https://github.com/Lamiv/zaks-drive/releases/download/v0.1.0-beta.1/install.sh
less install.sh                          # read it before you run it
sh install.sh --version 0.1.0-beta.1     # pin a release
```

```text
Installing ZAKS Drive 0.1.0-beta.1 for linux/amd64 into /home/basis/.local
Downloading https://github.com/Lamiv/zaks-drive/releases/download/v0.1.0-beta.1/SHA256SUMS
Downloading https://github.com/Lamiv/zaks-drive/releases/download/v0.1.0-beta.1/zaks-drive_0.1.0-beta.1_linux_amd64.tar.gz
Checksum OK (SHA-256 matches SHA256SUMS).
Tip: add --verify-attestation to also verify the GitHub build attestation.
Installed:
  /home/basis/.local/bin/zaks-drive                       management command
  /home/basis/.local/share/zaks-drive/0.1.0-beta.1/       server binary and deployment templates
Docker: found, and your user can use it.
Docker Compose v2: found.
Next: zaks-drive setup     (see docs/cli.md)
```

What the installer guarantees:

| Step | Behaviour |
|---|---|
| Platform | Linux only; `x86_64` selects amd64, `aarch64` selects arm64; anything else stops with exit 3. |
| Download | HTTPS only (TLS 1.2 or newer). The version is pinned with `--version`; plain `latest` is allowed but the installer prints the version it resolved so you can pin it. GitHub's `latest` never points at a pre-release, so while only pre-releases exist you must pin. |
| Verification | The archive is checked against `SHA256SUMS` **before it is opened or run**. A missing entry, a malformed entry or a mismatch aborts with exit 6 and installs nothing. |
| Signature | With `--verify-attestation` (needs the GitHub CLI) it also runs `gh attestation verify` against the project repository. This is off by default because it needs `gh`; without it, integrity rests on HTTPS plus the checksum manifest of the same release. |
| Archive safety | Absolute paths, `..`, links, device files and members outside the release directory are refused before extraction. |
| Location | `--prefix DIR` (default `/usr/local` if you can write there, otherwise `~/.local`). If the location is not writable it stops with exit 7 and tells you to pick a prefix you own. |
| Secrets | None are written. No tenant settings are embedded or asked for. |
| Docker | Reported only. Missing pieces are explained with the official install link. |

Other options: `--verify-only` (download and verify, install nothing), `--base-url` (a mirror; HTTPS), `--no-docker-check`. To verify `install.sh` itself, compare it with the entry in `SHA256SUMS` or verify its attestation (see [Verify the download](#verify-a-download-yourself)).

Building the command yourself needs only Go: `go build -o zaks-drive ./cmd/zaks-drive`. No credentials are needed.

## First-time setup

```bash
zaks-drive setup
```

The wizard asks for the deployment directory (default `/opt/zaks-drive`), the bind address and ports, the Microsoft Entra tenant and client IDs, the client secret (typed without echo), the SharePoint site, library and folder, the SAP interface and its content repository IDs, allowed networks, TLS and the log level. It then shows a summary **without secrets**:

```text
Configuration written to /opt/zaks-drive (secrets are not shown):
  .env           mode 600  (Microsoft credentials, API keys, admin token)
  config.yaml    mode 644  (no secrets; the container must be able to read it)
  Listens on     10.20.0.5: REST 8080, SAP 8090; admin 127.0.0.1:9090
  Interfaces     rest, contentrepo
  Tenant         11111111-2222-3333-4444-555555555555
  Client         66666666-7777-8888-9999-aaaaaaaaaaaa (secret: set, 40 characters, not shown)
  SharePoint     https://contoso.sharepoint.com/sites/SAP_DOCS, library "Documents", folder "SAP_DMS"
  SAP repository Z1 (signature: required)
  WARNING: no allowed networks set: any host that can reach port 8090 may call the SAP interface
  TLS            not terminated here: put a TLS reverse proxy in front for production
  API keys       generated (3 values in .env: ADAPTER_API_KEY_S4, ADAPTER_API_KEY_FIORI, ADMIN_TOKEN)
  Image          image zaks-drive:0.1.0-beta.1 built from the release payload
```

How it protects you:

- The candidate `.env` and `config.yaml` are first written to a private temporary directory and validated with **the server's own configuration loader**. If they are invalid (for example a site URL copied from a library page) nothing is written and the error code is shown.
- `.env` is written at mode `600`, atomically, so it is never briefly readable by others. `config.yaml` holds no secrets and is `644` because the non-root container must read it.
- API keys and the admin token are generated randomly (256 bits).
- An existing configuration is not replaced without `--force`; then the old files are backed up (the `.env` backup stays `600`) and existing keys are kept.
- The client secret may be given as a file or environment variable for automation, never as a command-line value.

### Non-interactive setup

```bash
printf '%s' "$ENTRA_CLIENT_SECRET" > /run/secrets/zd-secret     # a file you manage; never put it on the command line
zaks-drive setup --non-interactive --dir /opt/zaks-drive \
  --site-url https://contoso.sharepoint.com/sites/SAP_DOCS --library Documents --root-path SAP_DMS \
  --tenant-id 11111111-2222-3333-4444-555555555555 --client-id 66666666-7777-8888-9999-aaaaaaaaaaaa \
  --client-secret-file /run/secrets/zd-secret \
  --content-repo Z1:required --allowed-network 10.20.0.0/16 --bind 10.20.0.5
```

Flags: `--dir`, `--bind`, `--rest-port`, `--cs-port`, `--admin-port`, `--interfaces`, `--site-url`, `--library`, `--root-path`, `--tenant-id`, `--client-id`, `--client-secret-file` (or `ZAKS_DRIVE_CLIENT_SECRET`), `--content-repo ID[:required|optional|none]` (repeatable), `--allowed-network` (repeatable), `--tls`, `--log-level`, `--no-build`, `--version`, `--force`. Missing required values exit with code 2 and a list.

Check the result without starting anything:

```bash
zaks-drive config validate --dir /opt/zaks-drive
```

This checks the files with only `.env` as the environment, exactly as the container sees it. It contacts neither Microsoft nor SAP. Every setting is documented in the [configuration reference](configuration.md).

## Start, stop, restart and status

```bash
zaks-drive start --dir /opt/zaks-drive
```

```text
Waiting for ZAKS Drive to become ready (up to 2m0s)...
ZAKS Drive is ready.
  REST API            http://<this-host>:8080/api/v1
  SAP Content Server  http://<this-host>:8090/ContentServer/ContentServer.dll
  Health and metrics  http://127.0.0.1:9090  (localhost only)
```

`start` validates the configuration first (an invalid file stops it before Docker is touched), makes sure the image exists, runs `docker compose up -d`, and waits for `/readyz`. If the container exits or keeps restarting it stops waiting at once, prints the last sanitized log lines and exits 5. Port conflicts are recognised: *"a host port is already in use: change REST_PORT, CS_PORT or ADMIN_PORT in .env (Ubuntu's Cockpit uses 9090)"*.

- `zaks-drive stop`: stops the containers. **It never removes containers or volumes.**
- `zaks-drive restart`: restarts and waits for readiness again (use it after editing `.env` or `config.yaml`).
- `zaks-drive status` (`--json` for scripts):

```text
Deployment   /opt/zaks-drive (release)
CLI          0.1.0-beta.1
Installed    0.1.1-beta.1  (previous: 0.1.0-beta.1, rollback available)
Server       0.1.1-beta.1
Container    zaks-drive-adapter-1: running, healthy, 0 restarts, image zaks-drive:0.1.1-beta.1
Ready        yes
```

Only the containers that `docker compose` reports for **this deployment's compose file** are ever touched or inspected. Other containers on the host are never listed, stopped or changed.

The deployment directory is found in this order: `--dir`, `$ZAKS_DRIVE_DIR`, the current directory, `./deploy`, `/opt/zaks-drive`.

## Logs

```bash
zaks-drive logs                       # last 100 lines
zaks-drive logs --follow
zaks-drive logs --tail 200 --since 30m
zaks-drive logs --level warn --iface contentrepo
zaks-drive logs --request-id 3f2a9c1b7d4e5f60a1b2c3d4
```

Timestamps and request IDs are preserved. `--level` (debug, info, warn, error) and `--iface` (rest, contentrepo) filter structured lines; lines that are not structured (Docker's own messages) are always shown so errors are never hidden.

**Every line is sanitized before it is shown**, in two independent ways: the secret values of your `.env` are masked wherever they appear (also URL-encoded), and credential-shaped text is masked by pattern (bearer tokens, JWTs, `Authorization` headers, `secKey` and `tempauth` query values, `client_secret=`, JSON fields such as `"token"`). The server also avoids logging such data in the first place ([logging policy](operations/security.md#logging-policy)); the command does not assume it succeeded.

Writing logs to a file is an export and asks first:

```text
$ zaks-drive logs --tail 500 --output /tmp/zd.log
Log export warning: the file will contain operational data: SharePoint file and folder names, SAP document
IDs, client addresses and timing. Credentials, tokens, signed URLs and secKey values are masked, but masking is
a safety net, not a guarantee. Review the file before you share it, and store it as confidential.
Write the sanitized logs to /tmp/zd.log? [y/N]:
```

The file is created with mode `600` and an existing file is never overwritten. Without a terminal you must pass `--yes`.

## Health checks: doctor

```bash
zaks-drive doctor                  # local checks only
zaks-drive doctor --json           # machine-readable
zaks-drive doctor --connectivity   # also test Microsoft access (contacts Microsoft)
zaks-drive doctor --list           # every diagnostic and how it works
```

```text
[PASS   ] docker-compose      Docker Compose 2.29.0
[PASS   ] config-valid        config.yaml and .env load without errors
[WARN   ] repository-mapping  contentServer.allowedNetworks is empty: any host that reaches the SAP port may call it
                                fix: tighten the settings before production use
[PASS   ] readiness           ready: the server reached SharePoint with its credentials
[PASS   ] sap-endpoint        serverInfo answers and lists 1 configured repositories (SAP itself was not contacted)
[SKIPPED] graph-connectivity  not requested: this test contacts Microsoft and writes small test files to SharePoint

Summary: 13 passed, 3 warnings, 0 failed, 4 skipped
Not covered: the SAP side. Local checks cannot see inside SAP. In SAP use OAC0 (connection test),
STRUST (compare the certificate fingerprint) and the reports RSCMSTH0 to RSCMSTH3.
```

Exit code 0 when nothing FAILED (1 otherwise; `--strict` also fails on warnings). A diagnostic whose input is missing is `SKIPPED` with the reason, never silently left out. Findings are PASS, WARN, FAIL or SKIPPED.

### The diagnostics

| ID | What it checks and how | FAIL when | WARN when |
|---|---|---|---|
| `os-platform` | OS and CPU architecture of the host running the command | | not Linux, or not amd64/arm64 |
| `docker-installed` | `docker version` | the `docker` command is missing | |
| `docker-daemon` | `docker version`, reading its error to tell a stopped daemon from a permission problem | the daemon is down, or your user may not use the socket | |
| `docker-compose` | `docker compose version` | the Compose v2 plugin is missing | |
| `deployment-files` | `docker-compose.yml`, `.env`, `config.yaml` in the deployment directory | a file is missing | |
| `file-permissions` | mode of `.env` and `tls/tls.key` | `.env` is readable by every user | `.env` readable by its group; TLS key readable by others |
| `config-valid` | loads `config.yaml` with the server's own loader, `.env` only as environment; contacts nothing | the loader reports errors (codes in [error codes](error-codes.md)) | |
| `api-keys` | length of API keys and admin token in `.env`; whether REST authentication is disabled | | a key under 32 characters, or authentication disabled |
| `repositories` | asks the server's admin API (`ADMIN_TOKEN` from `.env`, localhost only): changes waiting for a restart, fallback to the last working state, repositories defined but not served | the server fell back to the last working repository state | changes pending a restart, or a repository not served |
| `repository-mapping` | every SAP content repository maps to a repository; risky settings | a mapping points nowhere | signature `none`, or no `allowedNetworks` with the SAP interface enabled |
| `container-state` | `docker compose ps` for this deployment, restart counts via `docker inspect` of those IDs | stopped, unhealthy, or 3 or more restarts | not started, 1 or 2 restarts, health still starting |
| `container-resources` | OOM-kill flag and memory limit of the running container | killed for exceeding its memory limit | no memory limit |
| `ports` | if running: the expected ports are published; if not: tries to bind each host port | a port is in use by another process | the container does not publish an expected port |
| `health-endpoint` | `GET /healthz` on `127.0.0.1` (admin port) | no answer from a running container | |
| `readiness` | `GET /readyz`; a failure is classified as: Entra rejected the credentials, secret expired, token has no permissions, site grant missing, location not found, network, throttling | not ready | |
| `server-version` | `GET /version`, recorded installed version, compatibility policy | | different compatibility lines, or installed and running versions differ |
| `tls-certificate` | with TLS in the server: `tls/tls.crt` validity and that `tls.key` matches | expired, unreadable or mismatched | expires within 30 days |
| `sap-endpoint` | unsigned `serverInfo` on this host's Content Server port; compares the repositories it lists with `config.yaml` | no answer, or a repository is missing | refused because of `allowedNetworks` (expected) |
| `sap-certificates` | certificate list through the admin API (`ADMIN_TOKEN` from `.env`, localhost only); never prints certificate contents | an active certificate has expired | a repository that checks signatures has no active certificate yet |
| `disk-space` | free space of the file system holding the deployment | under 200 MiB | under 2 GiB |
| `graph-connectivity` | only with `--connectivity`: runs the server's `check` inside the container | a required SharePoint access is missing | |

Docker's own data directory and SAP are not inspected. The command reads only: the files of the deployment, the container list of its compose file, `docker inspect` of those containers, and `127.0.0.1` HTTP endpoints. It never runs `ss`, `lsof` or other host tools; where those would help it only suggests them.

## Repositories: several storage locations

A deployment can serve several repositories (`Z1`, `ZH`, `AD`, ...). Each one stores its documents either in **SharePoint / OneDrive** or in a **local folder** on the server, and can be exposed to SAP as a content repository of the same ID. The design is in [RFC 0001](rfcs/0001-repositories-and-storage-backends.md).

Repositories that you create with `zaks-drive repo` are **managed**: their definitions, their secrets and the documents of local repositories live in the server's own **state volume** (`/var/lib/zaks-drive`), never in `config.yaml`. Repositories written in `config.yaml` stay valid; they are **static** and read-only for these commands.

```bash
zaks-drive repo list                          # repositories, backend, source, SAP ID, state
zaks-drive repo add AD --backend local --sap  # a local-folder repository, exposed to SAP as AD
zaks-drive repo add ZH --backend sharepoint --site-url https://contoso.sharepoint.com/sites/hr \
    --library Documents --root-path SAP_HR --sap \
    --tenant-id <guid> --client-id <guid> --secret-file ./zh-secret     # its own Microsoft application
zaks-drive repo update AD --versions 10       # change only what you name
zaks-drive repo check ZH                      # test one repository (SharePoint: contacts Microsoft)
zaks-drive repo remove AD                     # detach: no longer served, documents are kept
```

| Command | What it does |
|---|---|
| `repo list [--json]` | Every repository with backend, source (`static` or `managed`), SAP content repository, state (`serving`, `pending restart`) and location; detached repositories are listed separately. |
| `repo show <id> [--json]` | The settings of one repository. Secrets are shown only as *set* or *NOT set*. |
| `repo add <id> --backend local\|sharepoint ...` | Creates a repository. The ID is **exactly 2 characters, A-Z or 0-9**. |
| `repo update <id> ...` | Changes the options you pass. `--global-app` returns to the global `graph.*` application, `--no-sap` stops exposing it to SAP. |
| `repo remove <id>` | **Detaches** the repository. Documents are **not** deleted, and adding it again serves them again. A repository exposed to SAP needs `--allow-in-use`. |
| `repo check <id>` | Tests one repository with the server's own code. Local: write, read, replace (a version is kept), delete, free disk space. SharePoint: the `check` steps for that repository, with its own application. |
| `repo purge <id> --plan` / `--execute` | Deletes the documents of a **detached local** repository. See [Purging](#purging-a-repository). |

**Options.** Local: `--path FOLDER` (below the data directory; default: the ID), `--versions N` (previous versions kept per file, 0 to 99, default 5). SharePoint: `--site-url`, `--library`, `--root-path`, `--user-id`, `--drive-id`, `--site-id`. Own Microsoft application: `--tenant-id`, `--client-id` and the secret from `--secret-file FILE`, `--secret-stdin` or a hidden prompt; **a secret is never accepted as a command-line value**. SAP: `--sap`, `--cont-rep ID` (default: the ID), `--signature required|optional|none`. Others: `--description`, `--read-only`, `--yes`, `--no-restart`, `--dir`.

**How a change is made.** The command does not edit files. When the server runs it calls the server's **admin API** (localhost, `ADMIN_TOKEN` from `.env`); when the server is stopped it runs the same operation in a **one-off container** of the same image and volume (`docker compose run --rm`). Either way the files are created by the server's own user, a secret is written as its own mode-600 file, and the server's own loader validates the result: an invalid change is rolled back and nothing is saved. Every change is recorded in `audit.log` in the volume (who, when, which repository; never a secret).

**Applying a change.** A change takes effect by a **restart through maintenance mode**: new requests get `503` with `Retry-After` (SAP retries those), running requests finish for up to `server.drainTimeout` (default 30 s), then the server ends and Docker starts it again with the new repositories. Requests that outlast the drain are cut and their request IDs are logged. `--no-restart` leaves the change pending; `status` and `doctor` then report "changes pending restart". If the new definitions do not validate at start, the server keeps serving the **last working state** and says so in `status`, `doctor` and the log.

**First repository.** With the supplied compose files a server with no repository yet starts, reports `not ready: no repository is configured yet` and waits. Add the first one with `zaks-drive repo add` (a one-off container is used while the server has nothing to serve) and restart.

**Requirements.** The admin API needs `ADMIN_TOKEN` in `.env` (long and random). Without it the change is still saved through the one-off container, but it cannot be applied with a drained restart: use `zaks-drive restart`.

### Local repositories

The documents are files in a folder of the state volume. Good to know:

- One server instance owns the folder; there is no replication, locking across hosts or NFS guarantee. **Back up the `state` volume**, and use an encrypted volume or disk if you need encryption at rest.
- Every operation is confined to the repository's folder; links inside it are ignored.
- On replace, the previous contents are kept as versions (0 to 99, `--versions`).
- Search matches file names only; there are no thumbnails and no redirect downloads. Unsupported thumbnails answer `THUMBNAIL_UNAVAILABLE`.
- Names are limited to 255 bytes; the SharePoint rule of 400 characters for a whole path does not apply.

### Purging a repository

`repo remove` only detaches. **`repo purge` deletes the documents** and is deliberately hard to do:

- **SAP has no purge.** SAP keeps references (DMS originals, archive links, attachments). Purging the data behind them leaves SAP pointing at documents that no longer exist, and other repositories integrated with that data can become unstable. This cannot be repaired from here, and **you take full responsibility**.
- It exists **only** in this command on the Docker host, never in the admin API or a UI, and only for **detached local** repositories. SharePoint content is never deleted by ZAKS Drive.
- The repository must already be detached and the server restarted without it; the command refuses while the server still serves it or changes are pending.
- `repo purge <id> --plan` counts what would be deleted and prints a **challenge phrase** valid for 15 minutes and bound to exactly that state of the folder. `repo purge <id> --execute --i-understand-this-destroys-documents` then asks you to type the repository ID and the phrase (without a terminal: `--typed-id` and `--challenge`). If anything in the folder changed since the plan, or the phrase expired, it is refused.
- Deletion stays inside the repository's own folder and removes links without following them. Plan and purge are written to the audit log. There is no `--yes`, no environment variable and no setting that skips these steps.

## SAP troubleshooting

`doctor` covers what is visible from the Linux host: the endpoint answers, the repository IDs match, the SAP signing certificate was received and activated, the certificate has not expired. It **cannot** see inside SAP. For the SAP side:

| Question | Where to look in SAP |
|---|---|
| Does SAP reach the server? | OAC0, select the repository, connection test |
| Is the signing certificate the right one? | STRUST (System PSE): compare its SHA-256 fingerprint with `sap-certificates` / `zaks-drive-server certs list` |
| Do the document commands work? | SE38: reports `RSCMSTH0` to `RSCMSTH3`, `RSCMSTA0`, `RSCMSTAC` |
| Why was a request refused? | `zaks-drive logs --iface contentrepo --level warn`; look the `code=` up in [error codes](error-codes.md) |

More in [SAP troubleshooting](sap/troubleshooting.md) and [OAC0 and certificates](sap/oac0-configuration.md). Activating a certificate is a server command run inside the container:

```bash
docker compose -f /opt/zaks-drive/docker-compose.yml exec adapter /zaks-drive-server certs list
docker compose -f /opt/zaks-drive/docker-compose.yml exec adapter /zaks-drive-server certs activate Z1 '<authId from the list>'
```

## Microsoft Graph connectivity

`zaks-drive doctor` tells you, without contacting Microsoft, what the server itself reports in `/readyz` and classifies it:

```text
[FAIL   ] readiness  Microsoft Entra rejected the credentials (secret, client ID or tenant ID) (not ready: TOKEN_FAILED ...)
                       fix: use the secret VALUE (not the secret ID) and check GRAPH_TENANT_ID and GRAPH_CLIENT_ID in .env
```

To test the access end to end (this **contacts Microsoft Entra ID and Microsoft Graph** with the app's credentials and creates and deletes small test files in the configured SharePoint folder):

```bash
zaks-drive check
```

```text
  PASS  Credentials configured
  PASS  Access token from Microsoft Entra ID
  PASS  Application permission in the token
  PASS  [DMS] Find the library
  PASS  [DMS] Write: upload a small file
  PASS  [DMS] Read: download and compare
  PASS  [DMS] Resumable upload session (files over 4 MiB)
  PASS  [DMS] Version history
  PASS  [DMS] Delete the test file
  passed 10, failed 0, skipped 0
  All required access is in place.
```

`check` runs the server's `check` inside the running container and prints its output sanitized; its exit code is the server's. Setup of the Microsoft side: [Entra app registration](microsoft-365/entra-app-registration.md), [Graph permissions](microsoft-365/graph-permissions.md).

## Updates and rollback

```bash
zaks-drive update --check                  # compare versions; changes nothing
zaks-drive update --version 0.1.1          # update to an explicit version
zaks-drive rollback                        # return to the previous version
```

`update` is explicit about what it does:

1. Resolves the target (`--version`, or the latest release; pre-releases must be named).
2. Compares it with the installed version and applies the [compatibility policy](#compatibility-policy). A move to **another compatibility line**, or a **downgrade**, needs `--allow-breaking` in scripts (an interactive prompt otherwise). `--yes` alone is not enough for those.
3. Downloads `SHA256SUMS` and the archive over HTTPS and **verifies the checksum before opening the archive** (exit 6 on any problem; nothing is changed).
4. Unpacks it with strict rules (only regular files below the release directory; no links, `..` or absolute paths; known file names) and checks that `release.json` and `VERSION` say the requested version.
5. Builds the image `zaks-drive:<version>` from the verified server binary (the image label records the version and the source revision).
6. Restarts on the new image, waits for readiness, and **returns to the previous version automatically** if it does not become ready.

```text
Downloading release 0.1.1 and verifying its checksum...
Checksum OK (SHA-256 abd5fa7138fd5e19f1744f558ad83e2cae989e7021f990930cb567359b08b39f).
Building the image zaks-drive:0.1.1...
Restarting on 0.1.1...
zaks-drive: the new version did not become ready: the container zaks-drive-adapter-1 keeps restarting (6 restarts)
Going back to 0.1.0...
zaks-drive: the update to 0.1.1 failed and ZAKS Drive is running 0.1.0 again; configuration and data were not changed
```

What is preserved: `.env`, `config.yaml`, the `spool` volume and every document in SharePoint are never touched. No images or volumes are ever deleted. There is **no automatic data migration**; the release notes say if a version needs one.

Exit codes of a failed update: `1` the update failed and the previous version runs again; `5` neither version became ready; `6` a download failed verification; `7` approval was missing.

### Compatibility policy

- The management command and the server of one release have the same version, but they are separate programs and need not be identical.
- A **compatibility line** is `MAJOR` for versions 1.0.0 and later, and `MAJOR.MINOR` for `0.y.z` (before 1.0 a minor release may change interfaces).
- The command manages servers on its own line. Another line is a *breaking* move: `doctor` warns, `update` asks for approval. Update the command first with `install.sh --version <version>`; the command is not replaced by `update`.
- Development builds (`dev`) cannot be compared; the command says so and does not guess.

### Pinning images

Releases are built into an image on your host, so the image tag is your own build of a **verified** server binary. To pin the base image too, set a digest: `ZAKS_DRIVE_BASE_IMAGE=gcr.io/distroless/static-debian12@sha256:<digest> zaks-drive update ...`. The image ID is printed after the build and the label `org.opencontainers.image.revision` ties the image to the source commit in `release.json`. No image is published to a registry yet, so there is no registry digest to pin; when one is, it will be documented here.

## Support bundles

```bash
zaks-drive support bundle
```

```text
The support bundle will contain:
    README.txt           what the bundle contains and does not contain
    versions.json        command, installed and running versions
    system.json          OS, architecture, Docker versions, free disk space
    status.json          container states, restart counts, readiness
  * doctor.json          local diagnostics report (no Microsoft test)
    config-summary.json  allowlisted configuration summary
  * logs.txt             last 1000 log lines, credentials masked

It will NOT contain .env, config.yaml, environment variables, secrets, API keys, tokens, certificates,
private keys, documents, or SharePoint site, library and folder names.
* These files can contain operational identifiers (SharePoint file names, SAP document IDs, client addresses).
  Credentials are masked, but read them before you share the archive.
Collect this information and write the archive? [y/N]:
```

The bundle is built from **allowlisted fields**, not by copying files and hiding secrets: `config-summary.json` is generated from typed values (interfaces, ports, repository IDs, signature modes, counts) and omits tenant and client IDs, hosts, site, library and folder names and address lists. No environment, mounts or `docker compose config` output is ever read. Logs are sanitized, and a final check **refuses to write the archive** if any secret value of the deployment is still in it. The archive is created with mode `600`, an existing file is never overwritten, and nothing is uploaded: you decide whether to attach it to an issue.

Options: `--output FILE`, `--logs-tail N`, `--no-logs`, `--yes`.

## Security considerations

The command is a privileged operational tool. What it does about that:

| Risk | Control |
|---|---|
| Secrets on screen, in logs or in files | Never printed; setup output shows only counts. Logs and `check` output are sanitized by value and pattern; the bundle is allowlisted and fails closed. Secrets reach the command only through a file, the environment or a hidden prompt. |
| World-readable secret files | `.env` is created `600` atomically; backups keep that; `doctor` fails if `.env` is readable by others. |
| Shell injection | Docker and `stty` are started with explicit argument lists, never through a shell. Versions, tags and revisions are validated before they reach an argument. |
| Operating outside the deployment | Every Compose command passes the deployment's compose file explicitly; container IDs for `docker inspect` come only from that file's `ps`. Other containers are never listed or touched. |
| Destructive operations | `stop` never removes containers or volumes. No command deletes volumes or images. Updates, rollbacks, log exports and bundles ask first (or need `--yes`). Moves to another compatibility line need `--allow-breaking`. |
| Unverified downloads | Archives are verified against `SHA256SUMS` before use; the extraction allowlist refuses links and path tricks; HTTPS is required (HTTP only for `localhost`). |
| Privilege escalation | None. The installer and the command never call `sudo`; they print what you would need to do. |
| Telemetry | None. The command contacts the network only for `update` (the release location) and, when you ask, for `check` and `doctor --connectivity` (Microsoft, through the server). |
| Local diagnostics versus remote tests | `doctor` talks only to Docker and `127.0.0.1`. Microsoft is contacted only by `check` or `doctor --connectivity`, which say so. |

Limits: sanitizing is a safety net, not a proof; sharing logs shares business identifiers. Release integrity rests on HTTPS and the checksum manifest unless you verify the build attestation. The Microsoft-side secret and the Entra app are outside the command's reach.

### Verify a download yourself

```bash
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify zaks-drive_0.1.0-beta.1_linux_amd64.tar.gz --repo Lamiv/zaks-drive
```

## Uninstall

There is deliberately no `uninstall` command: removal is destructive and you should see each step. Documents live in SharePoint and are **not** affected by any step below; the container holds only the temporary `spool` volume.

```bash
zaks-drive stop --dir /opt/zaks-drive                              # 1. stop (keeps everything)
docker compose -f /opt/zaks-drive/docker-compose.yml down          # 2. remove the containers (keeps the volume)
docker compose -f /opt/zaks-drive/docker-compose.yml down -v       #    optional: also the spool volume (temporary upload data)
docker image rm zaks-drive:0.1.0-beta.1                            # 3. remove an image you no longer need (compose needs ZAKS_DRIVE_VERSION set)
rm -r /opt/zaks-drive                                              # 4. the deployment directory: this deletes .env and config.yaml
rm ~/.local/bin/zaks-drive && rm -r ~/.local/share/zaks-drive      # 5. the command and the release payloads
```

Also: delete or rotate the client secret in Microsoft Entra, and remove the repository in SAP (OAC0). Back up `.env` and `config.yaml` first if you may need them.

## Reference

### Commands

| Command | Purpose |
|---|---|
| `help`, `version` | help; command, installed and running versions (`--json`) |
| `setup` | create or update a deployment (interactive or `--non-interactive`) |
| `config validate` | validate the files without starting anything |
| `start`, `stop`, `restart`, `status` | lifecycle (`status --json`) |
| `logs` | sanitized logs (`--follow`, `--tail`, `--since`, `--level`, `--iface`, `--request-id`, `--output`) |
| `doctor` | diagnostics (`--json`, `--connectivity`, `--strict`, `--list`) |
| `check` | the server's SharePoint access checklist (contacts Microsoft) |
| `repo` | `list`, `show`, `add`, `update`, `remove`, `check`, `purge`: manage repositories ([details](#repositories-several-storage-locations)) |
| `update`, `rollback` | verified updates with automatic rollback |
| `support bundle` | sanitized diagnostics archive |

### Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | a check or operation failed |
| 2 | bad command line or invalid answers |
| 3 | Docker or Docker Compose missing or not usable |
| 4 | no deployment found, or it is not configured |
| 5 | started but not ready |
| 6 | a download failed verification |
| 7 | declined, or approval missing (non-interactive without `--yes` or `--allow-breaking`) |

### Environment variables

| Variable | Meaning |
|---|---|
| `ZAKS_DRIVE_DIR` | default deployment directory |
| `ZAKS_DRIVE_SHARE` | where release payloads live (default `<prefix>/share/zaks-drive`) |
| `ZAKS_DRIVE_VERSION` | forces the image tag Compose uses (normally taken from the recorded state) |
| `ZAKS_DRIVE_CLIENT_SECRET` | the client secret for non-interactive `setup` (a file is safer) |
| `ZAKS_DRIVE_BASE_IMAGE` | base image (pin it by digest) for image builds |
| `ZAKS_DRIVE_RELEASE_BASE_URL` | release location for `update` (HTTPS, or HTTP for localhost) |

### Files

```text
<deployment>/docker-compose.yml     compose file (release layout; the repository's deploy/ is the source layout)
<deployment>/.env                   secrets and ports                      mode 600
<deployment>/config.yaml            settings, no secrets                   mode 644
<deployment>/tls/                   tls.crt and tls.key when the server terminates TLS
<deployment>/.zaks-drive/state.json installed and previous version, no secrets
<prefix>/bin/zaks-drive             the management command
<prefix>/share/zaks-drive/<ver>/    verified release payload (server binary, templates)
```

### Source checkouts

In a checkout of the repository, `deploy/` already is a deployment directory (its compose file builds from source). `zaks-drive start --dir deploy` then runs `docker compose up -d --build`. `setup --force` writes only `.env` and `config.yaml` there and leaves the compose file alone; `update` and `rollback` refuse to manage a source checkout (use git). Design notes for contributors: [CLI design](development/cli-design.md).
