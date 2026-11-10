package accesscheck

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Lamiv/zaks-drive/internal/auth"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/graphmock"
)

func newEnv(t *testing.T) (*graph.Client, auth.TokenSource, string) {
	t.Helper()
	mh, err := graphmock.New(graphmock.Options{DataDir: t.TempDir(), Drives: []string{"drive1"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mh)
	t.Cleanup(srv.Close)
	tokens := auth.NewClientCredentials(srv.URL, "tenant", "client", "secret", "scope", srv.Client())
	gc := graph.New(graph.Options{BaseURL: srv.URL + "/v1.0", MaxConcurrency: 8, MaxRetries: 1, MaxRetryBackoff: 50 * time.Millisecond},
		tokens, http.DefaultTransport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return gc, tokens, srv.URL
}

func cfgFor(repos ...config.RepositoryConfig) config.Config {
	var c config.Config
	c.Graph.ClientSecret = "secret"
	c.Repositories = repos
	return c
}

func TestAllChecksPass(t *testing.T) {
	gc, tokens, _ := newEnv(t)
	var out bytes.Buffer
	code := Run(context.Background(), cfgFor(config.RepositoryConfig{ID: "DMS", DriveID: "drive1", RootPath: "SAP_DMS"}), gc, tokens, &out)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	for _, want := range []string{"[PASS] Application permission in the token", "[PASS] [DMS] Write: upload a small file",
		"[PASS] [DMS] Read: download and compare", "[PASS] [DMS] Delete the test file", "passed 10, failed 0, skipped 0", "library URL: https://contoso.sharepoint.com/sites/sap/drive1 | root folder: SAP_DMS"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "secret") && strings.Contains(out.String(), "secret length") == false {
		t.Error("secret may have leaked into the report")
	}
}

func TestMissingLibraryFailsAndSkipsDependents(t *testing.T) {
	gc, tokens, _ := newEnv(t)
	var out bytes.Buffer
	code := Run(context.Background(), cfgFor(config.RepositoryConfig{ID: "DMS", DriveID: "nope", RootPath: "SAP_DMS"}), gc, tokens, &out)
	// A drive ID is taken as given, so the first real access (write) fails.
	if code != 1 || !strings.Contains(out.String(), "[FAIL] [DMS] Write: upload a small file") ||
		!strings.Contains(out.String(), "[SKIP] [DMS] Read: download and compare") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "fix     : Check SHAREPOINT_SITE_URL") {
		t.Errorf("expected a fix hint:\n%s", out.String())
	}
}

type noRoles struct{ auth.TokenSource }

func (noRoles) Token(context.Context) (string, error) {
	b := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return "mock-" + b("{}") + "." + b(`{"aud":"graph"}`) + ".sig", nil
}

func TestTokenWithoutRolesIsReported(t *testing.T) {
	gc, tokens, _ := newEnv(t)
	var out bytes.Buffer
	code := Run(context.Background(), cfgFor(config.RepositoryConfig{ID: "DMS", DriveID: "drive1"}), gc, noRoles{tokens}, &out)
	if code != 1 || !strings.Contains(out.String(), "[FAIL] Application permission in the token") ||
		!strings.Contains(out.String(), "roles in token: NONE") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
}

func TestReadOnlyRepositorySkipsWrites(t *testing.T) {
	gc, tokens, _ := newEnv(t)
	var out bytes.Buffer
	code := Run(context.Background(), cfgFor(config.RepositoryConfig{ID: "RO", DriveID: "drive1", ReadOnly: true}), gc, tokens, &out)
	if code != 0 || !strings.Contains(out.String(), "[SKIP] [RO] Write: upload a small file") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
}

func TestRepositoryWithOwnApplicationIsCheckedWithItsOwnToken(t *testing.T) {
	gc, tokens, _ := newEnv(t)
	ownGC, ownTokens, _ := newEnv(t)
	var asked []string
	resolve := func(rc config.RepositoryConfig) (*graph.Client, auth.TokenSource) {
		asked = append(asked, rc.ID)
		return ownGC, ownTokens
	}
	var out bytes.Buffer
	code := Run(context.Background(), cfgFor(
		config.RepositoryConfig{ID: "DMS", DriveID: "drive1", RootPath: "SAP_DMS"},
		config.RepositoryConfig{ID: "ZH", DriveID: "drive1", RootPath: "SAP_ZH", ClientSecretFile: "secrets/ZH/client-secret", ClientSecret: "never-printed"},
	), gc, tokens, &out, resolve)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	for _, want := range []string{"[PASS] Credentials configured", "[PASS] (ZH) own application: Credentials configured",
		"[PASS] (ZH) own application: Access token from Microsoft Entra ID", "[PASS] [ZH] Write: upload a small file", "[PASS] [DMS] Write: upload a small file"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	if len(asked) != 1 || asked[0] != "ZH" {
		t.Errorf("only the repository with its own application needs a resolver, asked for %v", asked)
	}
	if strings.Contains(out.String(), "never-printed") {
		t.Fatal("a repository secret reached the report")
	}
}

func TestLocalRepositoriesNeedNoMicrosoftAccess(t *testing.T) {
	gc, tokens, _ := newEnv(t)
	var out bytes.Buffer
	if code := Run(context.Background(), cfgFor(config.RepositoryConfig{ID: "AD", Backend: config.BackendLocal}), gc, tokens, &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "[PASS] [AD] local repository") || strings.Contains(out.String(), "Access token") {
		t.Fatalf("a local-only server must not be asked for Microsoft access:\n%s", out.String())
	}
}
