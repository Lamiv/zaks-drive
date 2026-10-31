package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/Lamiv/zaks-drive/internal/config"
)

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

func (b *browser) login(token string) (*http.Response, string) {
	resp, body := b.do("POST", "/admin/api/v1/session", `{"token":"`+token+`"}`, b.origin())
	if resp.StatusCode == 200 {
		var v struct{ Csrf string }
		_ = json.Unmarshal([]byte(body), &v)
		b.csrf = v.Csrf
	}
	return resp, body
}

// change is a state-changing call as the page makes it.
func (b *browser) change(method, path, body string) (int, string) {
	h := b.origin()
	h["X-CSRF-Token"] = b.csrf
	resp, out := b.do(method, path, body, h)
	return resp.StatusCode, out
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
	// the page itself holds no data: every API call needs a session or the token
	if resp, _ := b.do("GET", "/admin/api/v1/repositories", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the API without credentials: %d", resp.StatusCode)
	}
	if resp, _ := b.do("GET", "/admin/ui", "", nil); resp.Request.URL.Path != "/admin/ui/" {
		t.Errorf("the bare path must redirect to the page: %s", resp.Request.URL.Path)
	}
}

func TestUILoginSetsAHardenedSessionCookie(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e)

	// a session probe without a login is harmless and counts as no failed attempt
	resp, body := b.do("GET", "/admin/api/v1/session", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `"authenticated": false`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, body = b.login(testToken)
	if resp.StatusCode != 200 || b.csrf == "" {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "zd_session" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/admin" || cookie.MaxAge <= 0 {
		t.Fatalf("the session cookie must be HttpOnly, SameSite=Strict and scoped: %+v", cookie)
	}
	if strings.Contains(body, testToken) || strings.Contains(cookie.Value, testToken) {
		t.Fatal("the token must never be echoed or stored in the cookie")
	}
	// reading works with the cookie alone; the page can recover its CSRF token after a reload
	if resp, body := b.do("GET", "/admin/api/v1/repositories", "", nil); resp.StatusCode != 200 {
		t.Fatalf("read with the session: %d %s", resp.StatusCode, body)
	}
	if _, body := b.do("GET", "/admin/api/v1/session", "", nil); !strings.Contains(body, `"authenticated": true`) || !strings.Contains(body, b.csrf) {
		t.Fatalf("session status: %s", body)
	}
	// audit: the login is recorded with the source, never the token
	_, audit := b.do("GET", "/admin/api/v1/audit", "", nil)
	if !strings.Contains(audit, `"action":"login"`) || strings.Contains(audit, testToken) {
		t.Fatalf("audit: %s", audit)
	}
}

func TestUILoginRefusals(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e)
	if resp, _ := b.do("POST", "/admin/api/v1/session", `{"token":"`+testToken+`"}`, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a login without an Origin must be refused: %d", resp.StatusCode)
	}
	if resp, _ := b.do("POST", "/admin/api/v1/session", `{"token":"`+testToken+`"}`, map[string]string{"Origin": "http://evil.example"}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-origin login must be refused: %d", resp.StatusCode)
	}
	if resp, body := b.login("wrong"); resp.StatusCode != http.StatusUnauthorized || len(resp.Cookies()) != 0 || errCode(body) != "AUTH_ADMIN_UNAUTHORIZED" {
		t.Errorf("wrong token: %d %s", resp.StatusCode, body)
	}
	if resp, _ := b.do("POST", "/admin/api/v1/session", `not json`, b.origin()); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad body: %d", resp.StatusCode)
	}
	if resp, _ := b.do("POST", "/admin/api/v1/session", `{"token":"`+strings.Repeat("x", 10000)+`"}`, b.origin()); resp.StatusCode == 200 {
		t.Error("an oversized login body was accepted")
	}
}

