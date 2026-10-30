// Package contentrepo implements the SAP Content Server HTTP interface
// (ArchiveLink / KPro "HTTP content server", pVersion 0045/0046/0047) on top
// of the shared SharePoint storage service, so SAP DMS, GOS and ArchiveLink
// can use SharePoint as a content repository configured in OAC0.
package contentrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/observability"
	"github.com/Lamiv/zaks-drive/internal/safelog"
	"github.com/Lamiv/zaks-drive/internal/storage"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

const defaultPVersion = "0046"

type Server struct {
	svc     *storage.Service
	cfg     config.ContentServerConfig
	reps    map[string]*contentRep
	order   []string
	certs   *certStore
	docs    *docStore
	allowed []*net.IPNet
	log     *slog.Logger
	version string
	now     func() time.Time
}

func New(ctx context.Context, svc *storage.Service, cfg config.ContentServerConfig, cacheTTL time.Duration, version string, log *slog.Logger) (*Server, error) {
	s := &Server{
		svc:     svc,
		cfg:     cfg,
		reps:    map[string]*contentRep{},
		docs:    &docStore{svc: svc, cache: &docCache{ttl: cacheTTL, m: map[string]docCacheEntry{}}},
		log:     log,
		version: version,
		now:     time.Now,
	}
	for _, rc := range cfg.Repositories {
		shared, err := svc.Repository(rc.Repository)
		if err != nil {
			return nil, fmt.Errorf("contRep %s: %w", rc.ContRep, err)
		}
		// The REST API's handle has this folder reserved (see ReserveFolders);
		// the content server owns it and uses an unrestricted handle.
		repo := shared.Unrestricted()
		s.reps[rc.ContRep] = &contentRep{id: rc.ContRep, cfg: rc, repo: repo, folder: rc.Folder, shards: map[string]bool{}}
		location := strings.TrimRight(repo.WebURL, "/") + "/" + strings.Trim(repo.RootPath+"/"+rc.Folder, "/")
		if repo.Kind() == "local" {
			location = "local folder of repository " + rc.Repository + ", below " + rc.Folder
		}
		log.Info("content repository: SAP documents are stored here", "contRep", rc.ContRep, "location", location)
		s.order = append(s.order, rc.ContRep)
	}
	for _, cidr := range cfg.AllowedNetworks {
		n, err := config.ParseNetwork(cidr)
		if err != nil {
			return nil, apperr.New(apperr.CfgNetworkInvalid, "contentServer.allowedNetworks entry %q: %v", cidr, err)
		}
		s.allowed = append(s.allowed, n)
	}
	s.certs = newCertStore(svc, s.reps, log)
	if err := s.certs.reload(ctx, 0); err != nil {
		log.Warn("could not load content server certificates; will retry", safelog.Err(err))
	}
	return s, nil
}

// ReserveFolders hides every configured content server folder (documents,
// sidecars and certificate records) from the shared repository handles used
// by the REST API, so REST callers can neither read nor tamper with them.
// It is applied even when the contentrepo interface is disabled.
func ReserveFolders(svc *storage.Service, cfg config.ContentServerConfig) error {
	for _, rc := range cfg.Repositories {
		if err := svc.ReservePath(rc.Repository, rc.Folder); err != nil {
			return fmt.Errorf("contRep %s: %w", rc.ContRep, err)
		}
	}
	return nil
}

// RefreshCertificates periodically reloads certificates so activations done
// through another adapter instance take effect.
func (s *Server) RefreshCertificates(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.certs.reload(ctx, every/2); err != nil {
				s.log.Warn("certificate reload failed", safelog.Err(err))
			}
		}
	}
}

// ---- request parsing ----

type params struct {
	cmd    string
	values map[string]string // lower-cased names
}

func (p params) get(name string) string { return p.values[strings.ToLower(name)] }

