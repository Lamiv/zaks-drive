package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lamiv/zaks-drive/internal/adminui"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/contentrepo"
	"github.com/Lamiv/zaks-drive/internal/drain"
	"github.com/Lamiv/zaks-drive/internal/repoadmin"
	"github.com/Lamiv/zaks-drive/internal/storage"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type adminEnv struct {
	t       *testing.T
	api     *repoAPI
	srv     *httptest.Server
	state   string
	logs    *syncBuf
	stopped atomic.Int32
	gate    *drain.Gate
}

const testToken = "a-long-random-admin-token"

func newAdminEnv(t *testing.T, token string) *adminEnv { return buildAdminEnv(t, token, false) }

func buildAdminEnv(t *testing.T, token string, withUI bool) *adminEnv {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	body := "server: { interfaces: [rest, contentrepo], stateDir: \"" + filepath.ToSlash(state) + "\" }\n" +
		"graph: { tenantId: t, clientId: c, clientSecret: s }\nauth: { disabled: true }\n" +
		"repositories:\n  - { id: DMS, siteUrl: \"https://contoso.sharepoint.com/sites/x\" }\n" +
		"contentServer:\n  repositories:\n    - { contRep: Z1, repository: DMS }\n"
	if err := os.WriteFile(cfgFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	ring := adminui.NewRing(200)
	e := &adminEnv{t: t, state: state, logs: logs, gate: drain.New(time.Second)}
	mgr := &repoadmin.Manager{StateDir: state, Actor: "api", Load: func() (config.Config, error) { return config.Load(cfgFile) }}
	applied, _ := mgr.Fingerprint()
	var ready atomic.Bool
	ready.Store(true)
	e.api = &repoAPI{
		mgr: mgr, token: token, applied: applied, gate: e.gate, ready: &ready, version: "test", drain: 300 * time.Millisecond,
		log:  slog.New(ring.Handler(slog.NewTextHandler(logs, nil))),
		stop: func() { e.stopped.Add(1) },
		ring: ring,
		check: func(context.Context, string) (string, bool, error) {
			return "report", true, nil
		},
	}
	if withUI {
		e.api.sessions = adminui.NewSessions()
		acc, _ := adminui.OpenAccount(state)
		e.api.account = acc
		var svc atomic.Pointer[storage.Service]
		var cs atomic.Pointer[contentrepo.Server]
		var status atomic.Value
		status.Store("starting")
		ui := &uiState{cfg: cfgForUI(), ready: &ready, status: &status, svc: &svc, cs: &cs, mgr: mgr, applied: applied, version: "test"}
		e.api.diagnose, e.api.settings = ui.diagnose, ui.settings
	}
	mux := http.NewServeMux()
	e.api.register(mux)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *adminEnv) call(method, path, token, body string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		e.t.Errorf("%s %s: Cache-Control %q; admin responses must not be cached", method, path, cc)
	}
	return resp.StatusCode, string(b)
}

func errCode(body string) string {
	var v struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &v)
	return v.Error.Code
}

func TestRepositoryAPIRefusesToWorkWithoutAnAdminToken(t *testing.T) {
	e := newAdminEnv(t, "")
	for _, p := range [][2]string{{"GET", "/admin/api/v1/repositories"}, {"POST", "/admin/api/v1/repositories"}, {"POST", "/admin/api/v1/apply"}, {"GET", "/admin/api/v1/status"}} {
		status, body := e.call(p[0], p[1], "", `{}`)
		if status != http.StatusServiceUnavailable || errCode(body) != "ADMIN_TOKEN_REQUIRED" {
			t.Errorf("%s %s without a configured token: %d %s", p[0], p[1], status, body)
		}
		// presenting an empty or guessed bearer must not help either
		if status, _ := e.call(p[0], p[1], "Bearer ", `{}`); status == http.StatusOK {
			t.Errorf("%s %s accepted an empty token", p[0], p[1])
		}
	}
}

