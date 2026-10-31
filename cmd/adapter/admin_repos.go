package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lamiv/zaks-drive/internal/adminui"
	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/contentrepo"
	"github.com/Lamiv/zaks-drive/internal/drain"
	"github.com/Lamiv/zaks-drive/internal/repoadmin"
)

// repoBody is the JSON body of a create or update: the repository settings
// plus, optionally, the Microsoft client secret value. The secret is written
// to its own file and never echoed, logged or stored in the definition.
type repoBody struct {
	repoadmin.Spec
	ClientSecret *string `json:"clientSecret,omitempty"`
}

const maxAdminBody = 64 << 10

func decodeBody(r *http.Request, w http.ResponseWriter, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.New(apperr.AdminRepoInvalid, "the request body is not the expected JSON: %s", firstLine(err.Error()))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return apperr.New(apperr.AdminRepoInvalid, "the request body has more than one JSON value")
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// repoAPI is the repository administration API on the admin listener:
//
//	GET    /admin/api/v1/status
//	GET    /admin/api/v1/repositories
//	POST   /admin/api/v1/repositories
//	GET    /admin/api/v1/repositories/{id}
//	PUT    /admin/api/v1/repositories/{id}
//	DELETE /admin/api/v1/repositories/{id}[?allowInUse=true]
//	POST   /admin/api/v1/repositories/{id}/check
//	POST   /admin/api/v1/apply            {"drainSeconds": 30}
//	POST   /admin/api/v1/apply/cancel
//	GET    /admin/api/v1/audit?lines=100
//
// Every call needs "Authorization: Bearer <ADMIN_TOKEN>"; without a
// configured token the API refuses to work at all, because it writes secrets.
type repoAPI struct {
	mgr      *repoadmin.Manager
	token    string
	applied  string // fingerprint of the repository state the server started with
	fallback string
	gate     *drain.Gate
	ready    *atomic.Bool
	version  string
	drain    time.Duration
	// stop ends the server through its normal graceful shutdown; the
	// container's restart policy then starts it again with the new state.
	stop func()
	log  *slog.Logger
	// check tests one repository (see runRepositoryCheck).
	check func(ctx context.Context, id string) (string, bool, error)

	failMu    sync.Mutex
	failures  int
	blockedTo time.Time

	applyMu  sync.Mutex
	cancelCh chan struct{}
	applying atomic.Bool // set once the drain finished and the shutdown began

	started time.Time

	// The UI (RFC 0002). sessions is nil when the UI is disabled.
	sessions *adminui.Sessions
	ring     *adminui.Ring
	certs    func() *contentrepo.Server
	diagnose func(context.Context) []finding
	settings func() map[string]any
	// serving returns the IDs of the repositories the server serves.
	serving func() []string
}

const (
	maxFailures   = 10
	failureWindow = time.Minute
)

func (a *repoAPI) register(mux *http.ServeMux) {
	h := func(pattern string, fn http.HandlerFunc) { mux.HandleFunc(pattern, a.guard(fn)) }
	h("GET /admin/api/v1/status", a.status)
	h("GET /admin/api/v1/repositories", a.list)
	h("POST /admin/api/v1/repositories", a.create)
	h("GET /admin/api/v1/repositories/{id}", a.get)
	h("PUT /admin/api/v1/repositories/{id}", a.update)
	h("DELETE /admin/api/v1/repositories/{id}", a.detach)
	h("POST /admin/api/v1/repositories/{id}/check", a.checkRepo)
	h("POST /admin/api/v1/apply", a.apply)
	h("POST /admin/api/v1/apply/cancel", a.cancelApply)
	h("GET /admin/api/v1/audit", a.audit)
	h("GET /admin/api/v1/detached", a.detached)
	h("GET /admin/api/v1/logs", a.logs)
	h("GET /admin/api/v1/settings", a.settingsHandler)
	h("GET /admin/api/v1/diagnostics", a.diagnostics)
	if a.certs == nil {
		a.certs = func() *contentrepo.Server { return nil }
	}
	h("GET /admin/api/v1/certificates", a.certList)
	h("POST /admin/api/v1/certificates/activate", a.certSet(true))
	h("POST /admin/api/v1/certificates/deactivate", a.certSet(false))
	if a.sessions != nil {
		// The login itself needs no session; it is guarded by the token, the Origin check and the lockout.
		mux.HandleFunc("GET /admin/api/v1/session", a.sessionStatus)
		mux.HandleFunc("POST /admin/api/v1/session", a.login)
		mux.HandleFunc("DELETE /admin/api/v1/session", a.logout)
		ui := adminui.Handler()
		mux.Handle("GET "+adminui.Path, ui)
		mux.HandleFunc("GET /admin/ui", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, adminui.Path, http.StatusMovedPermanently)
		})
	}
}

