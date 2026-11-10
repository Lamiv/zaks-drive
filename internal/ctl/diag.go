package ctl

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Lamiv/zaks-drive/internal/config"
)

// Status is the result class of one diagnostic.
type Status string

const (
	StatusPass    Status = "PASS"
	StatusWarn    Status = "WARN"
	StatusFail    Status = "FAIL"
	StatusSkipped Status = "SKIPPED"
)

// Finding is the outcome of one diagnostic.
type Finding struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// DiagInfo documents one diagnostic: what it looks at, how, and when it fails or warns.
// docs/cli.md lists every entry (a test keeps the two in step).
type DiagInfo struct {
	ID     string
	Title  string
	How    string
	Fail   string
	Warn   string
	Remote bool // contacts something other than this host
}

// Diagnostics is the documented list of everything `zaks-drive doctor` can report.
var Diagnostics = []DiagInfo{
	{ID: "os-platform", Title: "Operating system and architecture", How: "Reads the OS and CPU architecture the command runs on.",
		Warn: "Not Linux, or not amd64/arm64: other platforms are untested."},
	{ID: "docker-installed", Title: "Docker CLI", How: "Runs `docker version`.", Fail: "The docker command is not on PATH."},
	{ID: "docker-daemon", Title: "Docker daemon access", How: "Runs `docker version` and reads the error to tell a stopped daemon from a permission problem.",
		Fail: "The daemon is not running, or this user may not use the Docker socket."},
	{ID: "docker-compose", Title: "Docker Compose v2", How: "Runs `docker compose version`.", Fail: "The Compose v2 plugin is missing."},
	{ID: "deployment-files", Title: "Deployment files", How: "Looks for docker-compose.yml, .env and config.yaml in the deployment directory.",
		Fail: "A file is missing."},
	{ID: "file-permissions", Title: "Secret file permissions", How: "Reads the mode of .env and of tls/tls.key.",
		Fail: ".env is readable by every user.", Warn: ".env is readable by the group, or the TLS key by others."},
	{ID: "config-valid", Title: "Configuration validity", How: "Loads config.yaml with the server's own loader, with only .env as the environment (like the container). Nothing is contacted.",
		Fail: "The loader reports errors (each has a code, see docs/error-codes.md)."},
	{ID: "api-keys", Title: "API key strength", How: "Measures the length of the API keys and admin token in .env; checks that REST authentication is not disabled.",
		Warn: "A key is shorter than 32 characters, or authentication is disabled."},
	{ID: "repository-mapping", Title: "Repository mappings", How: "Checks that every SAP content repository maps to a configured repository and reviews risky settings.",
		Fail: "A content repository maps to a repository that does not exist.", Warn: "Signature mode none, or no allowedNetworks while the SAP interface is enabled."},
	{ID: "repositories", Title: "Managed repositories", How: "Asks the server's admin API (ADMIN_TOKEN from .env, localhost only) whether repository changes wait for a restart, whether the server fell back to the last working state, and whether every repository is served.",
		Fail: "The server fell back to the last working repository state because the current one is invalid.", Warn: "Changes are pending a restart, or a repository is defined but not served."},
	{ID: "admin-password", Title: "Admin UI password", How: "Asks the server's admin API whether the admin web page still accepts the default login admin/admin (before the first login has chosen a password).",
		Warn: "The default password is still active."},
	{ID: "container-state", Title: "Container state and restarts", How: "Asks `docker compose ps` (only this deployment's containers) and `docker inspect` for restart counts.",
		Fail: "The container is stopped, unhealthy, or restarted three or more times.", Warn: "Not started yet, or restarted once or twice, or health is still starting."},
	{ID: "container-resources", Title: "Memory limit and OOM kills", How: "Reads `docker inspect` State.OOMKilled and the memory limit of the running container.",
		Fail: "The container was killed for exceeding its memory limit.", Warn: "No memory limit is set."},
	{ID: "ports", Title: "Port conflicts", How: "If the container runs, checks that it publishes the expected ports; otherwise tries to bind each host port.",
		Fail: "A port is already in use by another process.", Warn: "The container runs but does not publish an expected port."},
	{ID: "health-endpoint", Title: "Health endpoint", How: "GET /healthz on 127.0.0.1 at the admin port.", Fail: "No answer from a running container."},
	{ID: "readiness", Title: "Readiness and Microsoft Graph state", How: "GET /readyz on 127.0.0.1. The server reports whether it could reach SharePoint with its credentials; a failure is classified (secret, permissions, site grant, location, network).",
		Fail: "The server is not ready."},
	{ID: "server-version", Title: "Server and command versions", How: "GET /version and the recorded installed version; applies the compatibility policy (same compatibility line).",
		Warn: "Different compatibility lines, or the installed and running versions differ."},
	{ID: "tls-certificate", Title: "TLS certificate", How: "If the server terminates TLS, reads <dir>/tls/tls.crt and tls.key: validity period and that the key matches.",
		Fail: "Expired, unreadable, or key and certificate do not match.", Warn: "Expires within 30 days."},
	{ID: "sap-endpoint", Title: "SAP Content Server endpoint", How: "Calls serverInfo (unsigned) on the Content Server port of this host and compares the repositories it lists with config.yaml. Does not contact SAP.",
		Fail: "The endpoint does not answer or a configured repository is missing.", Warn: "The endpoint refuses this host because of allowedNetworks (expected)."},
	{ID: "sap-certificates", Title: "SAP signing certificates", How: "Lists certificates through the admin API (ADMIN_TOKEN from .env, localhost only). Never prints certificate contents.",
		Fail: "An active certificate has expired.", Warn: "A repository that checks signatures has no active certificate yet."},
	{ID: "disk-space", Title: "Disk space", How: "Free space of the file system that holds the deployment directory.",
		Fail: "Less than 200 MiB free.", Warn: "Less than 2 GiB free."},
	{ID: "graph-connectivity", Title: "Microsoft connectivity (explicit)", How: "Only with --connectivity: runs `check` inside the container, which signs in to Microsoft Entra ID and tests SharePoint access (it creates and deletes small test files).",
		Fail: "A required SharePoint access is missing.", Remote: true},
}

