package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/repoadmin"
)

// The repo commands manage the repositories of a running or stopped
// deployment. They never edit files themselves: when the server runs they call
// its admin API (localhost, ADMIN_TOKEN from .env); when it does not, they run
// the same operations in a one-off container of the same image and volume, so
// the files are always created by the server's own user and validated by the
// server's own loader. See docs/rfcs/0001-repositories-and-storage-backends.md.

// remoteError is a coded error reported by the server.
type remoteError struct {
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
	More   string `json:"more"`
}

func (e *remoteError) Error() string {
	if e.Detail != "" {
		return e.Code + ": " + e.Detail
	}
	return e.Code + ": " + e.Title
}

// repoStatus mirrors the server's /admin/api/v1/status.
type repoStatus struct {
	Version        string    `json:"version"`
	Ready          bool      `json:"ready"`
	PendingRestart bool      `json:"pendingRestart"`
	Fallback       string    `json:"fallback"`
	Draining       bool      `json:"draining"`
	InFlight       int       `json:"inFlight"`
	DrainSeconds   int       `json:"drainSeconds"`
	StartedAt      time.Time `json:"startedAt"`
	Serving        []string  `json:"serving"`
}

type repoBody struct {
	repoadmin.Spec
	ClientSecret *string `json:"clientSecret,omitempty"`
}

// repoClient talks to the server of one deployment.
type repoClient struct {
	a       *App
	d       *Deployment
	token   string
	running bool
}

func (a *App) newRepoClient(ctx context.Context, d *Deployment) *repoClient {
	c := &repoClient{a: a, d: d}
	if env, err := ReadEnv(d.EnvFile); err == nil {
		c.token = env["ADMIN_TOKEN"]
	}
	if cs, err := a.managed(ctx, d); err == nil {
		for _, ct := range cs {
			c.running = c.running || ct.State == "running"
		}
	}
	return c
}

// viaAPI reports whether calls go to the running server.
func (c *repoClient) viaAPI() bool { return c.running && c.token != "" }

var errAPIUnreachable = errors.New("the admin API does not answer")

