# Roadmap

A public, community-oriented view of where ZAKS Drive stands and where help is wanted. **Nothing here is a promise, an approval or a schedule.** Items move when someone does the work. Proposals become design discussions through the [RFC process](rfcs/README.md), and decisions are made as described in [the governance page](governance.md).

Find work by label: [`good first issue`](https://github.com/Lamiv/zaks-drive/labels/good%20first%20issue), [`help wanted`](https://github.com/Lamiv/zaks-drive/labels/help%20wanted), [`sap-integration`](https://github.com/Lamiv/zaks-drive/labels/sap-integration), [`documentation`](https://github.com/Lamiv/zaks-drive/labels/documentation), [`performance`](https://github.com/Lamiv/zaks-drive/labels/performance), [`security`](https://github.com/Lamiv/zaks-drive/labels/security).

## Implemented and verified

See the [README feature table](../README.md#what-is-verified) for the evidence behind each line.

- REST API: upload, download, folders, versions, search, paging.
- SharePoint site and OneDrive access through Microsoft Graph, including `Sites.Selected`.
- SAP Content Server HTTP 4.5 commands and signed URLs: SAP reports RSCMSTH0 to RSCMSTH3, RSCMSTA0 and RSCMSTAC passed on S/4HANA 2025.
- SAP DMS originals: tested end to end once by the project owner (upload from SAP, storage in SharePoint, retrieval).
- Error catalogue with causes and fixes, `zaks-drive check`, a safe-logging policy, Prometheus metrics, SemVer versioning from git tags.

## Under development

- Documenting the exact SAP customising (document type, storage category, KPro) behind the verified DMS test, so others can reproduce it.
- A first pre-release (`v0.1.0-beta.1`) once the owner approves ([release process](development/release-process.md)).

## Proposed by the community

Drafts for discussion, not approved or scheduled:

- [RFC 0001](rfcs/0001-repositories-and-storage-backends.md): several repositories (`Z1`, `ZH`, `AD`, ...), each with a local-folder or SharePoint backend and its own settings and secrets, managed from `zaks-drive repo ...`.
- [RFC 0002](rfcs/0002-server-admin-ui.md): an admin UI built into the server for repositories, certificates and diagnostics.

Have an idea of your own? Start a discussion or write an [RFC](rfcs/README.md). Accepted proposals will be listed here with a link; being listed does not mean scheduled.

## Seeking contributors

These are open to anyone; comment on the issue (or open one) before starting a large piece.

| Area | What would help | Good for |
|---|---|---|
| SAP testing | GOS attachments and ArchiveLink scenarios; reports RSCMSTAT and RSCMSTHS; other S/4HANA releases and DMS configurations | SAP developers and Basis |
| Documentation | ABAP and Fiori examples; deployment guides for other environments; troubleshooting knowledge | Everyone |
| Performance | Load tests against a real test tenant instead of the emulator; tuning guidance | Go and infrastructure engineers |
| Security | Entra authentication with a certificate instead of a client secret; per-key rate limiting; an independent review of signature verification | Security engineers |
| Operations | Image publishing with SBOM and build provenance; tested arm64 builds; Kubernetes manifests or a Helm chart; a tested high-availability pattern | Infrastructure engineers |
| Integrations | Additional storage providers; Content Server `attrSearch` | Go developers (start with an RFC) |

## Deferred

- A document browsing frontend. ZAKS Drive is a content storage gateway, not a DMS application.
- Other storage back ends behind the storage interface, until an RFC settles the design.

## Not planned

- CMIS: SharePoint does not support it.
- Mapping SAP users to SharePoint permissions, or replacing SAP's own authorisation.
