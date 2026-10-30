package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lamiv/zaks-drive/internal/apperr"
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
}

// guard authenticates the caller and slows down guessing: after
// maxFailures wrong tokens the API answers 429 for a minute.
func (a *repoAPI) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if a.token == "" {
			apiFail(w, apperr.New(apperr.AdminTokenRequired, "ADMIN_TOKEN is not set"))
			return
		}
		a.failMu.Lock()
		if time.Now().Before(a.blockedTo) {
			wait := time.Until(a.blockedTo)
			a.failMu.Unlock()
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			apiFail(w, apperr.New(apperr.AuthAdminUnauthorized, "too many wrong tokens; try again in %d seconds", int(wait.Seconds())+1).WithStatus(http.StatusTooManyRequests))
			return
		}
		a.failMu.Unlock()
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+a.token)) != 1 {
			a.failMu.Lock()
			a.failures++
			if a.failures >= maxFailures {
				a.blockedTo, a.failures = time.Now().Add(failureWindow), 0
				a.log.Warn("admin API locked after repeated wrong tokens", "seconds", int(failureWindow.Seconds()))
			}
			a.failMu.Unlock()
			apiFail(w, apperr.New(apperr.AuthAdminUnauthorized, "the Authorization header is missing or does not carry the ADMIN_TOKEN"))
			return
		}
		a.failMu.Lock()
		a.failures = 0
		a.failMu.Unlock()
		next(w, r)
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