// authenticate decides whether the caller may use the admin API. Two
// credentials exist: the Bearer ADMIN_TOKEN (the command, scripts) and, when
// the UI is enabled, a session cookie obtained by logging in with that token
// (the browser). A change made with a cookie must also carry the session's
// CSRF token and come from this server's own page (Origin). After maxFailures
// wrong tokens the API answers 429 for a minute, whichever way they arrive.
func (a *repoAPI) authenticate(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	adminui.SecureHeaders(w.Header())
	if a.token == "" {
		apiFail(w, apperr.New(apperr.AdminTokenRequired, "ADMIN_TOKEN is not set"))
		return false
	}
	if !a.notBlocked(w) {
		return false
	}
	auth := r.Header.Get("Authorization")
	if auth != "" && subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+a.token)) == 1 {
		a.clearFailures()
		return true
	}
	if a.sessions != nil {
		if c, err := r.Cookie(adminui.CookieName); err == nil {
			if csrf, ok := a.sessions.Lookup(c.Value); ok {
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					if !adminui.SameOrigin(r) || !adminui.CSRFMatches(r, csrf) {
						apiFail(w, apperr.New(apperr.AdminCSRFInvalid, "missing or wrong CSRF token or Origin"))
						return false
					}
				}
				return true
			}
		}
	}
	if auth != "" { // a wrong token counts; a missing credential (a probe by the UI) does not
		a.recordFailure("api", r)
	}
	apiFail(w, apperr.New(apperr.AuthAdminUnauthorized, "the Authorization header is missing or does not carry the ADMIN_TOKEN"))
	return false
}

func (a *repoAPI) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.authenticate(w, r) {
			next(w, r)
		}
	}
}

// notBlocked answers 429 while the lockout lasts.
func (a *repoAPI) notBlocked(w http.ResponseWriter) bool {
	a.failMu.Lock()
	defer a.failMu.Unlock()
	if wait := time.Until(a.blockedTo); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		apiFail(w, apperr.New(apperr.AuthAdminUnauthorized, "too many wrong tokens; try again in %d seconds", int(wait.Seconds())+1).WithStatus(http.StatusTooManyRequests))
		return false
	}
	return true
}

func (a *repoAPI) clearFailures() {
	a.failMu.Lock()
	a.failures = 0
	a.failMu.Unlock()
}

// recordFailure counts a wrong token, locks the API after maxFailures, and
// audits it (the source address, never the token).
func (a *repoAPI) recordFailure(via string, r *http.Request) {
	a.failMu.Lock()
	a.failures++
	locked := a.failures >= maxFailures
	if locked {
		a.blockedTo, a.failures = time.Now().Add(failureWindow), 0
	}
	a.failMu.Unlock()
	if locked {
		a.log.Warn("admin API locked after repeated wrong tokens", "seconds", int(failureWindow.Seconds()), "via", via)
	}
	if via == "login" {
		a.mgr.Audit("login-failed", "-", "from "+remoteHost(r))
	}
}

func remoteHost(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// ---- session (login with the admin token) ----

func (a *repoAPI) sessionStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	adminui.SecureHeaders(w.Header())
	if c, err := r.Cookie(adminui.CookieName); err == nil {
		if csrf, ok := a.sessions.Lookup(c.Value); ok {
			writeAPI(w, http.StatusOK, map[string]any{"authenticated": true, "csrf": csrf})
			return
		}
	}
	writeAPI(w, http.StatusOK, map[string]any{"authenticated": false})
}

