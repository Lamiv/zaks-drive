package drain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestsRunNormallyUntilDraining(t *testing.T) {
	g := New(30 * time.Second)
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
	if !g.Begin() || g.Begin() {
		t.Fatal("Begin must report true once")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("a draining server must answer 503 with Retry-After: %d %v", rec.Code, rec.Header())
	}
	g.Cancel()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusNoContent || g.Draining() {
		t.Fatalf("after Cancel service must resume: %d", rec.Code)
	}
}

func TestRunningRequestsFinishAndAreCounted(t *testing.T) {
	g := New(time.Second)
	release := make(chan struct{})
	started := make(chan struct{})
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		done <- rec.Code
	}()
	<-started
	g.Begin()
	if g.InFlight() != 1 {
		t.Fatalf("in flight %d", g.InFlight())
	}
	if left := g.Wait(context.Background(), 60*time.Millisecond); left != 1 {
		t.Fatalf("Wait must report the request that outlasted the drain: %d", left)
	}
	if _, total := g.Running(); total != 1 {
		t.Fatalf("running %d", total)
	}
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("an accepted request must complete normally, got %d", code)
	}
	if left := g.Wait(context.Background(), time.Second); left != 0 {
		t.Fatalf("after completion %d remain", left)
	}
}

func TestWaitHonoursTheContext(t *testing.T) {
	g := New(time.Second)
	g.mu.Lock()
	g.active[1] = "id-1"
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	if left := g.Wait(ctx, time.Minute); left != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("left=%d after %s", left, time.Since(start))
	}
	ids, total := g.Running()
	if total != 1 || len(ids) != 1 || ids[0] != "id-1" {
		t.Fatalf("%v %d", ids, total)
	}
}
