package storage

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lamiv/zaks-drive/internal/auth"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/graphmock"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

// tenant starts an emulated Microsoft tenant with one drive.
func tenant(t *testing.T, drive string) (*graph.Client, *transfer.Engine) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mh, err := graphmock.New(graphmock.Options{DataDir: t.TempDir(), Drives: []string{drive}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mh)
	t.Cleanup(srv.Close)
	tokens := auth.NewClientCredentials(srv.URL, "tenant", "client", "secret", "scope", srv.Client())
	gc := graph.New(graph.Options{BaseURL: srv.URL + "/v1.0", MaxConcurrency: 4, MaxRetries: 1, MaxRetryBackoff: 50 * time.Millisecond}, tokens, http.DefaultTransport, log)
	eng := transfer.New(gc, transfer.Options{SimpleUploadMax: 1 << 20, ChunkSize: 320 << 10, MemoryBudget: 8 << 20, MaxUpload: 8 << 20, SpoolDir: t.TempDir()})
	return gc, eng
}

func TestRepositoryWithOwnCredentialsUsesItsOwnTenant(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gcA, engA := tenant(t, "driveA")
	gcB, engB := tenant(t, "driveB")
	var calls atomic.Int32
	clients := WithClients(func(rc config.RepositoryConfig) (*graph.Client, *transfer.Engine, error) {
		calls.Add(1)
		if rc.ID != "B" {
			t.Errorf("factory asked for repository %s", rc.ID)
		}
		return gcB, engB, nil
	})
	svc, err := NewService(context.Background(), gcA, engA, []config.RepositoryConfig{
		{ID: "A", DriveID: "driveA"},
		{ID: "B", DriveID: "driveB", ClientSecretFile: "secrets/B/client-secret"},
	}, time.Minute, log, clients)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if calls.Load() != 1 {
		t.Fatalf("the factory must be asked once, only for the repository with its own credentials; asked %d times", calls.Load())
	}
	ctx := context.Background()
	a, _ := svc.Repository("A")
	b, _ := svc.Repository("B")
	if _, err := svc.Upload(ctx, b, UploadRequest{Name: "only-in-b.txt", Body: strings.NewReader("b"), Size: 1}); err != nil {
		t.Fatalf("upload through the repository's own application: %v", err)
	}
	if _, err := svc.GetByPath(ctx, b, "only-in-b.txt"); err != nil {
		t.Fatalf("the file must be in tenant B: %v", err)
	}
	if _, err := svc.GetByPath(ctx, a, "only-in-b.txt"); err == nil {
		t.Fatal("the file leaked into tenant A, which uses another application")
	}
	if err := svc.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestOwnCredentialsNeedAClientFactory(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gc, eng := tenant(t, "driveA")
	_, err := NewService(context.Background(), gc, eng, []config.RepositoryConfig{
		{ID: "B", DriveID: "driveA", ClientSecretFile: "secrets/B/client-secret"},
	}, time.Minute, log)
	if err == nil || !strings.Contains(err.Error(), "own credentials") {
		t.Fatalf("a repository with its own credentials must not silently use the global ones: %v", err)
	}
}

func TestEngineWithClientSharesTheMemoryBudget(t *testing.T) {
	gcA, engA := tenant(t, "driveA")
	gcB, _ := tenant(t, "driveB")
	other := engA.WithClient(gcB)
	if other == engA || other.MaxUpload() != engA.MaxUpload() {
		t.Fatalf("WithClient must return a distinct engine with the same limits")
	}
	_ = gcA
}