func (a *repoAPI) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	adminui.SecureHeaders(w.Header())
	if a.token == "" {
		apiFail(w, apperr.New(apperr.AdminTokenRequired, "ADMIN_TOKEN is not set"))
		return
	}
	if !a.notBlocked(w) {
		return
	}
	if !adminui.SameOrigin(r) {
		apiFail(w, apperr.New(apperr.AdminCSRFInvalid, "the login must come from the admin page"))
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := dec.Decode(&body); err != nil {
		apiFail(w, apperr.New(apperr.AdminRepoInvalid, "the login body is not the expected JSON"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(a.token)) != 1 {
		a.recordFailure("login", r)
		apiFail(w, apperr.New(apperr.AuthAdminUnauthorized, "the token is not the ADMIN_TOKEN"))
		return
	}
	a.clearFailures()
	id, csrf := a.sessions.Create()
	http.SetCookie(w, adminui.Cookie(id, r.TLS != nil, a.sessions.Max))
	a.mgr.Audit("login", "-", "from "+remoteHost(r))
	writeAPI(w, http.StatusOK, map[string]any{"csrf": csrf})
}

func (a *repoAPI) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	adminui.SecureHeaders(w.Header())
	if !adminui.SameOrigin(r) {
		apiFail(w, apperr.New(apperr.AdminCSRFInvalid, "the logout must come from the admin page"))
		return
	}
	if c, err := r.Cookie(adminui.CookieName); err == nil {
		a.sessions.Delete(c.Value)
	}
	http.SetCookie(w, adminui.Cookie("", r.TLS != nil, -1))
	writeAPI(w, http.StatusOK, map[string]any{"authenticated": false})
}

// ---- logs, settings, diagnostics, certificates ----

func (a *repoAPI) logs(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if n <= 0 || n > 1000 {
		n = 200
	}
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(r.URL.Query().Get("level"))); err != nil {
		level = slog.LevelInfo
	}
	entries := []adminui.Entry{}
	if a.ring != nil {
		entries = a.ring.Entries(n, level, r.URL.Query().Get("requestId"))
	}
	if entries == nil {
		entries = []adminui.Entry{}
	}
	writeAPI(w, http.StatusOK, entries)
}

func (a *repoAPI) settingsHandler(w http.ResponseWriter, r *http.Request) {
	s := map[string]any{}
	if a.settings != nil {
		s = a.settings()
	}
	writeAPI(w, http.StatusOK, s)
}

// finding is one server-side diagnostic.
type finding struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"` // PASS, WARN, FAIL, SKIPPED
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

func (a *repoAPI) diagnostics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out := []finding{}
	if a.diagnose != nil {
		out = a.diagnose(ctx)
	}
	writeAPI(w, http.StatusOK, map[string]any{"value": out})
}

func (a *repoAPI) certList(w http.ResponseWriter, r *http.Request) {
	cs := a.certs()
	if cs == nil {
		writeAPI(w, http.StatusOK, map[string]any{"value": []any{}})
		return
	}
	list, err := cs.CertificateList(r.Context())
	if err != nil {
		apiFail(w, err)
		return
	}
	if list == nil {
		list = []contentrepo.CertificateInfo{}
	}
	writeAPI(w, http.StatusOK, map[string]any{"value": list})
}

func (a *repoAPI) certSet(active bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cs := a.certs()
		if cs == nil {
			apiFail(w, apperr.New(apperr.CLIAdminRouteMissing, "the contentrepo interface is not enabled").WithStatus(http.StatusNotFound))
			return
		}
		var body struct {
			ContRep string `json:"contRep"`
			AuthID  string `json:"authId"`
		}
		if err := decodeBody(r, w, &body); err != nil || body.ContRep == "" || body.AuthID == "" {
			apiFail(w, apperr.New(apperr.CertRequestInvalid, "the body must be JSON like {\"contRep\":\"Z1\",\"authId\":\"CN=...\"}"))
			return
		}
		rec, err := cs.SetCertificateActive(r.Context(), body.ContRep, body.AuthID, active)
		if err != nil {
			apiFail(w, err)
			return
		}
		a.mgr.Audit(map[bool]string{true: "cert-activate", false: "cert-deactivate"}[active], body.ContRep, "")
		writeAPI(w, http.StatusOK, rec)
	}
}