// api calls the admin API. A transport failure is errAPIUnreachable so the
// caller can fall back to a one-off container; an HTTP error with a body is
// a *remoteError.
func (c *repoClient) api(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.a.readyURL(c.d, "/admin/api/v1"+path), rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.a.HTTP.Do(req)
	if err != nil {
		return nil, errAPIUnreachable
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		var e struct {
			Error remoteError `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Code != "" {
			return nil, &e.Error
		}
		return nil, &remoteError{Code: "HTTP_" + fmt.Sprint(resp.StatusCode), Title: http.StatusText(resp.StatusCode)}
	}
	return b, nil
}

// oneOff runs `zaks-drive-server repos <op> ...` in a one-off container. The
// JSON body (which may hold a secret) goes through standard input.
func (c *repoClient) oneOff(ctx context.Context, body any, op string, args ...string) ([]byte, error) {
	var stdin io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		stdin = bytes.NewReader(b)
	}
	sub := append([]string{"run", "--rm", "-T", "--no-deps", "adapter", "repos", op}, args...)
	res, err := c.a.compose(ctx, c.d, RunOpts{Stdin: stdin}, sub...)
	if err != nil {
		return nil, err
	}
	var out struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  remoteError     `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out); err != nil {
		return nil, fmt.Errorf("the one-off container did not answer as expected (exit %d): %s", res.ExitCode, oneLine(trimLines(c.a.sanitizerFor(c.d).Text(res.Stderr+res.Stdout), 3)))
	}
	if !out.OK {
		return nil, &out.Error
	}
	return out.Result, nil
}

// repoOp is one operation; the transport is chosen by call.
type repoOp struct {
	name       string // list, get, add, update, detach, check, detached, audit, purge-plan, purge
	id         string
	body       any
	allowInUse bool
	lines      int
	extra      []string
}

// apiOnly is true for the operations that must never run through the admin
// API: a purge exists only on the host.
func (o repoOp) oneOffOnly() bool { return o.name == "purge-plan" || o.name == "purge" }

// call runs an operation through the best transport and returns the result JSON.
func (c *repoClient) call(ctx context.Context, op repoOp) ([]byte, error) {
	if c.viaAPI() && !op.oneOffOnly() {
		var raw []byte
		var err error
		switch op.name {
		case "list":
			raw, err = c.api(ctx, "GET", "/repositories", nil)
		case "get":
			raw, err = c.api(ctx, "GET", "/repositories/"+op.id, nil)
		case "add":
			raw, err = c.api(ctx, "POST", "/repositories", op.body)
		case "update":
			raw, err = c.api(ctx, "PUT", "/repositories/"+op.id, op.body)
		case "detach":
			raw, err = c.api(ctx, "DELETE", "/repositories/"+op.id+fmt.Sprintf("?allowInUse=%v", op.allowInUse), nil)
		case "check":
			raw, err = c.api(ctx, "POST", "/repositories/"+op.id+"/check", nil)
		case "detached":
			raw, err = c.api(ctx, "GET", "/detached", nil)
		case "audit":
			raw, err = c.api(ctx, "GET", fmt.Sprintf("/audit?lines=%d", op.lines), nil)
			if err == nil { // the API streams lines; the one-off wraps them in a string
				b, _ := json.Marshal(string(raw))
				raw = b
			}
		}
		if !errors.Is(err, errAPIUnreachable) {
			return raw, err
		}
		// The server runs but its API does not answer: the files can still be changed.
	}
	var args []string
	if op.id != "" {
		args = append(args, op.id)
	}
	if op.allowInUse {
		args = append(args, "--allow-in-use")
	}
	if op.name == "audit" {
		args = append(args, fmt.Sprint(op.lines))
	}
	args = append(args, op.extra...)
	return c.oneOff(ctx, op.body, op.name, args...)
}

// status returns the server's status, or an error when the API is not usable.
func (c *repoClient) status(ctx context.Context) (repoStatus, error) {
	var st repoStatus
	if !c.viaAPI() {
		return st, errAPIUnreachable
	}
	b, err := c.api(ctx, "GET", "/status", nil)
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// ---- command ----

func (a *App) cmdRepo(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.repoUsage(a.Err)
		return ExitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return a.repoList(ctx, rest)
	case "show":
		return a.repoShow(ctx, rest)
	case "add":
		return a.repoAddUpdate(ctx, rest, true)
	case "update":
		return a.repoAddUpdate(ctx, rest, false)
	case "remove", "detach":
		return a.repoRemove(ctx, rest)
	case "check":
		return a.repoCheck(ctx, rest)
	case "purge":
		return a.repoPurge(ctx, rest)
	case "help", "-h", "--help":
		a.repoUsage(a.Out)
		return ExitOK
	}
	fmt.Fprintf(a.Err, "zaks-drive repo: unknown command %q\n\n", sub)
	a.repoUsage(a.Err)
	return ExitUsage
}

func (a *App) repoUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: zaks-drive repo <command> [options]

Commands:
  list [--json]                  repositories, their backend and state
  show <id> [--json]             the settings of one repository (secrets shown as set or not set)
  add <id> --backend local|sharepoint ...
                                 create a repository
  update <id> ...                change settings
  remove <id>                    detach a repository: it is no longer served; documents are NOT deleted
  check <id>                     test the access of one repository
  purge <id> --plan | --execute  delete the documents of a detached local repository (deliberately hard)

Common options: --dir DIR, --yes, --no-restart, --json
Add and update:
  --description TEXT  --read-only
  local:        --path FOLDER (default: the id)  --versions N (0-99, default 5)
  SharePoint:   --site-url URL  --library NAME  --root-path PATH  --user-id UPN  --drive-id ID  --site-id ID
  own Microsoft application (optional): --tenant-id GUID  --client-id GUID
                and the secret from --secret-file FILE, --secret-stdin or a hidden prompt (never a flag value)
  SAP:          --sap  --cont-rep ID  --signature required|optional|none
Changes are applied by a restart that first drains running requests (--no-restart leaves them pending).
`)
}

// prepareRepo finds the deployment and returns a client.
func (a *App) prepareRepo(ctx context.Context, dir string) (*Deployment, *repoClient, int) {
	d, code := a.prepare(ctx, dir)
	if code != ExitOK {
		return nil, nil, code
	}
	return d, a.newRepoClient(ctx, d), ExitOK
}

// failRemote prints a coded server error (sanitized) and returns ExitFail.
func (a *App) failRemote(d *Deployment, err error) int {
	san := a.sanitizerFor(d)
	var re *remoteError
	if errors.As(err, &re) {
		fmt.Fprintf(a.Err, "zaks-drive: %s\n", san.Text(re.Error()))
		if re.Fix != "" {
			fmt.Fprintf(a.Err, "  fix: %s\n", san.Text(re.Fix))
		}
		if re.More != "" {
			fmt.Fprintf(a.Err, "  more: %s\n", re.More)
		}
		return ExitFail
	}
	return a.fail(ExitFail, san.Text(err.Error()), "run: zaks-drive doctor")
}

type listResult struct {
	Value   []repoadmin.View `json:"value"`
	Problem *remoteError     `json:"problem"`
}

func (a *App) repoList(ctx context.Context, args []string) int {
	fset := flag.NewFlagSet("repo list", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	asJSON := fset.Bool("json", false, "machine-readable output")
	if err := fset.Parse(args); err != nil {
		return ExitUsage
	}
	d, c, code := a.prepareRepo(ctx, *dir)
	if code != ExitOK {
		return code
	}
	raw, err := c.call(ctx, repoOp{name: "list"})
	if err != nil {
		return a.failRemote(d, err)
	}
	var lr listResult
	if err := json.Unmarshal(raw, &lr); err != nil {
		return a.fail(ExitFail, "the answer of the server could not be read", "")
	}
	var detached []repoadmin.Spec
	if raw, err := c.call(ctx, repoOp{name: "detached"}); err == nil {
		var dr struct {
			Value []repoadmin.Spec `json:"value"`
		}
		if json.Unmarshal(raw, &dr) == nil {
			detached = dr.Value
		}
	}
	st, stErr := c.status(ctx)
	if *asJSON {
		out := map[string]any{"repositories": lr.Value, "detached": detached}
		if stErr == nil {
			out["server"] = st
		}
		if lr.Problem != nil {
			out["problem"] = lr.Problem
		}
		b, _ := marshalJSON(out)
		a.printf("%s\n", a.sanitizerFor(d).Text(string(b)))
		return ExitOK
	}
	a.printRepoTable(d, lr.Value, detached, st, stErr == nil)
	if lr.Problem != nil {
		fmt.Fprintf(a.Err, "\nThe repository files do not validate as a whole:\n  %s\n", a.sanitizerFor(d).Text(oneLine(lr.Problem.Detail)))
	}
	if stErr == nil && st.PendingRestart {
		a.printf("\nChanges are pending a restart. Apply them with: zaks-drive restart (drains running requests first only through `repo add/update/remove`)\n")
	}
	if stErr == nil && st.Fallback != "" {
		a.printf("\nWARNING: the server started with the LAST WORKING repository state because the current one is invalid:\n  %s\n",
			a.sanitizerFor(d).Text(oneLine(st.Fallback)))
	}
	return ExitOK
}

func (a *App) printRepoTable(d *Deployment, views []repoadmin.View, detached []repoadmin.Spec, st repoStatus, haveStatus bool) {
	san := a.sanitizerFor(d)
	if len(views) == 0 {
		a.printf("No repositories yet. Create one with: zaks-drive repo add <id> --backend local|sharepoint\n")
	} else {
		a.printf("%-4s %-11s %-8s %-8s %-8s %s\n", "ID", "BACKEND", "SOURCE", "SAP", "STATE", "LOCATION")
		serving := map[string]bool{}
		for _, id := range st.Serving {
			serving[id] = true
		}
		for _, v := range views {
			sap := "-"
			if v.SAP != nil {
				sap = firstNonEmptyString(v.SAP.ContRep, v.ID)
			}
			state := "unknown"
			if haveStatus {
				switch {
				case serving[v.ID]:
					state = "serving"
				case !st.Ready && len(st.Serving) == 0:
					state = "starting"
				default:
					state = "pending"
				}
			}
			a.printf("%-4s %-11s %-8s %-8s %-8s %s\n", v.ID, v.Backend, v.Source, sap, state, san.Line(repoLocation(v.Spec)))
		}
	}
	if len(detached) > 0 {
		a.printf("\nDetached (documents are kept; add them again to serve them, purge to delete):\n")
		for _, s := range detached {
			a.printf("  %-4s %s\n", s.ID, san.Line(repoLocation(s)))
		}
	}
}

func firstNonEmptyString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// repoLocation is a one-line description of where a repository keeps its documents.
func repoLocation(s repoadmin.Spec) string {
	if s.Backend == config.BackendLocal {
		p, n := "", config.DefaultLocalVersions
		if s.Local != nil {
			p = s.Local.Path
			if s.Local.Versions != nil {
				n = *s.Local.Versions
			}
		}
		return fmt.Sprintf("local folder %s (keeps %d versions)", p, n)
	}
	loc := s.SiteURL
	switch {
	case s.DriveID != "":
		loc = "drive " + s.DriveID
	case s.UserID != "":
		loc = "OneDrive of " + s.UserID
	case s.SiteID != "":
		loc = "site " + s.SiteID
	}
	if s.DriveName != "" {
		loc += " / " + s.DriveName
	}
	if s.RootPath != "" {
		loc += " / " + s.RootPath
	}
	if s.TenantID != "" {
		loc += " (own application)"
	}
	return loc
}

func (a *App) repoShow(ctx context.Context, args []string) int {
	id, args := splitID(args)
	fset := flag.NewFlagSet("repo show", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	asJSON := fset.Bool("json", false, "machine-readable output")
	if err := fset.Parse(args); err != nil {
		return ExitUsage
	}
	if id == "" {
		id = fset.Arg(0)
	}
	if id == "" {
		fmt.Fprintln(a.Err, "usage: zaks-drive repo show <id> [--json]")
		return ExitUsage
	}
	d, c, code := a.prepareRepo(ctx, *dir)
	if code != ExitOK {
		return code
	}
	raw, err := c.call(ctx, repoOp{name: "get", id: id})
	if err != nil {
		return a.failRemote(d, err)
	}
	var v repoadmin.View
	if err := json.Unmarshal(raw, &v); err != nil {
		return a.fail(ExitFail, "the answer of the server could not be read", "")
	}
	san := a.sanitizerFor(d)
	if *asJSON {
		b, _ := marshalJSON(v)
		a.printf("%s\n", san.Text(string(b)))
		return ExitOK
	}
	for _, l := range describeRepo(v.Spec, v.SecretSet, v.Source) {
		a.printf("%s\n", san.Line(l))
	}
	return ExitOK
}

// describeRepo lists the settings of a repository, secrets only as set or not set.
func describeRepo(s repoadmin.Spec, secretSet bool, source string) []string {
	var l []string
	add := func(k, v string) {
		if v != "" {
			l = append(l, fmt.Sprintf("%-14s %s", k, v))
		}
	}
	add("Repository", s.ID)
	add("Description", s.Description)
	add("Backend", s.Backend)
	add("Source", source)
	if s.ReadOnly {
		add("Access", "read-only")
	}
	if s.Backend == config.BackendLocal {
		add("Location", repoLocation(s))
	} else {
		add("Site URL", s.SiteURL)
		add("OneDrive user", s.UserID)
		add("Site ID", s.SiteID)
		add("Drive ID", s.DriveID)
		add("Library", s.DriveName)
		add("Folder", s.RootPath)
		if s.TenantID != "" {
			add("Application", "own: tenant "+s.TenantID+", client "+s.ClientID)
			if secretSet {
				add("Client secret", "set")
			} else {
				add("Client secret", "NOT set")
			}
		} else {
			add("Application", "the global graph.* settings")
		}
	}
	if s.SAP != nil {
		sig := s.SAP.Signature
		if sig == "" {
			sig = "required"
		}
		add("SAP", fmt.Sprintf("content repository %s, signature %s", firstNonEmptyString(s.SAP.ContRep, s.ID), sig))
	} else {
		add("SAP", "not exposed (REST only)")
	}
	return l
}

// splitID takes a leading positional argument (the repository ID).
func splitID(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// ---- add and update ----

type repoFlags struct {
	dir, backend, description         string
	readOnly                          bool
	path                              string
	versions                          int
	siteURL, library, rootPath        string
	userID, driveID, siteID           string
	tenantID, clientID                string
	secretFile                        string
	secretStdin, askSecret, globalApp bool
	sap, noSAP                        bool
	contRep, signature                string
	yes, noRestart                    bool
}

func (f *repoFlags) bind(fset *flag.FlagSet, update bool) {
	fset.StringVar(&f.dir, "dir", "", "deployment directory")
	fset.StringVar(&f.backend, "backend", "", "local or sharepoint")
	fset.StringVar(&f.description, "description", "", "free text")
	fset.BoolVar(&f.readOnly, "read-only", false, "refuse writes")
	fset.StringVar(&f.path, "path", "", "local folder below the data directory (default: the id)")
	fset.IntVar(&f.versions, "versions", config.DefaultLocalVersions, "previous versions kept per file, 0 to 99")
	fset.StringVar(&f.siteURL, "site-url", "", "SharePoint site address")
	fset.StringVar(&f.library, "library", "", "document library name")
	fset.StringVar(&f.rootPath, "root-path", "", "folder inside the library")
	fset.StringVar(&f.userID, "user-id", "", "user principal name whose OneDrive is used")
	fset.StringVar(&f.driveID, "drive-id", "", "drive ID")
	fset.StringVar(&f.siteID, "site-id", "", "site ID")
	fset.StringVar(&f.tenantID, "tenant-id", "", "own Entra tenant ID")
	fset.StringVar(&f.clientID, "client-id", "", "own Entra application (client) ID")
	fset.StringVar(&f.secretFile, "secret-file", "", "file holding the client secret")
	fset.BoolVar(&f.secretStdin, "secret-stdin", false, "read the client secret from standard input")
	fset.BoolVar(&f.askSecret, "ask-secret", false, "prompt for the client secret (hidden)")
	fset.BoolVar(&f.sap, "sap", false, "expose the repository to SAP")
	fset.StringVar(&f.contRep, "cont-rep", "", "SAP content repository ID (default: the id)")
	fset.StringVar(&f.signature, "signature", "", "required, optional or none")
	fset.BoolVar(&f.yes, "yes", false, "do not ask for confirmation")
	fset.BoolVar(&f.noRestart, "no-restart", false, "leave the change pending instead of restarting")
	if update {
		fset.BoolVar(&f.globalApp, "global-app", false, "return to the global graph.* application")
		fset.BoolVar(&f.noSAP, "no-sap", false, "stop exposing the repository to SAP")
	}
}

// readSecret gets the client secret from a file, standard input or a hidden
// prompt. It returns nil when none was asked for.
func (a *App) readSecret(f *repoFlags, haveExisting bool) (*string, error) {
	switch {
	case f.secretFile != "":
		b, err := os.ReadFile(f.secretFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read the secret file: %w", err)
		}
		s := strings.TrimSpace(string(b))
		return &s, nil
	case f.secretStdin:
		line, err := a.reader().ReadString('\n')
		if err != nil && line == "" {
			return nil, errors.New("no secret on standard input")
		}
		s := strings.TrimSpace(line)
		return &s, nil
	case f.askSecret:
		s, err := a.askSecret("Client secret", haveExisting)
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		return &s, nil
	}
	return nil, nil
}

func (a *App) repoAddUpdate(ctx context.Context, args []string, create bool) int {
	name := "update"
	if create {
		name = "add"
	}
	id, args := splitID(args)
	var f repoFlags
	fset := flag.NewFlagSet("repo "+name, flag.ContinueOnError)
	fset.SetOutput(a.Err)
	f.bind(fset, !create)
	if err := fset.Parse(args); err != nil {
		return ExitUsage
	}
	if id == "" {
		id = fset.Arg(0)
	}
	if id == "" {
		fmt.Fprintf(a.Err, "usage: zaks-drive repo %s <id> [options]  (see: zaks-drive repo help)\n", name)
		return ExitUsage
	}
	if !config.ValidManagedID(id) {
		return a.fail(ExitUsage, fmt.Sprintf("the repository ID %q must be exactly 2 characters, A-Z or 0-9 (the length of an SAP content repository ID)", id), "for example: ZH or A1")
	}
	set := map[string]bool{}
	fset.Visit(func(fl *flag.Flag) { set[fl.Name] = true })

	d, c, code := a.prepareRepo(ctx, f.dir)
	if code != ExitOK {
		return code
	}

	var spec repoadmin.Spec
	var current *repoadmin.View
	if create {
		spec = repoadmin.Spec{ID: id}
	} else {
		raw, err := c.call(ctx, repoOp{name: "get", id: id})
		if err != nil {
			return a.failRemote(d, err)
		}
		var v repoadmin.View
		if err := json.Unmarshal(raw, &v); err != nil {
			return a.fail(ExitFail, "the answer of the server could not be read", "")
		}
		if v.Source != config.SourceManaged {
			return a.fail(ExitFail, fmt.Sprintf("repository %s is defined in config.yaml and cannot be changed with this command", id), "edit config.yaml and run: zaks-drive restart")
		}
		spec, current = v.Spec, &v
	}
	if msg := applyRepoFlags(&spec, &f, set, create); msg != "" {
		return a.fail(ExitUsage, msg, "see: zaks-drive repo help")
	}

	// The secret only when asked for; a repository with its own application needs one when created.
	secret, err := a.readSecret(&f, current != nil && current.SecretSet)
	if err != nil {
		return a.fail(ExitFail, err.Error(), "")
	}
	own := spec.TenantID != "" || spec.ClientID != ""
	if secret == nil && own && (current == nil || !current.SecretSet) {
		if a.Interactive != nil && a.Interactive() {
			if secret, err = a.readSecret(&repoFlags{askSecret: true}, false); err != nil {
				return a.fail(ExitFail, err.Error(), "")
			}
		} else {
			return a.fail(ExitUsage, "a repository with its own Microsoft application needs its client secret", "pass --secret-file FILE or --secret-stdin (a secret is never accepted as an argument)")
		}
	}

	// Summary without secrets, then confirmation.
	san := a.sanitizerFor(d)
	a.printf("\nThis will %s the repository:\n", map[bool]string{true: "create", false: "change"}[create])
	for _, l := range describeRepo(spec, secret != nil || (current != nil && current.SecretSet), config.SourceManaged) {
		a.printf("  %s\n", san.Line(l))
	}
	if secret != nil {
		a.printf("  %-14s %s\n", "Secret", "will be stored in the server's state volume (mode 600), never in the definition")
	}
	a.printf("\n%s\n", changeNote(c, f.noRestart))
	if !a.confirm(ctx, "Continue?", f.yes) {
		a.printf("Nothing was changed.\n")
		return ExitDeclined
	}

	body := repoBody{Spec: spec, ClientSecret: secret}
	op := repoOp{name: "add", id: id, body: body}
	if !create {
		op.name = "update"
	}
	raw, err := c.call(ctx, op)
	if err != nil {
		return a.failRemote(d, err)
	}
	var v repoadmin.View
	_ = json.Unmarshal(raw, &v)
	a.printf("Saved repository %s.\n", id)
	return a.applyChanges(ctx, d, c, f.noRestart)
}

// changeNote tells what happens after the change.
func changeNote(c *repoClient, noRestart bool) string {
	switch {
	case noRestart:
		return "The change will be saved and stay pending until the server is restarted (--no-restart)."
	case !c.running:
		return "ZAKS Drive is not running: the change takes effect when you start it."
	case c.token == "":
		return "ADMIN_TOKEN is not set in .env, so the change is saved but cannot be applied with a drained restart; restart with: zaks-drive restart."
	}
	return "Applying restarts ZAKS Drive: new requests get 503 with Retry-After while running ones finish (SAP retries), then it starts again."
}

// applyRepoFlags merges the flags that were set into spec; it returns a
// message for an unusable combination.
func applyRepoFlags(spec *repoadmin.Spec, f *repoFlags, set map[string]bool, create bool) string {
	if set["backend"] {
		spec.Backend = f.backend
	}
	if spec.Backend == "" {
		return "--backend local|sharepoint is required"
	}
	if spec.Backend != config.BackendLocal && spec.Backend != config.BackendSharePoint {
		return fmt.Sprintf("--backend %q is not known; use local or sharepoint", spec.Backend)
	}
	if set["description"] {
		spec.Description = f.description
	}
	if set["read-only"] {
		spec.ReadOnly = f.readOnly
	}
	switch spec.Backend {
	case config.BackendLocal:
		for _, n := range []string{"site-url", "library", "root-path", "user-id", "drive-id", "site-id", "tenant-id", "client-id", "global-app"} {
			if set[n] {
				return "--" + n + " does not apply to a local repository"
			}
		}
		if spec.Local == nil {
			spec.Local = &repoadmin.LocalSpec{}
		}
		if set["path"] {
			spec.Local.Path = f.path
		}
		if spec.Local.Path == "" {
			spec.Local.Path = spec.ID
		}
		if set["versions"] {
			v := f.versions
			spec.Local.Versions = &v
		}
	default:
		for _, n := range []string{"path", "versions"} {
			if set[n] {
				return "--" + n + " applies to a local repository only"
			}
		}
		spec.Local = nil
		if set["site-url"] {
			spec.SiteURL = f.siteURL
		}
		if set["library"] {
			spec.DriveName = f.library
		}
		if set["root-path"] {
			spec.RootPath = f.rootPath
		}
		if set["user-id"] {
			spec.UserID = f.userID
		}
		if set["drive-id"] {
			spec.DriveID = f.driveID
		}
		if set["site-id"] {
			spec.SiteID = f.siteID
		}
		if set["tenant-id"] {
			spec.TenantID = f.tenantID
		}
		if set["client-id"] {
			spec.ClientID = f.clientID
		}
		if set["global-app"] && f.globalApp {
			spec.TenantID, spec.ClientID = "", ""
		}
		if spec.SiteURL == "" && spec.UserID == "" && spec.DriveID == "" && spec.SiteID == "" {
			return "a SharePoint repository needs --site-url (or --user-id, --drive-id or --site-id)"
		}
		if (spec.TenantID == "") != (spec.ClientID == "") {
			return "--tenant-id and --client-id go together (own Microsoft application)"
		}
	}
	// SAP
	if set["sap"] || set["cont-rep"] || set["signature"] {
		if spec.SAP == nil {
			spec.SAP = &repoadmin.SAPSpec{}
		}
		if set["cont-rep"] {
			spec.SAP.ContRep = f.contRep
		}
		if set["signature"] {
			spec.SAP.Signature = f.signature
		}
	}
	if set["no-sap"] && f.noSAP {
		spec.SAP = nil
	}
	return ""
}

// applyChanges restarts the server through its maintenance drain so the
// pending change takes effect, and waits until the new start is ready.
func (a *App) applyChanges(ctx context.Context, d *Deployment, c *repoClient, noRestart bool) int {
	switch {
	case noRestart:
		a.printf("The change is pending. Apply it with: zaks-drive restart\n")
		return ExitOK
	case !c.running:
		a.printf("ZAKS Drive is not running; the change takes effect when you start it: zaks-drive start\n")
		return ExitOK
	case c.token == "":
		a.printf("The change is saved. ADMIN_TOKEN is not set in .env, so it cannot be applied with a drained restart. Restart with: zaks-drive restart\n")
		return ExitOK
	}
	before, err := c.status(ctx)
	if err != nil {
		return a.failRemote(d, err)
	}
	if _, err := c.api(ctx, "POST", "/apply", map[string]any{}); err != nil {
		return a.failRemote(d, err)
	}
	a.printf("Restarting to apply the change: refusing new requests, waiting up to %d s for %d running %s...\n",
		before.DrainSeconds, before.InFlight, plural(before.InFlight, "request", "requests"))
	deadline := a.Now().Add(5 * time.Minute)
	san := a.sanitizerFor(d)
	for {
		select {
		case <-ctx.Done():
			return a.fail(ExitUnhealthy, "interrupted while waiting for the restart", "check: zaks-drive status")
		case <-time.After(a.pollInterval()):
		}
		st, err := c.status(ctx)
		if err == nil && !st.StartedAt.Equal(before.StartedAt) && st.Ready {
			if st.Fallback != "" {
				fmt.Fprintf(a.Err, "zaks-drive: the server restarted, but the NEW SETTINGS WERE NOT APPLIED. It serves the last working state because:\n  %s\n",
					san.Text(oneLine(st.Fallback)))
				return ExitFail
			}
			a.printf("Applied. Serving: %s\n", strings.Join(st.Serving, ", "))
			return ExitOK
		}
		if !a.Now().Before(deadline) {
			return a.fail(ExitUnhealthy, "the server did not become ready again within 5 minutes", "see: zaks-drive logs, zaks-drive doctor")
		}
	}
}

// ---- remove ----

func (a *App) repoRemove(ctx context.Context, args []string) int {
	id, args := splitID(args)
	fset := flag.NewFlagSet("repo remove", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	allow := fset.Bool("allow-in-use", false, "detach a repository that is exposed to SAP")
	yes := fset.Bool("yes", false, "do not ask for confirmation")
	noRestart := fset.Bool("no-restart", false, "leave the change pending instead of restarting")
	if err := fset.Parse(args); err != nil {
		return ExitUsage
	}
	if id == "" {
		id = fset.Arg(0)
	}
	if id == "" {
		fmt.Fprintln(a.Err, "usage: zaks-drive repo remove <id> [--allow-in-use] [--yes] [--no-restart]")
		return ExitUsage
	}
	d, c, code := a.prepareRepo(ctx, *dir)
	if code != ExitOK {
		return code
	}
	raw, err := c.call(ctx, repoOp{name: "get", id: id})
	if err != nil {
		return a.failRemote(d, err)
	}
	var v repoadmin.View
	_ = json.Unmarshal(raw, &v)
	if v.Source != config.SourceManaged {
		return a.fail(ExitFail, fmt.Sprintf("repository %s is defined in config.yaml and cannot be removed with this command", id), "edit config.yaml and run: zaks-drive restart")
	}
	san := a.sanitizerFor(d)
	a.printf("This DETACHES repository %s (%s). It stops being served.\n", id, san.Line(repoLocation(v.Spec)))
	a.printf("  - Documents are NOT deleted: they stay where they are, and adding the repository again serves them again.\n")
	if v.SAP != nil {
		a.printf("  - WARNING: it is exposed to SAP as content repository %s. SAP keeps references to the documents; until it is\n"+
			"    served again, SAP gets errors for them. Remove it in SAP first (OAC0, KPro storage categories, ArchiveLink).\n", firstNonEmptyString(v.SAP.ContRep, id))
		if !*allow {
			return a.fail(ExitDeclined, "the repository is exposed to SAP", "repeat with --allow-in-use if you are sure")
		}
	}
	if v.TenantID != "" {
		a.printf("  - Its stored client secret is removed from the server.\n")
	}
	a.printf("\n%s\n", changeNote(c, *noRestart))
	if !a.confirm(ctx, "Detach it?", *yes) {
		a.printf("Nothing was changed.\n")
		return ExitDeclined
	}
	if _, err := c.call(ctx, repoOp{name: "detach", id: id, allowInUse: *allow}); err != nil {
		return a.failRemote(d, err)
	}
	a.printf("Detached repository %s.\n", id)
	return a.applyChanges(ctx, d, c, *noRestart)
}

// ---- check ----

func (a *App) repoCheck(ctx context.Context, args []string) int {
	id, args := splitID(args)
	fset := flag.NewFlagSet("repo check", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	if err := fset.Parse(args); err != nil {
		return ExitUsage
	}
	if id == "" {
		id = fset.Arg(0)
	}
	if id == "" {
		fmt.Fprintln(a.Err, "usage: zaks-drive repo check <id>")
		return ExitUsage
	}
	d, c, code := a.prepareRepo(ctx, *dir)
	if code != ExitOK {
		return code
	}
	a.printf("This tests the repository with the server's own code. A SharePoint repository contacts Microsoft (with its own\napplication if it has one) and creates and deletes small test files; a local one tests its folder.\n\n")
	raw, err := c.call(ctx, repoOp{name: "check", id: id})
	if err != nil {
		return a.failRemote(d, err)
	}
	var r struct {
		OK     bool   `json:"ok"`
		Report string `json:"report"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return a.fail(ExitFail, "the answer of the server could not be read", "")
	}
	san := a.sanitizerFor(d)
	for _, l := range strings.Split(strings.TrimRight(r.Report, "\n"), "\n") {
		fmt.Fprintln(a.Out, san.Line(l))
	}
	if !r.OK {
		return ExitFail
	}
	return ExitOK
}

