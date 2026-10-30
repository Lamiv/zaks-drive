package ctl

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lamiv/zaks-drive/internal/repoadmin"
)

// fakeServer is a stateful stand-in for the server's repository admin API.
type fakeServer struct {
	mu       sync.Mutex
	repos    map[string]repoadmin.View
	detached []repoadmin.Spec
	started  time.Time
	serving  []string
	pending  bool
	fallback string
	applied  int
	bodies   []string
}

func newFakeServer() *fakeServer {
	return &fakeServer{repos: map[string]repoadmin.View{}, started: time.Now().Add(-time.Hour).UTC()}
}

func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, status int, code, detail string) {
	writeJSONBody(w, status, map[string]any{"error": map[string]string{"code": code, "title": code, "detail": detail, "fix": "do the right thing"}})
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSONBody(w, 200, repoStatus{Version: "test", Ready: true, PendingRestart: f.pending, Fallback: f.fallback, DrainSeconds: 30, StartedAt: f.started, Serving: f.serving})
	})
	mux.HandleFunc("GET /admin/api/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		list := []repoadmin.View{}
		for _, v := range f.repos {
			list = append(list, v)
		}
		writeJSONBody(w, 200, map[string]any{"value": list})
	})
	mux.HandleFunc("GET /admin/api/v1/detached", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSONBody(w, 200, map[string]any{"value": f.detached})
	})
	mux.HandleFunc("GET /admin/api/v1/repositories/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		v, ok := f.repos[r.PathValue("id")]
		if !ok {
			apiErr(w, 404, "ADMIN_REPO_NOT_FOUND", "there is no repository")
			return
		}
		writeJSONBody(w, 200, v)
	})
	save := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			var raw json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&raw)
			f.bodies = append(f.bodies, string(raw))
			var b repoBody
			_ = json.Unmarshal(raw, &b)
			v := repoadmin.View{Spec: b.Spec, Source: "managed", SecretSet: b.ClientSecret != nil}
			if old, ok := f.repos[b.ID]; ok && b.ClientSecret == nil {
				v.SecretSet = old.SecretSet
			}
			f.repos[b.ID] = v
			f.pending = true
			writeJSONBody(w, status, v)
		}
	}
	mux.HandleFunc("POST /admin/api/v1/repositories", save(201))
	mux.HandleFunc("PUT /admin/api/v1/repositories/{id}", save(200))
	mux.HandleFunc("DELETE /admin/api/v1/repositories/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		v, ok := f.repos[r.PathValue("id")]
		if !ok {
			apiErr(w, 404, "ADMIN_REPO_NOT_FOUND", "there is no repository")
			return
		}
		if v.SAP != nil && r.URL.Query().Get("allowInUse") != "true" {
			apiErr(w, 409, "ADMIN_REPO_IN_USE", "exposed to SAP")
			return
		}
		delete(f.repos, v.ID)
		f.detached = append(f.detached, v.Spec)
		f.pending = true
		writeJSONBody(w, 200, map[string]any{"detached": v.ID})
	})
	mux.HandleFunc("POST /admin/api/v1/apply", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.applied++
		f.started = time.Now().UTC()
		f.pending = false
		f.serving = nil
		for id := range f.repos {
			f.serving = append(f.serving, id)
		}
		writeJSONBody(w, 202, map[string]any{"state": "draining"})
	})
	mux.HandleFunc("POST /admin/api/v1/repositories/{id}/check", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, 200, map[string]any{"ok": true, "report": "[PASS] fine " + secClient + "\n"})
	})
	return mux
}

func newRepoEnv(t *testing.T) (*env, *fakeServer) {
	t.Helper()
	e := newEnv(t)
	fs := newFakeServer()
	e.repoAPI = fs.handler()
	return e, fs
}

func (e *env) calledDockerRun() bool { return e.run.has(" run --rm -T --no-deps adapter repos") }

