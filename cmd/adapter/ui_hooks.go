package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Lamiv/zaks-drive/internal/adminui"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/contentrepo"
	"github.com/Lamiv/zaks-drive/internal/repoadmin"
	"github.com/Lamiv/zaks-drive/internal/safelog"
	"github.com/Lamiv/zaks-drive/internal/storage"
)

// uiState is what the UI's diagnostics and settings views read from the
// running server.
type uiState struct {
	cfg     config.Config
	ready   *atomic.Bool
	status  *atomic.Value
	svc     *atomic.Pointer[storage.Service]
	cs      *atomic.Pointer[contentrepo.Server]
	mgr     *repoadmin.Manager
	applied string
	version string
}

// diagnose runs the checks the server can make about itself. Host checks
// (Docker, ports, disk) belong to `zaks-drive doctor`.
func (u *uiState) diagnose(ctx context.Context) []finding {
	out := []finding{}
	add := func(id, title, status, detail, fix string) {
		out = append(out, finding{ID: id, Title: title, Status: status, Detail: safelog.Redact(safelog.OneLine(detail)), Fix: fix})
	}

	switch {
	case u.cfg.Fallback != "":
		add("fallback", "Repository definitions", "FAIL", "the server runs the last working repository state because the current one is invalid: "+u.cfg.Fallback,
			"Correct the repository named (zaks-drive repo list, or Repositories here), then apply and restart.")
	default:
		add("fallback", "Repository definitions", "PASS", "the current definitions were loaded", "")
	}
	if now, err := u.mgr.Fingerprint(); err == nil && now != u.applied {
		add("pending", "Pending changes", "WARN", "repository changes are waiting for a restart", "Use \"Apply and restart\".")
	} else {
		add("pending", "Pending changes", "PASS", "nothing is waiting", "")
	}

	svc := u.svc.Load()
	if svc == nil {
		reason, _ := u.status.Load().(string)
		add("repositories", "Repository access", "WARN", "the repositories are not resolved yet: "+reason, "See the log; for SharePoint run Test access.")
	} else if states := svc.States(ctx); len(states) == 0 {
		add("repositories", "Repository access", "WARN", "no repository is configured yet", "Add one under Repositories.")
	} else {
		for _, s := range states {
			title := "Repository " + s.ID + " (" + s.Kind + ")"
			if s.Err != nil {
				add("repository-"+s.ID, title, "FAIL", readinessReason(s.Err), "Run Test access for this repository.")
			} else {
				add("repository-"+s.ID, title, "PASS", "reachable", "")
			}
		}
	}

	cs := u.cs.Load()
	if cs == nil {
		add("sap-certificates", "SAP signing certificates", "SKIPPED", "the contentrepo interface is not enabled", "")
		return out
	}
	certs, err := cs.CertificateList(ctx)
	if err != nil {
		add("sap-certificates", "SAP signing certificates", "FAIL", err.Error(), "")
		return out
	}
	var problems []string
	for _, rep := range u.cfg.ContentServer.Repositories {
		if rep.Signature == "none" {
			problems = append(problems, rep.ContRep+": signature checking is off")
			continue
		}
		active := false
		for _, c := range certs {
			if c.ContRep != rep.ContRep || !c.Active {
				continue
			}
			active = true
			if time.Until(c.NotAfter) < 30*24*time.Hour {
				problems = append(problems, fmt.Sprintf("%s: the active certificate expires %s", rep.ContRep, c.NotAfter.Format("2006-01-02")))
			}
		}
		if !active {
			problems = append(problems, rep.ContRep+": no active certificate yet")
		}
	}
	if len(problems) > 0 {
		add("sap-certificates", "SAP signing certificates", "WARN", strings.Join(problems, "; "), "In SAP: OAC0 > Send certificate; then activate it under SAP certificates.")
	} else {
		add("sap-certificates", "SAP signing certificates", "PASS", "every repository that checks signatures has an active, valid certificate", "")
	}
	return out
}

// settings is the effective configuration without any secret.
func (u *uiState) settings() map[string]any {
	c := u.cfg
	return map[string]any{
		"version":                     u.version,
		"interfaces":                  c.Server.Interfaces,
		"restAddr":                    c.Server.RESTAddr,
		"contentRepoAddr":             c.Server.ContentRepoAddr,
		"adminAddr":                   c.Server.AdminAddr,
		"tlsByServer":                 c.Server.TLSCertFile != "",
		"maxInFlightPerListener":      c.Server.MaxInFlight,
		"drainSeconds":                int(c.Server.DrainTimeout.Seconds()),
		"stateDirectory":              c.Server.StateDir != "",
		"authDisabled":                c.Auth.Disabled,
		"apiKeys":                     len(c.Auth.APIKeys),
		"globalMicrosoftAppSet":       c.Graph.TenantID != "" && c.Graph.ClientID != "" && c.Graph.ClientSecret != "",
		"maxUploadBytes":              c.Transfer.MaxUpload,
		"memoryBudgetBytes":           c.Transfer.MemoryBudget,
		"downloadMode":                c.Transfer.DownloadMode,
		"sapAllowedNetworks":          len(c.ContentServer.AllowedNetworks),
		"sapClockSkew":                c.ContentServer.ClockSkew.String(),
		"sapAutoActivateCertificates": c.ContentServer.AutoActivateCertificates,
		"logLevel":                    c.Log.Level,
	}
}

// openAdminAccount opens the admin account and applies a reset request. A
// NO_ADMIN file in the state directory (created by `zaks-drive admin
// reset-password` or by hand) returns the account to the default admin/admin
// state, which the page forces to be changed at the first login; the server
// deletes the file once a new password is set.
func openAdminAccount(cfg config.Config, mgr *repoadmin.Manager, log *slog.Logger) *adminui.Account {
	if !cfg.Server.AdminUI {
		return nil
	}
	acc, err := adminui.OpenAccount(cfg.Server.StateDir)
	if err != nil {
		log.Error("cannot read the admin account", safelog.Err(err))
	}
	switch reset, rerr := acc.ApplyReset(); {
	case rerr != nil:
		log.Error("a reset of the admin password was requested (NO_ADMIN) but failed", safelog.Err(rerr))
	case reset:
		log.Warn("NO_ADMIN found: the admin password was reset to the default (admin/admin); a new one must be chosen at the next login, then the file is deleted")
		mgr.Audit("admin-reset", "-", "NO_ADMIN file at start")
	}
	switch {
	case !acc.Available():
		log.Warn("the admin UI cannot be used: it needs a state directory (server.stateDir) to store the admin password")
	case acc.Locked():
		log.Error("admin.json is unreadable, so nobody can log in to the admin UI; reset with `zaks-drive admin reset-password` (or create NO_ADMIN in the state volume) and restart")
	case acc.IsDefault():
		log.Warn("the admin UI uses the default login admin/admin until the first login changes it; keep the admin port on 127.0.0.1")
	}
	return acc
}