func parseParams(rawQuery string) params {
	p := params{values: map[string]string{}}
	for i, tok := range strings.Split(rawQuery, "&") {
		if tok == "" {
			continue
		}
		k, v, hasEq := strings.Cut(tok, "=")
		k, _ = url.QueryUnescape(k)
		if i == 0 && !hasEq {
			p.cmd = k
			continue
		}
		if dv, err := url.QueryUnescape(v); err == nil {
			v = dv
		}
		p.values[strings.ToLower(k)] = v
	}
	return p
}

// ---- errors ----

// csStatus maps an error's HTTP status to what the SAP specification
// defines: 200, 201, 400, 401, 403, 404, 406, 409, 500 (plus 405, 413, 501
// and 503, which SAP treats as errors).
func csStatus(e *apperr.Error) int {
	st := e.Status()
	if st >= 500 && st != http.StatusNotImplemented && st != http.StatusServiceUnavailable {
		return http.StatusInternalServerError
	}
	return st
}

// fail answers with the coded error behind err. SAP shows the
// X-ErrorDescription header, so it carries the code, what happened and the
// fix on one ASCII line.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var mbe *http.MaxBytesError
	if errors.Is(err, context.Canceled) {
		return
	}
	e, ok := apperr.As(err)
	if !ok {
		switch {
		case errors.As(err, &mbe):
			e = apperr.New(apperr.UploadTooLarge, "the request body is larger than %d bytes", mbe.Limit).Wrap(transfer.ErrTooLarge)
		case graph.Classify(err) != nil:
			e = graph.Classify(err)
		case errors.Is(err, storage.ErrInvalid):
			e = apperr.New(apperr.CSIDInvalid, "%v", err).Wrap(err)
		default:
			e = apperr.New(apperr.InternalError, "%s", safelog.Summary(err))
		}
	}
	status := csStatus(e)
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(max(int(e.RetryAfter.Seconds()+0.5), 1)))
	}
	// Every refused request is logged with its code and reason, so a test run
	// (for example report RSCMSTH2) can be diagnosed from `docker compose logs`.
	q := parseParams(r.URL.RawQuery)
	lvl := slog.LevelWarn
	if status >= 500 && status != http.StatusNotImplemented {
		lvl = slog.LevelError
	}
	s.log.LogAttrs(r.Context(), lvl, "content server request refused",
		slog.String("request_id", observability.RequestID(r.Context())),
		slog.String("command", q.cmd), slog.Int("status", status), slog.String("code", e.Code), slog.String("title", e.Title()),
		slog.String("contRep", q.get("contRep")), slog.String("docId", q.get("docId")), slog.String("compId", q.get("compId")),
		slog.String("method", r.Method), safelog.Err(err))
	msg := e.Message()
	if len(msg) > 700 {
		msg = msg[:700] + "..."
	}
	h := w.Header()
	h.Set("X-ErrorDescription", apperr.ASCII(msg))
	h.Set("X-ErrorCode", e.Code)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Del("Content-Length")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, msg+"\r\n")
	}
}