func TestRepoListEmptyAndPopulated(t *testing.T) {
	e, fs := newRepoEnv(t)
	if rc := e.main("repo", "list", "--dir", e.dir); rc != ExitOK || !strings.Contains(e.out.String(), "No repositories yet") {
		t.Fatalf("empty: %d\n%s", rc, e.both())
	}
	fs.repos["AD"] = repoadmin.View{Spec: repoadmin.Spec{ID: "AD", Backend: "local", Local: &repoadmin.LocalSpec{Path: "AD"}, SAP: &repoadmin.SAPSpec{}}, Source: "managed"}
	fs.serving = []string{"AD"}
	fs.detached = []repoadmin.Spec{{ID: "ZH", Backend: "local", Local: &repoadmin.LocalSpec{Path: "ZH"}}}
	fs.pending = true
	if rc := e.main("repo", "list", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	for _, w := range []string{"AD", "local", "managed", "serving", "keeps 5 versions", "Detached", "ZH", "pending a restart"} {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("list lacks %q:\n%s", w, e.out.String())
		}
	}
	if rc := e.main("repo", "list", "--json", "--dir", e.dir); rc != ExitOK {
		t.Fatal(rc)
	}
	var v map[string]any
	if err := json.Unmarshal(e.out.Bytes(), &v); err != nil || v["repositories"] == nil || v["detached"] == nil {
		t.Fatalf("not the expected JSON: %v %s", err, e.out.String())
	}
	if e.calledDockerRun() {
		t.Error("a running server with a token must be called through its API, not a one-off container")
	}
}

func TestRepoAddLocalAppliesWithADrainedRestart(t *testing.T) {
	e, fs := newRepoEnv(t)
	if rc := e.main("repo", "add", "AD", "--backend", "local", "--versions", "7", "--sap", "--dir", e.dir); rc != ExitDeclined {
		t.Fatalf("without --yes and without a terminal nothing may change: %d\n%s", rc, e.both())
	}
	if len(fs.repos) != 0 {
		t.Fatal("a declined change was saved")
	}
	rc := e.main("repo", "add", "AD", "--backend", "local", "--versions", "7", "--sap", "--yes", "--dir", e.dir)
	if rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	for _, w := range []string{"will create", "Applying restarts", "Saved repository AD", "Applied. Serving: AD"} {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, e.out.String())
		}
	}
	if fs.applied != 1 {
		t.Fatalf("apply called %d times", fs.applied)
	}
	var b repoBody
	if err := json.Unmarshal([]byte(fs.bodies[0]), &b); err != nil || b.Backend != "local" || b.Local == nil || b.Local.Path != "AD" ||
		b.Local.Versions == nil || *b.Local.Versions != 7 || b.SAP == nil {
		t.Fatalf("body sent to the server: %s", fs.bodies[0])
	}
}

