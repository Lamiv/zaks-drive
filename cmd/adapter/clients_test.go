package main

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/Lamiv/zaks-drive/internal/config"
)

func TestAppClientsAreSharedPerApplication(t *testing.T) {
	a := newAppClients(config.Default(), http.DefaultTransport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rc := func(id, tenant, client, secret string) config.RepositoryConfig {
		return config.RepositoryConfig{ID: id, TenantID: tenant, ClientID: client, ClientSecret: secret}
	}
	one := a.get(rc("ZH", "t1", "c1", "s1"))
	if again := a.get(rc("ZI", "t1", "c1", "s1")); again != one {
		t.Error("repositories using the same application must share one client")
	}
	if a.get(rc("ZJ", "t2", "c1", "s1")) == one {
		t.Error("another tenant must not share the client")
	}
	if a.get(rc("ZK", "t1", "c2", "s1")) == one {
		t.Error("another client ID must not share the client")
	}
	if a.get(rc("ZL", "t1", "c1", "rotated")) == one {
		t.Error("a different secret for the same client ID must not silently reuse the old one")
	}
}
