package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Lamiv/zaks-drive/internal/adminui"
	"github.com/Lamiv/zaks-drive/internal/config"
)

const uiPass = "ui-test-passphrase-123"

// cfgForUI is a configuration that holds synthetic secrets, to prove the
// settings view never shows them.
func cfgForUI() config.Config {
	c := config.Default()
	c.Server.AdminToken = "SYNTH-ADMIN-TOKEN-should-never-show"
	c.Graph.TenantID, c.Graph.ClientID, c.Graph.ClientSecret = "tenant-id-value", "client-id-value", "SYNTH-GRAPH-SECRET-should-never-show"
	c.Auth.APIKeys = []config.APIKeyConfig{{Name: "s4", Key: "SYNTH-API-KEY-should-never-show"}}
	c.ContentServer.Repositories = []config.ContentRepositoryConfig{{ContRep: "Z1", Repository: "DMS", Signature: "required"}}
	return c
}

// browser is an HTTP client with a cookie jar that behaves like the admin page:
// it sends its own origin and, once logged in, the CSRF token.
type browser struct {
	t    *testing.T
	e    *adminEnv
	c    *http.Client
	csrf string
}

func newBrowser(t *testing.T, e *adminEnv) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, e: e, c: &http.Client{Jar: jar}}
}

func (b *browser) do(method, path, body string, hdr map[string]string) (*http.Response, string) {
	b.t.Helper()
	req, _ := http.NewRequest(method, b.e.srv.URL+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func (b *browser) origin() map[string]string { return map[string]string{"Origin": b.e.srv.URL} }

func (b *browser) post(path, body string) (*http.Response, string) {
	h := b.origin()
	h["X-CSRF-Token"] = b.csrf
	return b.do("POST", path, body, h)
}

func (b *browser) login(user, pass string) (*http.Response, string) {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	resp, out := b.do("POST", "/admin/api/v1/session", string(body), b.origin())
	if resp.StatusCode == 200 {
		var v struct{ Csrf string }
		_ = json.Unmarshal([]byte(out), &v)
		b.csrf = v.Csrf
	}
	return resp, out
}

func (b *browser) setPassword(current, next string) (*http.Response, string) {
	body, _ := json.Marshal(map[string]string{"current": current, "new": next})
	resp, out := b.post("/admin/api/v1/session/password", string(body))
	if resp.StatusCode == 200 {
		var v struct{ Csrf string }
		_ = json.Unmarshal([]byte(out), &v)
		b.csrf = v.Csrf
	}
	return resp, out
}

// loggedIn logs in with admin/admin and chooses the first real password, as the page forces.
func (b *browser) loggedIn() *browser {
	b.t.Helper()
	if resp, out := b.login("admin", "admin"); resp.StatusCode != 200 {
		b.t.Fatalf("first login: %d %s", resp.StatusCode, out)
	}
	if resp, out := b.setPassword("admin", uiPass); resp.StatusCode != 200 {
		b.t.Fatalf("first password: %d %s", resp.StatusCode, out)
	}
	return b
}

// change is a state-changing call as the page makes it.
func (b *browser) change(method, path, body string) (int, string) {
	h := b.origin()
	h["X-CSRF-Token"] = b.csrf
	resp, out := b.do(method, path, body, h)
	return resp.StatusCode, out
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == adminui.CookieName {
			return c
		}
	}
	return nil
}

func TestUIIsServedWithoutLoginAndWithStrictHeaders(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e)
	resp, body := b.do("GET", "/admin/ui/", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "ZAKS Drive") {
		t.Fatalf("%d %s", resp.StatusCode, body[:min(200, len(body))])
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers: %v", resp.Header)
	}
	if resp, _ := b.do("GET", "/admin/api/v1/repositories", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the API without credentials: %d", resp.StatusCode)
	}
	if resp, _ := b.do("GET", "/admin/ui", "", nil); resp.Request.URL.Path != "/admin/ui/" {
		t.Errorf("the bare path must redirect to the page: %s", resp.Request.URL.Path)
	}
}

