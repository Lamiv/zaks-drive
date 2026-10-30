package ctl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/Lamiv/zaks-drive/internal/config"
)

// The support bundle is built from ALLOWLISTED fields. Nothing is copied from .env, from the
// container's environment or from config.yaml as text: each file below is generated from typed
// values chosen here. The logs are the one free-text part; they are sanitized, and a final scan
// refuses to write the archive if any secret value of the deployment is still present.

// ConfigSummary is the allowlisted view of the configuration. It deliberately omits tenant and
// client IDs, the SharePoint host and site names, library and folder names, API keys and any
// address list contents.
type ConfigSummary struct {
	Interfaces      []string       `json:"interfaces"`
	HostPorts       map[string]int `json:"hostPorts"`
	BindAddress     string         `json:"bindAddress"`
	TLSByServer     bool           `json:"tlsTerminatedByServer"`
	GraphConfigured bool           `json:"graphCredentialsConfigured"`
	Graph           map[string]int `json:"graph"`
	Server          map[string]any `json:"server"`
	Repositories    []RepoSummary  `json:"repositories"`
	// ManagedRepositories are the repositories kept in the server volume (only when the admin API was usable).
	ManagedRepositories []RepoRow         `json:"managedRepositories,omitempty"`
	PendingRestart      bool              `json:"pendingRestart,omitempty"`
	FallbackActive      bool              `json:"fallbackActive,omitempty"`
	ContentServer       ContentSummary    `json:"contentServer"`
	Auth                map[string]any    `json:"auth"`
	Transfer            map[string]any    `json:"transfer"`
	Log                 map[string]string `json:"log"`
}

// RepoSummary describes a repository without naming the SharePoint location.
type RepoSummary struct {
	ID             string `json:"id"`
	Backend        string `json:"backend"`      // sharepoint or local
	LocationType   string `json:"locationType"` // siteUrl, siteId, userId, driveId (SharePoint only)
	LibraryNameSet bool   `json:"libraryNameSet"`
	RootPathSet    bool   `json:"rootPathSet"`
	ReadOnly       bool   `json:"readOnly"`
}

// ContentSummary describes the SAP Content Server settings.
type ContentSummary struct {
	Repositories        []ContRepSummary `json:"repositories"`
	AllowedNetworkCount int              `json:"allowedNetworkCount"`
	ClockSkew           string           `json:"clockSkew"`
	AutoActivateCerts   bool             `json:"autoActivateCertificates"`
}

// ContRepSummary is one SAP content repository.
type ContRepSummary struct {
	ContRep    string `json:"contRep"`
	Repository string `json:"repository"`
	Signature  string `json:"signature"`
}

func summarizeConfig(cfg config.Config, p Ports) ConfigSummary {
	s := ConfigSummary{
		Interfaces:      cfg.Server.Interfaces,
		HostPorts:       map[string]int{"rest": p.REST, "sap": p.CS, "admin": p.Admin},
		BindAddress:     p.Bind,
		TLSByServer:     cfg.Server.TLSCertFile != "",
		GraphConfigured: cfg.Graph.TenantID != "" && cfg.Graph.ClientID != "" && cfg.Graph.ClientSecret != "",
		Graph:           map[string]int{"maxConcurrency": cfg.Graph.MaxConcurrency, "maxRetries": cfg.Graph.MaxRetries},
		Server: map[string]any{"maxInFlight": cfg.Server.MaxInFlight, "corsOriginCount": len(cfg.Server.CORSOrigins),
			"pprofEnabled": cfg.Server.EnablePprof, "adminTokenSet": cfg.Server.AdminToken != ""},
		Auth: map[string]any{"apiKeyCount": len(cfg.Auth.APIKeys), "jwtConfigured": cfg.Auth.JWT != nil, "disabled": cfg.Auth.Disabled},
		Transfer: map[string]any{"simpleUploadMaxBytes": cfg.Transfer.SimpleUploadMax, "chunkSizeBytes": cfg.Transfer.ChunkSize,
			"memoryBudgetBytes": cfg.Transfer.MemoryBudget, "maxUploadBytes": cfg.Transfer.MaxUpload, "downloadMode": cfg.Transfer.DownloadMode},
		Log: map[string]string{"level": cfg.Log.Level, "format": cfg.Log.Format},
	}
	for _, r := range cfg.Repositories {
		loc := "unknown"
		switch {
		case r.SiteURL != "":
			loc = "siteUrl"
		case r.SiteID != "":
			loc = "siteId"
		case r.UserID != "":
			loc = "userId"
		case r.DriveID != "":
			loc = "driveId"
		}
		s.Repositories = append(s.Repositories, RepoSummary{ID: r.ID, Backend: r.Backend, LocationType: loc, LibraryNameSet: r.DriveName != "", RootPathSet: r.RootPath != "", ReadOnly: r.ReadOnly})
	}
	s.ContentServer = ContentSummary{AllowedNetworkCount: len(cfg.ContentServer.AllowedNetworks), ClockSkew: cfg.ContentServer.ClockSkew.String(), AutoActivateCerts: cfg.ContentServer.AutoActivateCertificates}
	for _, c := range cfg.ContentServer.Repositories {
		s.ContentServer.Repositories = append(s.ContentServer.Repositories, ContRepSummary{ContRep: c.ContRep, Repository: c.Repository, Signature: c.Signature})
	}
	return s
}