func diagInfo(id string) DiagInfo {
	for _, d := range Diagnostics {
		if d.ID == id {
			return d
		}
	}
	return DiagInfo{ID: id, Title: id}
}

// DoctorOpts selects what doctor does.
type DoctorOpts struct {
	Connectivity bool
}

// DoctorReport is the machine-readable result.
type DoctorReport struct {
	CLIVersion string         `json:"cliVersion"`
	Deployment string         `json:"deployment"`
	Findings   []Finding      `json:"findings"`
	Summary    map[Status]int `json:"summary"`
}

type diagRun struct {
	a        *App
	d        *Deployment
	ctx      context.Context
	opts     DoctorOpts
	san      *Sanitizer
	findings []Finding

	dockerOK   bool
	cfg        config.Config
	cfgOK      bool
	envMap     map[string]string
	containers []Container
	running    bool
	ports      Ports
	ov         *repoOverview
	ovDone     bool
}

func (r *diagRun) add(id string, st Status, detail, fix string) {
	r.findings = append(r.findings, Finding{ID: id, Title: diagInfo(id).Title, Status: st, Detail: r.san.Line(oneLine(detail)), Fix: fix})
}

// runDoctor runs every diagnostic in order. A diagnostic whose input is missing is SKIPPED with
// the reason, never silently omitted.
func (a *App) runDoctor(ctx context.Context, d *Deployment, opts DoctorOpts) DoctorReport {
	r := &diagRun{a: a, d: d, ctx: ctx, opts: opts, san: a.sanitizerFor(d), ports: d.ports()}
	r.envMap, _ = ReadEnv(d.EnvFile)
	r.checkPlatform()
	r.checkDocker()
	r.checkFiles()
	r.checkPermissions()
	r.checkConfig()
	r.checkKeys()
	r.checkMappings()
	r.checkRepositories()
	r.checkAdminPassword()
	r.checkContainer()
	r.checkResources()
	r.checkPorts()
	r.checkHealth()
	r.checkReadiness()
	r.checkVersion()
	r.checkTLS()
	r.checkSAPEndpoint()
	r.checkSAPCertificates()
	r.checkDisk()
	r.checkConnectivity()

	rep := DoctorReport{CLIVersion: a.Version, Deployment: d.Dir, Findings: r.findings, Summary: map[Status]int{}}
	for _, f := range r.findings {
		rep.Summary[f.Status]++
	}
	return rep
}

func (r *diagRun) checkPlatform() {
	switch {
	case r.a.GOOS != "linux":
		r.add("os-platform", StatusWarn, fmt.Sprintf("%s/%s: the supported platform is Linux (amd64 or arm64)", r.a.GOOS, r.a.GOARCH), "run the management command on a Linux host")
	case r.a.GOARCH != "amd64" && r.a.GOARCH != "arm64":
		r.add("os-platform", StatusWarn, fmt.Sprintf("linux/%s is not tested (amd64 and arm64 are)", r.a.GOARCH), "")
	default:
		r.add("os-platform", StatusPass, fmt.Sprintf("linux/%s", r.a.GOARCH), "")
	}
}