// The first login: admin/admin works but only to choose a new password.
func TestFirstLoginForcesANewPasswordBeforeAnythingElse(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e)

	resp, body := b.do("GET", "/admin/api/v1/session", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `"authenticated": false`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, body = b.login("admin", "admin")
	if resp.StatusCode != 200 || !strings.Contains(body, `"mustChangePassword": true`) || b.csrf == "" {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	first := sessionCookie(resp)
	if first == nil || !first.HttpOnly || first.SameSite != http.SameSiteStrictMode || first.Path != "/admin" || first.MaxAge <= 0 {
		t.Fatalf("the session cookie must be HttpOnly, SameSite=Strict and scoped: %+v", first)
	}
	if _, body := b.do("GET", "/admin/api/v1/session", "", nil); !strings.Contains(body, `"mustChangePassword": true`) {
		t.Fatalf("the page must be told to ask for a new password: %s", body)
	}

	// nothing else works with that session, reads included
	for _, p := range [][2]string{{"GET", "/admin/api/v1/repositories"}, {"GET", "/admin/api/v1/status"}, {"GET", "/admin/api/v1/logs"},
		{"GET", "/admin/api/v1/audit"}, {"GET", "/admin/api/v1/settings"}, {"GET", "/admin/api/v1/certificates"}, {"GET", "/admin/api/v1/diagnostics"}} {
		resp, out := b.do(p[0], p[1], "", nil)
		if resp.StatusCode != http.StatusForbidden || errCode(out) != "ADMIN_PASSWORD_CHANGE_REQUIRED" {
			t.Errorf("%s %s with the default password: %d %s", p[0], p[1], resp.StatusCode, out)
		}
	}
	for _, p := range [][2]string{{"POST", "/admin/api/v1/repositories"}, {"POST", "/admin/api/v1/apply"}, {"DELETE", "/admin/api/v1/repositories/AD"}, {"POST", "/admin/api/v1/certificates/activate"}} {
		if status, out := b.change(p[0], p[1], `{"id":"AD","backend":"local","local":{"path":"AD"}}`); status != http.StatusForbidden || errCode(out) != "ADMIN_PASSWORD_CHANGE_REQUIRED" {
			t.Errorf("%s %s with the default password: %d %s", p[0], p[1], status, out)
		}
	}
	if views, _ := e.api.mgr.List(); len(views) != 1 {
		t.Fatalf("a restricted session changed state: %+v", views)
	}

	// choosing the password: every refusal first
	if resp, out := b.setPassword("wrong", uiPass); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong current password: %d %s", resp.StatusCode, out)
	}
	for _, weak := range []string{"short", "admin", "administrator", "aaaaaaaaaaaaaaaa", "password1234"} {
		if resp, out := b.setPassword("admin", weak); resp.StatusCode != http.StatusBadRequest || errCode(out) != "ADMIN_PASSWORD_REJECTED" {
			t.Errorf("weak password %q: %d %s", weak, resp.StatusCode, out)
		}
	}
	if resp, _ := b.do("POST", "/admin/api/v1/session/password", `{"current":"admin","new":"`+uiPass+`"}`, b.origin()); resp.StatusCode != http.StatusForbidden {
		t.Errorf("without a CSRF token: %d", resp.StatusCode)
	}
	if resp, _ := b.do("POST", "/admin/api/v1/session/password", `{"current":"admin","new":"`+uiPass+`"}`,
		map[string]string{"Origin": "http://evil.example", "X-CSRF-Token": b.csrf}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("from another origin: %d", resp.StatusCode)
	}
	if !e.api.account.IsDefault() {
		t.Fatal("a refused change must leave the default state as it was")
	}

	resp, out := b.setPassword("admin", uiPass)
	if resp.StatusCode != 200 || !strings.Contains(out, `"mustChangePassword": false`) {
		t.Fatalf("the change: %d %s", resp.StatusCode, out)
	}
	second := sessionCookie(resp)
	if second == nil || second.Value == first.Value {
		t.Fatal("the session must be replaced by a new one after the change")
	}
	if status, _ := (&browser{t: t, e: e, c: &http.Client{}}).rawGET("/admin/api/v1/repositories", first.Value); status != http.StatusUnauthorized {
		t.Errorf("the old session must be dead: %d", status)
	}
	if e.api.account.IsDefault() {
		t.Fatal("the default state must be over")
	}
	if resp, _ := b.do("GET", "/admin/api/v1/repositories", "", nil); resp.StatusCode != 200 {
		t.Fatalf("after the change the session works: %d", resp.StatusCode)
	}

	// admin/admin is gone for good; the new password works; neither the token nor the old pair
	other := newBrowser(t, e)
	for _, c := range [][2]string{{"admin", "admin"}, {"admin", testToken}, {"root", uiPass}, {"admin", uiPass + "x"}} {
		if resp, _ := other.login(c[0], c[1]); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%v must not log in: %d", c, resp.StatusCode)
		}
	}
	if resp, out := other.login("admin", uiPass); resp.StatusCode != 200 || !strings.Contains(out, `"mustChangePassword": false`) {
		t.Fatalf("login with the new password: %d %s", resp.StatusCode, out)
	}

	// audit records the events with the source, never a password
	_, audit := b.do("GET", "/admin/api/v1/audit", "", nil)
	for _, a := range []string{`"action":"login"`, `"action":"password-changed"`, "mustChange=true"} {
		if !strings.Contains(audit, a) {
			t.Errorf("audit lacks %s: %s", a, audit)
		}
	}
	for _, secret := range []string{uiPass, `"admin","password"`, "wrong"} {
		if strings.Contains(audit, secret) || strings.Contains(e.logs.String(), uiPass) {
			t.Fatalf("a password reached the audit log or the server log (%q)", secret)
		}
	}
	// the password is stored hashed, apart from the definitions
	raw, _ := os.ReadFile(filepath.Join(e.state, adminui.AccountFile))
	if strings.Contains(string(raw), uiPass) || !strings.Contains(string(raw), "argon2id") {
		t.Fatalf("admin.json: %s", raw)
	}
}

