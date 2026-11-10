# Backup and recovery

## What holds state

| Data | Where | Back up how |
|---|---|---|
| Documents of **SharePoint** repositories, SAP components, SAP metadata (`~sapdoc.json`), SAP certificates (`~certs`) | The SharePoint library | Microsoft 365 backup, retention policies and the SharePoint recycle bin. ZAKS Drive adds no backup of its own |
| Managed repositories, **local documents**, repository secrets, admin password hash | The `state` volume | [The state volume](#the-state-volume) below |
| `deploy/.env`, `deploy/config.yaml` | The server | Keep copies in a password vault or configuration store. `.env` holds secrets |
| The `spool` volume | The server | Not needed. It holds temporary upload data only |
| The container | Nowhere | Rebuild from the repository |

## The state volume

Managed repositories keep their data in the **`state` volume** (`/var/lib/zaks-drive` in the container). **Back it up.** It is the only place that holds:

| In the volume | What it is |
|---|---|
| `data/<path>/` | the **documents of local repositories** (and their `.versions/`) |
| `repos.d/`, `detached/`, `last-good/` | repository definitions (no secrets) |
| `secrets/<ID>/client-secret` | the client secret of a repository with its own Microsoft application (mode 600) |
| `admin.json` | the hash of the admin page password |
| `audit.log` | who changed what and when |

SharePoint repositories keep their *documents* in SharePoint; the volume then only holds their definitions and secrets, which is small but needed to bring the repository back without re-entering it.

**Never run `docker compose down -v` on a deployment with local repositories**: it deletes the volume. Stop the server for a consistent copy, then archive the volume (the archive holds secrets: keep it encrypted and restricted):

```bash
zaks-drive stop
docker run --rm -v <project>_state:/s -v "$PWD":/b alpine tar czf /b/zd-state-$(date +%F).tar.gz -C /s .
zaks-drive start
```

Restore: install and run `setup` on the new server with the same `.env`, `zaks-drive stop`, extract into the volume, `chown -R 65532:65532` it (the server's user), `zaks-drive start`, then `zaks-drive repo list` and `zaks-drive repo check <id>`. The whole sequence with commands: [guide, Scenario 10](../guide.md#scenario-10-back-up-and-restore). There is no replication between hosts; the local backend is for one server instance.

## Rebuild a server

1. Install Docker ([installation](../installation.md)).
2. Clone the repository, restore `deploy/.env` and `deploy/config.yaml`.
3. `docker compose -f deploy/docker-compose.yml up -d --build`, then `zaks-drive check`.

SAP needs no change if the host name stays the same: certificates live in SharePoint and remain active.

## Recover documents

| Situation | Action |
|---|---|
| A REST document was deleted | Restore from the SharePoint recycle bin (93 days by default) |
| A REST document was overwritten | Restore an earlier version in the file's version history |
| A SAP document is damaged or missing | Restore **the whole document folder together** (`<root>/ContentServer/<contRep>/<shard>/<docId>/`: the components and `~sapdoc.json`) from the recycle bin, the version history or your Microsoft 365 backup. To find the folder: `printf '%s' <docId> | sha1sum | cut -c1-2` is the shard |
| `CS_META_UNREADABLE` (409) | `~sapdoc.json` of that document is corrupt: restore that one file from its version history |
| The `~certs` folder was lost | Send the certificate again from OAC0 and activate it ([certificates](../sap/oac0-configuration.md#send-and-activate-the-certificate)) |
| The Entra secret expired or leaked | Create a new secret, update `.env`, recreate the container, delete the old secret ([secret management](security.md#secret-management)) |

## Moving to another library

SAP's certificates are stored with the documents. Move the whole content server folder including `~certs`, or send and activate the certificate again after the move. Update `SHAREPOINT_*` in `.env` and recreate the container; the startup log shows the new location.

## Not provided

Point-in-time restore of a whole repository, cross-library replication and restore tests are not part of ZAKS Drive. Test your Microsoft 365 backup restore for the SAP folder yourself before relying on it.