func TestRepositoryAPIAuthenticationAndLockout(t *testing.T) {
	e := newAdminEnv(t, testToken)
	if status, body := e.call("GET", "/admin/api/v1/repositories", "", ""); status != http.StatusUnauthorized || errCode(body) != "AUTH_ADMIN_UNAUTHORIZED" {
		t.Fatalf("no token: %d %s", status, body)
	}
	if status, _ := e.call("GET", "/admin/api/v1/repositories", testToken, ""); status != http.StatusOK {
		t.Fatalf("right token: %d", status)
	}
	for i := 0; i < maxFailures; i++ {
		e.call("GET", "/admin/api/v1/repositories", "wrong-"+strings.Repeat("x", i), "")
	}
	// now even the right token is refused for a while, with Retry-After
	req, _ := http.NewRequest("GET", e.srv.URL+"/admin/api/v1/repositories", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("after %d wrong tokens: %d, Retry-After %q", maxFailures, resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(e.logs.String(), "locked") {
		t.Error("the lockout must be logged")
	}
}

func TestRepositoryAPILifecycleAndSecrets(t *testing.T) {
	e := newAdminEnv(t, testToken)
	status, body := e.call("POST", "/admin/api/v1/repositories", testToken,
		`{"id":"ZH","backend":"sharepoint","siteUrl":"https://contoso.sharepoint.com/sites/hr","driveName":"Documents","rootPath":"SAP_HR",
		  "tenantId":"tenant-guid","clientId":"client-guid","clientSecret":"never-echo-me","sap":{"contRep":"ZH"}}`)
	if status != http.StatusCreated || !strings.Contains(body, `"secretSet": true`) {
		t.Fatalf("create: %d %s", status, body)
	}
	status, body = e.call("GET", "/admin/api/v1/repositories", testToken, "")
	if status != 200 || !strings.Contains(body, `"id": "ZH"`) || !strings.Contains(body, `"source": "static"`) {
		t.Fatalf("list: %d %s", status, body)
	}
	// stored apart, mode as the manager sets it, and never anywhere else
	if b, _ := os.ReadFile(filepath.Join(e.state, "secrets", "ZH", "client-secret")); strings.TrimSpace(string(b)) != "never-echo-me" {
		t.Fatalf("secret file: %q", b)
	}
	audit := ""
	for _, p := range []string{"/admin/api/v1/audit", "/admin/api/v1/repositories/ZH", "/admin/api/v1/status"} {
		_, b := e.call("GET", p, testToken, "")
		audit += b
	}
	if strings.Contains(audit+e.logs.String(), "never-echo-me") {
		t.Fatal("the secret appeared in an API response or a log")
	}

	// update keeps the secret when none is sent; changes are visible
	status, body = e.call("PUT", "/admin/api/v1/repositories/ZH", testToken,
		`{"backend":"sharepoint","siteUrl":"https://contoso.sharepoint.com/sites/hr","rootPath":"SAP_HR2","tenantId":"tenant-guid","clientId":"client-guid","sap":{"contRep":"ZH"}}`)
	if status != 200 || !strings.Contains(body, `"rootPath": "SAP_HR2"`) || !strings.Contains(body, `"secretSet": true`) {
		t.Fatalf("update: %d %s", status, body)
	}
	if status, body := e.call("PUT", "/admin/api/v1/repositories/ZH", testToken, `{"id":"ZZ","backend":"local","local":{"path":"x"}}`); status != 400 {
		t.Fatalf("id mismatch between URL and body: %d %s", status, body)
	}

	// SAP-exposed repositories need the explicit confirmation to detach
	if status, body := e.call("DELETE", "/admin/api/v1/repositories/ZH", testToken, ""); status != 409 || errCode(body) != "ADMIN_REPO_IN_USE" {
		t.Fatalf("detach in use: %d %s", status, body)
	}
	if status, body := e.call("DELETE", "/admin/api/v1/repositories/ZH?allowInUse=true", testToken, ""); status != 200 || !strings.Contains(body, `"documentsDeleted": false`) {
		t.Fatalf("detach: %d %s", status, body)
	}
	if status, body := e.call("GET", "/admin/api/v1/repositories/ZH", testToken, ""); status != 404 || errCode(body) != "ADMIN_REPO_NOT_FOUND" {
		t.Fatalf("after detach: %d %s", status, body)
	}
}

func TestRepositoryAPIRejectsBadInputWithCodes(t *testing.T) {
	e := newAdminEnv(t, testToken)
	cases := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"unknown field", "POST", "/admin/api/v1/repositories", `{"id":"AD","backend":"local","local":{"path":"AD"},"surprise":1}`, 400, "ADMIN_REPO_INVALID"},
		{"not json", "POST", "/admin/api/v1/repositories", `{`, 400, "ADMIN_REPO_INVALID"},
		{"two values", "POST", "/admin/api/v1/repositories", `{"id":"AD","backend":"local","local":{"path":"AD"}} {}`, 400, "ADMIN_REPO_INVALID"},
		{"bad id", "POST", "/admin/api/v1/repositories", `{"id":"../x","backend":"local","local":{"path":"x"}}`, 400, "CFG_REPO_ID_INVALID"},
		{"validation", "POST", "/admin/api/v1/repositories", `{"id":"AD","backend":"local","local":{"path":"AD","versions":150}}`, 400, ""},
		{"missing", "GET", "/admin/api/v1/repositories/ZZ", ``, 404, "ADMIN_REPO_NOT_FOUND"},
		{"static", "PUT", "/admin/api/v1/repositories/DM", `{"backend":"local","local":{"path":"x"}}`, 404, ""},
	}
	for _, c := range cases {
		status, body := e.call(c.method, c.path, testToken, c.body)
		if status != c.status || (c.code != "" && errCode(body) != c.code) || errCode(body) == "" {
			t.Errorf("%s: %d %s", c.name, status, body)
		}
	}
	// a body over the limit is refused, not buffered
	huge := `{"id":"AD","backend":"local","description":"` + strings.Repeat("a", maxAdminBody+10) + `"}`
	if status, _ := e.call("POST", "/admin/api/v1/repositories", testToken, huge); status != 400 {
		t.Errorf("oversized body: %d", status)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.state, "repos.d")); len(entries) != 0 {
		t.Errorf("rejected requests left files behind: %v", entries)
	}
}