func (b *browser) rawGET(path, cookie string) (int, string) {
	req, _ := http.NewRequest("GET", b.e.srv.URL+path, nil)
	req.AddCookie(&http.Cookie{Name: adminui.CookieName, Value: cookie})
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func TestUILoginRefusals(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e)
	good := `{"username":"admin","password":"admin"}`
	if resp, _ := b.do("POST", "/admin/api/v1/session", good, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a login without an Origin must be refused: %d", resp.StatusCode)
	}
	if resp, _ := b.do("POST", "/admin/api/v1/session", good, map[string]string{"Origin": "http://evil.example"}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-origin login must be refused: %d", resp.StatusCode)
	}
	if resp, out := b.login("admin", "wrong"); resp.StatusCode != http.StatusUnauthorized || sessionCookie(resp) != nil || errCode(out) != "AUTH_ADMIN_UNAUTHORIZED" {
		t.Errorf("wrong password: %d %s", resp.StatusCode, out)
	}
	if resp, out := b.login("nobody", "admin"); resp.StatusCode != http.StatusUnauthorized || strings.Contains(out, "nobody") {
		t.Errorf("wrong user must look exactly like a wrong password: %d %s", resp.StatusCode, out)
	}
	if resp, _ := b.do("POST", "/admin/api/v1/session", `not json`, b.origin()); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad body: %d", resp.StatusCode)
	}
	if resp, _ := b.do("POST", "/admin/api/v1/session", `{"username":"admin","password":"`+strings.Repeat("x", 10000)+`"}`, b.origin()); resp.StatusCode == 200 {
		t.Error("an oversized login body was accepted")
	}
	if resp, _ := b.do("POST", "/admin/api/v1/session/password", `{}`, b.origin()); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("changing a password without a session: %d", resp.StatusCode)
	}
}

