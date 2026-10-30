package ctl

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Lamiv/zaks-drive/internal/config"
)

// prepare finds the deployment and checks Docker. It returns an exit code != 0 when the
// command must stop (the message has been printed).
func (a *App) prepare(ctx context.Context, dirFlag string) (*Deployment, int) {
	d, err := a.locate(dirFlag)
	if err != nil {
		return nil, a.fail(ExitNoDeployment, err.Error(), "create one with: zaks-drive setup (or pass --dir DIR)")
	}
	if _, err := a.checkDocker(ctx); err != nil {
		var pe *PrereqError
		if asPrereq(err, &pe) {
			return nil, a.fail(ExitPrereq, pe.Msg, pe.Fix)
		}
		return nil, a.fail(ExitPrereq, err.Error(), "")
	}
	return d, ExitOK
}

func asPrereq(err error, target **PrereqError) bool {
	pe, ok := err.(*PrereqError)
	if ok {
		*target = pe
	}
	return ok
}

func (a *App) readyURL(d *Deployment, path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", d.ports().Admin, path)
}

// httpGet fetches a URL of the local admin listener and returns the status and a short body.
func (a *App) httpGet(ctx context.Context, url string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

func (a *App) pollInterval() time.Duration {
	if a.PollInterval > 0 {
		return a.PollInterval
	}
	return time.Second
}

// waitReady polls until /readyz answers "ready", the container stops, or the timeout passes.
// It returns "" when ready, otherwise the reason.
func (a *App) waitReady(ctx context.Context, d *Deployment, timeout time.Duration) string {
	deadline := a.Now().Add(timeout)
	last := "no answer yet"
	san := a.sanitizerFor(d)
	for {
		if cs, err := a.managed(ctx, d); err == nil {
			for _, c := range cs {
				if c.State == "exited" || c.State == "dead" {
					return fmt.Sprintf("the container %s stopped (exit code %d)", c.Name, c.ExitCode)
				}
				if c.State == "restarting" || c.RestartCount > 2 {
					return fmt.Sprintf("the container %s keeps restarting (%d restarts)", c.Name, c.RestartCount)
				}
			}
		}
		code, body, err := a.httpGet(ctx, a.readyURL(d, "/readyz"))
		switch {
		case err == nil && code == http.StatusOK:
			return ""
		case err == nil:
			last = san.Line(oneLine(body))
			if last == "" {
				last = fmt.Sprintf("HTTP %d", code)
			}
		default:
			last = "the admin endpoint does not answer yet"
		}
		if !a.Now().Before(deadline) {
			return "not ready after " + timeout.String() + ": " + last
		}
		select {
		case <-ctx.Done():
			return "interrupted"
		case <-time.After(a.pollInterval()):
		}
	}
}

func (a *App) cmdStart(ctx context.Context, args []string) int {
	return a.runLifecycle(ctx, "start", args)
}
func (a *App) cmdRestart(ctx context.Context, args []string) int {
	return a.runLifecycle(ctx, "restart", args)
}

func (a *App) cmdStop(ctx context.Context, args []string) int {
	fset := flag.NewFlagSet("stop", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	if err := fset.Parse(args); err != nil {
		return ExitUsage
	}
	d, code := a.prepare(ctx, *dir)
	if code != ExitOK {
		return code
	}
	// "stop" only stops the containers. It never removes containers or volumes.
	res, err := a.compose(ctx, d, RunOpts{}, "stop")
	if err != nil || res.ExitCode != 0 {
		return a.fail(ExitFail, "docker compose stop failed: "+oneLine(trimLines(a.sanitizerFor(d).Text(res.Stderr), 3)), "run: zaks-drive doctor")
	}
	a.printf("ZAKS Drive stopped. The containers and all data are kept; start again with: zaks-drive start\n")
	return ExitOK
}

func (a *App) runLifecycle(ctx context.Context, action string, args []string) int {
	fset := flag.NewFlagSet(action, flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	timeout := fset.Duration("timeout", 120*time.Second, "how long to wait until the server is ready")
	noWait := fset.Bool("no-wait", false, "do not wait for readiness")
	if err := fset.Parse(args); err != nil {
		return ExitUsage
	}
	d, code := a.prepare(ctx, *dir)
	if code != ExitOK {
		return code
	}
	cfg, problems := a.validateDeployment(d)
	if len(problems) > 0 {
		fmt.Fprintf(a.Err, "zaks-drive: the configuration is not valid, so ZAKS Drive was not %s:\n", map[string]string{"start": "started", "restart": "restarted"}[action])
		for _, p := range problems {
			fmt.Fprintf(a.Err, "  %s\n", p)
		}
		fmt.Fprintln(a.Err, "  fix: edit the files and run: zaks-drive config validate")
		return ExitNoDeployment
	}
	san := a.sanitizerFor(d)

	if action == "start" {
		if rc := a.ensureImages(ctx, d); rc != ExitOK {
			return rc
		}
		sub := []string{"up", "-d"}
		if d.SourceMode() {
			sub = append(sub, "--build")
		}
		res, err := a.compose(ctx, d, RunOpts{}, sub...)
		if err != nil || res.ExitCode != 0 {
			return a.fail(ExitFail, "docker compose up failed: "+oneLine(trimLines(san.Text(res.Stderr), 4)), startFailureFix(res.Stderr))
		}
	} else {
		res, err := a.compose(ctx, d, RunOpts{}, "restart")
		if err != nil || res.ExitCode != 0 {
			return a.fail(ExitFail, "docker compose restart failed: "+oneLine(trimLines(san.Text(res.Stderr), 4)), "if the containers do not exist yet, run: zaks-drive start")
		}
	}
	if *noWait {
		a.printf("ZAKS Drive %sed (not waiting for readiness).\n", action)
		return ExitOK
	}
	a.printf("Waiting for ZAKS Drive to become ready (up to %s)...\n", timeout.Round(time.Second))
	if reason := a.waitReady(ctx, d, *timeout); reason != "" {
		a.errorf("zaks-drive: ZAKS Drive did not become ready: %s\n", reason)
		a.printRecentLogs(ctx, d, 15)
		a.errorf("  fix: run: zaks-drive doctor   (add --connectivity to test Microsoft access)\n")
		return ExitUnhealthy
	}
	a.printEndpoints(d, cfg)
	return ExitOK
}

func startFailureFix(stderr string) string {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "port is already allocated") || strings.Contains(s, "address already in use"):
		return "a host port is already in use: change REST_PORT, CS_PORT or ADMIN_PORT in .env (Ubuntu's Cockpit uses 9090), or run: zaks-drive doctor"
	case strings.Contains(s, "no such image") || strings.Contains(s, "pull access denied"):
		return "the image zaks-drive:<version> does not exist locally: run zaks-drive setup (or zaks-drive update) to build it from a verified release"
	}
	return "run: zaks-drive doctor"
}

func (a *App) printEndpoints(d *Deployment, cfg config.Config) {
	p := d.ports()
	host := p.Bind
	if host == "0.0.0.0" || host == "" {
		host = "<this-host>"
	}
	a.printf("ZAKS Drive is ready.\n")
	if cfg.Server.Enabled("rest") {
		a.printf("  REST API            http://%s:%d/api/v1\n", host, p.REST)
	}
	if cfg.Server.Enabled("contentrepo") {
		a.printf("  SAP Content Server  http://%s:%d/ContentServer/ContentServer.dll\n", host, p.CS)
	}
	a.printf("  Health and metrics  http://127.0.0.1:%d  (localhost only)\n", p.Admin)
}

// printRecentLogs prints the last lines of sanitized logs to stderr, for failure diagnosis.
func (a *App) printRecentLogs(ctx context.Context, d *Deployment, n int) {
	res, err := a.compose(ctx, d, RunOpts{}, "logs", "--no-color", "--no-log-prefix", "--tail", fmt.Sprint(n))
	if err != nil || strings.TrimSpace(res.Stdout+res.Stderr) == "" {
		return
	}
	a.errorf("  last log lines (sanitized):\n")
	for _, l := range strings.Split(trimLines(a.sanitizerFor(d).Text(res.Stdout+res.Stderr), n), "\n") {
		a.errorf("    %s\n", l)
	}
}

// ensureImages makes sure the image(s) of a released deployment exist, building from a verified
// release payload when possible. In source mode compose builds the image itself.
func (a *App) ensureImages(ctx context.Context, d *Deployment) int {
	if d.SourceMode() {
		return ExitOK
	}
	res, err := a.compose(ctx, d, RunOpts{}, "config", "--images")
	if err != nil || res.ExitCode != 0 {
		return a.fail(ExitFail, "docker compose config failed: "+oneLine(trimLines(res.Stderr, 3)), "run: zaks-drive config validate; the compose file may be damaged")
	}
	for _, img := range strings.Fields(res.Stdout) {
		if a.imageExists(ctx, img) {
			continue
		}
		version := d.loadState().Current
		if rc := a.buildFromPayload(ctx, version); rc != ExitOK {
			return rc
		}
		if !a.imageExists(ctx, img) {
			return a.fail(ExitFail, "the image "+img+" does not exist and could not be built", "run: zaks-drive update --version <version> to build it from a verified release")
		}
	}
	return ExitOK
}

// ---- status and version ----

// StatusReport is the machine-readable form of zaks-drive status.
type StatusReport struct {
	Deployment string          `json:"deployment"`
	Mode       string          `json:"mode"` // release or source
	CLIVersion string          `json:"cliVersion"`
	Installed  string          `json:"installedVersion,omitempty"`
	Previous   string          `json:"previousVersion,omitempty"`
	Server     string          `json:"serverVersion,omitempty"`
	Ready      bool            `json:"ready"`
	ReadyNote  string          `json:"readyNote,omitempty"`
	Containers []ContainerInfo `json:"containers"`
	// Repositories is the allowlisted view of the repositories (only when the server's admin API is usable).
	Repositories   []RepoRow `json:"repositories,omitempty"`
	PendingRestart bool      `json:"pendingRestart,omitempty"`
	Fallback       bool      `json:"fallbackActive,omitempty"`
}

// ContainerInfo is the sanitized, allowlisted view of a container.
type ContainerInfo struct {
	Name         string   `json:"name"`
	Service      string   `json:"service"`
	State        string   `json:"state"`
	Health       string   `json:"health,omitempty"`
	ExitCode     int      `json:"exitCode,omitempty"`
	RestartCount int      `json:"restartCount"`
	Image        string   `json:"image"`
	Ports        []string `json:"ports,omitempty"`
}

func containerInfos(cs []Container) []ContainerInfo {
	out := make([]ContainerInfo, 0, len(cs))
	for _, c := range cs {
		out = append(out, ContainerInfo{Name: c.Name, Service: c.Service, State: c.State, Health: c.Health,
			ExitCode: c.ExitCode, RestartCount: c.RestartCount, Image: c.Image, Ports: c.Ports})
	}
	return out
}

func (a *App) statusReport(ctx context.Context, d *Deployment) (StatusReport, error) {
	r := StatusReport{Deployment: d.Dir, Mode: "release", CLIVersion: a.Version}
	if d.SourceMode() {
		r.Mode = "source"
	}
	st := d.loadState()
	r.Installed, r.Previous = st.Current, st.Previous
	cs, err := a.managed(ctx, d)
	if err != nil {
		return r, err
	}
	r.Containers = containerInfos(cs)
	if code, body, err := a.httpGet(ctx, a.readyURL(d, "/readyz")); err == nil {
		r.Ready = code == http.StatusOK
		if !r.Ready {
			r.ReadyNote = a.sanitizerFor(d).Line(oneLine(body))
		}
		if vc, vb, err := a.httpGet(ctx, a.readyURL(d, "/version")); err == nil && vc == http.StatusOK {
			r.Server = vb
		}
	} else {
		r.ReadyNote = "the admin endpoint does not answer"
	}
	if ov := a.repoOverview(ctx, d); ov != nil {
		r.Repositories, r.PendingRestart, r.Fallback = ov.rows(), ov.Status.PendingRestart, ov.Status.Fallback != ""
	}
	return r, nil
}

func (a *App) cmdStatus(ctx context.Context, args []string) int {
	fset := flag.NewFlagSet("status", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	asJSON := fset.Bool("json", false, "machine-readable output")
	if err := fset.Parse(args); err != nil {
		return ExitUsage
	}
	d, code := a.prepare(ctx, *dir)
	if code != ExitOK {
		return code
	}
	r, err := a.statusReport(ctx, d)
	if err != nil {
		return a.fail(ExitFail, err.Error(), "run: zaks-drive doctor")
	}
	if *asJSON {
		b, _ := marshalJSON(r)
		a.printf("%s\n", b)
	} else {
		a.printf("Deployment   %s (%s)\n", r.Deployment, r.Mode)
		a.printf("CLI          %s\n", r.CLIVersion)
		if r.Installed != "" {
			a.printf("Installed    %s", r.Installed)
			if r.Previous != "" {
				a.printf("  (previous: %s, rollback available)", r.Previous)
			}
			a.printf("\n")
		}
		if r.Server != "" {
			a.printf("Server       %s\n", r.Server)
		}
		if len(r.Containers) == 0 {
			a.printf("Containers   none (not started; run: zaks-drive start)\n")
		}
		for _, c := range r.Containers {
			h := c.State
			if c.Health != "" {
				h += ", " + c.Health
			}
			a.printf("Container    %s: %s, %d %s, image %s\n", c.Name, h, c.RestartCount, plural(c.RestartCount, "restart", "restarts"), c.Image)
		}
		switch {
		case r.Ready:
			a.printf("Ready        yes\n")
		case len(r.Containers) > 0:
			a.printf("Ready        NO: %s\n", r.ReadyNote)
		}
		for _, row := range r.Repositories {
			sap := ""
			if row.SAP {
				sap = ", SAP"
			}
			a.printf("Repository   %s: %s (%s%s), %s\n", row.ID, row.Backend, row.Source, sap, row.State)
		}
		if r.PendingRestart {
			a.printf("Pending      repository changes are waiting for a restart (zaks-drive restart)\n")
		}
		if r.Fallback {
			a.printf("WARNING      the server runs the LAST WORKING repository state because the current one is invalid (zaks-drive repo list)\n")
		}
	}
	if len(r.Containers) == 0 || !r.Ready {
		return ExitUnhealthy
	}
	return ExitOK
}

func (a *App) cmdVersion(ctx context.Context, args []string) int {
	fset := flag.NewFlagSet("version", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	asJSON := fset.Bool("json", false, "machine-readable output")
	if err := fset.Parse(args); err != nil {
		return ExitUsage
	}
	out := map[string]string{"cli": a.Version, "os": a.GOOS, "arch": a.GOARCH}
	if d, err := a.locate(*dir); err == nil {
		if s := d.loadState(); s.Current != "" {
			out["installed"] = s.Current
		}
		if code, body, err := a.httpGet(ctx, a.readyURL(d, "/version")); err == nil && code == http.StatusOK {
			out["server"] = body
		}
	}
	if *asJSON {
		b, _ := marshalJSON(out)
		a.printf("%s\n", b)
		return ExitOK
	}
	a.printf("zaks-drive (management command)  %s  (%s/%s)\n", out["cli"], out["os"], out["arch"])
	if v, ok := out["installed"]; ok {
		a.printf("installed release                %s\n", v)
	}
	if v, ok := out["server"]; ok {
		a.printf("running server                   %s\n", v)
	} else {
		a.printf("running server                   not reachable (not started, or no deployment found)\n")
	}
	return ExitOK
}
