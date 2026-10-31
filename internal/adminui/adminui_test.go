package adminui

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestAssetsAreServedWithStrictHeaders(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	for _, p := range []string{"/admin/ui/", "/admin/ui/app.js", "/admin/ui/style.css"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		h := resp.Header
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "connect-src 'self'", "base-uri 'none'", "form-action 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP lacks %q: %s", p, want, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "*") {
			t.Errorf("%s: CSP is too loose: %s", p, csp)
		}
		if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" ||
			h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: missing security headers: %v", p, h)
		}
	}
	req, _ := http.NewRequest("POST", srv.URL+"/admin/ui/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to the UI: %d", resp.StatusCode)
	}
	if resp, _ := http.Get(srv.URL + "/admin/ui/../../etc/passwd"); resp != nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Error("a path outside the assets was served")
		}
	}
}

// The page must load nothing from outside and carry nothing that the CSP
// would have to allow: no inline script or style, no event-handler attributes,
// no external URL, and the script must not use the sinks that turn text into markup.
func TestAssetsReferenceOnlyThemselvesAndAvoidDangerousSinks(t *testing.T) {
	files := Assets()
	for _, want := range []string{"index.html", "app.js", "style.css"} {
		if _, ok := files[want]; !ok {
			t.Fatalf("asset %s is missing", want)
		}
	}
	external := regexp.MustCompile(`(?i)(https?:)?//[a-z0-9.-]+\.[a-z]{2,}`)
	for name, b := range files {
		s := string(b)
		// URLs inside hint text such as contoso.sharepoint.com placeholders are fine; real fetches are not.
		for _, m := range external.FindAllString(s, -1) {
			if !strings.Contains(m, "contoso.sharepoint.com") {
				t.Errorf("%s references %q", name, m)
			}
		}
		if strings.Contains(strings.ToLower(s), "@import") {
			t.Errorf("%s imports another stylesheet", name)
		}
	}
	html := string(files["index.html"])
	for _, bad := range []string{"<style", "style=", "onclick=", "onload=", "onerror=", "javascript:"} {
		if strings.Contains(strings.ToLower(html), bad) {
			t.Errorf("index.html contains %q, which the CSP would block or which signals inline code", bad)
		}
	}
	for _, m := range regexp.MustCompile(`(?is)<script[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(m, "src=") {
			t.Errorf("inline script tag: %s", m)
		}
	}
	js := string(files["app.js"])
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setTimeout(\"", "localStorage", "sessionStorage", "document.cookie"} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js uses %s", bad)
		}
	}
	if !strings.Contains(js, "X-CSRF-Token") {
		t.Error("the script must send the CSRF token with changes")
	}
	if !strings.Contains(js, `autocomplete: "new-password"`) || !strings.Contains(js, `type: "password"`) {
		t.Error("the secret field must be a password field that the browser does not fill or remember")
	}
}

func TestSessionLifetimeAndRevocation(t *testing.T) {
	s := NewSessions()
	now := time.Now()
	s.now = func() time.Time { return now }
	id, csrf := s.Create()
	if id == "" || csrf == "" || id == csrf || len(id) < 40 {
		t.Fatalf("weak identifiers: %q %q", id, csrf)
	}
	if got, ok := s.Lookup(id); !ok || got != csrf {
		t.Fatal("a new session must be valid")
	}
	if _, ok := s.Lookup("guess"); ok {
		t.Fatal("an unknown ID was accepted")
	}
	now = now.Add(s.Idle - time.Minute)
	if _, ok := s.Lookup(id); !ok {
		t.Fatal("activity must extend the idle time")
	}
	now = now.Add(s.Idle + time.Minute)
	if _, ok := s.Lookup(id); ok {
		t.Fatal("an idle session must expire")
	}
	id2, _ := s.Create()
	for i := 0; i < 20; i++ { // keep it active; the absolute limit still applies
		now = now.Add(s.Idle - time.Minute)
		s.Lookup(id2)
	}
	if _, ok := s.Lookup(id2); ok {
		t.Fatal("a session must not outlive its absolute lifetime")
	}
	id3, _ := s.Create()
	s.Delete(id3)
	if _, ok := s.Lookup(id3); ok {
		t.Fatal("a logged-out session is still valid")
	}
}

func TestSessionStoreIsBounded(t *testing.T) {
	s := NewSessions()
	var first string
	for i := 0; i < maxSessions+10; i++ {
		id, _ := s.Create()
		if i == 0 {
			first = id
		}
	}
	if len(s.m) > maxSessions {
		t.Fatalf("%d sessions kept", len(s.m))
	}
	if _, ok := s.Lookup(first); ok {
		t.Fatal("the oldest session should have been dropped")
	}
}

func TestCookieAttributes(t *testing.T) {
	c := Cookie("abc", false, time.Hour)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/admin" || c.Secure {
		t.Fatalf("%+v", c)
	}
	if !Cookie("abc", true, time.Hour).Secure {
		t.Fatal("the cookie must be Secure over TLS")
	}
}

func TestSameOriginAndCSRFChecks(t *testing.T) {
	mk := func(origin string) *http.Request {
		r := httptest.NewRequest("POST", "http://127.0.0.1:9090/admin/api/v1/apply", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	if !SameOrigin(mk("http://127.0.0.1:9090")) {
		t.Error("the page's own origin must pass")
	}
	for _, o := range []string{"", "http://evil.example", "http://127.0.0.1:9091", "null", "http://127.0.0.1:9090.evil.example"} {
		if SameOrigin(mk(o)) {
			t.Errorf("origin %q accepted", o)
		}
	}
	r := mk("")
	r.Header.Set("X-CSRF-Token", "secret-token")
	if !CSRFMatches(r, "secret-token") || CSRFMatches(r, "other") || CSRFMatches(mk(""), "") {
		t.Error("CSRF comparison wrong")
	}
}

func TestRingKeepsRecentEventsRedactsAndFilters(t *testing.T) {
	ring := NewRing(100)
	var passed []string
	next := slog.NewTextHandler(writerFunc(func(p []byte) { passed = append(passed, string(p)) }), nil)
	log := slog.New(ring.Handler(next)).With("component", "test")
	for i := 0; i < 130; i++ {
		log.Info("event", "n", i)
	}
	log.Error("failed", "request_id", "req-123", "error", "Bearer abcdefghijklmnopqrstuvwxyz0123456789 was rejected")
	log.Debug("noise")

	all := ring.Entries(0, slog.LevelDebug, "")
	if len(all) != 100 {
		t.Fatalf("the ring must be bounded: %d entries", len(all))
	}
	if last := all[len(all)-1]; last.Msg != "failed" || !strings.Contains(last.Attrs, "component=test") {
		t.Fatalf("newest entry: %+v", last)
	}
	if got := ring.Entries(5, slog.LevelInfo, ""); len(got) != 5 {
		t.Fatalf("tail: %d", len(got))
	}
	if errs := ring.Entries(0, slog.LevelError, ""); len(errs) != 1 || errs[0].Msg != "failed" {
		t.Fatalf("level filter: %+v", errs)
	}
	if byID := ring.Entries(0, slog.LevelDebug, "req-123"); len(byID) != 1 {
		t.Fatalf("request ID filter: %+v", byID)
	}
	for _, e := range all {
		if strings.Contains(e.Attrs, "abcdefghijklmnopqrstuvwxyz0123456789") {
			t.Fatalf("a credential reached the ring: %+v", e)
		}
	}
	if len(passed) == 0 {
		t.Fatal("events must still reach the real handler")
	}
	if ctx := context.Background(); !ring.Handler(next).Enabled(ctx, slog.LevelInfo) {
		t.Fatal("Enabled must follow the real handler")
	}
}

func TestRingBoundsFieldLength(t *testing.T) {
	ring := NewRing(100)
	log := slog.New(ring.Handler(slog.NewTextHandler(writerFunc(func([]byte) {}), nil)))
	log.Info(strings.Repeat("m", 5000), "k", strings.Repeat("v", 5000))
	e := ring.Entries(0, slog.LevelInfo, "")[0]
	if len(e.Msg) > maxField+5 || len(e.Attrs) > maxField+10 {
		t.Fatalf("lengths %d %d", len(e.Msg), len(e.Attrs))
	}
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }
