# Testing

## What runs without any credentials

```bash
gofmt -l .                 # must print nothing
go vet ./...
go test -race -count=1 ./...
python3 scripts/check_docs.py      # documentation links and anchors
```

All of these run in CI ([`.github/workflows/ci.yml`](../../.github/workflows/ci.yml)) without Microsoft 365 or SAP credentials.

| Area | Where | What it covers |
|---|---|---|
| Configuration | `internal/config` | Defaults, environment overrides, every validation message |
| Error catalogue | `internal/apperr` | Codes, formatting, and that `docs/error-codes.md` is current |
| Graph client | `internal/graph` | Retries, throttling, error classification, drive resolution (site, library, user drive) |
| Storage and transfer | `internal/storage`, `internal/transfer` | Names and limits, paging cursors, uploads, upload sessions, memory budget; the local backend (confinement, links, ETags, versions, ranges, concurrent writers) |
| REST API | `internal/restapi` | Routes, authentication, permissions, conditional requests, ranges, problem responses |
| Content Server | `internal/contentrepo` | Every command, secKey verification for DSA, RSA and ECDSA, certificates, modes, rollback, concurrency, partial failures, reserved folder; benchmarks for signature cost |
| Emulator | `internal/graphmock` | Used by the tests above |

All of them run against `internal/graphmock`, not against Microsoft. Passing tests prove the code's behaviour against the emulator's model of Graph.

The REST and Content Server suites also run over the **local backend**: `ZAKS_TEST_BACKEND=local go test ./internal/restapi/... ./internal/contentrepo/...` (CI does this on every change). The few tests that assert SharePoint-only behaviour (the 400-character path limit, redirect downloads, thumbnails, Graph paging links) skip themselves with the reason.

## The management command and the installer

| Area | Where | What it covers |
|---|---|---|
| Command | `internal/ctl` | Parsing and exit codes, deployment discovery, Docker prerequisites, the exact arguments of every Docker call (no destructive operation, only the deployment's own compose file), lifecycle, status, log filtering and sanitizing, setup (interactive and not), 19 diagnostics each in each of its states, support-bundle secret exclusion, verified update, hostile archives, automatic rollback |
| Release and installer | `scripts/test_install.sh` | Release bundles for both architectures, reproducible checksums, bundle contents, architecture detection, install, tampered and malformed downloads, hostile archives, unwritable prefix, no `sudo`, attestation stand-ins |

`sh scripts/test_install.sh` needs Go, tar, gzip, curl and sha256sum; it uses no network and no credentials (`docker run --rm -v "$PWD:/src" -w /src golang:1.25 sh scripts/test_install.sh`). Real Docker is not used by the unit tests; see [CLI design](cli-design.md) for how the command was also checked against real Docker Compose.

## What needs real systems (not run in CI)

| Test | How | Status |
|---|---|---|
| Real SharePoint access | `zaks-drive check` or `scripts/check_access.py` with a tenant | Run manually against a real tenant during development; results are not part of CI |
| SAP standard reports | RSCMSTH0-3, RSCMSTA0, RSCMSTAC from SE38 | Passed once (2026-10-16); see [SAP](../sap/connect-s4hana-to-sharepoint.md#5-test-with-sap-standard-tools) |
| DMS originals (CV01N, CV03N) | In SAP | Verified end to end once by the project owner (upload, storage in SharePoint, retrieval) |
| GOS, ArchiveLink scenarios | In SAP | Not tested |
| End to end smoke against a running server | `scripts/e2e-smoke.sh` | Needs a running server and key; works with the emulator stack |
| Load tests | `loadtest/run.sh` | [Load testing](load-testing.md) |

## Smoke test with the emulator

```bash
docker compose -f loadtest/docker-compose.yml up -d --build graphmock adapter
API_KEY=loadtest-key CS_REPO=Z1 scripts/e2e-smoke.sh
docker compose -f loadtest/docker-compose.yml down -v
```

## Running CodeQL locally

CI runs CodeQL on every pull request. To reproduce a finding on your machine, download the CodeQL bundle from [github/codeql-action releases](https://github.com/github/codeql-action/releases) (about 700 MB), then, from the repository root:

```bash
codeql database create /tmp/zd-db --language=go --source-root=.
codeql database analyze /tmp/zd-db codeql/go-queries:codeql-suites/go-code-scanning.qls --format=sarif-latest --output=/tmp/zd.sarif
```

The SARIF file contains the data-flow path of every result (the GitHub alert page hides it behind "Show paths"). A new result is a defect to fix, not a warning to dismiss. Three results of the full suite are known and reviewed: `go/weak-sensitive-data-hashing` (API keys are random tokens, see [security](../operations/security.md)), `go/request-forgery` (paging cursors are HMAC-signed and the Graph client accepts only links under its own base URL) and `go/reflected-xss` (SAP `serverInfo` answers `text/plain`; `pVersion` is restricted to four digits).

## Writing tests

Put tests next to the code. Use the emulator for Graph behaviour; if you add a Graph quirk to the emulator, state in a comment where it was observed. Do not add tests that need real credentials to the default `go test` run.