type bundleItem struct {
	Name        string
	Description string
	Business    bool // contains operational identifiers (file names, document IDs, addresses)
	Build       func() ([]byte, error)
}

const bundleReadme = `ZAKS Drive support bundle

This archive was created by "zaks-drive support bundle". It contains only information chosen on purpose:

  versions.json        versions of the command, the installed release and the running server
  system.json          operating system, architecture, Docker and Compose versions, free disk space
  status.json          container states, restart counts, readiness (no environment, no mounts)
  doctor.json          the local diagnostics report (no Microsoft test unless you ran it yourself)
  config-summary.json  an allowlisted summary of the configuration (no IDs, names, hosts or secrets)
  logs.txt             the most recent log lines with credentials masked (may be absent)

It does NOT contain: .env, config.yaml, environment variables, client secrets, API keys, tokens,
certificates or private keys, SharePoint documents, or the SharePoint site, library and folder names.

logs.txt can still contain operational identifiers: file names, SAP document IDs and client
addresses. Read it before you send the archive to anyone.
`

func (a *App) bundleItems(ctx context.Context, d *Deployment, tail int, withLogs bool, san *Sanitizer) []bundleItem {
	marshal := marshalJSON
	items := []bundleItem{
		{Name: "README.txt", Description: "what the bundle contains and does not contain", Build: func() ([]byte, error) { return []byte(bundleReadme), nil }},
		{Name: "versions.json", Description: "command, installed and running versions", Build: func() ([]byte, error) {
			v := map[string]string{"cli": a.Version, "installed": d.loadState().Current, "previous": d.loadState().Previous}
			if code, body, err := a.httpGet(ctx, a.readyURL(d, "/version")); err == nil && code == 200 {
				v["server"] = body
			}
			return marshal(v)
		}},
		{Name: "system.json", Description: "OS, architecture, Docker versions, free disk space", Build: func() ([]byte, error) {
			v := map[string]any{"os": a.GOOS, "arch": a.GOARCH, "go": runtime.Version()}
			if p, err := a.checkDocker(ctx); err == nil {
				v["docker"], v["compose"] = p.DockerServer, p.Compose
			} else {
				v["docker"] = "unavailable"
			}
			if free, err := a.freeBytes(d.Dir); err == nil {
				v["freeDiskMiB"] = free >> 20
			}
			return marshal(v)
		}},
		{Name: "status.json", Description: "container states, restart counts, readiness", Build: func() ([]byte, error) {
			r, err := a.statusReport(ctx, d)
			if err != nil {
				return marshal(map[string]string{"error": san.Line(oneLine(err.Error()))})
			}
			r.Deployment = "(omitted)"
			return marshal(r)
		}},
		{Name: "doctor.json", Description: "local diagnostics report (no Microsoft test)", Business: true, Build: func() ([]byte, error) {
			rep := a.runDoctor(ctx, d, DoctorOpts{})
			rep.Deployment = "(omitted)"
			for i := range rep.Findings {
				// The local path can reveal user and host names.
				rep.Findings[i].Detail = strings.ReplaceAll(rep.Findings[i].Detail, d.Dir, "<deployment>")
				rep.Findings[i].Fix = strings.ReplaceAll(rep.Findings[i].Fix, d.Dir, "<deployment>")
			}
			return marshal(rep)
		}},
		{Name: "config-summary.json", Description: "allowlisted configuration summary", Build: func() ([]byte, error) {
			cfg, problems := a.validateDeployment(d)
			if len(problems) > 0 {
				return marshal(map[string]any{"valid": false, "problems": san.Lines(problems)})
			}
			sum := summarizeConfig(cfg, d.ports())
			// Repositories managed through `repo` live in the server's volume; they are
			// added as allowlisted rows (no location, tenant or client IDs).
			if ov := a.repoOverview(ctx, d); ov != nil {
				sum.ManagedRepositories = ov.rows()
				sum.PendingRestart = ov.Status.PendingRestart
				sum.FallbackActive = ov.Status.Fallback != ""
			}
			return marshal(sum)
		}},
	}
	if withLogs {
		items = append(items, bundleItem{Name: "logs.txt", Description: fmt.Sprintf("last %d log lines, credentials masked", tail), Business: true, Build: func() ([]byte, error) {
			res, err := a.compose(ctx, d, RunOpts{}, "logs", "--no-color", "--no-log-prefix", "--timestamps", "--tail", fmt.Sprint(tail))
			if err != nil {
				return []byte("logs could not be read: " + san.Line(oneLine(err.Error())) + "\n"), nil
			}
			return []byte(san.Text(res.Stdout + res.Stderr)), nil
		}})
	}
	return items
}

