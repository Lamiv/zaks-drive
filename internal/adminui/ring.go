package adminui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Lamiv/zaks-drive/internal/safelog"
)

// Entry is one log event as the UI shows it.
type Entry struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Attrs string    `json:"attrs,omitempty"`
}

// Ring keeps the most recent log events in memory so the UI can show them
// without access to Docker's log store. It sits behind the existing logging
// policy: it only sees what the server already logs, and it additionally
// passes every value through the same redaction as the support bundle. It is
// lost on restart.
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	lvl  []slog.Level
	next int
	full bool
}

// NewRing returns a ring of n entries.
func NewRing(n int) *Ring {
	if n < 100 {
		n = 100
	}
	return &Ring{buf: make([]Entry, n), lvl: make([]slog.Level, n)}
}

const maxField = 400

func clean(s string) string {
	s = safelog.Redact(safelog.OneLine(s))
	if len(s) > maxField {
		s = s[:maxField] + "..."
	}
	return s
}

func (r *Ring) add(e Entry, l slog.Level) {
	r.mu.Lock()
	r.buf[r.next], r.lvl[r.next] = e, l
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Entries returns up to n entries, oldest first, at or above min, and (when
// requestID is not empty) only those that mention it.
func (r *Ring) Entries(n int, min slog.Level, requestID string) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	size := r.next
	start := 0
	if r.full {
		size, start = len(r.buf), r.next
	}
	var out []Entry
	for i := 0; i < size; i++ {
		idx := (start + i) % len(r.buf)
		if r.lvl[idx] < min {
			continue
		}
		e := r.buf[idx]
		if requestID != "" && !strings.Contains(e.Attrs, requestID) && !strings.Contains(e.Msg, requestID) {
			continue
		}
		out = append(out, e)
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// Handler returns a slog handler that records every event in the ring and
// passes it on to next.
func (r *Ring) Handler(next slog.Handler) slog.Handler { return &ringHandler{ring: r, next: next} }

type ringHandler struct {
	ring  *Ring
	next  slog.Handler
	attrs []slog.Attr
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	var parts []string
	add := func(a slog.Attr) bool {
		parts = append(parts, a.Key+"="+clean(fmt.Sprint(a.Value.Resolve().Any())))
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	rec.Attrs(add)
	h.ring.add(Entry{Time: rec.Time.UTC(), Level: rec.Level.String(), Msg: clean(rec.Message), Attrs: strings.Join(parts, " ")}, rec.Level)
	return h.next.Handle(ctx, rec)
}

func (h *ringHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &ringHandler{ring: h.ring, next: h.next.WithAttrs(a), attrs: append(append([]slog.Attr{}, h.attrs...), a...)}
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	return &ringHandler{ring: h.ring, next: h.next.WithGroup(name), attrs: h.attrs}
}
