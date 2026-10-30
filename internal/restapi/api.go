// Package restapi implements the Document REST API, optimized for SAP Fiori
// (UploadSet/attachment controls) and other enterprise HTTP clients.
package restapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/auth"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/observability"
	"github.com/Lamiv/zaks-drive/internal/safelog"
	"github.com/Lamiv/zaks-drive/internal/storage"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

type Options struct {
	DownloadMode string // proxy | redirect
	MaxUpload    int64
	CORSOrigins  []string
}

type API struct {
	svc  *storage.Service
	auth *auth.Authenticator
	opts Options
	log  *slog.Logger
}

func New(svc *storage.Service, a *auth.Authenticator, opts Options, log *slog.Logger) *API {
	return &API{svc: svc, auth: a, opts: opts, log: log}
}

const prefix = "/api/v1"

// Handler returns the routed API (without observability middleware).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	r := func(pattern string, perm auth.Permission, h repoHandler) {
		mux.Handle(pattern, a.withRepo(perm, h))
	}
	mux.Handle("GET "+prefix+"/repositories", a.authenticated(a.listRepositories))

	r("POST "+prefix+"/repositories/{repo}/documents", auth.PermWrite, a.upload)
	r("GET "+prefix+"/repositories/{repo}/documents/{id}", auth.PermRead, a.getDocument)
	r("DELETE "+prefix+"/repositories/{repo}/documents/{id}", auth.PermDelete, a.deleteDocument)
	r("GET "+prefix+"/repositories/{repo}/documents/{id}/content", auth.PermRead, a.download)
	r("PUT "+prefix+"/repositories/{repo}/documents/{id}/content", auth.PermWrite, a.replace)
	r("GET "+prefix+"/repositories/{repo}/documents/{id}/thumbnail", auth.PermRead, a.thumbnail)
	r("GET "+prefix+"/repositories/{repo}/documents/{id}/versions", auth.PermRead, a.versions)
	r("GET "+prefix+"/repositories/{repo}/children", auth.PermRead, a.children)
	r("GET "+prefix+"/repositories/{repo}/lookup", auth.PermRead, a.lookup)
	r("POST "+prefix+"/repositories/{repo}/folders", auth.PermWrite, a.createFolder)
	r("GET "+prefix+"/repositories/{repo}/search", auth.PermRead, a.search)

	// Business-object attachments: documents stored under {objectType}/{objectKey}.
	r("GET "+prefix+"/repositories/{repo}/objects/{type}/{key}/documents", auth.PermRead, a.objectDocuments)
	r("POST "+prefix+"/repositories/{repo}/objects/{type}/{key}/documents", auth.PermWrite, a.upload)

	return a.cors(mux)
}

type repoHandler func(w http.ResponseWriter, r *http.Request, repo *storage.Repository)

