# RFCs: proposing significant changes

An RFC (request for comments) is a short, public design document for a change that is too large or too risky to review as an ordinary pull request. It lets contributors discuss the idea before anyone writes code.

**An RFC is not a promise.** Writing, discussing or even accepting an RFC does not commit anyone to implement, schedule or release it. "Accepted" means the maintainers consider the design sound and would welcome an implementation that follows it.

## When an RFC is needed

Write one for changes such as:

- an additional storage provider besides SharePoint Online and OneDrive;
- SAP ArchiveLink compatibility improvements, or new GOS or KPro behaviour;
- enhanced KPro operations or new Content Server commands (for example `attrSearch`);
- Fiori integration examples that need new API surface;
- authentication enhancements (for example certificate authentication to Entra ID, new inbound schemes);
- high-availability or multi-instance deployment patterns;
- anything that changes API routes, request or response contracts, configuration keys, the storage layout or SAP protocol behaviour (a **breaking change**).

An RFC is not needed for bug fixes, documentation, tests, small refactors or clearly compatible small features. If in doubt, open a discussion or an issue first.

## How it works

1. **Discuss first (optional but encouraged).** Float the idea in GitHub Discussions (when enabled) or an issue, to find out whether someone is already working on it.
2. **Write the RFC.** Copy [`0000-template.md`](0000-template.md) to `docs/rfcs/NNNN-short-title.md` (use the next free number; use `0000-...` while it is a draft pull request) and fill it in.
3. **Open a pull request** titled `RFC: <title>` with the label `rfc`. The pull request is the discussion.
4. **Discuss in the open.** Anyone may comment. The author answers questions and updates the document. Keep comments about the proposal.
5. **Decision.** Maintainers decide after the discussion has had reasonable time (usually at least two weeks for a significant change), and record the outcome and reasons in the RFC: *accepted*, *rejected* or *deferred*. Where consensus cannot be reached, a maintainer decides ([governance](../governance.md)).
6. **Implementation.** An accepted RFC is merged as a document. Implementation happens through ordinary pull requests that link to it, by the author or by anyone. The RFC's status is updated to *implemented* (with the version) when done.

Rejected and deferred RFCs are kept, with the reasons, so the same discussion does not need to be repeated.

## What an RFC must cover

| Section | What to say |
|---|---|
| **Summary** | One paragraph |
| **Motivation** | What problem is solved, for whom, and evidence it is real |
| **Design** | How it works, precisely enough for someone else to implement |
| **Alternatives** | What else was considered, including doing nothing |
| **Compatibility** | Effect on existing deployments, API routes, configuration keys, the storage layout and SAP protocol behaviour. Breaking changes must be called out with a migration path |
| **Security** | New attack surface, credentials, data exposure, logging ([logging policy](../operations/security.md#logging-policy)) |
| **Testing** | How it will be tested without Microsoft 365 or SAP credentials (the Graph emulator), and what needs real systems |
| **Migration** | How existing users move to it, and how to roll back |
| **Open questions** | What is not decided yet |

## Statuses

`draft` → `accepted` / `rejected` / `deferred` → `implemented` (or `withdrawn` by the author).

## Index

This index lists the RFCs in this directory.

| RFC | Title | Status |
|---|---|---|
| [0001](0001-repositories-and-storage-backends.md) | Multiple repositories with local or SharePoint storage | draft |
| [0002](0002-server-admin-ui.md) | Admin UI built into the server | draft |
