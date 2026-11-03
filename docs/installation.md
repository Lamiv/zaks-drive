# Installation

Install ZAKS Drive on a Linux server with Docker and connect it to SharePoint Online or OneDrive for Business. To try it without a tenant first, use the [quickstart](quickstart.md).

> **Looking for the full path?** The [setup and usage guide](guide.md) takes you from install to SAP, several repositories, the admin page, backup and updates, with commands and checks at every step; every flag is in the [CLI reference](cli-reference.md).

## The quick way: the zaks-drive command

On Linux, the [`zaks-drive` command](cli.md) does the steps below for you: `install.sh` (verified download), `zaks-drive setup` (validated configuration, secrets at mode 600), `zaks-drive start`, `zaks-drive doctor`. This page describes what happens underneath and the manual way from a source checkout.

## Requirements

| Item | Notes |
|---|---|
| Linux server with Docker Engine and Compose v2 | Tested on Ubuntu with Docker; 2 vCPU, 2 GB RAM and 20 GB disk is a reasonable start. The container is limited to 1 GB RAM |
| Outbound HTTPS (443) | To `login.microsoftonline.com`, `graph.microsoft.com` and `*.sharepoint.com`. GitHub and `proxy.golang.org` are needed only while building the image |
| Inbound access | Applications to 8080; SAP application servers to 8090 |
| Accurate time (NTP) | SAP's signed URLs expire after minutes |
| A Microsoft Entra app and a SharePoint location | [Entra registration](microsoft-365/entra-app-registration.md), [Graph permissions](microsoft-365/graph-permissions.md), [SharePoint configuration](microsoft-365/sharepoint-configuration.md) |

Measured resource use with the emulator at 100 concurrent clients was about 0.4 CPU core and 350 MB of memory ([load testing](development/load-testing.md)). Real Microsoft Graph latency and throttling will differ.

## 1. Test the Microsoft credentials first

```bash
python3 scripts/check_access.py deploy/.env <tenant>.sharepoint.com sites/<site> <library> SAP_DMS
```

It needs only Python 3 and checks token, permissions, site, library, upload, download, upload session, versions and delete, printing PASS, FAIL or SKIP with the fix for each. It never prints the secret and removes its test files.

## 2. Get the code

```bash
git clone https://github.com/Lamiv/zaks-drive.git /opt/zaks-drive
cd /opt/zaks-drive
```

Without internet access on the server, build elsewhere and transfer the image:

```bash
VERSION=$(scripts/version.sh)
docker build --target adapter --build-arg VERSION=$VERSION -t zaks-drive:$VERSION .
docker save zaks-drive:$VERSION -o zaks-drive.tar     # on the build machine
docker load -i zaks-drive.tar                       # on the server; then set ZAKS_DRIVE_VERSION=<version> and omit --build below
```

## 3. Configure

Two files, neither committed to git:

```bash
cp deploy/.env.example deploy/.env && chmod 600 deploy/.env
cp deploy/config.example.yaml deploy/config.yaml && chmod 644 deploy/config.yaml
```

`deploy/.env` holds secrets and the SharePoint location (`GRAPH_TENANT_ID`, `GRAPH_CLIENT_ID`, `GRAPH_CLIENT_SECRET`, `SHAREPOINT_SITE_URL`, `SHAREPOINT_LIBRARY`, `SHAREPOINT_ROOT_PATH`, API keys, `ADMIN_TOKEN`). Generate keys with `openssl rand -base64 32`. `deploy/config.yaml` holds settings; it contains no secrets. Every key is described in the [configuration reference](configuration.md).

To serve SAP, add `contentrepo` to `server.interfaces` and list the repository IDs under `contentServer.repositories`.

## 4. Start and verify

```bash
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml ps
docker compose -f deploy/docker-compose.yml exec adapter /zaks-drive check
curl -s localhost:9090/readyz
```

`check` tests every access against the real SharePoint and exits 0 only when all pass. `/readyz` answers `ready`, or `503` with the reason. The startup log names the library where documents are stored:

```bash
docker compose -f deploy/docker-compose.yml logs adapter | grep 'stored here'
```

For a fuller end-to-end test, `scripts/e2e-smoke.sh` exercises uploads, downloads, ranges, versions, paging and parallel clients, then deletes what it created.

## 5. Restrict network access

Port 9090 is bound to `127.0.0.1`. Do not expose it. Restrict 8080 and 8090 to the networks that need them, with `contentServer.allowedNetworks`, binding to one interface in the Compose file, or a firewall in front of the host. Docker's published ports bypass `ufw`. For TLS see [security](operations/security.md#depends-on-your-deployment).

## 6. Connect SAP

[Connect SAP S/4HANA to SharePoint using ZAKS Drive](sap/connect-s4hana-to-sharepoint.md).

## Troubleshooting

[Operations troubleshooting](operations/troubleshooting.md), [error codes](error-codes.md).
