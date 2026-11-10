# CLI reference

Every command and flag of the **`zaks-drive`** management command (run on the host) and of the **`zaks-drive-server`** program (run in the container). A test keeps this page complete: it fails if a flag is added to a command and not listed here. For walk-throughs see the [setup and usage guide](guide.md); for the reasoning, the [command guide](cli.md).

Conventions: flags may be written `-flag` or `--flag`; `--flag=value` and `--flag value` both work. Flags marked *repeatable* may be given several times. **A secret is never accepted as a flag value**; secrets come from a file, standard input, an environment variable or a hidden prompt.

## Common to every command that works on a deployment

| Flag | Meaning |
|---|---|
| `--dir DIR` | The deployment directory (the one holding `docker-compose.yml`). Default: `$ZAKS_DRIVE_DIR`, then the current directory, `./deploy`, `/opt/zaks-drive`, whichever holds a `docker-compose.yml`. |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | a check or an operation failed |
| 2 | bad command line |
| 3 | Docker or Docker Compose missing or not usable |
| 4 | no deployment found, or it is not configured |
| 5 | started, but did not become ready |
| 6 | a downloaded artifact failed verification |
| 7 | declined: you answered no, or there is no terminal and no `--yes` |

## zaks-drive

### help, version

| Command | Purpose |
|---|---|
| `zaks-drive help`, `-h`, `--help` | the command list |
| `zaks-drive version [--dir DIR] [--json]` | the version of the command, the installed release and the running server |
| `zaks-drive -v`, `--version` | the version of the command |

### setup

Create or update a deployment: validate, then write `.env` (mode 600) and `config.yaml`, and build the image from the verified release payload. Interactive unless `--non-interactive`.

| Flag | Meaning | Default |
|---|---|---|
| `--dir DIR` | deployment directory | `/opt/zaks-drive` |
| `--non-interactive` | do not prompt; take everything from flags and environment | |
| `--force` | replace an existing configuration (backups are kept) | |
| `--no-build` | do not build the image from the release payload | |
| `--version V` | release version to install | the version of this command |
| `--bind ADDR` | host address for the REST and SAP ports | `0.0.0.0` |
| `--rest-port N` | host port of the REST API | `8080` |
| `--cs-port N` | host port of the SAP Content Server interface | `8090` |
| `--admin-port N` | host port of the admin and health endpoint (bound to `127.0.0.1`) | `9090` |
| `--interfaces LIST` | comma-separated: `rest`, `contentrepo` | both |
| `--site-url URL` | SharePoint site address, e.g. `https://contoso.sharepoint.com/sites/SAP_DOCS` | |
| `--library NAME` | document library display name | `Documents` |
| `--root-path PATH` | folder inside the library | `SAP_DMS` |
| `--tenant-id GUID` | Microsoft Entra directory (tenant) ID | |
| `--client-id GUID` | Microsoft Entra application (client) ID | |
| `--client-secret-file FILE` | file holding the client secret **value** (or set `ZAKS_DRIVE_CLIENT_SECRET`, or type it when prompted) | |
| `--content-repo ID[:MODE]` | SAP content repository, `MODE` is `required`, `optional` or `none`; *repeatable* | `Z1:required` |
| `--allowed-network CIDR` | CIDR or IP allowed to call the SAP interface; *repeatable* | any |
| `--tls` | ZAKS Drive terminates TLS itself (put `tls.crt` and `tls.key` into `<dir>/tls`) | |
| `--log-level L` | `debug`, `info`, `warn` or `error` | `info` |

### config validate

`zaks-drive config validate [--dir DIR]`: check `.env` and `config.yaml` with the server's own loader, with only `.env` as the environment. Contacts neither Microsoft nor SAP. Repositories that live only in the state volume are not read here (use `repo list`).

### start, stop, restart

| Command | Flags | Purpose |
|---|---|---|
| `zaks-drive start` | `--dir`, `--no-wait`, `--timeout D` | start and wait until ready (`--timeout` default `2m`; `--no-wait` returns at once) |
| `zaks-drive stop` | `--dir` | stop; containers and data are kept |
| `zaks-drive restart` | `--dir`, `--no-wait`, `--timeout D` | stop and start; **not drained**: running requests are cut. Use `repo`/the admin page for a drained restart |

### status