func TestStatusReportsPendingRestart(t *testing.T) {
	e := newAdminEnv(t, testToken)
	_, body := e.call("GET", "/admin/api/v1/status", testToken, "")
	if !strings.Contains(body, `"pendingRestart": false`) || !strings.Contains(body, `"ready": true`) || !strings.Contains(body, `"version": "test"`) {
		t.Fatalf("fresh state: %s", body)
	}
	e.call("POST", "/admin/api/v1/repositories", testToken, `{"id":"AD","backend":"local","local":{"path":"AD"}}`)
	_, body = e.call("GET", "/admin/api/v1/status", testToken, "")
	if !strings.Contains(body, `"pendingRestart": true`) {
		t.Fatalf("after a change: %s", body)
	}
	e.call("DELETE", "/admin/api/v1/repositories/AD", testToken, "")
	_, body = e.call("GET", "/admin/api/v1/status", testToken, "")
	if !strings.Contains(body, `"pendingRestart": false`) {
		t.Fatalf("after reverting the change: %s", body)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestApplyDrainsThenStops(t *testing.T) {
	e := newAdminEnv(t, testToken)
	status, body := e.call("POST", "/admin/api/v1/apply", testToken, `{"drainSeconds":1}`)
	if status != http.StatusAccepted || !strings.Contains(body, `"state": "draining"`) {
		t.Fatalf("apply: %d %s", status, body)
	}
	if status, body := e.call("POST", "/admin/api/v1/apply", testToken, ""); status != 409 || errCode(body) != "ADMIN_APPLY_BUSY" {
		t.Fatalf("second apply: %d %s", status, body)
	}
	waitFor(t, "the server to stop", func() bool { return e.stopped.Load() == 1 })
	if !e.gate.Draining() {
		t.Error("new requests must stay refused until the process ends")
	}
	if status, body := e.call("POST", "/admin/api/v1/apply/cancel", testToken, ""); status != 409 {
		t.Errorf("cancelling after the drain finished: %d %s", status, body)
	}
	audit, _ := os.ReadFile(filepath.Join(e.state, "audit.log"))
	if !strings.Contains(string(audit), `"action":"apply"`) {
		t.Errorf("apply must be audited: %s", audit)
	}
}

func TestApplyCutsRequestsThatOutlastTheDrainAndNamesThem(t *testing.T) {
	e := newAdminEnv(t, testToken)
	e.api.drain = 100 * time.Millisecond
	release := make(chan struct{})
	started := make(chan struct{})
	h := e.gate.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release }))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/slow", nil))
	<-started
	defer close(release)

	if status, _ := e.call("POST", "/admin/api/v1/apply", testToken, ""); status != http.StatusAccepted {
		t.Fatalf("apply: %d", status)
	}
	waitFor(t, "the server to stop", func() bool { return e.stopped.Load() == 1 })
	if !strings.Contains(e.logs.String(), "they are cut") || !strings.Contains(e.logs.String(), "count=1") {
		t.Errorf("cut requests must be logged with their number:\n%s", e.logs.String())
	}
}