func (r *diagRun) checkDocker() {
	p, err := r.a.checkDocker(r.ctx)
	if err == nil {
		r.dockerOK = true
		r.add("docker-installed", StatusPass, "Docker Engine "+p.DockerServer, "")
		r.add("docker-daemon", StatusPass, "the daemon answers and this user may use it", "")
		r.add("docker-compose", StatusPass, "Docker Compose "+p.Compose, "")
		return
	}
	pe, ok := err.(*PrereqError)
	if !ok {
		pe = &PrereqError{Kind: "other", Msg: err.Error()}
	}
	switch pe.Kind {
	case "missing":
		r.add("docker-installed", StatusFail, pe.Msg, pe.Fix)
		r.add("docker-daemon", StatusSkipped, "Docker is not installed", "")
		r.add("docker-compose", StatusSkipped, "Docker is not installed", "")
	case "compose":
		r.add("docker-installed", StatusPass, "the docker command works", "")
		r.add("docker-daemon", StatusPass, "the daemon answers and this user may use it", "")
		r.add("docker-compose", StatusFail, pe.Msg, pe.Fix)
	default:
		r.add("docker-installed", StatusPass, "the docker command exists", "")
		r.add("docker-daemon", StatusFail, pe.Msg, pe.Fix)
		r.add("docker-compose", StatusSkipped, "the Docker daemon is not usable", "")
	}
}

func (r *diagRun) checkFiles() {
	var missing []string
	for _, f := range []string{r.d.ComposeFile, r.d.EnvFile, r.d.ConfigFile} {
		if st, err := os.Stat(f); err != nil || st.IsDir() {
			missing = append(missing, filepath.Base(f))
		}
	}
	if len(missing) > 0 {
		r.add("deployment-files", StatusFail, "missing in "+r.d.Dir+": "+strings.Join(missing, ", "), "run: zaks-drive setup --dir "+r.d.Dir)
		return
	}
	mode := "release"
	if r.d.SourceMode() {
		mode = "source checkout (builds the image from source)"
	}
	r.add("deployment-files", StatusPass, "docker-compose.yml, .env and config.yaml found in "+r.d.Dir+" ("+mode+")", "")
}

func (r *diagRun) checkPermissions() {
	if r.a.GOOS == "windows" || runtime.GOOS == "windows" {
		r.add("file-permissions", StatusSkipped, "file modes are not meaningful on Windows", "")
		return
	}
	st, err := os.Stat(r.d.EnvFile)
	if err != nil {
		r.add("file-permissions", StatusSkipped, ".env does not exist", "")
		return
	}
	var notes []string
	status := StatusPass
	if m := st.Mode().Perm(); m&0o007 != 0 {
		status = StatusFail
		notes = append(notes, fmt.Sprintf(".env is readable by every user (mode %04o)", m))
	} else if m&0o070 != 0 {
		status = StatusWarn
		notes = append(notes, fmt.Sprintf(".env is readable by its group (mode %04o)", m))
	}
	if kst, err := os.Stat(filepath.Join(r.d.Dir, "tls", "tls.key")); err == nil && kst.Mode().Perm()&0o007 != 0 {
		if status == StatusPass {
			status = StatusWarn
		}
		notes = append(notes, fmt.Sprintf("tls/tls.key is readable by every user (mode %04o)", kst.Mode().Perm()))
	}
	if status == StatusPass {
		r.add("file-permissions", StatusPass, fmt.Sprintf(".env mode %04o", st.Mode().Perm()), "")
		return
	}
	r.add("file-permissions", status, strings.Join(notes, "; "), "run: chmod 600 "+r.d.EnvFile)
}

func (r *diagRun) checkConfig() {
	cfg, problems := r.a.validateDeployment(r.d)
	if len(problems) > 0 {
		shown := problems
		if len(shown) > 6 {
			shown = append(shown[:6:6], fmt.Sprintf("... and %d more", len(problems)-6))
		}
		r.add("config-valid", StatusFail, strings.Join(shown, " | "), "fix the files, then run: zaks-drive config validate")
		return
	}
	r.cfg, r.cfgOK = cfg, true
	r.add("config-valid", StatusPass, "config.yaml and .env load without errors", "")
}

func (r *diagRun) checkKeys() {
	if _, err := os.Stat(r.d.EnvFile); err != nil {
		r.add("api-keys", StatusSkipped, ".env does not exist", "")
		return
	}
	var weak []string
	for k, v := range r.envMap {
		u := strings.ToUpper(k)
		if (strings.Contains(u, "API_KEY") || u == "ADMIN_TOKEN") && v != "" && len(v) < 32 {
			weak = append(weak, k)
		}
	}
	sort.Strings(weak)
	switch {
	case r.cfgOK && r.cfg.Auth.Disabled:
		r.add("api-keys", StatusWarn, "REST authentication is disabled (auth.disabled / AUTH_DISABLED)", "enable it outside of tests")
	case len(weak) > 0:
		r.add("api-keys", StatusWarn, "shorter than 32 characters: "+strings.Join(weak, ", "), "generate random keys: openssl rand -hex 32")
	default:
		r.add("api-keys", StatusPass, "keys and admin token are at least 32 characters", "")
	}
}