// ---- dispatch ----

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serve)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	p := parseParams(r.URL.RawQuery)
	r.Pattern = "cs:" + p.cmd // metrics/log route label
	if !s.allowedClient(r) {
		s.fail(w, r, apperr.New(apperr.CSClientNotAllowed, "the address %s is not in contentServer.allowedNetworks (%s)", clientIP(r), strings.Join(s.cfg.AllowedNetworks, ", ")))
		return
	}
	if s.log.Enabled(r.Context(), slog.LevelDebug) {
		// LOG_LEVEL=debug shows what SAP actually sends (secKey redacted).
		s.log.Debug("content server request", "method", r.Method, "command", p.cmd, "query", redactSecKey(r.URL.RawQuery),
			"content_type", r.Header.Get("Content-Type"), "content_length", r.ContentLength, "user_agent", r.UserAgent(),
			"request_id", observability.RequestID(r.Context()))
	}
	pv := p.get("pVersion")
	if !validPVersion(pv) {
		pv = defaultPVersion // SAP sends four digits (0045 to 0047); anything else is not echoed back
	}
	w.Header().Set("X-pVersion", pv)

	if p.cmd == "serverInfo" {
		s.serverInfo(w, r, p, pv)
		return
	}
	if p.cmd == "" {
		s.fail(w, r, apperr.New(apperr.CSCommandMissing, "the query string %q has no command", r.URL.RawQuery))
		return
	}
	cr, ok := s.reps[p.get("contRep")]
	if !ok {
		s.fail(w, r, apperr.New(apperr.CSContRepUnknown, "content repository %q is not configured; configured: %s", p.get("contRep"), strings.Join(s.order, ", ")))
		return
	}
	var err error
	switch p.cmd {
	case "info":
		err = s.info(w, r, p, cr, pv)
	case "get":
		err = s.get(w, r, p, cr, pv)
	case "docGet":
		err = s.docGet(w, r, p, cr, pv)
	case "create":
		err = s.create(w, r, p, cr, pv)
	case "update":
		err = s.update(w, r, p, cr, pv)
	case "append":
		err = s.appendComp(w, r, p, cr, pv)
	case "delete":
		err = s.deleteDoc(w, r, p, cr, pv)
	case "search":
		err = s.search(w, r, p, cr, pv)
	case "putCert":
		err = s.putCert(w, r, p, cr)
	case "mCreate":
		err = s.mCreate(w, r, p, cr)
	case "adminContRep":
		// SAP's CSADMIN administration command (adminContRep&configGet /
		// configSet / certGet ...). It reads and writes the content server's
		// own configuration, which this adapter keeps in config.yaml, and it is
		// not used by DMS, GOS, attachments or ArchiveLink at runtime. Third-party
		// content servers answer 501; SAP then only reports that CSADMIN cannot
		// administer this server.
		err = apperr.New(apperr.CSAdminUnsupported, "adminContRep %s is not supported (SAP CSADMIN); this content server is configured in config.yaml", adminOperation(p))
		s.log.Info("adminContRep requested (not supported)", "operation", adminOperation(p), "contRep", cr.id)
	case "attrSearch", "getCert":
		// attrSearch (print-list attribute index) and getCert (4.6 cache
		// server) are not implemented; see docs/content-server.md.
		err = apperr.New(apperr.CSCommandUnsupported, "the command %s is not supported by this content server", p.cmd)
	default:
		err = apperr.New(apperr.CSCommandUnknown, "%q is not a command of the SAP Content Server HTTP interface", p.cmd)
	}
	if err != nil {
		s.fail(w, r, err)
	}
}

func (s *Server) allowedClient(r *http.Request) bool {
	if len(s.allowed) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	for _, n := range s.allowed {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// adminOperation names the adminContRep sub-command, which SAP may send as
// "adminContRep&configGet" (second query token) or as operation=configGet.
func adminOperation(p params) string {
	if op := p.get("operation"); op != "" {
		return op
	}
	for _, k := range []string{"configget", "configset", "certget", "statget", "contrepstat", "export", "docidlist", "serverinfo"} {
		if _, ok := p.values[k]; ok {
			return k
		}
	}
	return "(unknown operation)"
}

var secKeyRe = regexp.MustCompile(`(?i)(seckey=)[^&]*`)

func redactSecKey(q string) string { return secKeyRe.ReplaceAllString(q, "${1}<redacted>") }

func requireMethod(r *http.Request, methods ...string) error {
	for _, m := range methods {
		if r.Method == m {
			return nil
		}
	}
	return apperr.New(apperr.CSMethodNotAllowed, "the command %s does not accept %s; use %s", parseParams(r.URL.RawQuery).cmd, r.Method, strings.Join(methods, " or "))
}

func requireParam(p params, names ...string) error {
	for _, n := range names {
		if p.get(n) == "" {
			return apperr.New(apperr.CSParamMissing, "the URL parameter %s is required", n)
		}
	}
	return nil
}

// clientIP is the address the connection came from.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// validPVersion accepts the protocol version SAP sends: exactly four ASCII digits.
func validPVersion(v string) bool {
	if len(v) != 4 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	return true
}
