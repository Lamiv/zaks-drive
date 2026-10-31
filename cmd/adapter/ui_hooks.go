package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

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