// firstSecretIn reports whether b contains any of the secret values (the fail-closed check).
func firstSecretIn(b []byte, secrets []string) bool {
	for _, sv := range secrets {
		if sv != "" && bytes.Contains(b, []byte(sv)) {
			return true
		}
	}
	return false
}

// Lines sanitizes each string of a slice.
func (s *Sanitizer) Lines(in []string) []string {
	out := make([]string, len(in))
	for i, l := range in {
		out[i] = s.Line(l)
	}
	return out
}

func (a *App) cmdSupport(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] != "bundle" {
		fmt.Fprintln(a.Err, "usage: zaks-drive support bundle [--dir DIR] [--output FILE] [--logs-tail N] [--no-logs] [--yes]")
		return ExitUsage
	}
	fset := flag.NewFlagSet("support bundle", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	output := fset.String("output", "", "archive path (default ./zaks-drive-support-<time>.tar.gz; an existing file is never overwritten)")
	tail := fset.Int("logs-tail", 1000, "number of recent log lines to include")
	noLogs := fset.Bool("no-logs", false, "leave the logs out")
	yes := fset.Bool("yes", false, "confirm without asking")
	if err := fset.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	if *tail < 1 || *tail > 50000 {
		return a.fail(ExitUsage, "--logs-tail must be between 1 and 50000", "")
	}
	d, err := a.locate(*dir)
	if err != nil {
		return a.fail(ExitNoDeployment, err.Error(), "pass --dir DIR")
	}
	san := a.sanitizerFor(d)
	items := a.bundleItems(ctx, d, *tail, !*noLogs, san)

	a.printf("The support bundle will contain:\n")
	business := false
	for _, it := range items {
		mark := " "
		if it.Business {
			mark = "*"
			business = true
		}
		a.printf("  %s %-20s %s\n", mark, it.Name, it.Description)
	}
	a.printf("\nIt will NOT contain .env, config.yaml, environment variables, secrets, API keys, tokens, certificates,\nprivate keys, documents, or SharePoint site, library and folder names.\n")
	if business {
		a.printf("* These files can contain operational identifiers (SharePoint file names, SAP document IDs, client addresses).\n  Credentials are masked, but read them before you share the archive.\n")
	}
	if !a.confirm(ctx, "Collect this information and write the archive?", *yes) {
		a.printf("Nothing was collected.\n")
		return ExitDeclined
	}

	name := *output
	if name == "" {
		name = "zaks-drive-support-" + a.Now().UTC().Format("20060102T150405Z") + ".tar.gz"
	}
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	secrets := NewSanitizer(d.secretValues()).values
	now := a.Now()
	type written struct {
		name string
		size int
	}
	var done []written
	for _, it := range items {
		b, err := it.Build()
		if err != nil {
			return a.fail(ExitFail, "could not build "+it.Name+": "+err.Error(), "")
		}
		// Fail closed: a secret value of this deployment must never be in the archive.
		if firstSecretIn(b, secrets) {
			return a.fail(ExitFail, "refusing to write the bundle: "+it.Name+" still contains a secret value of this deployment",
				"this is a bug in the sanitizer; please report it (without the file)")
		}
		if err := tw.WriteHeader(&tar.Header{Name: it.Name, Mode: 0o600, Size: int64(len(b)), ModTime: now, Typeflag: tar.TypeReg}); err != nil {
			return a.fail(ExitFail, err.Error(), "")
		}
		if _, err := tw.Write(b); err != nil {
			return a.fail(ExitFail, err.Error(), "")
		}
		done = append(done, written{it.Name, len(b)})
	}
	if err := tw.Close(); err != nil {
		return a.fail(ExitFail, err.Error(), "")
	}
	if err := gz.Close(); err != nil {
		return a.fail(ExitFail, err.Error(), "")
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return a.fail(ExitFail, "cannot create "+name+": "+err.Error(), "choose a path that does not exist yet")
	}
	if _, err := f.Write(raw.Bytes()); err != nil {
		f.Close()
		return a.fail(ExitFail, err.Error(), "")
	}
	if err := f.Close(); err != nil {
		return a.fail(ExitFail, err.Error(), "")
	}
	a.printf("\nWrote %s (%d bytes, mode 600):\n", name, raw.Len())
	for _, w := range done {
		a.printf("  %-20s %6d bytes\n", w.name, w.size)
	}
	a.printf("Review the archive before you send it. Nothing was uploaded.\n")
	return ExitOK
}