func TestRepoAddNoRestartLeavesTheChangePending(t *testing.T) {
	e, fs := newRepoEnv(t)
	if rc := e.main("repo", "add", "AD", "--backend", "local", "--yes", "--no-restart", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	if fs.applied != 0 || !strings.Contains(e.out.String(), "pending") {
		t.Fatalf("applied=%d\n%s", fs.applied, e.out.String())
	}
}

func TestRepoAddRejectsBadInputBeforeAnyServerCall(t *testing.T) {
	e, fs := newRepoEnv(t)
	for name, args := range map[string][]string{
		"id too long":            {"repo", "add", "ABC", "--backend", "local"},
		"id lower case":          {"repo", "add", "ad", "--backend", "local"},
		"id path":                {"repo", "add", "../", "--backend", "local"},
		"no backend":             {"repo", "add", "AD"},
		"unknown backend":        {"repo", "add", "AD", "--backend", "s3"},
		"sharepoint no location": {"repo", "add", "ZH", "--backend", "sharepoint"},
		"site on local":          {"repo", "add", "AD", "--backend", "local", "--site-url", "https://x"},
		"versions on sharepoint": {"repo", "add", "ZH", "--backend", "sharepoint", "--site-url", "https://x", "--versions", "3"},
		"half an application":    {"repo", "add", "ZH", "--backend", "sharepoint", "--site-url", "https://x", "--tenant-id", guidA},
		"a secret as a flag":     {"repo", "add", "ZH", "--backend", "sharepoint", "--site-url", "https://x", "--client-secret", "oops"},
	} {
		if rc := e.main(append(args, "--yes", "--dir", e.dir)...); rc != ExitUsage {
			t.Errorf("%s: exit %d\n%s", name, rc, e.both())
		}
	}
	if len(fs.repos) != 0 || len(fs.bodies) != 0 {
		t.Fatal("a rejected command reached the server")
	}
}

func TestRepoSecretComesFromFileOrStdinAndNeverShows(t *testing.T) {
	e, fs := newRepoEnv(t)
	secretFile := filepath.Join(t.TempDir(), "s")
	must(t, os.WriteFile(secretFile, []byte("  "+secClient+"\n"), 0o600))
	base := []string{"repo", "add", "ZH", "--backend", "sharepoint", "--site-url", "https://contoso.sharepoint.com/sites/hr",
		"--tenant-id", guidA, "--client-id", guidB, "--yes", "--no-restart", "--dir", e.dir}

	if rc := e.main(base...); rc != ExitUsage || !strings.Contains(e.err.String(), "--secret-file") {
		t.Fatalf("without a secret and without a terminal: %d\n%s", rc, e.both())
	}
	if rc := e.main(append(base, "--secret-file", secretFile)...); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	if !strings.Contains(fs.bodies[0], secClient) || strings.Contains(fs.bodies[0], "  "+secClient) {
		t.Fatalf("the trimmed secret must travel in the request body: %s", fs.bodies[0])
	}
	if strings.Contains(e.both(), secClient) {
		t.Fatalf("the secret was printed:\n%s", e.both())
	}
	for _, c := range e.run.all() {
		if strings.Contains(c, secClient) {
			t.Fatalf("the secret reached a command line: %s", c)
		}
	}

	e2, fs2 := newRepoEnv(t)
	e2.app.In = strings.NewReader(secClient + "\n")
	if rc := e2.main(append(base[:len(base)-2], "--secret-stdin", "--dir", e2.dir)...); rc != ExitOK {
		t.Fatalf("stdin: exit %d\n%s", rc, e2.both())
	}
	if !strings.Contains(fs2.bodies[0], secClient) || strings.Contains(e2.both(), secClient) {
		t.Fatal("secret from stdin: not sent, or printed")
	}

	// interactive: a hidden prompt
	e3, fs3 := newRepoEnv(t)
	e3.app.Interactive = func() bool { return true }
	asked := 0
	e3.app.ReadSecret = func(string) (string, error) { asked++; return secClient, nil }
	if rc := e3.main(append(append([]string{}, base[:len(base)-2]...), "--dir", e3.dir)...); rc != ExitOK || asked != 1 || !strings.Contains(fs3.bodies[0], secClient) {
		t.Fatalf("prompt: exit %d asked %d\n%s", rc, asked, e3.both())
	}
}

func TestRepoUpdateChangesOnlyWhatWasSet(t *testing.T) {
	e, fs := newRepoEnv(t)
	three := 3
	fs.repos["AD"] = repoadmin.View{Spec: repoadmin.Spec{ID: "AD", Description: "Archive", Backend: "local", Local: &repoadmin.LocalSpec{Path: "AD", Versions: &three}, SAP: &repoadmin.SAPSpec{Signature: "optional"}}, Source: "managed"}
	fs.repos["ZH"] = repoadmin.View{Spec: repoadmin.Spec{ID: "ZH", Backend: "sharepoint", SiteURL: "https://x", TenantID: guidA, ClientID: guidB}, Source: "managed", SecretSet: true}
	fs.repos["ST"] = repoadmin.View{Spec: repoadmin.Spec{ID: "ST", Backend: "sharepoint", SiteURL: "https://x"}, Source: "static"}

	if rc := e.main("repo", "update", "AD", "--versions", "9", "--yes", "--no-restart", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	got := fs.repos["AD"]
	if *got.Local.Versions != 9 || got.Description != "Archive" || got.SAP == nil || got.SAP.Signature != "optional" {
		t.Fatalf("update touched more than it was told to: %+v", got)
	}
	if rc := e.main("repo", "update", "ZH", "--global-app", "--yes", "--no-restart", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	if fs.repos["ZH"].TenantID != "" || fs.repos["ZH"].ClientID != "" {
		t.Fatalf("--global-app must clear the application: %+v", fs.repos["ZH"])
	}
	if rc := e.main("repo", "update", "AD", "--no-sap", "--yes", "--no-restart", "--dir", e.dir); rc != ExitOK || fs.repos["AD"].SAP != nil {
		t.Fatalf("--no-sap: %d %+v", rc, fs.repos["AD"].SAP)
	}
	if rc := e.main("repo", "update", "ST", "--description", "x", "--yes", "--dir", e.dir); rc != ExitFail || !strings.Contains(e.err.String(), "config.yaml") {
		t.Fatalf("a static repository must be refused with the reason: %d\n%s", rc, e.both())
	}
	if rc := e.main("repo", "update", "QQ", "--description", "x", "--yes", "--dir", e.dir); rc != ExitFail || !strings.Contains(e.err.String(), "ADMIN_REPO_NOT_FOUND") {
		t.Fatalf("unknown repository: %d\n%s", rc, e.both())
	}
}

func TestRepoRemoveDetachesAndProtectsSAPUse(t *testing.T) {
	e, fs := newRepoEnv(t)
	fs.repos["AD"] = repoadmin.View{Spec: repoadmin.Spec{ID: "AD", Backend: "local", Local: &repoadmin.LocalSpec{Path: "AD"}, SAP: &repoadmin.SAPSpec{ContRep: "AD"}}, Source: "managed"}
	fs.repos["AE"] = repoadmin.View{Spec: repoadmin.Spec{ID: "AE", Backend: "local", Local: &repoadmin.LocalSpec{Path: "AE"}}, Source: "managed"}

	if rc := e.main("repo", "remove", "AD", "--yes", "--dir", e.dir); rc != ExitDeclined || !strings.Contains(e.both(), "exposed to SAP") {
		t.Fatalf("SAP-exposed without --allow-in-use: %d\n%s", rc, e.both())
	}
	if _, ok := fs.repos["AD"]; !ok {
		t.Fatal("it was detached anyway")
	}
	if rc := e.main("repo", "remove", "AE", "--dir", e.dir); rc != ExitDeclined {
		t.Fatalf("without --yes and a terminal: %d", rc)
	}
	if rc := e.main("repo", "remove", "AE", "--yes", "--no-restart", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	for _, w := range []string{"Documents are NOT deleted", "Detached repository AE"} {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, e.out.String())
		}
	}
	if rc := e.main("repo", "remove", "AD", "--allow-in-use", "--yes", "--no-restart", "--dir", e.dir); rc != ExitOK || len(fs.repos) != 0 {
		t.Fatalf("with the explicit confirmation: %d %v", rc, fs.repos)
	}
}

func TestRepoApplyReportsAFallback(t *testing.T) {
	e, fs := newRepoEnv(t)
	fs.fallback = "[CFG_REPO_LOCAL_INVALID] the new definition is broken"
	rc := e.main("repo", "add", "AD", "--backend", "local", "--yes", "--dir", e.dir)
	if rc != ExitFail || !strings.Contains(e.err.String(), "NEW SETTINGS WERE NOT APPLIED") || !strings.Contains(e.err.String(), "CFG_REPO_LOCAL_INVALID") {
		t.Fatalf("a restart that fell back must not be reported as success: %d\n%s", rc, e.both())
	}
}

func oneOffHandler(e *env, answer string) {
	inner := e.run.handler
	e.run.handler = func(c call) (Result, error) {
		if strings.Contains(c.String(), " run --rm -T --no-deps adapter repos") {
			return Result{Stdout: answer}, nil
		}
		return inner(c)
	}
}

func TestRepoWithAStoppedServerUsesAOneOffContainer(t *testing.T) {
	e := newEnv(t)
	e.useFake(func(f *dockerFake) { f.noContainer = true })
	oneOffHandler(e, `{"ok":true,"result":{"id":"ZH","backend":"sharepoint","source":"managed","secretSet":true}}`)
	secretFile := filepath.Join(t.TempDir(), "s")
	must(t, os.WriteFile(secretFile, []byte(secClient), 0o600))

	rc := e.main("repo", "add", "ZH", "--backend", "sharepoint", "--site-url", "https://contoso.sharepoint.com/sites/hr",
		"--tenant-id", guidA, "--client-id", guidB, "--secret-file", secretFile, "--yes", "--dir", e.dir)
	if rc != ExitOK || !strings.Contains(e.out.String(), "not running") {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	var run *call
	for i, c := range e.run.calls {
		if strings.Contains(c.String(), " run --rm -T --no-deps adapter repos add") {
			run = &e.run.calls[i]
		}
	}
	if run == nil {
		t.Fatalf("no one-off container was started: %v", e.run.all())
	}
	if !strings.Contains(run.Stdin, secClient) {
		t.Errorf("the secret must travel on standard input: %q", run.Stdin)
	}
	if strings.Contains(run.String(), secClient) || strings.Contains(e.both(), secClient) {
		t.Error("the secret appeared in an argument or in the output")
	}
	if !strings.Contains(run.String(), "-f "+filepath.Join(e.dir, "docker-compose.yml")) {
		t.Errorf("the one-off container must use this deployment's compose file only: %s", run.String())
	}
}

func TestRepoOneOffErrorsAreCodedAndSanitized(t *testing.T) {
	e := newEnv(t)
	e.useFake(func(f *dockerFake) { f.noContainer = true })
	oneOffHandler(e, `{"ok":false,"error":{"code":"CFG_REPO_LOCAL_INVALID","title":"x","detail":"path `+secClient+` is wrong","fix":"use a name","more":"https://example/doc"}}`)
	rc := e.main("repo", "add", "AD", "--backend", "local", "--yes", "--dir", e.dir)
	if rc != ExitFail || !strings.Contains(e.err.String(), "CFG_REPO_LOCAL_INVALID") || !strings.Contains(e.err.String(), "fix: use a name") {
		t.Fatalf("%d\n%s", rc, e.both())
	}
	if strings.Contains(e.both(), secClient) {
		t.Fatalf("an error leaked a secret value:\n%s", e.both())
	}
	oneOffHandler(e, "not json at all")
	if rc := e.main("repo", "list", "--dir", e.dir); rc != ExitFail {
		t.Fatalf("a broken answer must fail: %d", rc)
	}
}

func TestRepoFallsBackToAOneOffWhenTheAPIDoesNotAnswer(t *testing.T) {
	e := newEnv(t) // running container, but the fake admin endpoint has no repository API
	oneOffHandler(e, `{"ok":true,"result":{"value":[]}}`)
	e.repoAPI = nil
	// the fake endpoint answers 404 for /admin/api/v1: that is an HTTP answer, so use a dead port instead
	e.useFake(func(f *dockerFake) { f.adminPort = 1 })
	oneOffHandler(e, `{"ok":true,"result":{"value":[]}}`)
	d := newDeployment(e.dir)
	envText, _ := os.ReadFile(d.EnvFile)
	must(t, os.WriteFile(d.EnvFile, []byte(strings.Replace(string(envText), "ADMIN_PORT=", "ADMIN_PORT=1\n#old=", 1)), 0o600))
	if rc := e.main("repo", "list", "--dir", e.dir); rc != ExitOK || !e.calledDockerRun() {
		t.Fatalf("exit %d, one-off used: %v\n%s", rc, e.calledDockerRun(), e.both())
	}
}

func TestRepoWithoutAnAdminTokenCannotDrainButStillSaves(t *testing.T) {
	e := newEnv(t)
	d := newDeployment(e.dir)
	envText, _ := os.ReadFile(d.EnvFile)
	must(t, os.WriteFile(d.EnvFile, []byte(strings.Replace(string(envText), "ADMIN_TOKEN="+secAdmin, "ADMIN_TOKEN=", 1)), 0o600))
	oneOffHandler(e, `{"ok":true,"result":{"id":"AD","backend":"local","source":"managed"}}`)
	rc := e.main("repo", "add", "AD", "--backend", "local", "--yes", "--dir", e.dir)
	if rc != ExitOK || !strings.Contains(e.out.String(), "ADMIN_TOKEN is not set") || !e.calledDockerRun() {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
}

func TestRepoCheckPrintsASanitizedReport(t *testing.T) {
	e, _ := newRepoEnv(t)
	rc := e.main("repo", "check", "AD", "--dir", e.dir)
	if rc != ExitOK || !strings.Contains(e.out.String(), "[PASS] fine") || strings.Contains(e.both(), secClient) {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
}

func TestRepoPurgeIsHostOnlyAndHardToDoByAccident(t *testing.T) {
	e, fs := newRepoEnv(t)
	plan := `{"ok":true,"result":{"id":"AD","path":"AD","files":12,"folders":3,"links":0,"bytes":5242880,"newest":"2026-10-01T10:00:00Z","challenge":"PURGE-AD-abc-0123456789ab","expiresAt":"2026-10-30T12:00:00Z"}}`
	oneOffHandler(e, plan)

	// still served, or changes pending: refused before any container starts
	fs.serving = []string{"AD"}
	if rc := e.main("repo", "purge", "AD", "--plan", "--dir", e.dir); rc != ExitFail || !strings.Contains(e.err.String(), "being served") || e.calledDockerRun() {
		t.Fatalf("served: %d\n%s", rc, e.both())
	}
	fs.serving, fs.pending = nil, true
	if rc := e.main("repo", "purge", "AD", "--plan", "--dir", e.dir); rc != ExitFail || !strings.Contains(e.err.String(), "pending a restart") {
		t.Fatalf("pending: %d\n%s", rc, e.both())
	}
	fs.pending = false

	if rc := e.main("repo", "purge", "AD", "--plan", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("plan: %d\n%s", rc, e.both())
	}
	for _, w := range []string{"12 in 3 folders", "5.0 MiB", "PURGE-AD-abc-0123456789ab", "SAP has no purge", "full responsibility", "Nothing was deleted", "backup"} {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("plan output lacks %q:\n%s", w, e.out.String())
		}
	}
	if !e.run.has("adapter repos purge-plan AD") {
		t.Fatalf("the plan must run in a one-off container: %v", e.run.all())
	}

	// execute: acknowledgement, then ID and phrase
	if rc := e.main("repo", "purge", "AD", "--execute", "--dir", e.dir); rc != ExitDeclined {
		t.Fatalf("without the acknowledgement: %d", rc)
	}
	if rc := e.main("repo", "purge", "AD", "--execute", "--i-understand-this-destroys-documents", "--dir", e.dir); rc != ExitDeclined || !strings.Contains(e.err.String(), "--typed-id") {
		t.Fatalf("no terminal and no phrase: %d\n%s", rc, e.both())
	}
	if e.run.has("adapter repos purge AD") {
		t.Fatal("a purge was started although a safeguard was missing")
	}

	oneOffHandler(e, `{"ok":true,"result":{"id":"AD","files":12,"folders":3,"bytes":5242880}}`)
	rc := e.main("repo", "purge", "AD", "--execute", "--i-understand-this-destroys-documents", "--typed-id", "AD", "--challenge", "PURGE-AD-abc-0123456789ab", "--dir", e.dir)
	if rc != ExitOK || !strings.Contains(e.out.String(), "Purged repository AD: 12 documents") {
		t.Fatalf("complete purge: %d\n%s", rc, e.both())
	}
	if !e.run.has("adapter repos purge AD --typed-id AD --challenge PURGE-AD-abc-0123456789ab --i-understand-this-destroys-documents") {
		t.Fatalf("arguments: %v", e.run.all())
	}

	// interactive: prompts for the ID and the phrase
	e.app.Interactive = func() bool { return true }
	e.app.In = strings.NewReader("AD\nPURGE-AD-abc-0123456789ab\n")
	if rc := e.main("repo", "purge", "AD", "--execute", "--i-understand-this-destroys-documents", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("interactive purge: %d\n%s", rc, e.both())
	}
	for _, c := range fs.bodies {
		if strings.Contains(c, "purge") {
			t.Fatal("a purge must never be sent to the admin API")
		}
	}
}

func TestRepoUsageAndConfigValidateWithManagedRepositories(t *testing.T) {
	e := newEnv(t)
	if rc := e.main("repo"); rc != ExitUsage {
		t.Errorf("repo alone: %d", rc)
	}
	if rc := e.main("repo", "frobnicate"); rc != ExitUsage {
		t.Errorf("unknown subcommand: %d", rc)
	}
	if rc := e.main("repo", "help"); rc != ExitOK || !strings.Contains(e.out.String(), "purge") {
		t.Errorf("help: %d", rc)
	}
	// repositories that live only in the state volume must not make the host-side validation fail
	cfg := "server:\n  interfaces: [rest]\nauth:\n  disabled: true\n"
	must(t, os.WriteFile(filepath.Join(e.dir, "config.yaml"), []byte(cfg), 0o644))
	if rc := e.main("config", "validate", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("a deployment whose repositories are all managed must validate: %d\n%s", rc, e.both())
	}
}

func TestStatusDoctorAndBundleShowRepositoriesWithoutLocations(t *testing.T) {
	e, fs := newRepoEnv(t)
	two := 2
	fs.repos["AD"] = repoadmin.View{Spec: repoadmin.Spec{ID: "AD", Backend: "local", Local: &repoadmin.LocalSpec{Path: "secret-folder-name", Versions: &two}, SAP: &repoadmin.SAPSpec{}}, Source: "managed"}
	fs.repos["ZH"] = repoadmin.View{Spec: repoadmin.Spec{ID: "ZH", Backend: "sharepoint", SiteURL: "https://contoso.sharepoint.com/sites/hr", TenantID: guidA, ClientID: guidB}, Source: "managed", SecretSet: true}
	fs.serving = []string{"AD"}
	fs.pending = true

	if rc := e.main("status", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("status: %d\n%s", rc, e.both())
	}
	for _, w := range []string{"Repository   AD: local (managed, SAP), serving", "Repository   ZH: sharepoint (managed), pending restart", "waiting for a restart"} {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("status lacks %q:\n%s", w, e.out.String())
		}
	}
	if rc := e.main("status", "--json", "--dir", e.dir); rc != ExitOK || !strings.Contains(e.out.String(), `"pendingRestart": true`) {
		t.Fatalf("status --json: %d\n%s", rc, e.out.String())
	}

	f, _ := e.doctor(t)
	wantStatus(t, f, "repositories", StatusWarn)
	fs.pending = false
	fs.serving = []string{"AD", "ZH"}
	f, _ = e.doctor(t)
	wantStatus(t, f, "repositories", StatusPass)
	fs.fallback = "[CFG_REPO_LOCAL_INVALID] broken"
	f, _ = e.doctor(t)
	wantStatus(t, f, "repositories", StatusFail)

	// the support bundle carries rows, never locations or IDs of the Microsoft application
	dest := filepath.Join(t.TempDir(), "b.tar.gz")
	if rc := e.main("support", "bundle", "--dir", e.dir, "--output", dest, "--yes"); rc != ExitOK {
		t.Fatalf("bundle: %d\n%s", rc, e.both())
	}
	files := readBundle(t, dest)
	for name, body := range files {
		for _, bad := range []string{"secret-folder-name", "contoso", guidA, guidB} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains %q", name, bad)
			}
		}
	}
	if !strings.Contains(files["config-summary.json"], `"managedRepositories"`) || !strings.Contains(files["config-summary.json"], `"id": "AD"`) && !strings.Contains(files["config-summary.json"], `"id":"AD"`) {
		t.Errorf("the config summary lacks the managed repositories:\n%s", files["config-summary.json"])
	}
}