func writeAPI(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// apiError is the JSON shape of every failure.
type apiError struct {
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
	More   string `json:"more,omitempty"`
}

func toAPIError(err error) (int, apiError) {
	e, ok := apperr.As(err)
	if !ok {
		e = apperr.New(apperr.InternalError, "%v", err)
	}
	status := e.Status()
	if strings.HasPrefix(e.Code, "CFG_") && status == http.StatusInternalServerError {
		status = http.StatusBadRequest
	}
	return status, apiError{Code: e.Code, Title: e.Title(), Detail: strings.TrimSpace(e.Detail), Fix: e.Fix(), More: e.URL()}
}

func apiFail(w http.ResponseWriter, err error) {
	status, body := toAPIError(err)
	writeAPI(w, status, map[string]any{"error": body})
}

// ---- handlers ----

type statusBody struct {
	Version        string `json:"version"`
	Ready          bool   `json:"ready"`
	PendingRestart bool   `json:"pendingRestart"`
	Fallback       string `json:"fallback,omitempty"`
	Draining       bool   `json:"draining"`
	InFlight       int    `json:"inFlight"`
	DrainSeconds   int    `json:"drainSeconds"`
	// StartedAt identifies this run of the server; a changed value after an
	// apply proves the restart happened.
	StartedAt time.Time `json:"startedAt"`
	// Serving lists the repositories the running server serves right now (a
	// purge must never touch one of them).
	Serving []string `json:"serving"`
}

func (a *repoAPI) status(w http.ResponseWriter, r *http.Request) {
	now, err := a.mgr.Fingerprint()
	if err != nil {
		apiFail(w, err)
		return
	}
	writeAPI(w, http.StatusOK, statusBody{
		Version: a.version, Ready: a.ready.Load(), PendingRestart: now != a.applied, Fallback: a.fallback,
		Draining: a.gate.Draining(), InFlight: a.gate.InFlight(), DrainSeconds: int(a.drain.Seconds()),
		StartedAt: a.started, Serving: a.servingIDs(),
	})
}

func (a *repoAPI) list(w http.ResponseWriter, r *http.Request) {
	views, err := a.mgr.List()
	if err != nil && len(views) == 0 {
		apiFail(w, err)
		return
	}
	body := map[string]any{"value": views}
	if err != nil {
		_, e := toAPIError(err)
		body["problem"] = e
	}
	writeAPI(w, http.StatusOK, body)
}

func (a *repoAPI) get(w http.ResponseWriter, r *http.Request) {
	v, err := a.mgr.Get(r.PathValue("id"))
	if err != nil {
		apiFail(w, err)
		return
	}
	writeAPI(w, http.StatusOK, v)
}

func (a *repoAPI) create(w http.ResponseWriter, r *http.Request) {
	var b repoBody
	if err := decodeBody(r, w, &b); err != nil {
		apiFail(w, err)
		return
	}
	secret := ""
	if b.ClientSecret != nil {
		secret = *b.ClientSecret
	}
	v, err := a.mgr.Add(b.Spec, secret)
	if err != nil {
		apiFail(w, err)
		return
	}
	a.log.Info("repository added through the admin API", "repository", v.ID, "backend", v.Backend)
	writeAPI(w, http.StatusCreated, v)
}

func (a *repoAPI) update(w http.ResponseWriter, r *http.Request) {
	var b repoBody
	if err := decodeBody(r, w, &b); err != nil {
		apiFail(w, err)
		return
	}
	if b.ID == "" {
		b.ID = r.PathValue("id")
	}
	if b.ID != r.PathValue("id") {
		apiFail(w, apperr.New(apperr.AdminRepoInvalid, "the id in the body (%q) differs from the one in the URL (%q)", b.ID, r.PathValue("id")))
		return
	}
	v, err := a.mgr.Update(b.Spec, b.ClientSecret)
	if err != nil {
		apiFail(w, err)
		return
	}
	a.log.Info("repository updated through the admin API", "repository", v.ID)
	writeAPI(w, http.StatusOK, v)
}

func (a *repoAPI) detach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.mgr.Detach(id, r.URL.Query().Get("allowInUse") == "true"); err != nil {
		apiFail(w, err)
		return
	}
	a.log.Info("repository detached through the admin API", "repository", id)
	writeAPI(w, http.StatusOK, map[string]any{"detached": id, "documentsDeleted": false})
}

