// Command adapter runs the SAP <-> SharePoint document adapter.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Lamiv/zaks-drive/internal/accesscheck"
	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/auth"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/contentrepo"
	"github.com/Lamiv/zaks-drive/internal/drain"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/observability"
	"github.com/Lamiv/zaks-drive/internal/repoadmin"
	"github.com/Lamiv/zaks-drive/internal/restapi"
	"github.com/Lamiv/zaks-drive/internal/safelog"
	"github.com/Lamiv/zaks-drive/internal/storage"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

// version is set at build time (-X main.version=...).
var version = "dev"

const (
	productName    = "ZAKS Drive"
	productDesc    = "Open-Source Enterprise Content Gateway"
	productSummary = "Self-hosted content gateway connecting SAP S/4HANA and enterprise applications to SharePoint Online and OneDrive for Business."
)

func exeName() string {
	n := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if n == "" || n == "adapter" {
		return "zaks-drive" // "adapter" is the pre-rename name, kept as an alias in the image
	}
	return n
}

func printVersion() {
	fmt.Printf("%s %s (%s, %s/%s)\n", productName, version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func usage(w io.Writer) {
	x := exeName()
	fmt.Fprintf(w, `%s %s - %s
%s

Usage:
  %[5]s [-config file]                      run the server (default; the command "serve" does the same)
  %[5]s check                               verify every access the server needs: PASS / FAIL / SKIP, with the fix
  %[5]s certs list|activate|deactivate ...  manage SAP certificates on a running server
  %[5]s errors                              print the error-code catalogue as Markdown
  %[5]s version                             print the version
  %[5]s -healthcheck                        probe the local /healthz endpoint (Docker HEALTHCHECK)

This is the SERVER binary (zaks-drive-server; also reachable as zaks-drive and adapter inside the image, for compatibility).
To install, start, stop, update and troubleshoot a deployment from the host, use the management command: zaks-drive (docs/cli.md).

Configuration: -config <file> or the CONFIG_FILE variable (default in Docker: /etc/adapter/config.yaml).
Documentation: README.md, docs/index.md and docs/error-codes.md in the repository.
`, productName, version, productDesc, productSummary, x)
}

func main() {
	cfgPath := flag.String("config", os.Getenv("CONFIG_FILE"), "path to YAML config")
	healthcheck := flag.Bool("healthcheck", false, "probe the local admin /healthz endpoint and exit (for Docker HEALTHCHECK)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() { usage(os.Stderr) }
	flag.Parse()
	if *showVersion {
		printVersion()
		return
	}
	if *healthcheck {
		os.Exit(probe())
	}
	switch cmd := flag.Arg(0); cmd {
	case "version":
		printVersion()
		return
	case "help":
		usage(os.Stdout)
		return
	case "repos":
		os.Exit(reposCLI(*cfgPath, flag.Args()[1:]))
	case "certs":
		os.Exit(certsCLI(flag.Args()[1:]))
	case "errors":
		// Prints the error-code catalogue as Markdown (docs/error-codes.md).
		fmt.Print(apperr.Markdown())
		return
	case "", "serve", "check":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, productName+" cannot start because of its configuration:\n"+apperr.Format(err))
		os.Exit(2)
	}
	log := observability.NewLogger(cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(log)
	if flag.Arg(0) == "check" {
		os.Exit(runCheck(cfg))
	}
	log.Info(productName+" starting", "version", version, "interfaces", cfg.Server.Interfaces, "go", runtime.Version())
	if err := run(cfg, *cfgPath, log); err != nil {
		if ae, ok := apperr.As(err); ok {
			log.Error(productName+" stopped", "code", ae.Code, "title", ae.Title(), "detail", safelog.OneLine(safelog.Redact(ae.Detail)), "fix", ae.Fix())
		} else {
			log.Error(productName+" stopped", safelog.Err(err))
		}
		fmt.Fprintln(os.Stderr, productName+" stopped:\n"+apperr.Format(err))
		os.Exit(1)
	}
}

func run(cfg config.Config, cfgPath string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	transport := graph.NewTransport(cfg.Graph.MaxIdleConns)
	tokenHTTP := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	tokens := auth.NewClientCredentials(cfg.Graph.AuthorityURL, cfg.Graph.TenantID, cfg.Graph.ClientID, cfg.Graph.ClientSecret, cfg.Graph.Scope, tokenHTTP)
	gc := graph.New(graph.Options{
		BaseURL:         cfg.Graph.BaseURL,
		MaxConcurrency:  cfg.Graph.MaxConcurrency,
		MaxRetries:      cfg.Graph.MaxRetries,
		MaxRetryBackoff: cfg.Graph.MaxRetryBackoff,
	}, tokens, transport, log)
	engine := transfer.New(gc, transfer.Options{
		SimpleUploadMax: cfg.Transfer.SimpleUploadMax,
		ChunkSize:       cfg.Transfer.ChunkSize,
		MemoryBudget:    cfg.Transfer.MemoryBudget,
		MaxUpload:       cfg.Transfer.MaxUpload,
		SpoolDir:        cfg.Transfer.SpoolDir,
	})

	// Health endpoints come up first so a failing start is diagnosable:
	// /readyz reports why the adapter is not ready instead of resetting
	// the connection.
	var ready atomic.Bool
	var status atomic.Value // string: current readiness reason
	status.Store("starting: resolving repositories")
	adminMux := adminHandler(cfg.Server.EnablePprof, &ready, &status)
	gate := drain.New(cfg.Server.DrainTimeout)
	var restarting atomic.Bool // set when the server ends to apply repository changes
	mgr := &repoadmin.Manager{StateDir: cfg.Server.StateDir, Actor: "api", Load: func() (config.Config, error) { return config.Load(cfgPath) }}
	applied, _ := mgr.Fingerprint()
	if cfg.Fallback != "" {
		log.Error("the repository definitions in the state directory are not valid; serving the last working state", "problem", safelog.OneLine(safelog.Redact(cfg.Fallback)))
	} else if cfg.Server.StateDir != "" {
		if err := mgr.Snapshot(); err != nil {
			log.Warn("cannot save the last working repository state", safelog.Err(err))
		}
	}
	admin := newServer(cfg.Server, cfg.Server.AdminAddr, adminMux)
	admin.TLSConfig = nil
	errCh := make(chan error, 8)
	serve := func(s *http.Server, useTLS bool) {
		log.Info("listening", "addr", s.Addr, "tls", useTLS, "version", version)
		var err error
		if useTLS {
			err = s.ListenAndServeTLS(cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
		} else {
			err = s.ListenAndServe()
		}
		if !errors.Is(err, http.ErrServerClosed) {
			if errors.Is(err, syscall.EADDRINUSE) {
				err = apperr.New(apperr.StartupPortInUse, "cannot listen on %s: the address is already in use inside the container (do restAddr, contentRepoAddr and adminAddr differ?)", s.Addr).Wrap(err)
			}
			errCh <- fmt.Errorf("%s: %w", s.Addr, err)
		}
	}
	go serve(admin, false)

	// Resolving site/library IDs needs Graph; retry so the container
	// survives starting before the network or IdP is reachable.
	apps := newAppClients(cfg, transport, log)
	var serving atomic.Pointer[storage.Service] // set once the repositories are resolved
	// Repository administration works from the first moment, also while the
	// repositories cannot be resolved yet, so a broken change can be repaired.
	(&repoAPI{
		mgr: mgr, token: cfg.Server.AdminToken, applied: applied, fallback: cfg.Fallback, gate: gate, ready: &ready,
		version: version, drain: cfg.Server.DrainTimeout, log: log, started: time.Now().UTC(),
		serving: func() []string {
			var ids []string
			if s := serving.Load(); s != nil {
				for _, r := range s.Repositories() {
					ids = append(ids, r.ID)
				}
			}
			return ids
		},
		stop: func() { restarting.Store(true); stop() },
		check: func(ctx context.Context, id string) (string, bool, error) {
			current, _ := config.Load(cfgPath) // a half-valid state still lets one repository be tested
			return checkRepository(ctx, current, id, gc, tokens, apps)
		},
	}).register(adminMux)

	var svc *storage.Service
	for attempt := 1; ; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s, err := storage.NewService(rctx, gc, engine, cfg.Repositories, cfg.Graph.MetadataCacheTTL, log, storage.WithClients(apps.storageClients(engine)))
		cancel()
		if err == nil {
			svc = s
			serving.Store(s)
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := min(time.Duration(attempt)*2*time.Second, 30*time.Second)
		hint := initHint(ctx, err, tokens)
		reason := safelog.Redact(safelog.OneLine(safelog.Summary(err)))
		code := apperr.StartupRepositoryFailed
		if ae := graph.Classify(err); ae != nil {
			reason, code = ae.Message(), ae.Code
		}
		status.Store("not ready: the configured SharePoint location cannot be used yet. " + reason + hint)
		log.Error("repository initialisation failed; retrying", "code", code, safelog.Err(err), "hint", strings.TrimSpace(hint), "retry_in", wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case <-time.After(wait):
		}
	}

	// Record the permissions the app actually has, to make 401/403 on later
	// calls easy to explain.
	if tok, err := tokens.Token(ctx); err == nil {
		log.Info("graph application permissions", "roles", auth.TokenRoles(tok))
	}
	defer svc.Close()
	if err := contentrepo.ReserveFolders(svc, cfg.ContentServer); err != nil {
		return err
	}
	// Paging cursors must verify on every instance sharing these credentials.
	// The "sharepointadapter/cursor" label predates the rename. It is HMAC key material, not a module
	// path; changing it would invalidate paging cursors that clients still hold.
	cursorKey := sha256.Sum256([]byte("sharepointadapter/cursor|" + cfg.Graph.TenantID + "|" + cfg.Graph.ClientID + "|" + cfg.Graph.ClientSecret))
	if cfg.Graph.ClientSecret != "" { // without global credentials (local repositories only) the random per-process key stays
		svc.SetCursorKey(cursorKey[:])
	}

	authn, err := auth.NewAuthenticator(cfg.Auth, &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		return err
	}
	if cfg.Auth.Disabled {
		log.Warn("inbound authentication is DISABLED; do not use in production")
	}

	go readinessLoop(ctx, svc, &ready, &status, log)

	var servers []*http.Server
	if cfg.Server.Enabled("rest") {
		api := restapi.New(svc, authn, restapi.Options{
			DownloadMode: cfg.Transfer.DownloadMode,
			MaxUpload:    cfg.Transfer.MaxUpload,
			CORSOrigins:  cfg.Server.CORSOrigins,
		}, log)
		servers = append(servers, newServer(cfg.Server, cfg.Server.RESTAddr,
			observability.Middleware("rest", cfg.Server.MaxInFlight, log, gate.Wrap(api.Handler()))))
	}
	if cfg.Server.Enabled("contentrepo") {
		cs, err := contentrepo.New(ctx, svc, cfg.ContentServer, cfg.Graph.MetadataCacheTTL, version, log)
		if err != nil {
			return err
		}
		go cs.RefreshCertificates(ctx, time.Minute)
		cs.RegisterAdmin(adminMux, cfg.Server.AdminToken)
		servers = append(servers, newServer(cfg.Server, cfg.Server.ContentRepoAddr,
			observability.Middleware("contentrepo", cfg.Server.MaxInFlight, log, gate.Wrap(cs.Handler()))))
	}
	if len(servers) == 0 {
		return errors.New("no interfaces enabled (server.interfaces)")
	}
	useTLS := cfg.Server.TLSCertFile != "" && cfg.Server.TLSKeyFile != ""
	for _, s := range servers {
		go serve(s, useTLS)
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		return err
	}
	ready.Store(false)
	shutdownFor := cfg.Server.ShutdownTimeout
	if restarting.Load() {
		shutdownFor = 2 * time.Second // the drain already waited; whatever still runs is cut
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownFor)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range append(servers, admin) {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			_ = s.Shutdown(sctx)
		}(s)
	}
	wg.Wait()
	return nil
}

func newServer(sc config.ServerConfig, addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: sc.ReadHeaderTO,
		IdleTimeout:       sc.IdleTimeout,
		MaxHeaderBytes:    64 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		// No Read/WriteTimeout: large transfers may legitimately take minutes.
	}
}

func readinessLoop(ctx context.Context, svc *storage.Service, ready *atomic.Bool, status *atomic.Value, log *slog.Logger) {
	check := func() {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		err := svc.Ping(cctx)
		if len(svc.Repositories()) == 0 {
			err = errNoRepositories
		}
		if err != nil {
			if errors.Is(err, errNoRepositories) {
				status.Store("not ready: no repository is configured yet; add one with `zaks-drive repo add`")
			} else {
				status.Store("not ready: repository check failed: " + safelog.Redact(safelog.OneLine(readinessReason(err))))
			}
		} else {
			status.Store("ready")
		}
		if was := ready.Swap(err == nil); was != (err == nil) {
			if err != nil {
				log.Error("readiness check failed", safelog.Err(err))
			} else {
				log.Info("ready")
			}
		}
	}
	check()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

func adminHandler(enablePprof bool, ready *atomic.Bool, status *atomic.Value) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			reason, _ := status.Load().(string)
			http.Error(w, reason, http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(version + "\n"))
	})
	if enablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

// probe is used as the container health check because the distroless image
// has no shell or curl.
func probe() int {
	u := os.Getenv("HEALTHCHECK_URL")
	if u == "" {
		u = "http://127.0.0.1:9090/healthz"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(u)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// certsCLI manages content server certificates through the local admin
// listener, e.g. inside the container:
//
//	/zaks-drive certs list
//	/zaks-drive certs activate Z1 CN=S4H
func certsCLI(args []string) int {
	base := os.Getenv("ADMIN_URL")
	if base == "" {
		base = "http://127.0.0.1:9090"
	}
	usage := func() int {
		fmt.Fprintln(os.Stderr, "usage: adapter certs list | activate <contRep> <authId> | deactivate <contRep> <authId>")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	var req *http.Request
	switch args[0] {
	case "list":
		req, _ = http.NewRequest(http.MethodGet, base+"/admin/contentserver/certificates", nil)
	case "activate", "deactivate":
		if len(args) != 3 {
			return usage()
		}
		body, _ := json.Marshal(map[string]string{"contRep": args[1], "authId": args[2]})
		req, _ = http.NewRequest(http.MethodPost, base+"/admin/contentserver/certificates/"+args[0], bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	default:
		return usage()
	}
	if t := os.Getenv("ADMIN_TOKEN"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, apperr.Format(apperr.New(apperr.CLIAdminUnreachable, "no answer from %s: %v", base, err)))
		return 1
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		fmt.Fprintln(os.Stderr, apperr.Format(apperr.New(apperr.CLIAdminRouteMissing, "%s answered 404 for the certificate endpoints", base)))
		return 1
	case http.StatusUnauthorized:
		if os.Getenv("ADMIN_TOKEN") == "" {
			fmt.Fprintln(os.Stderr, apperr.Format(apperr.New(apperr.AuthAdminUnauthorized, "ADMIN_TOKEN is not set in this environment, so the request carried no token")))
			return 1
		}
	}
	_, _ = io.Copy(os.Stdout, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// initHint explains a failed repository lookup. A 401 from Graph although
// Entra issued a token almost always means the token carries no (or the
// wrong) application permissions, so the token's roles are shown.
func initHint(ctx context.Context, err error, tokens *auth.ClientCredentials) string {
	ae := graph.Classify(err)
	if ae == nil || (ae.Code != apperr.SPUnauthorized && ae.Code != apperr.SPAccessDenied) {
		return ""
	}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tok, terr := tokens.Token(tctx)
	if terr != nil {
		return ""
	}
	roles := auth.TokenRoles(tok)
	if len(roles) == 0 {
		return " | the access token has NO application permissions (roles): add Microsoft Graph APPLICATION permissions to the app registration and click 'Grant admin consent'"
	}
	return " | the token carries these roles: " + strings.Join(roles, ", ") + ". siteUrl needs Sites.Selected plus a site grant, or Sites.ReadWrite.All; userId (OneDrive) needs Files.ReadWrite.All; check that admin consent was granted"
}

// runCheck implements `zaks-drive check`: it verifies every access the adapter
// needs against the real Graph and prints a PASS/FAIL/SKIP checklist.
func runCheck(cfg config.Config) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := observability.NewLogger("warn", "text")
	transport := graph.NewTransport(cfg.Graph.MaxIdleConns)
	tokens := auth.NewClientCredentials(cfg.Graph.AuthorityURL, cfg.Graph.TenantID, cfg.Graph.ClientID, cfg.Graph.ClientSecret, cfg.Graph.Scope,
		&http.Client{Transport: transport, Timeout: 30 * time.Second})
	gc := graph.New(graph.Options{
		BaseURL: cfg.Graph.BaseURL, MaxConcurrency: cfg.Graph.MaxConcurrency,
		MaxRetries: cfg.Graph.MaxRetries, MaxRetryBackoff: cfg.Graph.MaxRetryBackoff,
	}, tokens, transport, log)
	return accesscheck.Run(ctx, cfg, gc, tokens, os.Stdout, newAppClients(cfg, transport, log).checkResolver)
}

// readinessReason explains a failed readiness check to the operator who calls /readyz:
// the application error's message when there is one, otherwise a structural summary.
func readinessReason(err error) string {
	if ae := graph.Classify(err); ae != nil {
		return ae.Message()
	}
	return safelog.Summary(err)
}

// appClients creates, once per Microsoft application (tenant, client and
// secret), a token source and a Graph client with their own concurrency
// limit and throttle handling, so a throttled tenant does not slow another.
// Repositories that use the same application share them. The transfer memory
// budget stays shared (see transfer.Engine.WithClient).
type appClients struct {
	cfg       config.Config
	transport http.RoundTripper
	log       *slog.Logger
	mu        sync.Mutex
	apps      map[string]*appClient
}

type appClient struct {
	gc     *graph.Client
	tokens auth.TokenSource
}

func newAppClients(cfg config.Config, transport http.RoundTripper, log *slog.Logger) *appClients {
	return &appClients{cfg: cfg, transport: transport, log: log, apps: map[string]*appClient{}}
}

func (a *appClients) get(rc config.RepositoryConfig) *appClient {
	sum := sha256.Sum256([]byte(rc.ClientSecret))
	key := rc.TenantID + "|" + rc.ClientID + "|" + string(sum[:8])
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.apps[key]; ok {
		return c
	}
	tokens := auth.NewClientCredentials(a.cfg.Graph.AuthorityURL, rc.TenantID, rc.ClientID, rc.ClientSecret, a.cfg.Graph.Scope,
		&http.Client{Transport: a.transport, Timeout: 30 * time.Second})
	c := &appClient{tokens: tokens, gc: graph.New(graph.Options{
		BaseURL:         a.cfg.Graph.BaseURL,
		MaxConcurrency:  a.cfg.Graph.MaxConcurrency,
		MaxRetries:      a.cfg.Graph.MaxRetries,
		MaxRetryBackoff: a.cfg.Graph.MaxRetryBackoff,
	}, tokens, a.transport, a.log)}
	a.apps[key] = c
	return c
}

// storageClients is the storage.ClientFunc for repositories with their own credentials.
func (a *appClients) storageClients(engine *transfer.Engine) storage.ClientFunc {
	return func(rc config.RepositoryConfig) (*graph.Client, *transfer.Engine, error) {
		c := a.get(rc)
		return c.gc, engine.WithClient(c.gc), nil
	}
}

// checkResolver is the accesscheck.Resolver for `check`.
func (a *appClients) checkResolver(rc config.RepositoryConfig) (*graph.Client, auth.TokenSource) {
	c := a.get(rc)
	return c.gc, c.tokens
}

// errNoRepositories is the readiness failure of a server whose state
// directory has no repository yet.
var errNoRepositories = errors.New("no repository configured")