func TestApplyCanBeCancelledDuringTheDrain(t *testing.T) {
	e := newAdminEnv(t, testToken)
	e.api.drain = 5 * time.Second
	release := make(chan struct{})
	started := make(chan struct{})
	h := e.gate.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release }))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/slow", nil))
	<-started
	defer close(release)

	e.call("POST", "/admin/api/v1/apply", testToken, "")
	if !e.gate.Draining() {
		t.Fatal("maintenance mode must start at once")
	}
	status, body := e.call("POST", "/admin/api/v1/apply/cancel", testToken, "")
	if status != 200 || !strings.Contains(body, "running") || e.gate.Draining() {
		t.Fatalf("cancel: %d %s draining=%v", status, body, e.gate.Draining())
	}
	time.Sleep(200 * time.Millisecond)
	if e.stopped.Load() != 0 {
		t.Fatal("a cancelled restart must not stop the server")
	}
	// and a new restart can be started afterwards
	if status, _ := e.call("POST", "/admin/api/v1/apply", testToken, ""); status != http.StatusAccepted {
		t.Fatalf("apply after cancel: %d", status)
	}
	e.call("POST", "/admin/api/v1/apply/cancel", testToken, "")
}

func TestCheckEndpointCallsTheChecker(t *testing.T) {
	e := newAdminEnv(t, testToken)
	status, body := e.call("POST", "/admin/api/v1/repositories/ZH/check", testToken, "")
	if status != 200 || !strings.Contains(body, `"ok": true`) || !strings.Contains(body, `"report": "report"`) {
		t.Fatalf("%d %s", status, body)
	}
}

// runRepos runs the one-off command with the given standard input and returns
// its exit code and standard output.
func runRepos(t *testing.T, cfgPath, stdin string, args ...string) (int, map[string]any) {
	t.Helper()
	oldIn, oldOut := os.Stdin, os.Stdout
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	os.Stdin, os.Stdout = inR, outW
	go func() { _, _ = inW.WriteString(stdin); inW.Close() }()
	code := reposCLI(cfgPath, args)
	outW.Close()
	raw, _ := io.ReadAll(outR)
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("output is not JSON: %s", raw)
	}
	if strings.Contains(string(raw), "one-off-secret") {
		t.Fatal("the secret was echoed")
	}
	return code, v
}