func TestUILoginIsRateLimitedAndAudited(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e)
	for i := 0; i < maxFailures; i++ {
		b.login("wrong-guess")
	}
	resp, _ := b.login(testToken)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("after %d wrong tokens even the right one must wait: %d", maxFailures, resp.StatusCode)
	}
	// the lockout covers the API as well
	if r, _ := b.do("GET", "/admin/api/v1/status", "", map[string]string{"Authorization": "Bearer " + testToken}); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the API during the lockout: %d", r.StatusCode)
	}
	e.api.failMu.Lock()
	e.api.blockedTo = time.Now().Add(-time.Second)
	e.api.failMu.Unlock()
	_, audit := e.call("GET", "/admin/api/v1/audit", testToken, "")
	if strings.Count(audit, `"action":"login-failed"`) != maxFailures || strings.Contains(audit, "wrong-guess") {
		t.Fatalf("failed logins must be audited without the guessed value: %s", audit)
	}
}

func TestUIChangesNeedCSRFTokenAndOrigin(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e)
	b.login(testToken)
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
	if _, err := e.api.mgr.List(); err != nil {
		t.Fatal(err)
	}
	if views, _ := e.api.mgr.List(); len(views) != 1 { // only the static repository
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
	if status, _ := e.call("POST", "/admin/api/v1/repositories", testToken, `{"id":"AD","backend":"local","local":{"path":"AD"}}`); status != http.StatusCreated {
		t.Fatalf("the command and scripts use the Bearer token: %d", status)
	}
}

func TestUILogoutAndSessionExpiry(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e)
	b.login(testToken)
	if resp, _ := b.do("DELETE", "/admin/api/v1/session", "", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("logout without an Origin: %d", resp.StatusCode)
	}
	if resp, _ := b.do("DELETE", "/admin/api/v1/session", "", b.origin()); resp.StatusCode != 200 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := b.do("GET", "/admin/api/v1/repositories", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after logout: %d", resp.StatusCode)
	}
	// a stolen cookie value does not survive logout
	b2 := newBrowser(t, e)
	b2.login(testToken)
	e.api.sessions.Idle = time.Nanosecond
	time.Sleep(5 * time.Millisecond)
	if resp, _ := b2.do("GET", "/admin/api/v1/repositories", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an idle session must expire: %d", resp.StatusCode)
	}
}

func TestUIDisabledAndTokenless(t *testing.T) {
	off := buildAdminEnv(t, testToken, false)
	for _, p := range []string{"/admin/ui/", "/admin/api/v1/session"} {
		if resp, _ := newBrowser(t, off).do("GET", p, "", nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("with the UI disabled %s answered %d", p, resp.StatusCode)
		}
	}
	noToken := buildAdminEnv(t, "", true)
	b := newBrowser(t, noToken)
	resp, body := b.do("POST", "/admin/api/v1/session", `{"token":""}`, b.origin())
	if resp.StatusCode != http.StatusServiceUnavailable || errCode(body) != "ADMIN_TOKEN_REQUIRED" {
		t.Fatalf("without an ADMIN_TOKEN nobody can log in, not even with an empty token: %d %s", resp.StatusCode, body)
	}
}

func TestUILogsSettingsDiagnosticsAndCertificates(t *testing.T) {
	e := buildAdminEnv(t, testToken, true)
	b := newBrowser(t, e)
	b.login(testToken)

	// a secret travels through a create call and an error: it must not reach the ring
	e.api.log.Error("create failed", "request_id", "req-abc", "error", "Bearer SYNTH0123456789abcdefghijklmnop rejected")
	e.api.log.Info("plain line")
	_, logs := b.do("GET", "/admin/api/v1/logs?tail=50&level=INFO", "", nil)
	var entries []map[string]any
	if err := json.Unmarshal([]byte(logs), &entries); err != nil || len(entries) == 0 {
		t.Fatalf("logs: %v %s", err, logs)
	}
	if strings.Contains(logs, "SYNTH0123456789abcdefghijklmnop") {
		t.Fatalf("a credential reached the log view: %s", logs)
	}
	if _, byID := b.do("GET", "/admin/api/v1/logs?requestId=req-abc", "", nil); !strings.Contains(byID, "create failed") || strings.Contains(byID, "plain line") {
		t.Errorf("request ID filter: %s", byID)
	}

	_, settings := b.do("GET", "/admin/api/v1/settings", "", nil)
	for _, secret := range []string{"SYNTH-ADMIN-TOKEN", "SYNTH-GRAPH-SECRET", "SYNTH-API-KEY", "tenant-id-value", "client-id-value"} {
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
	b := newBrowser(t, e)
	b.login(testToken)
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