func (r *diagRun) checkMappings() {
	if !r.cfgOK {
		r.add("repository-mapping", StatusSkipped, "the configuration is not valid (see config-valid)", "")
		return
	}
	repos := map[string]bool{}
	for _, rp := range r.cfg.Repositories {
		repos[rp.ID] = true
	}
	var fails, warns []string
	for _, c := range r.cfg.ContentServer.Repositories {
		if !repos[c.Repository] {
			fails = append(fails, fmt.Sprintf("content repository %s maps to %q, which is not in repositories", c.ContRep, c.Repository))
		}
		if c.Signature == "none" {
			warns = append(warns, fmt.Sprintf("%s has signature none (SAP signatures are not checked)", c.ContRep))
		}
	}
	if r.cfg.Server.Enabled("contentrepo") && len(r.cfg.ContentServer.AllowedNetworks) == 0 {
		warns = append(warns, "contentServer.allowedNetworks is empty: any host that reaches the SAP port may call it")
	}
	switch {
	case len(fails) > 0:
		r.add("repository-mapping", StatusFail, strings.Join(fails, "; "), "fix contentServer.repositories in config.yaml")
	case len(warns) > 0:
		r.add("repository-mapping", StatusWarn, strings.Join(warns, "; "), "tighten the settings before production use")
	default:
		r.add("repository-mapping", StatusPass, fmt.Sprintf("%d repositories, %d SAP content repositories, all mapped", len(r.cfg.Repositories), len(r.cfg.ContentServer.Repositories)), "")
	}
}

func (r *diagRun) checkContainer() {
	if !r.dockerOK || !fileExists(r.d.ComposeFile) {
		r.add("container-state", StatusSkipped, "Docker or the compose file is not available", "")
		return
	}
	cs, err := r.a.managed(r.ctx, r.d)
	if err != nil {
		r.add("container-state", StatusFail, err.Error(), "check the compose file: docker compose -f "+r.d.ComposeFile+" config")
		return
	}
	r.containers = cs
	if len(cs) == 0 {
		r.add("container-state", StatusWarn, "ZAKS Drive is not started", "run: zaks-drive start")
		return
	}
	status, notes := StatusPass, []string{}
	for _, c := range cs {
		line := fmt.Sprintf("%s: %s", c.Name, c.State)
		if c.Health != "" {
			line += ", " + c.Health
		}
		line += fmt.Sprintf(", %d restarts", c.RestartCount)
		notes = append(notes, line)
		escalate := func(s Status) {
			if s == StatusFail || status == StatusPass {
				status = s
			}
		}
		switch {
		case c.State == "running":
			r.running = true
			if c.Health == "unhealthy" || c.RestartCount >= 3 {
				escalate(StatusFail)
			} else if c.Health == "starting" || c.RestartCount > 0 {
				escalate(StatusWarn)
			}
		default:
			escalate(StatusFail)
		}
	}
	fix := ""
	if status != StatusPass {
		fix = "run: zaks-drive logs --level warn --tail 50"
	}
	r.add("container-state", status, strings.Join(notes, "; "), fix)
}

func (r *diagRun) checkResources() {
	if !r.running {
		r.add("container-resources", StatusSkipped, "the container is not running", "")
		return
	}
	args := []string{"inspect", "--format", "{{.State.OOMKilled}} {{.HostConfig.Memory}}"}
	for _, c := range r.containers {
		if c.State == "running" {
			args = append(args, c.ID)
		}
	}
	res, err := r.a.Runner.Run(r.ctx, "docker", args, RunOpts{})
	if err != nil || res.ExitCode != 0 {
		r.add("container-resources", StatusSkipped, "docker inspect did not answer", "")
		return
	}
	f := strings.Fields(strings.SplitN(res.Stdout, "\n", 2)[0])
	if len(f) != 2 {
		r.add("container-resources", StatusSkipped, "unexpected docker inspect output", "")
		return
	}
	mem, _ := strconv.ParseInt(f[1], 10, 64)
	switch {
	case f[0] == "true":
		r.add("container-resources", StatusFail, "the container was killed for exceeding its memory limit", "raise mem_limit in docker-compose.yml, or lower transfer.memoryBudgetBytes in config.yaml")
	case mem == 0:
		r.add("container-resources", StatusWarn, "no memory limit is set on the container", "set mem_limit (the shipped compose files use 1g)")
	default:
		r.add("container-resources", StatusPass, fmt.Sprintf("memory limit %d MiB, no OOM kill", mem>>20), "")
	}
}

// portInUse reports whether a TCP listen on host:port fails.
func portInUse(host string, port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return true
	}
	_ = l.Close()
	return false
}

func (r *diagRun) checkPorts() {
	type want struct {
		name string
		host string
		port int
	}
	wants := []want{{"REST", r.ports.Bind, r.ports.REST}, {"SAP", r.ports.Bind, r.ports.CS}, {"admin", "127.0.0.1", r.ports.Admin}}
	if r.running {
		var pub []string
		for _, c := range r.containers {
			pub = append(pub, c.Ports...)
		}
		joined := strings.Join(pub, " ")
		var missing []string
		for _, w := range wants {
			if r.cfgOK && ((w.name == "REST" && !r.cfg.Server.Enabled("rest")) || (w.name == "SAP" && !r.cfg.Server.Enabled("contentrepo"))) {
				continue
			}
			if !strings.Contains(joined, ":"+strconv.Itoa(w.port)+"->") {
				missing = append(missing, fmt.Sprintf("%s %d", w.name, w.port))
			}
		}
		if len(missing) > 0 {
			r.add("ports", StatusWarn, "the container does not publish: "+strings.Join(missing, ", "), "compare REST_PORT, CS_PORT and ADMIN_PORT in .env with docker-compose.yml")
			return
		}
		r.add("ports", StatusPass, "the container publishes REST, SAP and admin ports as configured", "")
		return
	}
	var busy []string
	for _, w := range wants {
		busyFn := r.a.PortBusy
		if busyFn == nil {
			busyFn = portInUse
		}
		if busyFn(w.host, w.port) {
			busy = append(busy, fmt.Sprintf("%s %s:%d", w.name, w.host, w.port))
		}
	}
	if len(busy) > 0 {
		r.add("ports", StatusFail, "already in use by another process: "+strings.Join(busy, ", "),
			"find it with: ss -ltnp | grep -E ':(8080|8090|9090)'  (Ubuntu's Cockpit uses 9090); or change REST_PORT, CS_PORT, ADMIN_PORT in .env")
		return
	}
	r.add("ports", StatusPass, "the host ports are free", "")
}

func (r *diagRun) checkHealth() {
	if !r.running {
		r.add("health-endpoint", StatusSkipped, "the container is not running", "")
		return
	}
	code, _, err := r.a.httpGet(r.ctx, r.a.readyURL(r.d, "/healthz"))
	switch {
	case err != nil:
		r.add("health-endpoint", StatusFail, fmt.Sprintf("no answer on 127.0.0.1:%d", r.ports.Admin), "run: zaks-drive logs --tail 50")
	case code != http.StatusOK:
		r.add("health-endpoint", StatusFail, fmt.Sprintf("/healthz answered HTTP %d", code), "run: zaks-drive logs --tail 50")
	default:
		r.add("health-endpoint", StatusPass, "/healthz answers", "")
	}
}

func (r *diagRun) checkReadiness() {
	if !r.running {
		r.add("readiness", StatusSkipped, "the container is not running", "")
		return
	}
	code, body, err := r.a.httpGet(r.ctx, r.a.readyURL(r.d, "/readyz"))
	switch {
	case err != nil:
		r.add("readiness", StatusFail, "the admin endpoint does not answer", "run: zaks-drive logs --tail 50")
	case code == http.StatusServiceUnavailable && strings.Contains(body, noRepositoryMarker):
		r.add("readiness", StatusWarn, "the server is running but no repository is configured yet", "add one: zaks-drive repo add AD --backend local --yes (or --backend sharepoint ...)")
	case code == http.StatusOK:
		r.add("readiness", StatusPass, "ready: every repository is reachable", "")
	default:
		cat, fix := classifyReadiness(body)
		r.add("readiness", StatusFail, cat+" ("+oneLine(body)+")", fix)
	}
}

// classifyReadiness turns the server's not-ready explanation into a failure category and a fix.
// It works on the text the server reports, so it needs no network access of its own.
func classifyReadiness(body string) (category, fix string) {
	b := strings.ToLower(body)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(b, strings.ToLower(s)) {
				return true
			}
		}
		return false
	}
	switch {
	case has("aadsts7000222", "client secret keys for this application have expired", "expired"):
		return "Microsoft Entra: the client secret has expired", "create a new client secret, update GRAPH_CLIENT_SECRET in .env, then: zaks-drive restart"
	case has("TOKEN_FAILED", "invalid_client", "aadsts7000215", "aadsts700016", "aadsts90002", "unauthorized_client"):
		return "Microsoft Entra rejected the credentials (secret, client ID or tenant ID)", "use the secret VALUE (not the secret ID) and check GRAPH_TENANT_ID and GRAPH_CLIENT_ID in .env; see docs/microsoft-365/entra-app-registration.md"
	case has("SP_UNAUTHORIZED", "NO application permissions", "token roles"):
		return "Microsoft Graph accepted the sign-in but the token carries no usable permissions", "add an Application permission and grant admin consent; see docs/microsoft-365/graph-permissions.md"
	case has("SP_ACCESS_DENIED", "accessdenied", "403"):
		return "SharePoint denied access (missing site grant or read-only grant)", "with Sites.Selected grant the app the write role on the site; see docs/microsoft-365/graph-permissions.md"
	case has("SP_SITE_NOT_FOUND", "SP_LIBRARY_NOT_FOUND", "SP_USER_DRIVE_NOT_FOUND", "CFG_SITE_URL_INVALID", "CFG_REPO_LOCATION_MISSING"):
		return "the configured SharePoint location cannot be found", "check SHAREPOINT_SITE_URL, SHAREPOINT_LIBRARY and SHAREPOINT_ROOT_PATH in .env; see docs/microsoft-365/sharepoint-configuration.md"
	case has("SP_UNREACHABLE", "SP_TIMEOUT", "no such host", "connection refused", "dial tcp", "i/o timeout"):
		return "the server cannot reach Microsoft (network, DNS or proxy)", "allow outbound HTTPS (443) to login.microsoftonline.com, graph.microsoft.com and *.sharepoint.com"
	case has("SP_THROTTLED", "429"):
		return "Microsoft is throttling requests (usually temporary)", "wait and retry; lower graph.maxConcurrency if it persists"
	}
	return "the server reports it is not ready", "run: zaks-drive logs --level warn --tail 50, and look the error code up in docs/error-codes.md"
}

