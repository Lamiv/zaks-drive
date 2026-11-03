# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). The project has not had a tagged release yet; see the [release process](docs/development/release-process.md).

## [Unreleased]

> **Highlight of this release: the command-line interface.** ZAKS Drive now has a `zaks-drive` command that installs, configures, starts, stops, updates and troubleshoots a deployment (`setup`, `start`, `status`, `logs`, `doctor`, `check`, `update`, `rollback`, `support bundle`), with a verifying Linux installer and reproducible release bundles. Start with the [command guide](docs/cli.md). Existing Docker Compose deployments keep working unchanged.

### Added

- **Multiple repositories with local or SharePoint storage** ([RFC 0001](docs/rfcs/0001-repositories-and-storage-backends.md)). `zaks-drive repo list|show|add|update|remove|check|purge` manages repositories such as `Z1`, `ZH`, `AD` (managed IDs are exactly 2 characters). A repository stores its documents in SharePoint/OneDrive (optionally with its **own Entra application**) or in a **local folder** of the server (confined to the folder, links ignored, 0 to 99 versions kept). Definitions, secrets and local documents live in a new `state` volume (`/var/lib/zaks-drive`); secrets are separate mode-600 files, never in a definition, a flag or a log. Repositories in `config.yaml` keep working unchanged.
- **Applying changes by a drained restart**: new requests get `503` with `Retry-After` (SAP retries), running requests finish (`server.drainTimeout`, default 30 s), the server restarts. If the new definitions are invalid the server keeps serving the last working state and says so. A server with a state directory may start with no repository yet (not ready until one is added).
- **Admin API** (`/admin/api/v1`) for repositories, checks, apply, certificates, diagnostics, logs, audit and settings. It requires `ADMIN_TOKEN` and refuses to work without one.
- **Admin UI** ([RFC 0002](docs/rfcs/0002-server-admin-ui.md), [guide](docs/ui.md)) on the admin listener: repositories, SAP certificates, diagnostics, logs, audit, settings, "Apply and restart". **On by default.** Login is the user `admin`; the first login uses the well-known `admin` / `admin`, which works **only to choose a new password** (at least 12 characters; nothing else is reachable until then). The password is kept only as a salted Argon2id hash in the state volume. A forgotten password is reset with `zaks-drive admin reset-password` (or an empty `NO_ADMIN` file in the state volume plus a restart): the login becomes `admin` / `admin` again, to be changed at once, and the server deletes `NO_ADMIN` when the new password is set. Short session cookie, CSRF and Origin checks, lockout that doubles on repeats, strict CSP, nothing loaded from other sites. `server.adminUI: false` turns it off; `zaks-drive doctor` warns while the default password is active. The `ADMIN_TOKEN` stays the credential of the command and scripts. **Not yet independently security-reviewed; change the default password on first start and keep the admin port on 127.0.0.1.**
- `repo purge` deletes the documents of a *detached local* repository, only on the Docker host, with a plan, a time-limited challenge bound to the folder's state, the typed ID and an explicit flag. It does not exist in the admin API or UI.
- `status`, `doctor` (new `repositories` diagnostic) and the support bundle show repositories without locations or application IDs. New error codes (`CFG_REPO_*`, `ADMIN_*`, `THUMBNAIL_UNAVAILABLE`).
- **`zaks-drive` management command** (`cmd/zaks-drive`): `setup` (interactive or non-interactive), `config validate`, `start`, `stop`, `restart`, `status`, `logs`, `doctor` (19 documented diagnostics), `check`, `update`, `rollback`, `support bundle`. See [docs/cli.md](docs/cli.md). The server is now the separate binary `zaks-drive-server`; inside the image `/zaks-drive` and `/adapter` remain as compatibility names of the server.
- Linux release bundles (amd64, arm64) built reproducibly by `scripts/build-release.sh`, a verifying installer `scripts/install.sh`, offline installer tests (`scripts/test_install.sh`), and a manual release workflow with provenance attestation (draft release only).
- `BIND_ADDRESS` in `.env` selects the host address of the REST and SAP ports (default `0.0.0.0`, unchanged behaviour).
- ZAKS Drive branding: `zaks-drive` binary and image, `version` command, usage text, start-up banner, OCI labels. See [branding and compatibility](docs/development/branding-and-compatibility.md).
- Error codes with cause and fix on every error, a generated [error catalogue](docs/error-codes.md), and `zaks-drive errors`.
- SharePoint location (`SHAREPOINT_SITE_URL`, `SHAREPOINT_LIBRARY`, `SHAREPOINT_ROOT_PATH`) read from `.env`; all configuration problems are reported together.
- OneDrive (`userId`) drive resolution in the Graph emulator and tests.
- Community documentation: `GOVERNANCE.md`, a rewritten `CONTRIBUTING.md`, an RFC process (`docs/rfcs/`), a community roadmap, an SAP test report issue template, CODEOWNERS and a label set.
- The repository is renamed to `zaks-drive`, and the Go module path is now `github.com/Lamiv/zaks-drive`.
- One versioning scheme (SemVer from the git tag) for the CLI, image tag, OCI label and release artifacts; `scripts/version.sh`, `ZAKS_DRIVE_VERSION` in Compose, and CI/release checks. See [versioning](docs/development/release-process.md#versioning).
- Documentation restructure: index, quickstart, installation, configuration reference, architecture, SAP guides, Microsoft 365 guides, operations and development guides; `scripts/check_docs.py` link checker.
- Governance files: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `SUPPORT.md`, `ROADMAP.md`, issue and pull request templates, CI workflow and Dependabot configuration.

### Changed

- **Upgrade note.** The Compose files add a `state` volume and `STATE_DIR`. `/readyz` is now ready only when **every** repository is reachable (it used to check the first). Global `graph.*` credentials are only required when a repository uses them. Storage is now behind a backend interface; REST routes, SAP behaviour, configuration keys, ports and metric names are unchanged.
- Documentation files moved: `docs/openapi.yaml` to `docs/api/`, SAP documents to `docs/sap/`, `docs/load-test-results.md` to `docs/development/`. `SETUP.md`, `docs/content-server.md` and `docs/load-test-results.md` remain as pointers.
- The Compose file builds the image `zaks-drive:latest`. The service, volume, ports, routes, configuration keys and metric names are unchanged.

### Deprecated

- The `/adapter` binary name inside the image is an alias kept for existing Compose files, health checks and scripts. It is planned for removal in a later minor release; this entry will state the version when decided.

### Security

- Fixed clear-text logging of sensitive data (CodeQL "Clear-text logging of sensitive information"). A caller-supplied `Authorization` value without a space was echoed in the authentication error detail, and from there into the log and the response; Graph and Entra error text and `url.Error` strings (which contain pre-authenticated URLs) could be logged or returned. Logs now carry only approved fields; see the [logging policy](docs/operations/security.md#logging-policy). Regression tests inject synthetic secrets and a policy test guards log calls. A follow-up fixed what CodeQL still reported on the pushed branch: it treats `Header.Get(name)` with a variable header name as a possible credential, so request headers are now read by literal name (enforced by a test). Caller-supplied request IDs and `X-CorrelationID` are still honoured. SAP `pVersion` is now only echoed when it is four digits.
- Reviewed documentation of controls; see [security](docs/operations/security.md) for what is implemented and what is not.
