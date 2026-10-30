# RFC 0001: Multiple repositories with local or SharePoint storage

- **Status:** draft
- **Author:** @Lamiv (with Claude)
- **Created:** 2026-10-30
- **Discussion:** the pull request that adds this file
- **Related:** [RFC 0002: server-embedded admin UI](0002-server-admin-ui.md)

> An RFC is a proposal for discussion. It is not a promise that the change will be built, scheduled or released. See [docs/rfcs/README.md](README.md).

## Summary

Make **the repository** a first-class, independently managed object. A deployment can hold any number of repositories (`Z1`, `ZH`, `AD`, ...). Each one chooses its own **storage backend**: a **local folder** on the host, or **SharePoint / OneDrive**, and a SharePoint repository carries its own site, library, root folder and, optionally, its own Microsoft Entra credentials. Repositories are created, changed and removed with `zaks-drive repo ...` (and, through [RFC 0002](0002-server-admin-ui.md), from the server's admin UI). Changes take effect by a restart. Existing configurations keep working unchanged.

## Motivation

- **Today** several SAP content repositories already work: `contentServer.repositories` accepts many entries (`Z1`, `Z2`, ...), each with its own signature mode and folder. But all of them share **one** SharePoint library, **one** set of Microsoft credentials, and one backend. Adding one means editing `config.yaml` by hand and restarting.
- **SAP landscapes need more than one repository.** Different document types, systems (DMS, ArchiveLink, GOS), departments or tenants usually get different OAC0 repositories (`ZH`, `AD`, ...).
- **Not every repository belongs in SharePoint.** Some stores must stay on local or mounted disk, for cost, residency or latency, or while a Microsoft 365 tenant is not available.
- **Different repositories may belong to different tenants or sites**, with different credentials. A shared global secret does not fit that.

## Vocabulary

Three words were used loosely before; this RFC fixes them.

| Term | Meaning | Where it appears |
|---|---|---|
| **Repository** | One named store with a backend, owned by this server. The new first-class object | `zaks-drive repo ...`, admin UI, REST path `/api/v1/repositories/{id}` |
| **Content repository ID** (`contRep`) | The ID created in SAP transaction OAC0 | SAP Content Server interface (`contRep=ZH`) |
| **Backend** | Where a repository's bytes live: `local` or `sharepoint` | repository settings |

A repository with `sap: {...}` settings is also exposed to SAP under its content repository ID (default: the repository ID). A repository without them is REST-only. Today's two config sections (`repositories` and `contentServer.repositories`) describe the same thing in two places; both stay valid ([migration](#migration)).

## Design

### Repository settings

One small file per repository, `repos.d/<id>.yaml`, holding **no secrets**:

```yaml
# repos.d/ZH.yaml
id: ZH                     # 1 or 2 letters/digits when exposed to SAP (the OAC0 ID); otherwise up to 32 of [A-Za-z0-9_-]
description: HR documents
backend: sharepoint        # sharepoint | local
readOnly: false

sharepoint:
  siteUrl: https://contoso.sharepoint.com/sites/HR     # or userId / siteId / driveId, as today
  library: Documents
  rootPath: SAP_HR
  # Optional: own Entra application. Omitted = the global graph.* credentials.
  tenantId: 11111111-2222-3333-4444-555555555555
  clientId: 66666666-7777-8888-9999-aaaaaaaaaaaa
  clientSecretFile: secrets/ZH/client-secret           # relative to the state directory

sap:                       # omit for a REST-only repository
  contRep: ZH              # default: the repository ID
  signature: required      # required | optional | none
  allowedNetworks: []      # optional, overrides contentServer.allowedNetworks for this repository
```

```yaml
# repos.d/AD.yaml
id: AD
description: Archive
backend: local
local:
  path: AD                 # below the data root (/var/lib/zaks-drive/data); see "Local backend"
  versions: 5              # previous versions kept per file (0 = none)
sap: { contRep: AD, signature: required }
```

Secrets are **never** in these files. A secret is a separate file (`secrets/<id>/client-secret`, mode 600) named by `clientSecretFile`, so repository settings can be shown, backed up and shared (with care) without exposing credentials, and the CLI, the sanitizer and the support bundle treat the `secrets/` directory as secret by construction.

### The state directory and who owns it

Repository definitions, their secrets and local data live in **one state directory** (`/var/lib/zaks-drive`, a named Docker volume by default):

```text
/var/lib/zaks-drive/
├── repos.d/<id>.yaml
├── secrets/<id>/client-secret          mode 600, owner = the server's user
├── data/<id>/...                        local backend contents (unless a host path is mounted, see below)
└── audit.log                            who changed which repository, when (no secrets)
```

**Why a volume owned by the server, not host files.** The container runs as a non-root user and its root file system is read-only. Host files mounted into it would have to be readable by that user, which forces either world-readable secrets or `chown` by an administrator. A volume owned by the server avoids both: secrets are created by the server process itself, at mode 600, and are never readable by other host users. The management command and the admin UI therefore **write through the server** (admin API, localhost only, `ADMIN_TOKEN`) instead of editing files:

- Server running: `zaks-drive repo ...` calls the admin API.
- Server stopped or not ready (first setup, a broken repository): the command runs the same operation in a **one-off container** from the same image and volume (`docker compose run --rm`), so the files are still created by the server's user. No process ever needs `sudo`.

`config.yaml` keeps the static settings (listeners, limits, API keys, global `graph.*`). It is still read-only to the container.

### Backend interface

`storage.Service` currently returns Graph types and calls the Graph client directly. It will depend on an interface instead (names indicative):

```go
type Backend interface {
    Kind() string                                   // "sharepoint" | "local"
    Ready(ctx) error                                // readiness of this repository
    Stat(ctx, id) / StatPath(ctx, path) (Item, error)
    List(ctx, path, top, cursor) (Page, error)
    Search(ctx, path, query, top, cursor) (Page, error)
    Create(ctx, path, body, size, conflict) (Item, error)
    Replace(ctx, id, ifMatch, body, size) (Item, error)
    Delete(ctx, id, ifMatch) error
    EnsureFolder(ctx, path) error
    Versions(ctx, id) ([]Version, error)
    Open(ctx, item, rangeHeader) (io.ReadCloser, Meta, error)
    Thumbnail(ctx, id, size) (io.ReadCloser, error)      // optional: ErrUnsupported
    Capabilities() Caps                                  // versions, thumbnails, redirectDownload, search, quota ...
}
```

`Item`, `Page` and `Version` become neutral types owned by `storage`; the Graph implementation adapts to them. The SAP Content Server layer and the REST API keep their behaviour, and ask `Capabilities()` instead of assuming SharePoint (for example: no `redirect` download for local, thumbnail answers 404 with a catalogue code).

The SAP-specific files (`~sapdoc.json`, `~certs/`) and the 256-way shard layout are implemented **above** the interface, so they behave the same on every backend and the existing 400-character path rule is applied only where a backend declares a limit.

### Local backend (first version)

Promise: **single instance, bounded versions.** One server instance owns a local folder. There is no multi-instance or NFS guarantee in this version.

| Topic | Behaviour |
|---|---|
| Location | `<data root>/<path>`; the data root is `/var/lib/zaks-drive/data` (a volume) by default. A repository may instead use a **host folder** the administrator mounts into the container; the command prints the exact Compose mount and checks that the container user can write to it |
| Confinement | Every operation goes through `os.Root`: no `..`, no escaping through links. Symbolic links in the store are refused |
| IDs | Opaque, stable for a path: a URL-safe encoding of the repository-relative path. There is no rename or move, so the ID cannot go stale |
| ETags | Derived from size, modification time and a per-file generation counter kept in a hidden sidecar; `If-Match` behaves as on SharePoint (412 on mismatch) |
| Writes | Temporary file in the same directory, `fsync`, atomic rename; per-path locks; uploads of unknown size spool to the existing spool directory first |
| Versions | The last *N* versions (default 5, configurable, `0` disables) are moved to a hidden `.versions/` folder on replace; `GET .../versions` lists them. Older ones are pruned |
| Search | File-name search by walking the tree, bounded by a limit; no content search |
| Thumbnails, redirect downloads | Not supported (declared in `Capabilities()`) |
| Names and paths | Linux file name limit (255 bytes); the same forbidden-character handling as SharePoint, so a document ID is the same on both backends |
| Space | Free space is checked before large uploads; `doctor` warns when a local repository's volume is low |
| Durability | The server does not replicate. Back up the volume or folder (the [backup guide](../operations/backup-recovery.md) gets a section) |
| Encryption at rest | Not provided; use an encrypted volume or disk |

### SharePoint backend with its own credentials

The current Graph code moves behind the interface unchanged. What is new:

- **Token sources are keyed by (tenant, client).** Repositories that use the same application share one token source; others get their own. Secrets are read from `clientSecretFile` at start and never logged.
- **One Graph client (concurrency limit, throttle handling) per credential**, so a throttled tenant does not slow another. Metrics gain a `repository` label.
- A repository without its own credentials uses the global `graph.*` settings, exactly as today.

### Management command

New commands (details and output in `docs/cli.md` when implemented):

| Command | Purpose |
|---|---|
| `zaks-drive repo list [--json]` | repositories, backend, SAP ID, state (ready / not ready, last check) |
| `zaks-drive repo add <id> --backend local\|sharepoint ...` | create a repository |
| `zaks-drive repo update <id> ...` | change settings (secrets only via file or prompt, never a flag value) |
| `zaks-drive repo remove <id>` | **detach** the repository. Documents are **not** deleted; `--purge` is not offered in the first version |
| `zaks-drive repo check <id>` | test access of one repository (the existing `check`, scoped to it); for SharePoint it contacts Microsoft, for local it checks write, read, version and space |
| `zaks-drive repo show <id>` | settings with secrets shown as set or not set |

Behaviour shared by the mutating commands: validate with the server's own loader first (a dry run that also tests access when asked), show a summary without secrets, ask for confirmation (`--yes` for scripts), write, then **apply by restart** (`--no-restart` leaves it pending; `status` and `doctor` report "changes pending restart"). Removing a repository that SAP still uses prints a warning and needs `--allow-in-use`.

`setup`, `doctor`, `status` and `support bundle` learn about repositories: per-repository readiness, certificate state per content repository (already per `contRep`), a repository table in `status`, and an allowlisted per-repository summary in the bundle (no URLs or tenant IDs).

### Applying changes: restart

Per the decision for this RFC, a change **restarts** the server (a brief interruption; SAP retries). The server validates the new state, writes it, drains, and exits cleanly; the container's `restart: unless-stopped` brings it back with the new repositories. Hot reload is out of scope here and can be revisited.

### Errors

New catalogue codes (names indicative) cover: repository not found, repository ID invalid or duplicate, backend unsupported, local path not writable, local folder outside the data root, secret file missing or not mode 600, per-repository credentials rejected, repository in use by SAP. Each has a cause and a fix, and `docs/error-codes.md` is regenerated.

## Alternatives

- **Keep editing `config.yaml`.** Works today for `ZH`, `AD`; no per-repository credentials, no local backend, no safe tooling. Rejected as the long-term answer.
- **Put secrets in `.env` variables per repository.** Fewer files, but `.env` mixes all repositories' secrets, rotation touches shared state, and per-repository removal is awkward. Rejected by the maintainer's decision for file-per-repository.
- **Host files mounted read-only into the container.** Simple for the CLI, but forces readable secrets or administrator `chown` (see above). Rejected.
- **Hot reload.** Avoids the restart but needs the server to swap repositories under live traffic and handle partial failures. Deferred.
- **A second storage product** (S3, Azure Blob): possible behind the same interface later; not part of this RFC.

## Compatibility

- **Existing deployments:** `repositories:` and `contentServer.repositories:` in `config.yaml` keep working with the same keys and defaults. They are treated as **static repositories**: shown in `repo list` as "defined in config.yaml", read-only for the CLI and the UI. `repos.d/` repositories are added to them; an ID that exists in both is a configuration error with a clear message.
- **API routes, request and response contracts, authentication, SAP protocol behaviour:** unchanged. Responses for local repositories differ only where a capability is absent (thumbnails, redirect), with catalogue errors.
- **Compose and the command line:** the release compose file gains a `state` volume and keeps the service name `adapter`, the ports and the `spool` volume. Existing `docker compose` commands and `zaks-drive` commands behave as before. Moving an existing deployment to the new compose file is done by `zaks-drive update`, which adds the volume and never removes one.
- **Storage layout:** SharePoint repositories keep exactly today's layout (`<root>/ContentServer/<contRep>/<shard>/<docId>/...`). Local repositories use the same logical layout under their folder.
- **Is this a breaking change?** No. The configuration model grows; nothing is renamed.

## Security

- **New secret store.** Per-repository client secrets live in the state volume at mode 600, owned by the server user. They are never in `repos.d/*.yaml`, never returned by the admin API (write-only), never printed, and are added to the command's sanitizer values and excluded from the support bundle by construction.
- **Local filesystem access** is the new attack surface: `os.Root` confinement, no links, names validated like SharePoint names, the data root is a volume the container owns, host folders are an explicit administrator choice. A repository's `path` can never point at the server's own config, state or secrets directories (rejected at validation).
- **Admin API** (new endpoints on the existing localhost-only admin listener, `ADMIN_TOKEN`): every mutating call is audited (`audit.log`: who, what, when; no values). Per-repository credentials do not widen SAP's or API keys' access: API keys already list repositories.
- **Logging policy applies** to the new code: only approved fields, no URLs or secrets, enforced by the existing tests and extended to the new packages.
- **Multi-tenant credentials** mean a bug could use the wrong tenant's token for a repository. The token source is selected once per repository at start-up and covered by tests that assert which credential each repository uses.

## Testing

Without Microsoft 365 or SAP credentials:

- Backend **contract tests** run the same suite against both backends (the local one for real, the SharePoint one through the Graph emulator): create, read ranges, replace with `If-Match`, delete, folders, versions, search, paging, names, concurrency, partial failure.
- The existing SAP Content Server and REST suites run over **both** backends.
- Local backend: path traversal and link attempts, crash safety (kill during write), versions pruning, space exhaustion, permissions.
- Per-repository credentials: which token source each repository uses; one throttled credential does not block another.
- CLI: fake runner tests for every `repo` command (exact Docker arguments, no destructive operation), one-off container path, confirmation, secret exclusion from output and bundle.
- A real-Docker pass with the Graph emulator, as for the CLI milestones.

Needs real systems: SAP reports (`RSCMSTH0` to `RSCMSTH3`) against a **local** repository; a second real tenant for per-repository credentials; a host-mounted folder with real ownership. These are reported, not assumed.

## Migration

- **No action needed** for existing deployments.
- **Optional:** `zaks-drive repo import` (later phase) converts static entries into `repos.d/` files so they become manageable from the CLI and the UI.
- **Rollback:** the state volume is additive. Going back to an older release ignores `repos.d/`; repositories that exist only there are not served until the newer release returns. `rollback` says so before it proceeds.

## Phasing

Each step is a separate, reviewable pull request.

1. **Backend interface and neutral types**, SharePoint implementation behind it, **no behaviour change** (the whole existing suite is the safety net).
2. **Repository model:** `repos.d`, static plus dynamic repositories, validation, catalogue codes, metrics label.
3. **Local backend** with the contract tests; SAP and REST suites run over it.
4. **Per-repository credentials** (token sources, per-credential Graph clients, secret files).
5. **State volume, admin API and the one-off container path.**
6. **`repo` commands** and the `status` / `doctor` / `setup` / bundle changes; documentation.
7. UI in [RFC 0002](0002-server-admin-ui.md).

## Open questions

- Content repository ID length. SAP's OAC0 IDs are short (we validate 1 or 2 characters today). Should REST-only repositories be allowed longer IDs, as proposed here?
- Default `versions` for local repositories (5?), and a size cap per repository (quota)?
- Should `repo remove` ever offer `--purge`, and if so with which safeguards?
- Do we need `readOnly` per SAP repository as well (it exists for REST repositories today)?
- Is the per-repository `allowedNetworks` override wanted, or is one global list enough?

## Outcome (filled in by a maintainer)

Decision: _pending_.