func (a *repoAPI) checkRepo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	report, ok, err := a.check(ctx, r.PathValue("id"))
	if err != nil {
		apiFail(w, err)
		return
	}
	writeAPI(w, http.StatusOK, map[string]any{"ok": ok, "report": report})
}

func (a *repoAPI) audit(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if n <= 0 || n > 1000 {
		n = 100
	}
	b, err := a.mgr.AuditLog(n)
	if err != nil {
		apiFail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	_, _ = w.Write(b)
}

// apply puts the server into maintenance mode, waits for running requests
// (bounded) and then ends the process so the container is restarted with the
// new repositories. It answers at once; /status shows the progress.
func (a *repoAPI) apply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DrainSeconds int `json:"drainSeconds"`
	}
	if r.ContentLength != 0 {
		if err := decodeBody(r, w, &body); err != nil {
			apiFail(w, err)
			return
		}
	}
	drainFor := a.drain
	if body.DrainSeconds > 0 {
		drainFor = time.Duration(min(body.DrainSeconds, 600)) * time.Second
	}
	a.applyMu.Lock()
	if !a.gate.Begin() {
		a.applyMu.Unlock()
		apiFail(w, apperr.New(apperr.AdminApplyBusy, "the server is already draining"))
		return
	}
	cancel := make(chan struct{})
	a.cancelCh = cancel
	a.applyMu.Unlock()

	inFlight := a.gate.InFlight()
	a.log.Warn("maintenance mode: refusing new requests to apply repository changes", "in_flight", inFlight, "drain_seconds", int(drainFor.Seconds()))
	a.mgr.Audit("apply", "-", fmt.Sprintf("drain=%ds inFlight=%d", int(drainFor.Seconds()), inFlight))
	go a.drainAndStop(cancel, drainFor)
	writeAPI(w, http.StatusAccepted, map[string]any{"state": "draining", "inFlight": inFlight, "drainSeconds": int(drainFor.Seconds())})
}

func (a *repoAPI) drainAndStop(cancel <-chan struct{}, drainFor time.Duration) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		select {
		case <-cancel:
			stop()
		case <-ctx.Done():
		}
	}()
	left := a.gate.Wait(ctx, drainFor)
	if ctx.Err() != nil {
		return // cancelled: service already resumed
	}
	a.applyMu.Lock()
	select {
	case <-cancel: // cancelled just now
		a.applyMu.Unlock()
		return
	default:
	}
	a.applying.Store(true)
	a.applyMu.Unlock()
	if left > 0 {
		ids, total := a.gate.Running()
		a.log.Warn("restarting with requests still running; they are cut", "count", total, "request_ids", ids)
	}
	a.log.Info("restarting to apply repository changes")
	a.stop()
}

func (a *repoAPI) cancelApply(w http.ResponseWriter, r *http.Request) {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	if a.applying.Load() {
		apiFail(w, apperr.New(apperr.AdminApplyBusy, "the drain has finished and the server is shutting down; it cannot be cancelled any more"))
		return
	}
	if !a.gate.Draining() {
		writeAPI(w, http.StatusOK, map[string]any{"state": "running"})
		return
	}
	if a.cancelCh != nil {
		close(a.cancelCh)
		a.cancelCh = nil
	}
	a.gate.Cancel()
	a.log.Info("maintenance mode cancelled; serving again")
	a.mgr.Audit("apply-cancel", "-", "")
	writeAPI(w, http.StatusOK, map[string]any{"state": "running"})
}

func (a *repoAPI) servingIDs() []string {
	if a.serving == nil {
		return []string{}
	}
	if ids := a.serving(); ids != nil {
		return ids
	}
	return []string{}
}

// detached lists repositories that were detached (read-only; a purge is not
// available here).
func (a *repoAPI) detached(w http.ResponseWriter, r *http.Request) {
	specs, err := a.mgr.Detached()
	if err != nil {
		apiFail(w, err)
		return
	}
	if specs == nil {
		specs = []repoadmin.Spec{}
	}
	writeAPI(w, http.StatusOK, map[string]any{"value": specs})
}
