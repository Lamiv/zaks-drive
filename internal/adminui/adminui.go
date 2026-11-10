// Package adminui serves the admin UI (RFC 0002): a few static files embedded
// in the binary, the session layer that exchanges the admin token for a
// short-lived cookie, and the log ring buffer behind the UI's log view.
//
// There is no front-end toolchain and nothing is loaded from outside: the
// page references only its own files and a strict Content-Security-Policy
// forbids inline scripts and styles, framing and any other origin.
package adminui

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed static
var assets embed.FS

// Path is where the UI is served on the admin listener.
const Path = "/admin/ui/"

// CookieName is the session cookie.
const CookieName = "zd_session"

// csp allows only the page's own files. No inline script or style, no
// framing, no forms posting elsewhere, no plugins.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// SecureHeaders sets the headers every UI and admin API response carries.
func SecureHeaders(h http.Header) {
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// Handler serves the embedded UI files under Path.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err) // the embed is part of the binary
	}
	files := http.StripPrefix(Path, http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SecureHeaders(w.Header())
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// Assets lists the embedded files (tests check what they reference).
func Assets() map[string][]byte {
	out := map[string][]byte{}
	_ = fs.WalkDir(assets, "static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := assets.ReadFile(p)
			out[strings.TrimPrefix(p, "static/")] = b
		}
		return nil
	})
	return out
}

// ---- sessions ----

// Sessions holds the logged-in browser sessions in memory. They are lost on
// restart (the administrator logs in again), have a short idle lifetime and
// an absolute one, and carry the CSRF token that state-changing requests must
// repeat in a header.
type Sessions struct {
	mu   sync.Mutex
	m    map[string]*session
	seq  uint64
	Idle time.Duration
	Max  time.Duration
	now  func() time.Time
}

type session struct {
	csrf string
	seq  uint64 // creation order, for evicting the oldest
	// mustChange marks a session opened with the default or reset password: it may
	// only change the password.
	mustChange    bool
	created, last time.Time
}

const maxSessions = 20

// NewSessions returns a store with a 30 minute idle and 8 hour absolute lifetime.
func NewSessions() *Sessions {
	return &Sessions{m: map[string]*session{}, Idle: 30 * time.Minute, Max: 8 * time.Hour, now: time.Now}
}

func token() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Create starts a session and returns its ID (the cookie value) and CSRF token.
func (s *Sessions) Create(mustChange bool) (id, csrf string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, v := range s.m {
		if s.expired(v, now) {
			delete(s.m, k)
		}
	}
	for len(s.m) >= maxSessions { // drop the oldest
		oldest, lowest := "", ^uint64(0)
		for k, v := range s.m {
			if v.seq < lowest {
				oldest, lowest = k, v.seq
			}
		}
		delete(s.m, oldest)
	}
	id, csrf = token(), token()
	s.seq++
	s.m[id] = &session{csrf: csrf, seq: s.seq, created: now, last: now, mustChange: mustChange}
	return id, csrf
}

func (s *Sessions) expired(v *session, now time.Time) bool {
	return now.Sub(v.last) > s.Idle || now.Sub(v.created) > s.Max
}

// Lookup returns the CSRF token of a valid session and extends its idle time.
func (s *Sessions) Lookup(id string) (csrf string, mustChange, ok bool) {
	if id == "" {
		return "", false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
	if !ok {
		return "", false, false
	}
	now := s.now()
	if s.expired(v, now) {
		delete(s.m, id)
		return "", false, false
	}
	v.last = now
	return v.csrf, v.mustChange, true
}

// Delete ends a session.
func (s *Sessions) Delete(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// Cookie builds the session cookie: HttpOnly, SameSite=Strict, scoped to the
// admin paths, Secure when served over TLS.
func Cookie(id string, secure bool, maxAge time.Duration) *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: id, Path: "/admin", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(maxAge.Seconds())}
}

// SameOrigin reports whether a state-changing request comes from the page
// the server itself served: an Origin header that names this host. A request
// without an Origin header is not accepted for cookie-authenticated changes.
func SameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// CSRFMatches compares the header with the session's token in constant time.
func CSRFMatches(r *http.Request, csrf string) bool {
	got := r.Header.Get("X-CSRF-Token")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(csrf)) == 1
}

// ReplaceAll ends every session and starts one new one: used after a password
// change, so a session that an attacker may hold does not survive it.
func (s *Sessions) ReplaceAll(mustChange bool) (id, csrf string) {
	s.mu.Lock()
	s.m = map[string]*session{}
	s.mu.Unlock()
	return s.Create(mustChange)
}
