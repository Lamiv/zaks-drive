package apperr

// The catalogue of every error code the adapter can return or print. Each
// code has a stable identifier, the default HTTP status, a one-line title,
// the usual cause and what to do about it. docs/error-codes.md is generated
// from this list (`adapter errors`), and a test fails when it is stale.

// Areas group the codes in the documentation, in this order.
const (
	AreaAuth    = "Authentication and permissions"
	AreaRepo    = "Repositories, folders and names"
	AreaDoc     = "Documents and uploads"
	AreaSP      = "SharePoint and Microsoft Graph"
	AreaCS      = "SAP Content Server interface"
	AreaConfig  = "Configuration and startup"
	AreaCLI     = "Command line and admin endpoints"
	AreaGeneral = "General"
)

var (
	// ---- Authentication and permissions ----
	AuthMissingCredentials = def(AreaAuth, "AUTH_MISSING_CREDENTIALS", 401, "No credentials were sent",
		"The request has neither an X-API-Key header nor an Authorization header.",
		"Send `X-API-Key: <key>` or `Authorization: Bearer <token>`. The keys are the ADAPTER_API_KEY_* values in deploy/.env.")
	AuthInvalidAPIKey = def(AreaAuth, "AUTH_INVALID_API_KEY", 401, "API key not recognised",
		"The key matches none of the entries in `auth.apiKeys`, or a bearer token was sent but `auth.jwt` is not configured.",
		"Check for typos and stray spaces or line breaks. After changing deploy/.env the container must be recreated (`docker compose ... up -d --force-recreate`). For JWT tokens configure `auth.jwt`.")
	AuthJWTInvalid = def(AreaAuth, "AUTH_JWT_INVALID", 401, "Bearer token rejected",
		"JWT validation failed. The detail names the reason: expired, wrong issuer or audience, bad signature, malformed token or unknown signing key.",
		"Request a fresh token for the audience configured in `auth.jwt`, and check `issuer`, `audience` and `jwksUrl`.")
	AuthPermissionDenied = def(AreaAuth, "AUTH_PERMISSION_DENIED", 403, "Permission denied",
		"The credential is valid but does not have the permission this operation needs (read, write or delete) for this repository.",
		"Use a key with the required permission, or extend `permissions` / `repositories` of that key in `auth.apiKeys` (config.yaml) and recreate the container.")
	AuthAdminUnauthorized = def(AreaAuth, "AUTH_ADMIN_UNAUTHORIZED", 401, "Admin token missing or wrong",
		"The admin endpoint requires `Authorization: Bearer <ADMIN_TOKEN>` and the token did not match.",
		"Send the ADMIN_TOKEN from deploy/.env. `adapter certs ...` inside the container reads it from the container environment; if you changed it, recreate the container.")

	// ---- Repositories, folders and names ----
	RepoNotFound = def(AreaRepo, "REPO_NOT_FOUND", 404, "Repository not found",
		"The repository ID does not exist, or the credential may not use it (the adapter does not reveal which).",
		"`GET /api/v1/repositories` lists the repositories this credential can use. IDs come from `repositories[].id` in config.yaml.")
	RepoReadOnly = def(AreaRepo, "REPO_READ_ONLY", 403, "Repository is read-only",
		"The repository is configured with `readOnly: true`.",
		"Write to another repository, or remove `readOnly: true` from it in config.yaml and recreate the container.")
	PathReserved = def(AreaRepo, "PATH_RESERVED", 403, "Folder is reserved for the SAP content server",
		"The path is inside `<rootPath>/ContentServer/<contRep>`, which only the SAP interface may read or change (documents, metadata and certificates live there).",
		"Use a different folder for REST uploads. Manage SAP documents through SAP.")
	NameInvalid = def(AreaRepo, "NAME_INVALID", 400, "Invalid file or folder name",
		"SharePoint does not accept the name: it is empty, `.` or `..`, contains one of \" * : < > ? / \\ | , starts or ends with a space, or ends with a dot.",
		"Rename it. The detail shows the offending name.")
	NameTooLong = def(AreaRepo, "NAME_TOO_LONG", 400, "Name too long",
		"A name is longer than 255 characters. Special characters count three characters each (stored as %XX).",
		"Shorten the name.")
	PathTooLong = def(AreaRepo, "PATH_TOO_LONG", 400, "Path too long for SharePoint",
		"The full path would exceed SharePoint's limit of 400 characters (the detail gives the length).",
		"Shorten the file name, use fewer or shorter folder levels, or shorten `rootPath` / the content server `folder` in config.yaml.")
	CursorInvalid = def(AreaRepo, "CURSOR_INVALID", 400, "Paging cursor not valid",
		"The `cursor` was not issued by this server for this folder or search, was modified, or belongs to a server that was restarted with different Graph credentials.",
		"Start again without `cursor`, then use the `nextCursor` of each response unchanged.")
	QueryEmpty = def(AreaRepo, "QUERY_EMPTY", 400, "Search text missing",
		"The search request has no `q` parameter or it is blank.", "Add `?q=<text>`.")
	FolderPathRequired = def(AreaRepo, "FOLDER_PATH_REQUIRED", 400, "Folder path required",
		"Creating a folder needs a non-empty path.", "Send `{\"path\": \"folder/subfolder\"}`.")
	ItemIsFolder = def(AreaRepo, "ITEM_IS_FOLDER", 400, "The item is a folder",
		"The operation (download or replace content) only works on files.", "Use the folder's `children` listing, or pass a file ID.")

	// ---- Documents and uploads ----
	DocNotFound = def(AreaDoc, "DOC_NOT_FOUND", 404, "Document not found",
		"No file or folder with that ID or path exists in this repository (or it was deleted, or it lies outside the repository's root folder).",
		"Check the ID or path. If an upload reports this, the repository's library or root folder may have been removed: run `zaks-drive check`.")
	NameConflict = def(AreaDoc, "NAME_CONFLICT", 409, "A file with that name already exists",
		"Upload with `conflict=fail` found an existing file of the same name.",
		"Use `conflict=rename` (default) to keep both, `conflict=replace` to overwrite, or choose another name.")
	ETagMismatch = def(AreaDoc, "ETAG_MISMATCH", 412, "Document changed since it was read",
		"The `If-Match` value does not match the document's current ETag, so someone else modified it.",
		"Read the document again, merge your change and retry with the new ETag.")
	UploadTooLarge = def(AreaDoc, "UPLOAD_TOO_LARGE", 413, "Upload too large",
		"The file exceeds `transfer.maxUploadBytes` (the detail gives the limit).",
		"Send a smaller file, or raise `transfer.maxUploadBytes` in config.yaml (SharePoint's own limit is 250 GB).")
	UploadFilenameRequired = def(AreaDoc, "UPLOAD_FILENAME_REQUIRED", 400, "File name missing",
		"An upload needs a file name and none was found.",
		"Add `?fileName=...`, or a `Slug` / `X-File-Name` header, or a `Content-Disposition` filename, or use a multipart file part with a filename.")
	UploadNoFilePart = def(AreaDoc, "UPLOAD_NO_FILE_PART", 400, "Multipart upload without a file",
		"The multipart body contains only form fields and no file part.",
		"Add the file as a part with a `filename`. Form fields `folder`, `conflict` and `fileName` must come before the file part.")
	UploadConflictInvalid = def(AreaDoc, "UPLOAD_CONFLICT_INVALID", 400, "Unknown conflict mode",
		"`conflict` must be `rename`, `replace` or `fail`.", "Use one of the three values.")
	UploadBodyInvalid = def(AreaDoc, "UPLOAD_BODY_INVALID", 400, "Request body could not be read",
		"The body is malformed (for example a broken multipart stream) or the client disconnected while sending.",
		"Check the Content-Type and boundary, and retry. The detail has the parser's message.")
	RequestBodyInvalid = def(AreaDoc, "REQUEST_BODY_INVALID", 400, "Request body not valid",
		"The JSON body is missing or malformed.", "Send valid JSON as described in the OpenAPI document (docs/openapi.yaml).")
	ThumbnailSizeInvalid = def(AreaDoc, "THUMBNAIL_SIZE_INVALID", 400, "Unknown thumbnail size",
		"`size` must be `small`, `medium` or `large`.", "Use one of the three values.")
	ThumbnailUnavailable = def(AreaDoc, "THUMBNAIL_UNAVAILABLE", 404, "No thumbnail for this repository",
		"The repository is stored in a local folder, which does not generate thumbnails (SharePoint repositories do).", "Download the document, or ask for thumbnails only on SharePoint repositories (`capabilities` in the repository list).")

	// ---- SharePoint and Microsoft Graph ----
	TokenFailed = def(AreaSP, "TOKEN_FAILED", 502, "Cannot obtain a Microsoft access token",
		"Microsoft Entra ID refused the adapter's credentials or could not be reached. The detail has Entra's own error.",
		"Run `zaks-drive check`. `invalid_client`: wrong or expired client secret (use the secret VALUE, not its ID). `AADSTS700016` or `unauthorized_client`: wrong client ID or tenant. `AADSTS90002`: wrong tenant ID. Network errors: allow outbound HTTPS to login.microsoftonline.com.")
	SPUnauthorized = def(AreaSP, "SP_UNAUTHORIZED", 502, "SharePoint rejected the adapter's token (401)",
		"Entra issued a token, but Graph refused it. The usual cause is that the app has no Graph APPLICATION permission, or admin consent was not granted.",
		"In Entra: App registration > API permissions, add the permission as type Application (Sites.Selected plus a site grant, or Sites.ReadWrite.All) and click Grant admin consent. Run `zaks-drive check`; its roles line shows what the token carries.")
	SPAccessDenied = def(AreaSP, "SP_ACCESS_DENIED", 502, "SharePoint denied access to this site or library (403)",
		"The token is valid but the app has no (or only read) access to this site or library.",
		"With Sites.Selected, grant the app role `write` on the site (POST /sites/{id}/permissions); a `read` grant cannot upload or delete. Or use Sites.ReadWrite.All. Run `zaks-drive check`.")
	SPSiteNotFound = def(AreaSP, "SP_SITE_NOT_FOUND", 502, "SharePoint site not found",
		"The site address does not exist, is misspelt, or the app cannot see it.",
		"Open the address in a browser to confirm it. Use https://<tenant>.sharepoint.com/sites/<name> (SHAREPOINT_SITE_URL). With Sites.Selected the site must also be granted to the app (README, step 2).")
	SPLibraryNotFound = def(AreaSP, "SP_LIBRARY_NOT_FOUND", 502, "Document library not found on the site",
		"`driveName` (SHAREPOINT_LIBRARY) does not match the display name of any library on the site; the detail lists the libraries the app can see.",
		"Set SHAREPOINT_LIBRARY to one of the listed names. It is the library's display name, which can differ from the URL segment (for example \"Documents\" for \"Shared Documents\").")
	SPUserDriveNotFound = def(AreaSP, "SP_USER_DRIVE_NOT_FOUND", 502, "OneDrive of the user not found",
		"The user has no provisioned OneDrive (never signed in, no licence) or `userId` is misspelt.",
		"Use the user's sign-in name (user@contoso.com). The user must have a OneDrive; a SharePoint site is usually the better choice for a shared repository.")
	SPNotFound = def(AreaSP, "SP_NOT_FOUND", 502, "SharePoint does not know this site, library or folder (404)",
		"Graph answered 404 for the configured location: the drive ID is wrong or deleted, or the library or root folder was removed.",
		"Check SHAREPOINT_SITE_URL, SHAREPOINT_LIBRARY and SHAREPOINT_ROOT_PATH (or driveId) in deploy/.env / config.yaml. Run `zaks-drive check`.")
	SPThrottled = def(AreaSP, "SP_THROTTLED", 503, "SharePoint is throttling requests",
		"Microsoft limits the request rate per app and tenant; the adapter already retried with back-off.",
		"Retry after the `Retry-After` seconds. Reduce `graph.maxConcurrency` in config.yaml or spread bulk loads over time.")
	SPUnreachable = def(AreaSP, "SP_UNREACHABLE", 502, "Cannot reach Microsoft Graph or SharePoint",
		"A network error occurred (connection refused or reset, DNS failure, TLS error, proxy). The detail has the system message.",
		"Allow outbound HTTPS (443) from the adapter server to graph.microsoft.com, login.microsoftonline.com and *.sharepoint.com; check DNS, proxy and clock.")
	SPTimeout = def(AreaSP, "SP_TIMEOUT", 504, "SharePoint did not answer in time",
		"A Graph call exceeded its time limit.", "Retry. For very large files check the network bandwidth to Microsoft 365.")
	SPLocked = def(AreaSP, "SP_LOCKED", 423, "The file is locked in SharePoint",
		"The file is checked out or being edited by someone else.", "Wait, or ask the user to close or check in the file in SharePoint, then retry.")
	SPQuotaExceeded = def(AreaSP, "SP_QUOTA_EXCEEDED", 507, "SharePoint storage quota exhausted",
		"The site collection or OneDrive has no free storage left.", "Free space or raise the quota in the SharePoint admin center.")
	SPRejected = def(AreaSP, "SP_REJECTED", 502, "SharePoint rejected the request",
		"Graph answered with an error that has no more specific code; the detail gives Graph's code and message.",
		"Read the detail. If it mentions a name, path or size, correct the request; otherwise quote the request ID when asking for help.")
	SPUnavailable = def(AreaSP, "SP_UNAVAILABLE", 502, "SharePoint is temporarily unavailable (5xx)",
		"Graph answered with a server error after the adapter's retries.",
		"Retry later and check https://status.cloud.microsoft . Quote the request ID if it persists.")

	// ---- SAP Content Server interface ----
	CSClientNotAllowed = def(AreaCS, "CS_CLIENT_NOT_ALLOWED", 403, "Client address not allowed",
		"The caller's IP address is not inside `contentServer.allowedNetworks`.",
		"Add the SAP application server's network (CIDR) to `contentServer.allowedNetworks` in config.yaml, or empty the list to allow all. The adapter sees the address the connection comes from; behind a proxy that is the proxy.")
	CSCommandMissing = def(AreaCS, "CS_COMMAND_MISSING", 400, "No command in the URL",
		"The query string must start with the command, for example `?get&pVersion=0047&contRep=Z1&docId=...`.", "Check the OAC0 HTTP script and server settings.")
	CSCommandUnknown = def(AreaCS, "CS_COMMAND_UNKNOWN", 400, "Unknown command",
		"The command is not part of the SAP Content Server HTTP interface.", "Check the URL; supported commands are listed in docs/content-server.md.")
	CSCommandUnsupported = def(AreaCS, "CS_COMMAND_UNSUPPORTED", 501, "Command not supported",
		"`attrSearch` and `getCert` are not implemented (print-list attribute search and the 0046 cache-server certificate fetch).",
		"Not needed for DMS, GOS or attachments. If a SAP function really requires it, open an issue with the SAP transaction and the request ID.")
	CSAdminUnsupported = def(AreaCS, "CS_ADMIN_UNSUPPORTED", 501, "adminContRep (CSADMIN) is not supported",
		"SAP's CSADMIN tool tried to read or write this content server's configuration. The adapter is configured in config.yaml instead.",
		"Edit deploy/config.yaml and recreate the container; manage certificates with `adapter certs`. Only CSADMIN is affected; DMS, GOS and attachments work normally.")
	CSMethodNotAllowed = def(AreaCS, "CS_METHOD_NOT_ALLOWED", 405, "HTTP method not allowed for this command",
		"The command was sent with an HTTP method it does not accept (for example `append` needs PUT).", "Use the method named in the detail.")
	CSParamMissing = def(AreaCS, "CS_PARAM_MISSING", 400, "A required URL parameter is missing",
		"The detail names the parameter (for example `docId` or `compId`).", "Add it to the URL.")
	CSContRepUnknown = def(AreaCS, "CS_CONTREP_UNKNOWN", 404, "Unknown content repository",
		"The `contRep` of the URL is not configured under `contentServer.repositories`.",
		"The ID in OAC0 must equal `contentServer.repositories[].contRep` in config.yaml (the detail lists the configured IDs).")
	CSDocNotFound = def(AreaCS, "CS_DOC_NOT_FOUND", 404, "Document not found",
		"No document with that `docId` exists in this content repository.",
		"Expected for a new document or after deletion. Otherwise check that SAP and the adapter use the same repository location (`docker compose logs adapter | grep 'stored here'`).")
	CSCompNotFound = def(AreaCS, "CS_COMP_NOT_FOUND", 404, "Component not found",
		"The document exists but has no component with that `compId`.", "List the components with `info`.")
	CSDocExists = def(AreaCS, "CS_DOC_EXISTS", 403, "Document already exists",
		"`create` was sent for a `docId` that already has components (SAP's documented answer is 403).", "Use `update` to change an existing document.")
	CSCompExists = def(AreaCS, "CS_COMP_EXISTS", 403, "Component already exists",
		"`create` was sent for a component that already exists.", "Use `update` to overwrite it.")
	CSMultipartRequired = def(AreaCS, "CS_MULTIPART_REQUIRED", 400, "multipart/form-data body required",
		"The request must carry a multipart body with a boundary, and the Content-Type header had none.", "Send Content-Type `multipart/form-data; boundary=...`.")
	CSMultipartInvalid = def(AreaCS, "CS_MULTIPART_INVALID", 400, "Multipart body could not be parsed",
		"The body is malformed (broken boundary, truncated stream).", "Enable `LOG_LEVEL=debug` to see what SAP sent (docs/content-server.md).")
	CSCompDuplicate = def(AreaCS, "CS_COMP_DUPLICATE", 400, "Two parts have the same component ID",
		"One request contained the same `compId` twice.", "Send each component once.")
	CSMCreateDocIDMissing = def(AreaCS, "CS_MCREATE_DOCID_MISSING", 400, "mCreate part without X-docId",
		"Every part of an `mCreate` request needs an `X-docId` header.", "Check the SAP client or the report that sent the request.")
	CSOffsetInvalid = def(AreaCS, "CS_OFFSET_INVALID", 400, "Invalid fromOffset / toOffset",
		"The offsets are not numbers, are negative (other than -1), or lie outside the component.", "Use 0 <= fromOffset <= toOffset < size; toOffset=-1 means the end.")
	CSNumResultsInvalid = def(AreaCS, "CS_NUMRESULTS_INVALID", 400, "Invalid numResults",
		"`numResults` must be a positive whole number.", "Fix the value.")
	CSSearchRangeTooLarge = def(AreaCS, "CS_SEARCH_RANGE_TOO_LARGE", 400, "Backward search range too large",
		"A backward `search` (fromOffset > toOffset) reads the range into memory and is limited to 64 MiB.", "Search a smaller range, or search forward.")
	CSIDInvalid = def(AreaCS, "CS_ID_INVALID", 400, "docId or compId not usable",
		"The identifier is empty or too long to store (more than 255 characters after encoding).", "Check what SAP sent (`LOG_LEVEL=debug`).")
	CSSecKeyMissing = def(AreaCS, "CS_SECKEY_MISSING", 401, "Signature (secKey) required",
		"The repository has `signature: required` (or the document is protected by docProt) and the URL has no `secKey`.",
		"In OAC0 the repository's \"No signature\" option must be unchecked so that SAP signs URLs; and the certificate must be sent and activated (README, step 9). To test without signatures use a separate repository with `signature: none`.")
	CSSecKeyParamsMissing = def(AreaCS, "CS_SECKEY_PARAMS_MISSING", 401, "accessMode, authId or expiration missing",
		"A signed URL must carry `accessMode`, `authId` and `expiration` next to `secKey`.", "Report this together with the request ID: SAP normally always sends them.")
	CSAccessModeDenied = def(AreaCS, "CS_ACCESSMODE_DENIED", 401, "Signed access mode does not allow this operation",
		"The signature only covers some operations (r = read, c = create, u = update, d = delete) and this command needs another.", "SAP creates a new URL per operation; report it with the request ID if it persists.")
	CSExpirationInvalid = def(AreaCS, "CS_EXPIRATION_INVALID", 401, "expiration is not a valid time",
		"`expiration` must be UTC in the form YYYYMMDDHHMMSS.", "Report with the request ID.")
	CSURLExpired = def(AreaCS, "CS_URL_EXPIRED", 401, "Signed URL expired",
		"The URL's `expiration` has passed (the detail shows both times). Usually the clocks of the SAP system and the adapter server differ.",
		"Synchronise time (NTP) on the adapter server and the SAP application server, or raise `contentServer.clockSkew` (default 5m).")
	CSCertUnknown = def(AreaCS, "CS_CERT_UNKNOWN", 401, "No certificate for this authId",
		"SAP has not sent its certificate for this repository and authId, or it was stored elsewhere (for example a different SharePoint location).",
		"In OAC0 choose Send certificate, then `adapter certs list`. If you moved the repository to a new library, send it again or move the `~certs` folder (SETUP.md).")
	CSCertInactive = def(AreaCS, "CS_CERT_INACTIVE", 401, "Certificate received but not activated",
		"Certificates sent by SAP stay inactive until an administrator activates them.",
		"Compare the fingerprint with STRUST, then `adapter certs activate <contRep> '<authId>'` (use the exact authId from `adapter certs list`).")
	CSSecKeyMalformed = def(AreaCS, "CS_SECKEY_MALFORMED", 401, "secKey could not be decoded",
		"The secKey is not valid Base64 or not a PKCS#7 signature.", "Check that no proxy alters the query string. Report with the request ID.")
	CSSecKeyInvalid = def(AreaCS, "CS_SECKEY_INVALID", 401, "Signature verification failed",
		"The signature does not match the URL's parameters and the activated certificate: the URL was modified, or SAP signed with a different key (for example after the PSE was renewed).",
		"If SAP renewed its PSE, send the certificate again and activate it. Do not disable signatures to work around this.")
	CSCertNotRecognized = def(AreaCS, "CS_CERT_NOT_RECOGNIZED", 406, "Certificate not recognised",
		"The body of `putCert` is not an X.509 certificate (DER, PEM or PKCS#7).", "Resend from OAC0 (Send certificate).")
	CSBusy = def(AreaCS, "CS_BUSY", 409, "Concurrent modification, retry",
		"Several writers changed the same document at once and the adapter's retries ran out.", "Repeat the SAP action.")
	CSMetaUnreadable = def(AreaCS, "CS_META_UNREADABLE", 409, "Document metadata unreadable",
		"The document's `~sapdoc.json` file is corrupt or cannot be read, so its protection and content types are unknown.",
		"Restore the document folder from the SharePoint recycle bin or version history (docs/content-server.md, section 8).")

	// ---- Configuration and startup ----
	CfgFileNotFound = def(AreaConfig, "CFG_FILE_NOT_FOUND", 0, "Configuration file not found",
		"The file given with `-config` / CONFIG_FILE does not exist.", "In Docker, `deploy/config.yaml` is mounted at /etc/adapter/config.yaml. Create it from deploy/config.example.yaml.")
	CfgFileUnreadable = def(AreaConfig, "CFG_FILE_UNREADABLE", 0, "Configuration file cannot be read",
		"The file exists but the container's non-root user may not read it.", "`chmod 644 deploy/config.yaml` (it holds no secrets; those are in deploy/.env).")
	CfgYAMLInvalid = def(AreaConfig, "CFG_YAML_INVALID", 0, "Configuration file is not valid YAML",
		"The YAML syntax or the structure is wrong; the detail gives the line.", "Fix the line named. Use spaces, not tabs, for indentation.")
	CfgGraphCredentials = def(AreaConfig, "CFG_GRAPH_CREDENTIALS_MISSING", 0, "Microsoft credentials missing",
		"`graph.tenantId`, `graph.clientId` or `graph.clientSecret` is empty. They normally come from GRAPH_TENANT_ID, GRAPH_CLIENT_ID and GRAPH_CLIENT_SECRET in deploy/.env.",
		"Fill in the named values in deploy/.env (the secret VALUE, not its ID) and recreate the container.")
	CfgNoRepositories = def(AreaConfig, "CFG_NO_REPOSITORIES", 0, "No repository configured",
		"The `repositories` list is empty.", "Add at least one repository (see deploy/config.example.yaml).")
	CfgRepoIDMissing       = def(AreaConfig, "CFG_REPO_ID_MISSING", 0, "Repository without id", "A `repositories` entry has no `id`.", "Give each repository a short unique `id`, for example `DMS`.")
	CfgRepoIDDuplicate     = def(AreaConfig, "CFG_REPO_ID_DUPLICATE", 0, "Duplicate repository id", "Two repositories share the same `id`.", "Make the IDs unique.")
	CfgRepoLocationMissing = def(AreaConfig, "CFG_REPO_LOCATION_MISSING", 0, "Repository has no SharePoint location",
		"None of `siteUrl`, `userId`, `siteId` or `driveId` is set. When they use `${SHAREPOINT_SITE_URL}` the variable is empty.",
		"Set SHAREPOINT_SITE_URL (and SHAREPOINT_LIBRARY, SHAREPOINT_ROOT_PATH) in deploy/.env, or write the values into config.yaml.")
	CfgSiteURLInvalid = def(AreaConfig, "CFG_SITE_URL_INVALID", 0, "SharePoint site URL not usable",
		"`siteUrl` (SHAREPOINT_SITE_URL) is not the address of a site: it is not a URL, does not start with https://, or is the address of a page inside a library (`.../Forms/AllItems.aspx`).",
		"Use only the site address, for example https://contoso.sharepoint.com/sites/SAP_DOCS, and put the library name into `driveName` (SHAREPOINT_LIBRARY).")
	CfgDownloadMode = def(AreaConfig, "CFG_DOWNLOAD_MODE_INVALID", 0, "Unknown download mode", "`transfer.downloadMode` must be `proxy` or `redirect`.", "Fix the value.")
	CfgAuthMissing  = def(AreaConfig, "CFG_AUTH_MISSING", 0, "No inbound authentication configured",
		"The REST interface is enabled but there is no API key or JWT setting, and `auth.disabled` is not true.", "Configure `auth.apiKeys` (and set ADAPTER_API_KEY_* in deploy/.env), or `auth.jwt`.")
	CfgAPIKeyInvalid = def(AreaConfig, "CFG_APIKEY_INVALID", 0, "API key entry not usable",
		"An `auth.apiKeys` entry has neither a `key` nor a 64-character `sha256`. If it uses `${ADAPTER_API_KEY_...}`, that variable is empty.", "Set the variable in deploy/.env or give the entry a key.")
	CfgInterfaceUnknown      = def(AreaConfig, "CFG_INTERFACE_UNKNOWN", 0, "Unknown interface name", "`server.interfaces` may only contain `rest` and `contentrepo`.", "Fix the list (or ADAPTER_INTERFACES).")
	CfgNoInterfaces          = def(AreaConfig, "CFG_NO_INTERFACES", 0, "No interface enabled", "`server.interfaces` is empty.", "Enable `rest`, `contentrepo` or both.")
	CfgCSRepositoriesMissing = def(AreaConfig, "CFG_CS_REPOSITORIES_MISSING", 0, "No SAP content repository configured",
		"The `contentrepo` interface is enabled but `contentServer.repositories` is empty.", "Add a repository with the same ID as in OAC0, or remove `contentrepo` from `server.interfaces`.")
	CfgContRepInvalid    = def(AreaConfig, "CFG_CONTREP_INVALID", 0, "Invalid contRep", "A content repository ID (as in OAC0) has 1 or 2 characters.", "Use the ID created in OAC0.")
	CfgContRepDuplicate  = def(AreaConfig, "CFG_CONTREP_DUPLICATE", 0, "Duplicate contRep", "Two entries use the same `contRep`.", "Make them unique.")
	CfgContRepRepository = def(AreaConfig, "CFG_CONTREP_REPOSITORY_UNKNOWN", 0, "contRep points to an unknown repository",
		"`contentServer.repositories[].repository` does not match any `repositories[].id`.", "Use one of the IDs under `repositories:`.")
	CfgSignatureInvalid = def(AreaConfig, "CFG_SIGNATURE_INVALID", 0, "Unknown signature mode", "`signature` must be `required`, `optional` or `none`.", "Use `required` for production.")
	CfgNetworkInvalid   = def(AreaConfig, "CFG_NETWORK_INVALID", 0, "Invalid network in allowedNetworks", "An entry is not an IP address or CIDR (for example 10.0.0.0/24).", "Fix the entry.")
	CfgRepoIDInvalid    = def(AreaConfig, "CFG_REPO_ID_INVALID", 0, "Repository ID not allowed",
		"Repositories managed through `zaks-drive repo` (files in the state directory's `repos.d`) have an ID of exactly 2 characters, A-Z or 0-9, the length of an SAP content repository ID, and the file must be named `<id>.yaml`.",
		"Rename the repository, for example `ZH` or `A1`.")
	CfgRepoBackendInvalid = def(AreaConfig, "CFG_REPO_BACKEND_INVALID", 0, "Unknown repository backend",
		"`backend` must be `sharepoint` or `local`; a local repository also needs `server.stateDir`.", "Fix the value, and set `server.stateDir` (STATE_DIR) for local repositories.")
	CfgRepoLocalInvalid = def(AreaConfig, "CFG_REPO_LOCAL_INVALID", 0, "Local repository settings not usable",
		"`local.path` is missing, absolute, or contains `..`; or `local.versions` is outside 0 to 99. A local repository's path is relative to the data folder in the state directory.",
		"Use a short relative folder name such as `AD`, and a `versions` number from 0 to 99 (default 5).")
	CfgRepoCredentialsIncomplete = def(AreaConfig, "CFG_REPO_CREDENTIALS_INCOMPLETE", 0, "Repository credentials incomplete",
		"A repository with its own Microsoft application needs `tenantId`, `clientId` and `clientSecretFile` together, and the other backend types take none of them.",
		"Set all three, or none to use the global `graph.*` credentials.")
	CfgRepoSecretUnreadable = def(AreaConfig, "CFG_REPO_SECRET_UNREADABLE", 0, "Repository secret file not usable",
		"`clientSecretFile` must be a file below `secrets/` in the state directory, readable by the server, empty-free, and (on Linux) not accessible to group or others (mode 600).",
		"Recreate the secret with `zaks-drive repo update <id>`, or `chmod 600` the file inside the state volume.")
	CfgRepoFileInvalid = def(AreaConfig, "CFG_REPO_FILE_INVALID", 0, "Repository definition file not usable",
		"A file in the state directory's `repos.d` is not valid YAML, has an unknown setting, is too large, or is not named after its repository ID.",
		"Fix the file named in the detail, or remove it. The server keeps the last working state when a change fails to load.")
	StartupPortInUse = def(AreaConfig, "STARTUP_PORT_IN_USE", 0, "A listen port is already in use",
		"Another program (for example Cockpit on 9090) or another container already uses the address.",
		"Set REST_PORT, CS_PORT or ADMIN_PORT in deploy/.env to a free host port and recreate the container. `ss -ltnp | grep :<port>` shows who holds it.")
	StartupRepositoryFailed = def(AreaConfig, "STARTUP_REPOSITORY_FAILED", 0, "Cannot reach the configured SharePoint location",
		"ZAKS Drive retries until the repository resolves; /readyz shows the reason meanwhile.", "Read the detail. Run `zaks-drive check` for a step-by-step checklist.")

	// ---- Command line and admin endpoints ----
	CLIAdminUnreachable = def(AreaCLI, "CLI_ADMIN_UNREACHABLE", 0, "Cannot reach the adapter's admin endpoint",
		"`adapter certs ...` talks to the running adapter on 127.0.0.1:9090 inside the container, and nothing answered.",
		"Check that the container runs (`docker compose ps`). Run the command with `docker compose exec adapter /zaks-drive ...`, or set ADMIN_URL.")
	CLIAdminRouteMissing = def(AreaCLI, "CLI_ADMIN_ROUTE_MISSING", 0, "Certificate administration is not available",
		"The admin endpoint answered 404: the `contentrepo` interface is not enabled, or the adapter is still starting.",
		"Enable `contentrepo` in `server.interfaces`, wait for /readyz, and retry.")
	CertNotReceived = def(AreaCLI, "CERT_NOT_RECEIVED", 400, "No certificate from SAP for this repository",
		"`activate` / `deactivate` needs a certificate that SAP already sent with putCert for this contRep and authId.",
		"In OAC0 choose Send certificate, run `adapter certs list`, and use the exact contRep and authId shown.")
	CertRequestInvalid = def(AreaCLI, "CERT_REQUEST_INVALID", 400, "Admin request body not valid",
		"The body must be JSON with `contRep` and `authId`.", "Use `adapter certs activate <contRep> '<authId>'`.")

	// ---- General ----
	InternalError = def(AreaGeneral, "INTERNAL_ERROR", 500, "Internal error",
		"An unexpected failure inside the adapter.", "Quote the request ID and the log lines around it when reporting (never include secrets).")
	ClientCancelled = def(AreaGeneral, "CLIENT_CANCELLED", 499, "Client closed the request",
		"The caller disconnected before the answer was complete.", "No action needed unless it happens for every request (check client timeouts).")
)