func TestOneOffCommandRunsTheSameOperations(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	body := "server: { interfaces: [rest], stateDir: \"" + filepath.ToSlash(state) + "\" }\ngraph: { tenantId: t, clientId: c, clientSecret: s }\nauth: { disabled: true }\n" +
		"repositories:\n  - { id: DMS, siteUrl: \"https://contoso.sharepoint.com/sites/x\" }\n"
	if err := os.WriteFile(cfgFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	code, v := runRepos(t, cfgFile, `{"id":"ZH","backend":"sharepoint","siteUrl":"https://contoso.sharepoint.com/sites/hr","tenantId":"t2","clientId":"c2","clientSecret":"one-off-secret"}`, "add")
	if code != 0 || v["ok"] != true {
		t.Fatalf("add: %d %v", code, v)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "secrets", "ZH", "client-secret")); strings.TrimSpace(string(b)) != "one-off-secret" {
		t.Fatalf("secret file %q", b)
	}
	code, v = runRepos(t, cfgFile, "", "list")
	if code != 0 || !strings.Contains(toJSON(v), `"ZH"`) {
		t.Fatalf("list: %d %v", code, v)
	}
	code, v = runRepos(t, cfgFile, "", "get", "ZH")
	if code != 0 || !strings.Contains(toJSON(v), `"secretSet":true`) {
		t.Fatalf("get: %d %v", code, v)
	}
	code, v = runRepos(t, cfgFile, `{"backend":"sharepoint","siteUrl":"https://contoso.sharepoint.com/sites/hr","rootPath":"X","tenantId":"t2","clientId":"c2"}`, "update", "ZH")
	if code != 0 {
		t.Fatalf("update: %v", v)
	}
	code, v = runRepos(t, cfgFile, `{"id":"ZH"`, "add")
	if code != 1 || v["ok"] != false {
		t.Fatalf("bad input must fail with exit 1 and a coded error: %d %v", code, v)
	}
	code, v = runRepos(t, cfgFile, "", "detach", "ZH")
	if code != 0 || !strings.Contains(toJSON(v), `"documentsDeleted":false`) {
		t.Fatalf("detach: %d %v", code, v)
	}
	code, v = runRepos(t, cfgFile, "", "audit", "10")
	if code != 0 || !strings.Contains(toJSON(v), `\"actor\":\"one-off\"`) {
		t.Fatalf("audit must name the one-off actor: %d %v", code, v)
	}
	if code, v = runRepos(t, cfgFile, "", "frobnicate"); code != 1 || v["ok"] != false {
		t.Fatalf("unknown operation: %d %v", code, v)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestPurgeIsNotAvailableThroughTheAdminAPI(t *testing.T) {
	e := newAdminEnv(t, testToken)
	e.call("POST", "/admin/api/v1/repositories", testToken, `{"id":"AD","backend":"local","local":{"path":"AD"}}`)
	for _, p := range [][2]string{
		{"POST", "/admin/api/v1/repositories/AD/purge"}, {"DELETE", "/admin/api/v1/repositories/AD/purge"},
		{"POST", "/admin/api/v1/purge"}, {"DELETE", "/admin/api/v1/repositories/AD/data"}, {"POST", "/admin/api/v1/repositories/AD/purge-plan"},
	} {
		if status, _ := e.call(p[0], p[1], testToken, `{}`); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d; a purge must not exist remotely", p[0], p[1], status)
		}
	}
	// a plain DELETE of the repository only detaches it and never touches documents
	if status, body := e.call("DELETE", "/admin/api/v1/repositories/AD", testToken, ""); status != 200 || !strings.Contains(body, `"documentsDeleted": false`) {
		t.Fatalf("%d %s", status, body)
	}
}

func TestOneOffPurgeNeedsTheExplicitFlagAndAMatchingPlan(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	body := "server: { interfaces: [rest], stateDir: \"" + filepath.ToSlash(state) + "\" }\ngraph: { tenantId: t, clientId: c, clientSecret: s }\nauth: { disabled: true }\n" +
		"repositories:\n  - { id: DMS, siteUrl: \"https://contoso.sharepoint.com/sites/x\" }\n"
	if err := os.WriteFile(cfgFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, v := runRepos(t, cfgFile, `{"id":"AD","backend":"local","local":{"path":"AD"}}`, "add"); code != 0 {
		t.Fatalf("add: %v", v)
	}
	if err := os.MkdirAll(filepath.Join(state, "data", "AD"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "data", "AD", "doc.txt"), []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, v := runRepos(t, cfgFile, "", "purge-plan", "AD"); code != 1 {
		t.Fatalf("an attached repository cannot be planned: %v", v)
	}
	if code, v := runRepos(t, cfgFile, "", "detach", "AD"); code != 0 {
		t.Fatalf("detach: %v", v)
	}
	code, v := runRepos(t, cfgFile, "", "purge-plan", "AD")
	if code != 0 {
		t.Fatalf("plan: %v", v)
	}
	phrase, _ := v["result"].(map[string]any)["challenge"].(string)
	if !strings.HasPrefix(phrase, "PURGE-AD-") {
		t.Fatalf("no challenge: %v", v)
	}
	// without the explicit flag nothing is deleted, even with the right phrase
	if code, _ := runRepos(t, cfgFile, "", "purge", "AD", "--typed-id", "AD", "--challenge", phrase); code != 1 {
		t.Fatal("a purge without --i-understand-this-destroys-documents must fail")
	}
	if code, _ := runRepos(t, cfgFile, "", "purge", "AD", "--typed-id", "AD", "--challenge", "PURGE-AD-0-000000000000", "--i-understand-this-destroys-documents"); code != 1 {
		t.Fatal("a wrong phrase must fail")
	}
	if _, err := os.Stat(filepath.Join(state, "data", "AD", "doc.txt")); err != nil {
		t.Fatal("a refused purge deleted something")
	}
	if code, v := runRepos(t, cfgFile, "", "purge", "AD", "--typed-id", "AD", "--challenge", phrase, "--i-understand-this-destroys-documents"); code != 0 {
		t.Fatalf("a complete, correct purge must work: %v", v)
	}
	if _, err := os.Stat(filepath.Join(state, "data", "AD")); err == nil {
		t.Fatal("the folder survived a correct purge")
	}
}