func (r *diagRun) checkVersion() {
	if !r.running {
		r.add("server-version", StatusSkipped, "the container is not running", "")
		return
	}
	code, body, err := r.a.httpGet(r.ctx, r.a.readyURL(r.d, "/version"))
	if err != nil || code != http.StatusOK {
		r.add("server-version", StatusSkipped, "/version does not answer", "")
		return
	}
	level, note := CompatNote(r.a.Version, body)
	detail := fmt.Sprintf("server %s, command %s: %s", body, r.a.Version, note)
	if inst := r.d.loadState().Current; inst != "" && inst != body && !strings.HasPrefix(body, inst) {
		r.add("server-version", StatusWarn, detail+fmt.Sprintf("; the installed release is %s but the running server reports %s", inst, body), "run: zaks-drive restart (or zaks-drive update if the image is wrong)")
		return
	}
	if level == "warn" {
		r.add("server-version", StatusWarn, detail, "")
		return
	}
	r.add("server-version", StatusPass, detail, "")
}

func (r *diagRun) checkTLS() {
	if !r.cfgOK || r.cfg.Server.TLSCertFile == "" {
		r.add("tls-certificate", StatusSkipped, "ZAKS Drive does not terminate TLS here (use a reverse proxy for HTTPS)", "")
		return
	}
	rel := strings.TrimPrefix(r.cfg.Server.TLSCertFile, "/etc/adapter/tls/")
	keyRel := strings.TrimPrefix(r.cfg.Server.TLSKeyFile, "/etc/adapter/tls/")
	if rel == r.cfg.Server.TLSCertFile || keyRel == r.cfg.Server.TLSKeyFile || strings.Contains(rel, "..") || strings.Contains(keyRel, "..") {
		r.add("tls-certificate", StatusSkipped, "the certificate path is not under /etc/adapter/tls, so it cannot be mapped to a host file", "")
		return
	}
	certPath, keyPath := filepath.Join(r.d.Dir, "tls", rel), filepath.Join(r.d.Dir, "tls", keyRel)
	b, err := os.ReadFile(certPath)
	if err != nil {
		r.add("tls-certificate", StatusFail, "cannot read "+certPath, "put tls.crt and tls.key into "+filepath.Join(r.d.Dir, "tls"))
		return
	}
	block, _ := pem.Decode(b)
	if block == nil {
		r.add("tls-certificate", StatusFail, certPath+" is not a PEM certificate", "")
		return
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		r.add("tls-certificate", StatusFail, "the certificate cannot be parsed", "")
		return
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		r.add("tls-certificate", StatusFail, "the certificate and the private key do not match or the key is unreadable", "check that tls.key belongs to tls.crt")
		return
	}
	left := cert.NotAfter.Sub(r.a.Now())
	switch {
	case left <= 0:
		r.add("tls-certificate", StatusFail, "expired on "+cert.NotAfter.UTC().Format("2006-01-02"), "renew the certificate, then: zaks-drive restart")
	case left < 30*24*time.Hour:
		r.add("tls-certificate", StatusWarn, fmt.Sprintf("expires in %d days (%s)", int(left.Hours()/24), cert.NotAfter.UTC().Format("2006-01-02")), "renew the certificate")
	default:
		r.add("tls-certificate", StatusPass, fmt.Sprintf("valid until %s, key matches", cert.NotAfter.UTC().Format("2006-01-02")), "")
	}
}

var contRepLineRe = regexp.MustCompile(`contRep="([^"]*)"`)

func (r *diagRun) sapProbeURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/ContentServer/ContentServer.dll?serverInfo&pVersion=0047", r.ports.CS)
}