// ---- purge ----

const purgeWarning = `
  A PURGE DELETES THE DOCUMENTS of this repository from the server. It cannot be undone and ZAKS Drive cannot restore anything.

  - SAP has no purge. SAP keeps its references (DMS originals, archive links, attachments). Purging the data
    behind them leaves SAP pointing at documents that no longer exist: users get errors, and other repositories
    that were integrated with this data can become unstable. That cannot be repaired from here.
  - You take full responsibility for this action. Make sure you have a backup you have tested.
`

func (a *App) repoPurge(ctx context.Context, args []string) int {
	id, args := splitID(args)
	fset := flag.NewFlagSet("repo purge", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	plan := fset.Bool("plan", false, "inspect what would be deleted and get a challenge phrase")
	execute := fset.Bool("execute", false, "delete, using the phrase of a current plan")
	typed := fset.String("typed-id", "", "the repository ID, typed again")
	phrase := fset.String("challenge", "", "the challenge phrase from --plan")
	understood := fset.Bool("i-understand-this-destroys-documents", false, "required acknowledgement")
	if err := fset.Parse(args); err != nil {
		return ExitUsage
	}
	if id == "" {
		id = fset.Arg(0)
	}
	if id == "" || *plan == *execute {
		fmt.Fprintln(a.Err, "usage: zaks-drive repo purge <id> --plan\n       zaks-drive repo purge <id> --execute --i-understand-this-destroys-documents [--typed-id ID --challenge PHRASE]")
		return ExitUsage
	}
	d, c, code := a.prepareRepo(ctx, *dir)
	if code != ExitOK {
		return code
	}
	san := a.sanitizerFor(d)

	// The server must already run without the repository.
	if st, err := c.status(ctx); err == nil {
		for _, s := range st.Serving {
			if s == id {
				return a.fail(ExitFail, fmt.Sprintf("repository %s is being served right now", id), "detach it (zaks-drive repo remove), restart the server, then purge")
			}
		}
		if st.PendingRestart {
			return a.fail(ExitFail, "repository changes are pending a restart, so the running server may still serve this repository", "restart first: zaks-drive restart")
		}
	}

	if *plan {
		raw, err := c.call(ctx, repoOp{name: "purge-plan", id: id})
		if err != nil {
			return a.failRemote(d, err)
		}
		var p repoadmin.PurgePlan
		if err := json.Unmarshal(raw, &p); err != nil {
			return a.fail(ExitFail, "the answer of the server could not be read", "")
		}
		a.printf("Purge plan for repository %s (local folder %s):\n", p.ID, san.Line(p.Path))
		a.printf("  documents  %d in %d folders, %s\n", p.Files, p.Folders, humanBytes(p.Bytes))
		if p.Links > 0 {
			a.printf("  links      %d (they are removed, never followed)\n", p.Links)
		}
		if !p.Newest.IsZero() {
			a.printf("  newest     %s\n", p.Newest.Format(time.RFC3339))
		}
		a.printf("%s\n", purgeWarning)
		a.printf("Nothing was deleted. To delete, run within %s, typing the repository ID and this phrase:\n\n  %s\n\n", repoadmin.PurgeValidity, p.Challenge)
		a.printf("  zaks-drive repo purge %s --execute --i-understand-this-destroys-documents\n", id)
		return ExitOK
	}

	// execute
	if !*understood {
		return a.fail(ExitDeclined, "a purge needs --i-understand-this-destroys-documents", "read the warning of: zaks-drive repo purge "+id+" --plan")
	}
	a.printf("%s\n", purgeWarning)
	if *typed == "" || *phrase == "" {
		if a.Interactive == nil || !a.Interactive() {
			return a.fail(ExitDeclined, "without a terminal, pass --typed-id and --challenge (the phrase printed by --plan)", "")
		}
		*typed = a.ask("Type the repository ID to confirm", "")
		*phrase = a.ask("Paste the challenge phrase from the plan", "")
	}
	raw, err := c.call(ctx, repoOp{name: "purge", id: id, extra: []string{"--typed-id", *typed, "--challenge", *phrase, "--i-understand-this-destroys-documents"}})
	if err != nil {
		return a.failRemote(d, err)
	}
	var r repoadmin.PurgeResult
	_ = json.Unmarshal(raw, &r)
	a.printf("Purged repository %s: %d documents, %d folders, %s deleted. The action was recorded in the audit log.\n", r.ID, r.Files, r.Folders, humanBytes(r.Bytes))
	return ExitOK
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// repoOverview is what status, doctor and the support bundle show about
// repositories. It is best-effort: nil when the server's API is not usable.
type repoOverview struct {
	Views  []repoadmin.View
	Status repoStatus
}

func (a *App) repoOverview(ctx context.Context, d *Deployment) *repoOverview {
	c := a.newRepoClient(ctx, d)
	if !c.viaAPI() {
		return nil
	}
	st, err := c.status(ctx)
	if err != nil {
		return nil
	}
	ov := &repoOverview{Status: st}
	if raw, err := c.api(ctx, "GET", "/repositories", nil); err == nil {
		var lr listResult
		if json.Unmarshal(raw, &lr) == nil {
			ov.Views = lr.Value
		}
	}
	sort.Slice(ov.Views, func(i, j int) bool { return ov.Views[i].ID < ov.Views[j].ID })
	return ov
}

// RepoRow is the allowlisted view of a repository for status output and the
// support bundle: no location, no tenant or client IDs, no folder names.
type RepoRow struct {
	ID       string `json:"id"`
	Backend  string `json:"backend"`
	Source   string `json:"source"`
	SAP      bool   `json:"sap"`
	OwnApp   bool   `json:"ownApplication"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	Versions *int   `json:"localVersions,omitempty"`
	State    string `json:"state,omitempty"`
}

// rows turns an overview into allowlisted rows.
func (ov *repoOverview) rows() []RepoRow {
	if ov == nil {
		return nil
	}
	serving := map[string]bool{}
	for _, id := range ov.Status.Serving {
		serving[id] = true
	}
	var out []RepoRow
	for _, v := range ov.Views {
		row := RepoRow{ID: v.ID, Backend: v.Backend, Source: v.Source, SAP: v.SAP != nil, OwnApp: v.TenantID != "", ReadOnly: v.ReadOnly, State: "pending restart"}
		if serving[v.ID] {
			row.State = "serving"
		}
		if v.Local != nil {
			row.Versions = v.Local.Versions
		}
		out = append(out, row)
	}
	return out
}