func (a *API) authenticated(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.auth.Authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sharepoint-adapter"`)
			a.fail(w, r, err)
			return
		}
		next(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

func (a *API) withRepo(perm auth.Permission, h repoHandler) http.Handler {
	return a.authenticated(func(w http.ResponseWriter, r *http.Request) {
		repo, err := a.svc.Repository(r.PathValue("repo"))
		p := auth.PrincipalFrom(r.Context())
		// Unknown repositories and repositories the credential cannot even
		// read look the same, so repository names are not leaked.
		if err != nil || !p.Can(repo.ID, perm) {
			if err == nil && p.Can(repo.ID, auth.PermRead) {
				a.fail(w, r, apperr.New(apperr.AuthPermissionDenied,
					"%s may read repository %q but this operation needs the %q permission (granted: %s)",
					p.Name, repo.ID, perm, permList(p)).
					WithFix("Use a key with %q permission on %q, or add it to this key's `permissions` / `repositories` in auth.apiKeys (config.yaml) and recreate the container.", perm, repo.ID))
				return
			}
			a.fail(w, r, apperr.New(apperr.RepoNotFound, "repository %q does not exist or %s may not use it; repositories available to this credential: %s",
				r.PathValue("repo"), p.Name, a.visibleRepos(p)))
			return
		}
		h(w, r, repo)
	})
}

// ---- handlers ----

func (a *API) listRepositories(w http.ResponseWriter, r *http.Request) {
	p := auth.PrincipalFrom(r.Context())
	type repoOut struct {
		ID       string   `json:"id"`
		ReadOnly bool     `json:"readOnly"`
		Access   []string `json:"access"`
	}
	out := []repoOut{}
	for _, repo := range a.svc.Repositories() {
		var access []string
		for _, perm := range []auth.Permission{auth.PermRead, auth.PermWrite, auth.PermDelete} {
			if p.Can(repo.ID, perm) {
				access = append(access, string(perm))
			}
		}
		if len(access) > 0 {
			out = append(out, repoOut{ID: repo.ID, ReadOnly: repo.ReadOnly, Access: access})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

func (a *API) getDocument(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	it, err := a.svc.Get(r.Context(), repo, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if it.ETag != "" {
		w.Header().Set("ETag", it.ETag)
	}
	writeJSON(w, http.StatusOK, toDocument(repo, it))
}

func (a *API) lookup(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	it, err := a.svc.GetByPath(r.Context(), repo, r.URL.Query().Get("path"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDocument(repo, it))
}

func (a *API) download(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	ctx := r.Context()
	it, err := a.svc.Get(ctx, repo, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if it.Folder {
		a.fail(w, r, apperr.New(apperr.ItemIsFolder, "item %s is a folder and has no content", it.ID))
		return
	}
	// cTag changes only when content changes: ideal validator for caches.
	etag := it.CTag
	if etag == "" {
		etag = it.ETag
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
		if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(inm, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	mode := a.opts.DownloadMode
	if m := r.URL.Query().Get("mode"); m == "redirect" || m == "proxy" {
		mode = m
	}
	if mode == "redirect" && it.DirectURL != "" {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, it.DirectURL, http.StatusFound)
		return
	}

	ct := "application/octet-stream"
	if it.MimeType != "" {
		ct = it.MimeType
	}
	disp := "attachment"
	if d := r.URL.Query().Get("disposition"); d == "inline" {
		disp = "inline"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", contentDisposition(disp, it.Name))
	h.Set("Cache-Control", "private, no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	if !it.Modified.IsZero() {
		h.Set("Last-Modified", it.Modified.UTC().Format(http.TimeFormat))
	}
	if r.Method == http.MethodHead {
		h.Set("Content-Length", strconv.FormatInt(it.Size, 10))
		h.Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		return
	}
	rng := r.Header.Get("Range")
	if ir := r.Header.Get("If-Range"); ir != "" && ir != etag {
		rng = ""
	}
	res, err := a.svc.Download(ctx, w, repo, it, rng)
	if err != nil {
		if res.Status == 0 {
			// Nothing written yet: a clean error response is still possible.
			for _, k := range []string{"Content-Type", "Content-Disposition", "ETag", "Last-Modified"} {
				h.Del(k)
			}
			a.fail(w, r, err)
			return
		}
		if ctx.Err() == nil {
			a.log.Warn("download interrupted", "request_id", observability.RequestID(ctx), "item", it.ID, "bytes", res.Bytes, safelog.Err(err))
		}
	}
}

func (a *API) thumbnail(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	size := r.URL.Query().Get("size")
	switch size {
	case "":
		size = "medium"
	case "small", "medium", "large":
	default:
		a.fail(w, r, apperr.New(apperr.ThumbnailSizeInvalid, "size is %q; use small, medium or large", size))
		return
	}
	resp, err := a.svc.Thumbnail(r.Context(), repo, r.PathValue("id"), size)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = a.svc.Engine().StreamResponse(r.Context(), w, resp)
}

func (a *API) versions(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	vs, err := a.svc.Versions(r.Context(), repo, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	type vOut struct {
		ID         string    `json:"id"`
		Size       int64     `json:"size"`
		ModifiedAt time.Time `json:"modifiedAt"`
		ModifiedBy string    `json:"modifiedBy,omitempty"`
	}
	out := make([]vOut, 0, len(vs))
	for _, v := range vs {
		out = append(out, vOut{ID: v.ID, Size: v.Size, ModifiedAt: v.Modified, ModifiedBy: v.ModifiedBy})
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

func (a *API) deleteDocument(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	if err := a.svc.Delete(r.Context(), repo, r.PathValue("id"), r.Header.Get("If-Match")); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) children(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	q := r.URL.Query()
	pg, err := a.svc.List(r.Context(), repo, q.Get("path"), topParam(q), q.Get("cursor"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writePage(w, repo, pg)
}

func (a *API) objectDocuments(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	folder, err := objectFolder(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	pg, err := a.svc.List(r.Context(), repo, folder, topParam(q), q.Get("cursor"))
	if errors.Is(err, storage.ErrNotFound) {
		// No attachments yet: the folder is created on first upload.
		writePage(w, repo, &storage.Page{})
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writePage(w, repo, pg)
}

func (a *API) search(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	q := r.URL.Query()
	pg, err := a.svc.Search(r.Context(), repo, q.Get("q"), topParam(q), q.Get("cursor"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writePage(w, repo, pg)
}

func (a *API) createFolder(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		a.fail(w, r, apperr.New(apperr.RequestBodyInvalid, "the body must be JSON like {\"path\": \"folder/subfolder\"}: %v", err))
		return
	}
	it, err := a.svc.CreateFolder(r.Context(), repo, body.Path)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toDocument(repo, it))
}

// upload accepts either a raw body (file name via ?fileName=, Slug,
// X-File-Name or Content-Disposition) or multipart/form-data with one file
// part. Form fields "folder" and "conflict" may precede the file part.
func (a *API) upload(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	q := r.URL.Query()
	req := storage.UploadRequest{Folder: q.Get("folder"), Size: r.ContentLength}
	if r.PathValue("type") != "" {
		folder, err := objectFolder(r)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		req.Folder = folder
	}
	conflict := q.Get("conflict")
	r.Body = http.MaxBytesReader(w, r.Body, a.opts.MaxUpload+1<<20)

	mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "multipart/form-data" {
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				a.fail(w, r, apperr.New(apperr.UploadNoFilePart, "the multipart body ended without a part that has a filename"))
				return
			}
			if err != nil {
				a.fail(w, r, apperr.New(apperr.UploadBodyInvalid, "the multipart body could not be read: %v", err).Wrap(storage.ErrInvalid))
				return
			}
			if part.FileName() == "" {
				v, _ := io.ReadAll(io.LimitReader(part, 4096))
				switch part.FormName() {
				case "folder":
					if r.PathValue("type") == "" {
						req.Folder = string(v)
					}
				case "conflict":
					conflict = string(v)
				case "fileName":
					req.Name = string(v)
				}
				continue
			}
			if req.Name == "" {
				req.Name = part.FileName()
			}
			req.Body, req.Size = part, -1
			break
		}
	} else {
		req.Name = fileNameFromRequest(r)
		req.Body = r.Body
	}
	if req.Name == "" {
		a.fail(w, r, apperr.New(apperr.UploadFilenameRequired, "no file name found in the fileName query, the Slug or X-File-Name header, the Content-Disposition header or a multipart filename"))
		return
	}
	switch conflict {
	case "", "rename":
		req.Conflict = storage.ConflictRename
	case "replace":
		req.Conflict = storage.ConflictReplace
	case "fail":
		req.Conflict = storage.ConflictFail
	default:
		a.fail(w, r, apperr.New(apperr.UploadConflictInvalid, "conflict is %q; use rename, replace or fail", conflict))
		return
	}
	it, err := a.svc.Upload(r.Context(), repo, req)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	doc := toDocument(repo, it)
	w.Header().Set("Location", doc.Links.Self)
	writeJSON(w, http.StatusCreated, doc)
}

func (a *API) replace(w http.ResponseWriter, r *http.Request, repo *storage.Repository) {
	r.Body = http.MaxBytesReader(w, r.Body, a.opts.MaxUpload+1)
	it, err := a.svc.Replace(r.Context(), repo, r.PathValue("id"), r.Header.Get("If-Match"), r.Body, r.ContentLength)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDocument(repo, it))
}

// ---- helpers ----

func objectFolder(r *http.Request) (string, error) {
	t, k := r.PathValue("type"), r.PathValue("key")
	if err := storage.ValidName(t); err != nil {
		return "", err
	}
	if err := storage.ValidName(k); err != nil {
		return "", err
	}
	return t + "/" + k, nil
}

func fileNameFromRequest(r *http.Request) string {
	if n := r.URL.Query().Get("fileName"); n != "" {
		return n
	}
	if v := firstNonEmpty(r.Header.Get("Slug"), r.Header.Get("X-File-Name")); v != "" {
		if dec, err := url.QueryUnescape(v); err == nil {
			return dec
		}
		return v
	}
	if cd := r.Header.Get("Content-Disposition"); cd != "" {
		if _, p, err := mime.ParseMediaType(cd); err == nil {
			return p["filename"]
		}
	}
	return ""
}

func topParam(q url.Values) int {
	n, _ := strconv.Atoi(q.Get("top"))
	if n <= 0 || n > 1000 {
		return 200
	}
	return n
}

// etagMatch reports whether an If-None-Match list matches etag. SharePoint
// tags contain commas ("c:{GUID},2"), so the list is split on quotes, not commas.
func etagMatch(header, etag string) bool {
	want := strings.Trim(etag, `"`)
	h := strings.TrimSpace(header)
	if h == "*" {
		return true
	}
	for {
		start := strings.IndexByte(h, '"')
		if start < 0 {
			return false
		}
		end := strings.IndexByte(h[start+1:], '"')
		if end < 0 {
			return false
		}
		if h[start+1:start+1+end] == want {
			return true
		}
		h = h[start+end+2:]
	}
}

func contentDisposition(kind, name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, ascii, url.PathEscape(name))
}

func writePage(w http.ResponseWriter, repo *storage.Repository, pg *storage.Page) {
	docs := make([]Document, 0, len(pg.Items))
	for i := range pg.Items {
		docs = append(docs, toDocument(repo, &pg.Items[i]))
	}
	body := map[string]any{"value": docs}
	if pg.Cursor != "" {
		body["nextCursor"] = pg.Cursor
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeProblem sends an RFC 7807 response extended with the error code, a
// hint (the fix) and a link that explains the code.
func writeProblem(w http.ResponseWriter, r *http.Request, e *apperr.Error) {
	status := e.Status()
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(max(int(e.RetryAfter.Seconds()+0.5), 1)))
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	detail := e.Detail
	if detail == "" {
		detail = e.Title()
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // keep <key> readable in hints
	_ = enc.Encode(map[string]any{
		"type":      e.URL(),
		"title":     e.Title(),
		"status":    status,
		"code":      e.Code,
		"detail":    detail,
		"hint":      e.Fix(),
		"requestId": observability.RequestID(r.Context()),
	})
}

// fail answers with the coded error behind err. Errors without a code are
// classified (SharePoint/Graph, size limits, cancellation) or reported as an
// internal error that quotes the message.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var mbe *http.MaxBytesError
	if errors.Is(err, context.Canceled) {
		w.WriteHeader(499) // the client went away; nothing useful to send
		return
	}
	e, ok := apperr.As(err)
	if !ok {
		switch {
		case errors.As(err, &mbe):
			e = apperr.New(apperr.UploadTooLarge, "the request body is larger than %d bytes (the limit is transfer.maxUploadBytes)", mbe.Limit).Wrap(transfer.ErrTooLarge)
		case graph.Classify(err) != nil:
			e = graph.Classify(err)
		default:
			e = apperr.New(apperr.InternalError, "%v", err)
		}
	}
	rid := observability.RequestID(r.Context())
	if e.Status() >= 500 {
		a.log.Error("request failed", "request_id", rid, "code", e.Code, "status", e.Status(), "title", e.Title(), safelog.Err(err))
	} else {
		a.log.Warn("request refused", "request_id", rid, "code", e.Code, "status", e.Status(), "title", e.Title())
	}
	writeProblem(w, r, e)
}

func permList(p *auth.Principal) string {
	var out []string
	for _, x := range p.Permissions {
		out = append(out, string(x))
	}
	return strings.Join(out, ", ")
}

// visibleRepos lists the repository IDs the principal can read.
func (a *API) visibleRepos(p *auth.Principal) string {
	var out []string
	for _, repo := range a.svc.Repositories() {
		if p.Can(repo.ID, auth.PermRead) {
			out = append(out, repo.ID)
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

func (a *API) cors(next http.Handler) http.Handler {
	if len(a.opts.CORSOrigins) == 0 {
		return next
	}
	allowAll := false
	allowed := map[string]bool{}
	for _, o := range a.opts.CORSOrigins {
		if o == "*" {
			allowAll = true
		}
		allowed[strings.TrimRight(o, "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (allowAll || allowed[origin]) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Expose-Headers", "ETag, Content-Disposition, Content-Range, Location, X-Request-ID, Retry-After")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, Slug, X-File-Name, If-Match, If-None-Match, Range, X-Request-ID, X-CSRF-Token")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
