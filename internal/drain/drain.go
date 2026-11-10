// Package drain puts a server into maintenance mode: new requests are
// answered 503 with Retry-After (SAP retries those, as for any unavailable
// content server) while the requests already running are allowed to finish.
package drain

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Lamiv/zaks-drive/internal/observability"
)

// Gate counts requests and can refuse new ones. The zero value is not
// usable; use New.
type Gate struct {
	mu       sync.Mutex
	draining bool
	next     uint64
	active   map[uint64]string // request ID (may be empty) by sequence
	retry    time.Duration
}

// New returns a gate that tells refused callers to retry after retryAfter.
func New(retryAfter time.Duration) *Gate {
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return &Gate{active: map[uint64]string{}, retry: retryAfter}
}

// Wrap counts the requests of next and refuses new ones while draining.
// Place it inside the logging middleware so the request ID is known.
func (g *Gate) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq, ok := g.enter(observability.RequestID(r.Context()))
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(g.retry.Seconds())))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("the server is restarting to apply a change; retry shortly\n"))
			return
		}
		defer g.leave(seq)
		next.ServeHTTP(w, r)
	})
}

func (g *Gate) enter(id string) (uint64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining {
		return 0, false
	}
	g.next++
	g.active[g.next] = id
	return g.next, true
}

func (g *Gate) leave(seq uint64) {
	g.mu.Lock()
	delete(g.active, seq)
	g.mu.Unlock()
}

// Begin starts refusing new requests. It reports false when it was already
// draining.
func (g *Gate) Begin() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	was := g.draining
	g.draining = true
	return !was
}

// Cancel resumes normal service.
func (g *Gate) Cancel() {
	g.mu.Lock()
	g.draining = false
	g.mu.Unlock()
}

// Draining reports whether new requests are being refused.
func (g *Gate) Draining() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.draining
}

// InFlight is the number of requests currently running.
func (g *Gate) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.active)
}

// Running returns the IDs of the requests still running (those without an
// ID are counted but not listed), sorted.
func (g *Gate) Running() (ids []string, total int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, id := range g.active {
		if id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, len(g.active)
}

// Wait blocks until no request is running, the timeout passes or ctx ends,
// and returns how many requests were still running.
func (g *Gate) Wait(ctx context.Context, timeout time.Duration) int {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if n := g.InFlight(); n == 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			return g.InFlight()
		case <-deadline.C:
			return g.InFlight()
		case <-tick.C:
		}
	}
}