func TestUILoginLockoutDoublesAndIsAudited(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e)
	retryAfter := func() int {
		resp, _ := b.login("admin", "admin")
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("expected the lockout, got %d", resp.StatusCode)
		}
		n, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return n
	}
	for i := 0; i < maxFailures; i++ {
		b.login("admin", "wrong-guess")
	}
	first := retryAfter() // even the right credentials wait
	if first < 55 || first > 62 {
		t.Fatalf("the first lockout lasts about a minute: %d", first)
	}
	if r, _ := b.do("GET", "/admin/api/v1/status", "", map[string]string{"Authorization": "Bearer " + testToken}); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the lockout covers the Bearer API too: %d", r.StatusCode)
	}
	expire := func() {
		e.api.failMu.Lock()
		e.api.blockedTo = time.Now().Add(-time.Second)
		e.api.failMu.Unlock()
	}
	expire()
	for i := 0; i < maxFailures; i++ {
		b.login("admin", "wrong-guess")
	}
	if second := retryAfter(); second < 115 || second > 122 {
		t.Fatalf("a repeated lockout must last longer (about 2 minutes): %d", second)
	}
	expire()
	if resp, _ := b.login("admin", "admin"); resp.StatusCode != 200 {
		t.Fatalf("after the lockout the right password works: %d", resp.StatusCode)
	}
	e.api.failMu.Lock()
	level := e.api.lockLevel
	e.api.failMu.Unlock()
	if level != 0 {
		t.Fatalf("a successful login resets the backoff: %d", level)
	}
	_, audit := e.call("GET", "/admin/api/v1/audit", testToken, "")
	if strings.Count(audit, `"action":"login-failed"`) != 2*maxFailures || strings.Contains(audit, "wrong-guess") {
		t.Fatalf("failed logins must be audited without the guessed value: %s", audit)
	}
}

func TestUIChangesNeedCSRFTokenAndOrigin(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e).loggedIn()
	body := `{"id":"AD","backend":"local","local":{"path":"AD"}}`

	cases := map[string]map[string]string{
		"no CSRF token, own origin":    {"Origin": e.srv.URL},
		"no origin, right CSRF token":  {"X-CSRF-Token": b.csrf},
		"foreign origin, right token":  {"Origin": "http://evil.example", "X-CSRF-Token": b.csrf},
		"own origin, wrong CSRF token": {"Origin": e.srv.URL, "X-CSRF-Token": "guess"},
		"own origin, empty CSRF token": {"Origin": e.srv.URL, "X-CSRF-Token": ""},
	}
	for name, h := range cases {
		resp, out := b.do("POST", "/admin/api/v1/repositories", body, h)
		if resp.StatusCode != http.StatusForbidden || errCode(out) != "ADMIN_CSRF_INVALID" {
			t.Errorf("%s: %d %s", name, resp.StatusCode, out)
		}
	}
	for _, m := range [][2]string{{"PUT", "/admin/api/v1/repositories/AD"}, {"DELETE", "/admin/api/v1/repositories/AD"}, {"POST", "/admin/api/v1/apply"}, {"POST", "/admin/api/v1/certificates/activate"}} {
		if resp, _ := b.do(m[0], m[1], `{}`, map[string]string{"Origin": e.srv.URL}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s without a CSRF token: %d", m[0], m[1], resp.StatusCode)
		}
	}
	if views, _ := e.api.mgr.List(); len(views) != 1 {
		t.Fatalf("a refused request changed state: %+v", views)
	}
	if status, out := b.change("POST", "/admin/api/v1/repositories", body); status != http.StatusCreated {
		t.Fatalf("a correct change must work: %d %s", status, out)
	}
	if status, _ := b.change("DELETE", "/admin/api/v1/repositories/AD", ""); status != 200 {
		t.Fatalf("detach: %d", status)
	}
}

func TestUIBearerTokenStillWorksForScriptsWithoutOrigin(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	// even while the UI still has the default password: the token is a separate credential
	if status, _ := e.call("POST", "/admin/api/v1/repositories", testToken, `{"id":"AD","backend":"local","local":{"path":"AD"}}`); status != http.StatusCreated {
		t.Fatalf("the command and scripts use the Bearer token: %d", status)
	}
	_, st := e.call("GET", "/admin/api/v1/status", testToken, "")
	if !strings.Contains(st, `"adminPasswordDefault": true`) {
		t.Fatalf("the status must tell the command that the default password is still active: %s", st)
	}
	b := newBrowser(t, e).loggedIn()
	if _, st := b.do("GET", "/admin/api/v1/status", "", nil); !strings.Contains(st, `"adminPasswordDefault": false`) {
		t.Fatalf("after the change: %s", st)
	}
}

