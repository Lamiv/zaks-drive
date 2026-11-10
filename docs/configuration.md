# Configuration reference

ZAKS Drive reads two files and a few environment variables. Both files live in `deploy/`, are **not** committed to Git (they are listed in `.gitignore`), and are created from the templates in the same folder.

| File | Holds | Template |
|---|---|---|
| `deploy/.env` | Secrets and the things that differ per installation: Microsoft credentials, the SharePoint location, API keys, ports | `deploy/.env.example` |
| `deploy/config.yaml` | Behaviour: interfaces, repositories, authentication, limits | `deploy/config.example.yaml` |

`config.yaml` may reference any environment variable as `${NAME}`. If a referenced variable is empty, the start-up error names it ([`CFG_REPO_LOCATION_MISSING`](error-codes.md#cfg_repo_location_missing), [`CFG_GRAPH_CREDENTIALS_MISSING`](error-codes.md#cfg_graph_credentials_missing) and others). After changing either file, recreate the container: `docker compose -f deploy/docker-compose.yml up -d --force-recreate`.

All problems in the configuration are reported at once, each with the setting, the likely cause and the fix. See [error codes](error-codes.md).

## `deploy/.env`

| Variable | Used for | Notes |
|---|---|---|
| `GRAPH_TENANT_ID` | Microsoft Entra directory (tenant) ID | Required |
| `GRAPH_CLIENT_ID` | Application (client) ID of the app registration | Required |
| `GRAPH_CLIENT_SECRET` | The client secret **Value** (not the Secret ID) | Required. Secret. |
| `SHAREPOINT_SITE_URL` | The SharePoint **site** address, for example `https://contoso.sharepoint.com/sites/SAP_DOCS` | Read by `config.example.yaml`; not a link to a library page |
| `SHAREPOINT_LIBRARY` | The document library's **display name**, for example `Documents` | Empty means the site's default library |
| `SHAREPOINT_ROOT_PATH` | Folder inside the library for everything ZAKS Drive stores | Created automatically. Empty means the library root |
| `ADAPTER_API_KEY_S4` | API key with `read`, `write`, `delete` for back-end systems | Name comes from the template and may be changed together with `config.yaml` |
| `ADAPTER_API_KEY_FIORI` | A read-only API key for consumers | Same |
| `ADMIN_TOKEN` | Protects the admin endpoints (certificate activation) | Secret |
| `REST_PORT`, `CS_PORT`, `ADMIN_PORT` | Host ports published by Docker Compose (defaults 8080, 8090, 9090) | Set only when a default is taken, for example Cockpit on 9090. Container ports do not change |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `debug` temporarily shows what SAP sends (secKey redacted, never bodies) |

Other variables that override `config.yaml` (all optional): `ADAPTER_INTERFACES` (comma separated), `DOWNLOAD_MODE`, `GRAPH_MAX_CONCURRENCY`, `METADATA_CACHE_TTL` (for example `60s`), `GRAPH_AUTHORITY_URL`, `GRAPH_BASE_URL`, `AUTH_DISABLED=true` (development only), `CONFIG_FILE` (path of the config file; set to `/etc/adapter/config.yaml` in the provided Compose files). The command line uses `ADMIN_URL` (default `http://127.0.0.1:9090`) and `ADMIN_TOKEN`; the Docker health check uses `HEALTHCHECK_URL`.

## `deploy/config.yaml`

### `server`

| Key | Default | Meaning |
|---|---|---|
| `interfaces` | `[rest]` | Which interfaces start: `rest` (REST API), `contentrepo` (SAP Content Server). The admin listener always runs |
| `restAddr` | `:8080` | Listen address of the REST API |
| `contentRepoAddr` | `:8090` | Listen address of the SAP Content Server interface |
| `adminAddr` | `:9090` | Health, metrics and certificate administration. Published on `127.0.0.1` only by the Compose file |
| `maxInFlight` | `1000` | Concurrent requests per listener; further requests get `503` with `Retry-After` |
| `readHeaderTimeout`, `idleTimeout`, `shutdownTimeout` | `10s`, `120s`, `30s` | HTTP server timeouts. There is deliberately no overall read or write timeout, so large transfers can take minutes |
| `corsOrigins` | `[]` | Browser origins allowed to call the REST API directly (`*` allows all). Empty disables CORS |
| `tlsCertFile`, `tlsKeyFile` | empty | Serve HTTPS on the REST and SAP listeners. Alternatively terminate TLS in a reverse proxy |
| `adminToken` | empty | Bearer token for the admin API (repositories, certificates, the [admin UI](ui.md)). Normally `${ADMIN_TOKEN}`. Repository administration and the UI refuse to work while it is empty |
| `stateDir` | empty | Folder for managed repositories: definitions, their secrets and local documents. The supplied Compose files set `STATE_DIR=/var/lib/zaks-drive` (the `state` volume). Without it, `zaks-drive repo` and local repositories are unavailable |
| `drainTimeout` | `30s` | How long a restart for a repository change waits for running requests (new ones get `503` with `Retry-After`) |
| `adminUI` | `true` | Serve the [admin UI](ui.md) on the admin listener. Login is the user `admin` with a password chosen at the first login ([guide](ui.md#logging-in)); it needs `server.stateDir` to store it |
| `enablePprof` | `false` | Go profiling endpoints on the admin listener. Keep off in production |

### `graph`

| Key | Default | Meaning |
|---|---|---|
| `tenantId`, `clientId`, `clientSecret` | from `.env` | Microsoft Entra application credentials (client-credentials flow) |
| `authorityUrl`, `baseUrl`, `scope` | Microsoft public cloud | Change only for national clouds or test servers |
| `maxConcurrency` | `64` | Simultaneous Microsoft Graph calls per instance. Lower it if SharePoint throttles ([`SP_THROTTLED`](error-codes.md#sp_throttled)) |
| `maxRetries`, `maxRetryBackoff` | `5`, `60s` | Retries for throttling, server errors and network errors, honouring `Retry-After` |
| `maxIdleConnsPerHost` | `256` | Connection pool size |
| `metadataCacheTtl` | `60s` | Caches item metadata and the short-lived download URL so a repeated read is one SharePoint call. `0` disables |

### `transfer`

| Key | Default | Meaning |
|---|---|---|
| `simpleUploadMaxBytes` | `4194304` (4 MiB) | Up to this size one request uploads the file; larger files use a resumable upload session |
| `chunkSizeBytes` | `5242880` (5 MiB) | Session chunk size; rounded down to a multiple of 320 KiB as Microsoft Graph requires |
| `memoryBudgetBytes` | `536870912` (512 MiB) | Hard cap on all transfer buffers. Requests wait instead of exhausting memory |
| `maxUploadBytes` | `2147483648` (2 GiB) | Largest accepted upload ([`UPLOAD_TOO_LARGE`](error-codes.md#upload_too_large)) |
| `spoolDir` | system temp dir; `/var/spool/adapter` in the Compose file | Disk spool for uploads that arrive without a `Content-Length` |
| `downloadMode` | `proxy` | `proxy` streams bytes through ZAKS Drive; `redirect` answers `302` to SharePoint's short-lived download URL |

### `auth` (REST API)

| Key | Meaning |
|---|---|
| `apiKeys[].name` | Label shown in logs and errors |
| `apiKeys[].key` or `apiKeys[].sha256` | The key, or its hex SHA-256 so no plaintext key is stored |
| `apiKeys[].repositories` | Repository IDs the key may use; empty or `["*"]` means all |
| `apiKeys[].permissions` | Any of `read`, `write`, `delete` (default `read`) |
| `jwt.issuer`, `jwt.audience`, `jwt.jwksUrl` | Validate bearer tokens (for example from Entra ID) against a JWKS |
| `jwt.rolesClaim`, `jwt.rolePrefix` | The claim that carries permissions (`roles` by default); a prefix such as `Documents.` is stripped, so `Documents.read` becomes `read`. JWT permissions apply to all repositories |
| `disabled` | Turns inbound authentication off. **Development only** |

At least one of `apiKeys` or `jwt` is required when the REST interface is enabled.

### `repositories`

A *repository* maps an ID that clients use to a document library and a root folder. Every item a client can reach lies inside that root.

| Key | Meaning |
|---|---|
| `id` | The repository ID used in REST URLs and referenced by `contentServer` |
| `siteUrl` + `driveName` | A SharePoint site and one of its libraries (the normal case). The site needs `Sites.Selected` with a `write` grant, or `Sites.ReadWrite.All` |
| `userId` | A user's OneDrive for Business, for example `jane@contoso.com`. Needs `Files.ReadWrite.All` |
| `siteId`, `driveId` | A Microsoft Graph site ID, or drive ID (starts with `b!`) |
| `rootPath` | Folder inside the library; created automatically |
| `readOnly` | Refuse every write ([`REPO_READ_ONLY`](error-codes.md#repo_read_only)) |

Use exactly one of `siteUrl`, `userId`, `siteId` or `driveId`.

### `contentServer` (SAP)

| Key | Default | Meaning |
|---|---|---|
| `allowedNetworks` | `[]` | IP addresses or CIDR ranges allowed to call the SAP interface. Empty allows all, so restrict it or use a firewall |
| `clockSkew` | `5m` | Tolerance when checking a signed URL's expiry |
| `autoActivateCertificates` | `false` | Activate certificates immediately when SAP sends them. Keep `false` in production |
| `repositories[].contRep` | | The content repository ID from SAP transaction OAC0 (1 or 2 characters) |
| `repositories[].repository` | | The repository `id` above that stores the documents |
| `repositories[].folder` | `ContentServer/<contRep>` | Folder below the repository root. **Reserved**: the REST API cannot read or change it |
| `repositories[].signature` | `required` | `required`: every request must carry a valid SAP signature. `optional`: signature needed only where the document's protection requires it. `none`: ignore signatures (tests only) |
| `repositories[].description` | | Shown by SAP's `serverInfo` |

See [SAP Content Server interface](sap/content-server.md) for how the layout, certificates and signature modes work.

### `log`

| Key | Default | Meaning |
|---|---|---|
| `level` | `info` | `debug`, `info`, `warn`, `error` |
| `format` | `json` | `json` or `text` |
