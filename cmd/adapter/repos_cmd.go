package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/auth"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/observability"
	"github.com/Lamiv/zaks-drive/internal/repoadmin"
)

// reposCLI implements `zaks-drive-server repos <operation>`, the same
// operations as the admin API, for when the server is not running: the
// management command starts this in a one-off container of the same image and
// volume, so the files are created by the server's own user. Input for add
// and update is JSON on standard input (the secret never travels as an
// argument); output is JSON on standard output.
//
//	repos list
//	repos get <id>
//	repos add                (stdin: {"id":"ZH",...,"clientSecret":"..."})
//	repos update <id>        (stdin: same shape)
//	repos detach <id> [--allow-in-use]
//	repos check <id>
//	repos audit [lines]
func reposCLI(cfgPath string, args []string) int {
	out := func(v any, err error) int {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err != nil {
			_, body := toAPIError(err)
			_ = enc.Encode(map[string]any{"ok": false, "error": body})
			return 1
		}
		_ = enc.Encode(map[string]any{"ok": true, "result": v})
		return 0
	}
	if len(args) == 0 {
		return out(nil, apperr.New(apperr.AdminRepoInvalid, "usage: repos list|get|add|update|detach|check|audit"))
	}

	// The one-off container has no listener and no token: shell access to
	// the Docker host is the credential, as for `docker compose exec`.
	cfg, loadErr := config.Load(cfgPath)
	mgr := &repoadmin.Manager{StateDir: cfg.Server.StateDir, Actor: "one-off", Load: func() (config.Config, error) { return config.Load(cfgPath) }}
	id := ""
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		id = args[1]
	}

	switch args[0] {
	case "list":
		views, err := mgr.List()
		if err != nil && len(views) == 0 {
			return out(nil, err)
		}
		return out(map[string]any{"value": views, "problem": problemOf(err)}, nil)
	case "get":
		return out(mgr.Get(id))
	case "add", "update":
		var b repoBody
		dec := json.NewDecoder(io.LimitReader(os.Stdin, maxAdminBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil {
			return out(nil, apperr.New(apperr.AdminRepoInvalid, "standard input is not the expected JSON: %s", firstLine(err.Error())))
		}
		if args[0] == "add" {
			secret := ""
			if b.ClientSecret != nil {
				secret = *b.ClientSecret
			}
			return out(mgr.Add(b.Spec, secret))
		}
		if b.ID == "" {
			b.ID = id
		}
		if id != "" && b.ID != id {
			return out(nil, apperr.New(apperr.AdminRepoInvalid, "the id in the JSON (%q) differs from the argument (%q)", b.ID, id))
		}
		return out(mgr.Update(b.Spec, b.ClientSecret))
	case "detach":
		allow := false
		for _, a := range args[1:] {
			allow = allow || a == "--allow-in-use"
		}
		if err := mgr.Detach(id, allow); err != nil {
			return out(nil, err)
		}
		return out(map[string]any{"detached": id, "documentsDeleted": false}, nil)
	case "audit":
		n := 100
		if len(args) > 1 {
			fmt.Sscan(args[1], &n)
		}
		b, err := mgr.AuditLog(n)
		if err != nil {
			return out(nil, err)
		}
		return out(string(b), nil)
	case "check":
		if loadErr != nil && len(cfg.Repositories) == 0 {
			return out(nil, loadErr)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		transport := graph.NewTransport(cfg.Graph.MaxIdleConns)
		tokens := auth.NewClientCredentials(cfg.Graph.AuthorityURL, cfg.Graph.TenantID, cfg.Graph.ClientID, cfg.Graph.ClientSecret, cfg.Graph.Scope,
			&http.Client{Transport: transport, Timeout: 30 * time.Second})
		log := observability.NewLogger("warn", "text")
		gc := graph.New(graph.Options{BaseURL: cfg.Graph.BaseURL, MaxConcurrency: cfg.Graph.MaxConcurrency, MaxRetries: cfg.Graph.MaxRetries, MaxRetryBackoff: cfg.Graph.MaxRetryBackoff}, tokens, transport, log)
		report, ok, err := checkRepository(ctx, cfg, id, gc, tokens, newAppClients(cfg, transport, log))
		if err != nil {
			return out(nil, err)
		}
		return out(map[string]any{"ok": ok, "report": report}, nil)
	}
	return out(nil, apperr.New(apperr.AdminRepoInvalid, "unknown operation %q", args[0]))
}

func problemOf(err error) any {
	if err == nil {
		return nil
	}
	_, body := toAPIError(err)
	return body
}