func (r *diagRun) checkSAPEndpoint() {
	if !r.cfgOK || !r.cfg.Server.Enabled("contentrepo") {
		r.add("sap-endpoint", StatusSkipped, "the SAP Content Server interface is not enabled", "")
		return
	}
	if !r.running {
		r.add("sap-endpoint", StatusSkipped, "the container is not running", "")
		return
	}
	code, body, hdr, err := r.a.httpDo(r.ctx, r.sapProbeURL(), nil)
	switch {
	case err != nil:
		r.add("sap-endpoint", StatusFail, fmt.Sprintf("no answer on port %d", r.ports.CS), "run: zaks-drive logs --iface contentrepo --tail 50")
	case code == http.StatusForbidden && strings.Contains(hdr.Get("X-ErrorCode"), "CS_CLIENT_NOT_ALLOWED"):
		r.add("sap-endpoint", StatusWarn, "the endpoint refuses this host because of contentServer.allowedNetworks, so it cannot be probed locally (expected when you restrict it to the SAP servers)",
			"test from an SAP application server instead: OAC0 connection test, or SAP report RSCMSTH0")
	case code != http.StatusOK:
		r.add("sap-endpoint", StatusFail, fmt.Sprintf("serverInfo answered HTTP %d", code), "run: zaks-drive logs --iface contentrepo --tail 50")
	default:
		listed := map[string]bool{}
		for _, m := range contRepLineRe.FindAllStringSubmatch(body, -1) {
			listed[m[1]] = true
		}
		var missing []string
		for _, c := range r.cfg.ContentServer.Repositories {
			if !listed[c.ContRep] {
				missing = append(missing, c.ContRep)
			}
		}
		if len(missing) > 0 {
			r.add("sap-endpoint", StatusFail, "serverInfo does not list: "+strings.Join(missing, ", "), "check contentServer.repositories in config.yaml and restart")
			return
		}
		r.add("sap-endpoint", StatusPass, fmt.Sprintf("serverInfo answers and lists %d configured repositories (SAP itself was not contacted)", len(r.cfg.ContentServer.Repositories)),
			"")
	}
}

type certInfo struct {
	ContRep     string    `json:"contRep"`
	AuthID      string    `json:"authId"`
	Active      bool      `json:"active"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"sha256Fingerprint"`
}

func (r *diagRun) checkSAPCertificates() {
	const sapHint = "In SAP: OAC0 > Send certificate; compare the fingerprint with STRUST (System PSE); then zaks-drive-server certs activate; verify with report RSCMSTH0. Local checks cannot see the SAP side."
	if !r.cfgOK || !r.cfg.Server.Enabled("contentrepo") {
		r.add("sap-certificates", StatusSkipped, "the SAP Content Server interface is not enabled", "")
		return
	}
	token := r.envMap["ADMIN_TOKEN"]
	if !r.running || token == "" {
		r.add("sap-certificates", StatusSkipped, "needs a running container and ADMIN_TOKEN in .env", "")
		return
	}
	u := fmt.Sprintf("http://127.0.0.1:%d/admin/contentserver/certificates", r.ports.Admin)
	code, body, _, err := r.a.httpDo(r.ctx, u, map[string]string{"Authorization": "Bearer " + token})
	switch {
	case err != nil:
		r.add("sap-certificates", StatusSkipped, "the admin endpoint does not answer", "")
		return
	case code == http.StatusNotFound || code == http.StatusServiceUnavailable:
		r.add("sap-certificates", StatusWarn, "the SAP interface is not ready yet", "retry in a minute")
		return
	case code == http.StatusUnauthorized:
		r.add("sap-certificates", StatusWarn, "the admin endpoint refused ADMIN_TOKEN from .env (was the container started with an older .env?)", "run: zaks-drive restart")
		return
	case code != http.StatusOK:
		r.add("sap-certificates", StatusWarn, fmt.Sprintf("the admin endpoint answered HTTP %d", code), "")
		return
	}
	var out struct {
		Value []certInfo `json:"value"`
	}
	if json.Unmarshal([]byte(body), &out) != nil {
		r.add("sap-certificates", StatusSkipped, "the certificate list could not be read", "")
		return
	}
	active := map[string]int{}
	inactive := map[string]int{}
	var expired []string
	for _, c := range out.Value {
		if c.Active {
			active[c.ContRep]++
			if !c.NotAfter.IsZero() && c.NotAfter.Before(r.a.Now()) {
				expired = append(expired, c.ContRep+" "+shortFP(c.Fingerprint))
			}
		} else {
			inactive[c.ContRep]++
		}
	}
	var notes []string
	status := StatusPass
	for _, c := range r.cfg.ContentServer.Repositories {
		if c.Signature == "none" {
			continue
		}
		switch {
		case active[c.ContRep] > 0:
			notes = append(notes, fmt.Sprintf("%s: %d active", c.ContRep, active[c.ContRep]))
		case inactive[c.ContRep] > 0:
			status = StatusWarn
			notes = append(notes, fmt.Sprintf("%s: %d received but NOT activated", c.ContRep, inactive[c.ContRep]))
		default:
			status = StatusWarn
			notes = append(notes, fmt.Sprintf("%s: no certificate received yet (expected until SAP sends it)", c.ContRep))
		}
	}
	if len(expired) > 0 {
		r.add("sap-certificates", StatusFail, "an active certificate has expired: "+strings.Join(expired, ", "), sapHint)
		return
	}
	if len(notes) == 0 {
		r.add("sap-certificates", StatusPass, "no repository checks signatures (signature none)", "")
		return
	}
	fix := ""
	if status != StatusPass {
		fix = sapHint
	}
	r.add("sap-certificates", status, strings.Join(notes, "; "), fix)
}