`zaks-drive status [--dir DIR] [--json]`: containers and health, readiness and why not, versions, repositories (when the server's admin API is reachable), pending repository changes, and a warning if the server runs the last working state. Exit 5 when not started or not ready.

### logs

Sanitized logs: secrets are masked by value and by pattern.

| Flag | Meaning |
|---|---|
| `--dir DIR` | deployment directory |
| `--tail N` | lines from the end (`0` = all; default `100`) |
| `-f`, `--follow` | follow the output |
| `--since D` | only logs newer than this, e.g. `30m` or `1h` |
| `--level L` | minimum severity: `debug`, `info`, `warn`, `error` |
| `--iface I` | only one interface: `rest` or `contentrepo` |
| `--component I` | same as `--iface` |
| `--request-id ID` | only the lines of one request |
| `--output FILE` | write the sanitized logs to a file instead of the terminal (asks first) |
| `--yes` | confirm exporting to a file without asking |

### doctor

`zaks-drive doctor`: 22 diagnostics, each PASS, WARN, FAIL or SKIPPED with the fix. Talks only to Docker and `127.0.0.1`.

| Flag | Meaning |
|---|---|
| `--dir DIR` | deployment directory |
| `--json` | machine-readable output |
| `--connectivity` | also test Microsoft access (contacts Microsoft, writes small test files) |
| `--strict` | exit 1 on warnings too |
| `--list` | list every diagnostic and how it works |

### check

`zaks-drive check [--dir DIR]`: run the server's access checklist inside the container (contacts Microsoft Entra ID and Microsoft Graph; creates and removes small test files in the configured folder). Each Microsoft application (the global one and any a repository brings) is tested as its own group. For one repository use `repo check`.

### repo

Manage repositories. Without a running server (or without `ADMIN_TOKEN`) the operations run in a one-off container of the same image and volume. Ids of managed repositories are **exactly 2 characters, A-Z or 0-9**. See [Repositories](cli.md#repositories-several-storage-locations).

| Command | Purpose |
|---|---|
| `repo list [--dir DIR] [--json]` | repositories with backend, source (`static` or `managed`), SAP content repository, state and location; detached repositories separately |
| `repo show ID [--dir DIR] [--json]` | the settings of one repository; secrets only as *set* or *NOT set* |
| `repo add ID --backend local\|sharepoint [options]` | create a repository |
| `repo update ID [options]` | change only the options you pass |
| `repo remove ID [--dir DIR] [--allow-in-use] [--yes] [--no-restart]` | **detach**: no longer served, documents kept. `--allow-in-use` is required for a repository exposed to SAP |
| `repo check ID [--dir DIR]` | access test of one repository |
| `repo purge ID --plan \| --execute [options]` | delete the documents of a **detached local** repository ([guide](guide.md#scenario-12-retire-a-repository-detach-re-add-purge)) |

Options of `repo add` and `repo update` (`--global-app` and `--no-sap` exist for `update` only):

| Flag | Meaning |
|---|---|
| `--dir DIR` | deployment directory |
| `--backend B` | `local` or `sharepoint`; required for `add`. **Chosen once**: `update` refuses to change it, because documents are not moved between the two |
| `--description TEXT` | free text |
| `--read-only` | refuse writes |
| `--path FOLDER` | local: folder below the data directory (default: the ID) |
| `--versions N` | local: previous versions kept per file, 0 to 99 (default 5) |
| `--site-url URL` | SharePoint site address |
| `--library NAME` | SharePoint: document library name |
| `--root-path PATH` | SharePoint: folder inside the library |
| `--user-id UPN` | SharePoint: use this user's OneDrive |
| `--drive-id ID`, `--site-id ID` | SharePoint: address the drive or site directly |
| `--tenant-id GUID`, `--client-id GUID` | an own Microsoft Entra application for this repository (both or neither) |
| `--secret-file FILE` | file holding the client secret value |
| `--secret-stdin` | read the client secret from standard input |
| `--ask-secret` | prompt for the client secret (hidden) |
| `--global-app` | `update`: return to the global `graph.*` application (removes the stored secret) |
| `--sap` | expose the repository to SAP |
| `--cont-rep ID` | SAP content repository ID (default: the repository ID) |
| `--signature M` | `required`, `optional` or `none` |
| `--no-sap` | `update`: stop exposing the repository to SAP |
| `--yes` | do not ask for confirmation |
| `--no-restart` | save the change but leave it pending instead of restarting |

Options of `repo purge`:

| Flag | Meaning |
|---|---|
| `--dir DIR` | deployment directory |
| `--plan` | inspect what would be deleted and get a challenge phrase (valid 15 minutes); deletes nothing |
| `--execute` | delete, using the phrase of a current plan |
| `--typed-id ID` | the repository ID, typed again (otherwise asked for on a terminal) |
| `--challenge PHRASE` | the phrase printed by `--plan` (otherwise asked for on a terminal) |
| `--i-understand-this-destroys-documents` | required acknowledgement for `--execute` |

### admin

`zaks-drive admin reset-password [--dir DIR] [--yes] [--no-restart]`: return the admin page login to `admin` / `admin` (to be changed at once). Creates the file `NO_ADMIN` in the state volume through a one-off container and restarts the server through its drain; the server deletes the file when the new password is set. `--no-restart` only records the request. See [Admin UI password](cli.md#admin-ui-password).

### update, rollback

| Command | Flags |
|---|---|
| `zaks-drive update` | `--dir`, `--check` (compare versions only), `--version V` (default: the latest release; pre-releases must be named), `--yes`, `--allow-breaking` (another compatibility line, or a downgrade), `--timeout D` (default `2m`), `--base-url URL` (release location; `https://` only; default the project's GitHub releases) |
| `zaks-drive rollback` | `--dir`, `--yes`, `--timeout D` |

Archives are verified against `SHA256SUMS` before they are opened; a failed update rolls back automatically.

### support bundle

`zaks-drive support bundle [--dir DIR] [--output FILE] [--logs-tail N] [--no-logs] [--yes]`: a sanitized archive for a bug report. It is built from allowlisted fields (repositories appear as rows without locations or application IDs), fails closed if a secret is found, and never overwrites an existing file. `--logs-tail` default `1000`; the default output is `./zaks-drive-support-<time>.tar.gz`.

## zaks-drive-server

Runs in the container. Inside the image `/zaks-drive` and `/adapter` are compatibility names of the same program. Run these through `docker compose exec adapter /zaks-drive-server ...` (set `ZAKS_DRIVE_VERSION` when you call `docker compose` yourself).

| Command | Purpose |
|---|---|
| `zaks-drive-server [-config FILE]` | run the server (the default; `serve` does the same). The file comes from `-config` or `CONFIG_FILE` |
| `zaks-drive-server check` | the access checklist (what `zaks-drive check` runs) |
| `zaks-drive-server certs list` | SAP certificates received and their state |
| `zaks-drive-server certs activate CONTREP 'AUTHID'` | activate a certificate (compare its fingerprint with STRUST first) |
| `zaks-drive-server certs deactivate CONTREP 'AUTHID'` | revoke it |
| `zaks-drive-server repos OPERATION` | the one-off repository operations the management command uses (JSON on standard input and output): `list`, `get ID`, `add`, `update ID`, `detach ID [--allow-in-use]`, `check ID`, `audit [N]`, `detached`, `admin-reset`, `purge-plan ID`, `purge ID --typed-id ID --challenge PHRASE --i-understand-this-destroys-documents`. You normally use `zaks-drive repo` instead |
| `zaks-drive-server errors` | print the [error catalogue](error-codes.md) as Markdown |
| `zaks-drive-server version`, `-version` | the version |
| `zaks-drive-server -healthcheck` | probe the local `/healthz` (the Docker health check) |

## Environment variables

| Variable | Used by | Meaning |
|---|---|---|
| `ZAKS_DRIVE_DIR` | `zaks-drive` | default deployment directory |
| `ZAKS_DRIVE_SHARE` | `zaks-drive` | where release payloads live (default `<prefix>/share/zaks-drive`) |
| `ZAKS_DRIVE_VERSION` | `zaks-drive`, Compose | forces the image tag (normally taken from the recorded state) |
| `ZAKS_DRIVE_CLIENT_SECRET` | `zaks-drive setup` | the client secret for non-interactive setup (a file is safer) |
| `ZAKS_DRIVE_BASE_IMAGE` | `zaks-drive setup`, `update` | base image for image builds (pin it by digest) |
| `ZAKS_DRIVE_RELEASE_BASE_URL` | `zaks-drive update` | release location (HTTPS, or HTTP for localhost) |
| `CONFIG_FILE` | server | path of `config.yaml` inside the container |
| `STATE_DIR` | server | the state directory (`/var/lib/zaks-drive` in the supplied Compose files) |
| `ADMIN_TOKEN` | server | Bearer token of the admin API (the command and scripts) |
| `ADMIN_URL` | `zaks-drive-server certs` | the admin endpoint it talks to (default `http://127.0.0.1:9090`) |
| `GRAPH_TENANT_ID`, `GRAPH_CLIENT_ID`, `GRAPH_CLIENT_SECRET` | server | the global Microsoft application |
| `ADAPTER_API_KEY_*` | server | REST API keys referenced from `config.yaml` |

All server settings and their environment overrides: [configuration reference](configuration.md).

## Files

```text
<deployment>/docker-compose.yml     compose file
<deployment>/.env                   secrets and ports                       mode 600
<deployment>/config.yaml            settings, no secrets                    mode 644
<deployment>/tls/                   tls.crt and tls.key when the server terminates TLS
<deployment>/.zaks-drive/state.json installed and previous version, no secrets

state volume, in the container at /var/lib/zaks-drive (written only by the server's user, 65532):
  repos.d/<ID>.yaml                 managed repository definitions, no secrets
  secrets/<ID>/client-secret        a repository's own client secret        mode 600
  data/<path>/                      documents of local repositories (and .versions/ inside each)
  detached/<ID>.yaml                definitions of detached repositories, no secrets
  last-good/repos.d/                the definitions of the last state that started
  admin.json                        admin page password hash (Argon2id)     mode 600
  NO_ADMIN                          present only while a password reset waits for the new password
  audit.log                         who changed what and when; never a value or a secret
```