func TestUILogoutAndSessionExpiry(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e).loggedIn()
	if resp, _ := b.do("DELETE", "/admin/api/v1/session", "", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("logout without an Origin: %d", resp.StatusCode)
	}
	if resp, _ := b.do("DELETE", "/admin/api/v1/session", "", b.origin()); resp.StatusCode != 200 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := b.do("GET", "/admin/api/v1/repositories", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after logout: %d", resp.StatusCode)
	}
	b2 := newBrowser(t, e)
	if resp, _ := b2.login("admin", uiPass); resp.StatusCode != 200 {
		t.Fatal("login")
	}
	e.api.sessions.Idle = time.Nanosecond
	time.Sleep(5 * time.Millisecond)
	if resp, _ := b2.do("GET", "/admin/api/v1/repositories", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an idle session must expire: %d", resp.StatusCode)
	}
}

func TestUIDisabledAndAccountProblems(t *testing.T) {
	off := buildAdminEnv(t, testToken, false)
	for _, p := range []string{"/admin/ui/", "/admin/api/v1/session"} {
		if resp, _ := newBrowser(t, off).do("GET", p, "", nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("with the UI disabled %s answered %d", p, resp.StatusCode)
		}
	}

	// no state directory: nowhere to keep a password, so the default pair must not work
	noState := buildAdminEnv(t, testToken, true)
	noState.api.account, _ = adminui.OpenAccount("")
	b := newBrowser(t, noState)
	if resp, out := b.login("admin", "admin"); resp.StatusCode != http.StatusServiceUnavailable || errCode(out) != "ADMIN_STATE_UNAVAILABLE" {
		t.Errorf("without a state directory: %d %s", resp.StatusCode, out)
	}

	// a corrupt record fails closed with a pointer to the reset, never to admin/admin
	locked := buildAdminEnv(t, testToken, true)
	must(t, os.MkdirAll(locked.state, 0o700))
	must(t, os.WriteFile(filepath.Join(locked.state, adminui.AccountFile), []byte("{broken"), 0o600))
	locked.api.account, _ = adminui.OpenAccount(locked.state)
	lb := newBrowser(t, locked)
	for _, pw := range []string{"admin", uiPass} {
		if resp, out := lb.login("admin", pw); resp.StatusCode != http.StatusLocked || errCode(out) != "ADMIN_ACCOUNT_LOCKED" || !strings.Contains(out, "reset-password") {
			t.Errorf("corrupt record: %d %s", resp.StatusCode, out)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// The reset: a NO_ADMIN file plus a restart returns to admin/admin with a forced change, and the
// server deletes the file once the new password is set.
func TestUIResetThroughNoAdminFile(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e).loggedIn()
	if resp, _ := newBrowser(t, e).login("admin", "admin"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("admin/admin must not work after the change")
	}
	time.Sleep(20 * time.Millisecond)
	must(t, adminui.RequestReset(e.state)) // what `zaks-drive admin reset-password` does

	// "restart": the server opens the account again and applies the request
	e.api.sessions = adminui.NewSessions() // sessions do not survive a restart
	acc, err := adminui.OpenAccount(e.state)
	must(t, err)
	reset, err := acc.ApplyReset()
	if err != nil || !reset {
		t.Fatalf("%v %v", reset, err)
	}
	e.api.account = acc
	if resp, _ := b.do("GET", "/admin/api/v1/repositories", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("the old session must not survive the restart")
	}
	other := newBrowser(t, e)
	if resp, _ := other.login("admin", uiPass); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("the old password must be gone")
	}
	if resp, out := other.login("admin", "admin"); resp.StatusCode != 200 || !strings.Contains(out, `"mustChangePassword": true`) {
		t.Fatalf("after a reset the default login works and demands a new password: %d %s", resp.StatusCode, out)
	}
	if _, err := os.Stat(filepath.Join(e.state, adminui.ResetFile)); err != nil {
		t.Fatal("NO_ADMIN must stay until a password is set")
	}
	if resp, out := other.setPassword("admin", "a brand new passphrase 99"); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if _, err := os.Stat(filepath.Join(e.state, adminui.ResetFile)); err == nil {
		t.Fatal("the server must delete NO_ADMIN once the new password is set")
	}
	if resp, _ := newBrowser(t, e).login("admin", "a brand new passphrase 99"); resp.StatusCode != 200 {
		t.Fatal("the new password works")
	}
}

func TestUILogsSettingsDiagnosticsAndCertificates(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e).loggedIn()

	e.api.log.Error("create failed", "request_id", "req-abc", "error", "Bearer SYNTH0123456789abcdefghijklmnop rejected")
	e.api.log.Info("plain line")
	_, logs := b.do("GET", "/admin/api/v1/logs?tail=50&level=INFO", "", nil)
	var entries []map[string]any
	if err := json.Unmarshal([]byte(logs), &entries); err != nil || len(entries) == 0 {
		t.Fatalf("logs: %v %s", err, logs)
	}
	if strings.Contains(logs, "SYNTH0123456789abcdefghijklmnop") || strings.Contains(logs, uiPass) {
		t.Fatalf("a credential reached the log view: %s", logs)
	}
	if _, byID := b.do("GET", "/admin/api/v1/logs?requestId=req-abc", "", nil); !strings.Contains(byID, "create failed") || strings.Contains(byID, "plain line") {
		t.Errorf("request ID filter: %s", byID)
	}

	_, settings := b.do("GET", "/admin/api/v1/settings", "", nil)
	for _, secret := range []string{"SYNTH-ADMIN-TOKEN", "SYNTH-GRAPH-SECRET", "SYNTH-API-KEY", "tenant-id-value", "client-id-value", uiPass} {
		if strings.Contains(settings, secret) {
			t.Fatalf("the settings view shows %q: %s", secret, settings)
		}
	}
	if !strings.Contains(settings, `"apiKeys": 1`) || !strings.Contains(settings, `"globalMicrosoftAppSet": true`) {
		t.Errorf("settings: %s", settings)
	}
	resp, diag := b.do("GET", "/admin/api/v1/diagnostics", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(diag, `"id": "fallback"`) || !strings.Contains(diag, `"id": "repositories"`) || !strings.Contains(diag, `"SKIPPED"`) {
		t.Fatalf("diagnostics: %d %s", resp.StatusCode, diag)
	}
	if _, certs := b.do("GET", "/admin/api/v1/certificates", "", nil); !strings.Contains(certs, `"value": []`) {
		t.Errorf("without the SAP interface running the list is empty: %s", certs)
	}
	if status, body := b.change("POST", "/admin/api/v1/certificates/activate", `{"contRep":"Z1","authId":"CN=X"}`); status != 404 || errCode(body) != "CLI_ADMIN_ROUTE_MISSING" {
		t.Errorf("activate without the SAP interface: %d %s", status, body)
	}
}

func TestUICreateWithSecretNeverReachesLogsOrResponses(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e).loggedIn()
	status, out := b.change("POST", "/admin/api/v1/repositories",
		`{"id":"ZH","backend":"sharepoint","siteUrl":"https://contoso.sharepoint.com/sites/hr","tenantId":"t2","clientId":"c2","clientSecret":"UI-SECRET-never-visible-123"}`)
	if status != http.StatusCreated || strings.Contains(out, "UI-SECRET") {
		t.Fatalf("%d %s", status, out)
	}
	for _, p := range []string{"/admin/api/v1/logs?level=DEBUG", "/admin/api/v1/audit", "/admin/api/v1/repositories", "/admin/api/v1/repositories/ZH", "/admin/api/v1/settings", "/admin/api/v1/diagnostics", "/admin/api/v1/status"} {
		if _, body := b.do("GET", p, "", nil); strings.Contains(body, "UI-SECRET") {
			t.Errorf("%s shows the secret", p)
		}
	}
	if strings.Contains(e.logs.String(), "UI-SECRET") {
		t.Error("the secret reached the server log")
	}
}

// Without ADMIN_TOKEN the command's Bearer calls are refused, but a logged-in UI session still works
// (the password is what protects it).
func TestUIWorksWithoutAnAdminTokenButBearerDoesNot(t *testing.T) {
	e := buildAdminEnv(t, "", true)
	if status, out := e.call("GET", "/admin/api/v1/repositories", "", ""); status != http.StatusServiceUnavailable || errCode(out) != "ADMIN_TOKEN_REQUIRED" {
		t.Fatalf("%d %s", status, out)
	}
	b := newBrowser(t, e).loggedIn()
	if resp, _ := b.do("GET", "/admin/api/v1/repositories", "", nil); resp.StatusCode != 200 {
		t.Fatalf("a session works without a token: %d", resp.StatusCode)
	}
}