func shortFP(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

func (r *diagRun) checkDisk() {
	free, err := r.a.freeBytes(r.d.Dir)
	if err != nil {
		r.add("disk-space", StatusSkipped, "free space cannot be determined on this platform", "")
		return
	}
	mib := free >> 20
	switch {
	case free < 200<<20:
		r.add("disk-space", StatusFail, fmt.Sprintf("%d MiB free where %s lives", mib, r.d.Dir), "free disk space (uploads of unknown size spool to the spool volume)")
	case free < 2<<30:
		r.add("disk-space", StatusWarn, fmt.Sprintf("%d MiB free where %s lives", mib, r.d.Dir), "free disk space")
	default:
		r.add("disk-space", StatusPass, fmt.Sprintf("%d MiB free where %s lives (Docker's own data directory is not checked)", mib, r.d.Dir), "")
	}
}

func (r *diagRun) checkConnectivity() {
	if !r.opts.Connectivity {
		r.add("graph-connectivity", StatusSkipped, "not requested: this test contacts Microsoft and writes small test files to SharePoint", "run: zaks-drive doctor --connectivity (or zaks-drive check)")
		return
	}
	if !r.running {
		r.add("graph-connectivity", StatusSkipped, "the container is not running", "run: zaks-drive start")
		return
	}
	res, err := r.a.execServerCheck(r.ctx, r.d, nil)
	if err != nil {
		r.add("graph-connectivity", StatusFail, err.Error(), "")
		return
	}
	summary := lastMeaningfulLines(r.san.Text(res.Stdout+res.Stderr), 3)
	if res.ExitCode == 0 {
		r.add("graph-connectivity", StatusPass, summary, "")
		return
	}
	r.add("graph-connectivity", StatusFail, summary, "run: zaks-drive check   (shows every step with its fix)")
}

func lastMeaningfulLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "=") && !strings.HasPrefix(t, "-") {
			keep = append(keep, t)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, " | ")
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (r *diagRun) checkRepositories() {
	if !r.dockerOK {
		r.add("repositories", StatusSkipped, "Docker is not usable (see docker-daemon)", "")
		return
	}
	ov := r.overview()
	if ov == nil {
		r.add("repositories", StatusSkipped, "the server is not running or its admin API is not usable (ADMIN_TOKEN in .env); use: zaks-drive repo list", "")
		return
	}
	serving := map[string]bool{}
	for _, id := range ov.Status.Serving {
		serving[id] = true
	}
	var notServed []string
	for _, v := range ov.Views {
		if !serving[v.ID] {
			notServed = append(notServed, v.ID)
		}
	}
	switch {
	case ov.Status.Fallback != "":
		r.add("repositories", StatusFail, "the server runs the last working repository state because the current one is invalid: "+oneLine(ov.Status.Fallback), "run: zaks-drive repo list, correct the repository named, then restart")
	case ov.Status.PendingRestart:
		r.add("repositories", StatusWarn, "repository changes are waiting for a restart", "apply them with: zaks-drive restart")
	case len(notServed) > 0:
		r.add("repositories", StatusWarn, "defined but not served: "+strings.Join(notServed, ", "), "restart: zaks-drive restart")
	default:
		r.add("repositories", StatusPass, fmt.Sprintf("%d %s, all served", len(ov.Views), plural(len(ov.Views), "repository", "repositories")), "")
	}
}

// overview asks the server's admin API once per run.
func (r *diagRun) overview() *repoOverview {
	if !r.ovDone {
		r.ov, r.ovDone = r.a.repoOverview(r.ctx, r.d), true
	}
	return r.ov
}

func (r *diagRun) checkAdminPassword() {
	ov := r.overview()
	switch {
	case ov == nil:
		r.add("admin-password", StatusSkipped, "the server is not running or its admin API is not usable (ADMIN_TOKEN in .env)", "")
	case ov.Status.AdminPasswordDefault:
		r.add("admin-password", StatusWarn, "the admin web page still accepts the default login admin / admin", "open the admin page and choose a password at the first login, or turn the page off with server.adminUI: false")
	default:
		r.add("admin-password", StatusPass, "the default admin password has been changed", "")
	}
}
