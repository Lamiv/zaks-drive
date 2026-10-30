package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Lamiv/zaks-drive/internal/apperr"
)

// stateFixture creates a state directory with the given repos.d files and
// returns a config file that points at it.
func stateFixture(t *testing.T, files map[string]string) (cfgPath, state string) {
	t.Helper()
	state = t.TempDir()
	for name, body := range files {
		p := filepath.Join(state, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(name, SecretsDir+"/") {
			mode = 0o600
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath = writeCfg(t, `
server: { interfaces: [rest, contentrepo], stateDir: "`+filepath.ToSlash(state)+`" }
graph: { tenantId: t, clientId: c, clientSecret: s }
auth: { disabled: true }
repositories:
  - { id: DMS, siteUrl: "https://contoso.sharepoint.com/sites/x" }
contentServer:
  repositories:
    - { contRep: Z1, repository: DMS }
`)
	return cfgPath, state
}

func TestManagedRepositoriesAreLoadedAndExposedToSAP(t *testing.T) {
	cfgPath, state := stateFixture(t, map[string]string{
		"repos.d/AD.yaml": "id: AD\ndescription: Archive\nbackend: local\nlocal: { path: AD, versions: 7 }\nsap: { signature: optional }\n",
		"repos.d/ZH.yaml": "id: ZH\nbackend: sharepoint\nsiteUrl: https://contoso.sharepoint.com/sites/hr\nrootPath: /SAP_HR/\n" +
			"tenantId: t2\nclientId: c2\nclientSecretFile: secrets/ZH/client-secret\nsap: { contRep: ZH }\n",
		"secrets/ZH/client-secret": "  top-secret\n",
	})
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(apperr.Format(err))
	}
	by := map[string]RepositoryConfig{}
	for _, r := range cfg.Repositories {
		by[r.ID] = r
	}
	if len(by) != 3 || by["DMS"].Source != SourceStatic || by["DMS"].Backend != BackendSharePoint {
		t.Fatalf("static repository changed: %+v", by["DMS"])
	}
	ad := by["AD"]
	if ad.Source != SourceManaged || ad.Backend != BackendLocal || ad.Local.KeepVersions() != 7 ||
		ad.LocalRoot != filepath.Join(state, DataDir, "AD") {
		t.Fatalf("local repository wrong: %+v", ad)
	}
	zh := by["ZH"]
	if zh.ClientSecret != "top-secret" || zh.RootPath != "SAP_HR" {
		t.Fatalf("sharepoint repository wrong: secret set=%v root=%q", zh.ClientSecret != "", zh.RootPath)
	}
	got := map[string]ContentRepositoryConfig{}
	for _, cr := range cfg.ContentServer.Repositories {
		got[cr.ContRep] = cr
	}
	if got["AD"].Repository != "AD" || got["AD"].Signature != "optional" || got["AD"].Folder != "ContentServer/AD" ||
		got["ZH"].Signature != "required" || got["Z1"].Repository != "DMS" {
		t.Fatalf("content repositories wrong: %+v", got)
	}
}

func TestLocalOnlyConfigurationNeedsNoGlobalCredentials(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, ReposDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, ReposDir, "AD.yaml"), []byte("id: AD\nbackend: local\nlocal: { path: AD }\nsap: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRAPH_TENANT_ID", "")
	t.Setenv("GRAPH_CLIENT_ID", "")
	t.Setenv("GRAPH_CLIENT_SECRET", "")
	p := writeCfg(t, "server: { interfaces: [contentrepo], stateDir: \""+filepath.ToSlash(state)+"\" }\nauth: { disabled: true }\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(apperr.Format(err))
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0].Local.KeepVersions() != DefaultLocalVersions {
		t.Fatalf("unexpected repositories: %+v", cfg.Repositories)
	}
}

func TestManagedRepositoryProblems(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"long id", map[string]string{"repos.d/ZHX.yaml": "id: ZHX\nbackend: local\nlocal: { path: x }\n"}, []string{"CFG_REPO_ID_INVALID", `"ZHX"`}},
		{"lowercase id", map[string]string{"repos.d/zh.yaml": "id: zh\nbackend: local\nlocal: { path: x }\n"}, []string{"CFG_REPO_ID_INVALID"}},
		{"file name differs", map[string]string{"repos.d/AD.yaml": "id: AB\nbackend: local\nlocal: { path: x }\n"}, []string{"CFG_REPO_FILE_INVALID", "must be named AB.yaml"}},
		{"unknown key", map[string]string{"repos.d/AD.yaml": "id: AD\nbackend: local\nlocal: { path: x }\nclientSecret: oops\n"}, []string{"CFG_REPO_FILE_INVALID", "AD.yaml"}},
		{"bad backend", map[string]string{"repos.d/AD.yaml": "id: AD\nbackend: s3\n"}, []string{"CFG_REPO_BACKEND_INVALID", `"s3"`}},
		{"no path", map[string]string{"repos.d/AD.yaml": "id: AD\nbackend: local\n"}, []string{"CFG_REPO_LOCAL_INVALID"}},
		{"absolute path", map[string]string{"repos.d/AD.yaml": "id: AD\nbackend: local\nlocal: { path: /etc }\n"}, []string{"CFG_REPO_LOCAL_INVALID"}},
		{"escaping path", map[string]string{"repos.d/AD.yaml": "id: AD\nbackend: local\nlocal: { path: ../secrets }\n"}, []string{"CFG_REPO_LOCAL_INVALID"}},
		{"too many versions", map[string]string{"repos.d/AD.yaml": "id: AD\nbackend: local\nlocal: { path: AD, versions: 100 }\n"}, []string{"CFG_REPO_LOCAL_INVALID", "0 to 99"}},
		{"negative versions", map[string]string{"repos.d/AD.yaml": "id: AD\nbackend: local\nlocal: { path: AD, versions: -1 }\n"}, []string{"CFG_REPO_LOCAL_INVALID"}},
		{"local with site", map[string]string{"repos.d/AD.yaml": "id: AD\nbackend: local\nlocal: { path: AD }\nsiteUrl: https://x.sharepoint.com/sites/a\n"}, []string{"do not apply"}},
		{"partial credentials", map[string]string{"repos.d/ZH.yaml": "id: ZH\nsiteUrl: https://x.sharepoint.com/sites/a\ntenantId: t\n"}, []string{"CFG_REPO_CREDENTIALS_INCOMPLETE"}},
		{"secret outside secrets", map[string]string{
			"repos.d/ZH.yaml": "id: ZH\nsiteUrl: https://x.sharepoint.com/sites/a\ntenantId: t\nclientId: c\nclientSecretFile: repos.d/AD.yaml\n"}, []string{"CFG_REPO_SECRET_UNREADABLE", "below secrets/"}},
		{"secret escapes", map[string]string{
			"repos.d/ZH.yaml": "id: ZH\nsiteUrl: https://x.sharepoint.com/sites/a\ntenantId: t\nclientId: c\nclientSecretFile: secrets/../../x\n"}, []string{"CFG_REPO_SECRET_UNREADABLE"}},
		{"secret missing", map[string]string{
			"repos.d/ZH.yaml": "id: ZH\nsiteUrl: https://x.sharepoint.com/sites/a\ntenantId: t\nclientId: c\nclientSecretFile: secrets/ZH/client-secret\n"}, []string{"CFG_REPO_SECRET_UNREADABLE", "does not exist"}},
		{"secret empty", map[string]string{
			"repos.d/ZH.yaml":          "id: ZH\nsiteUrl: https://x.sharepoint.com/sites/a\ntenantId: t\nclientId: c\nclientSecretFile: secrets/ZH/client-secret\n",
			"secrets/ZH/client-secret": " \n"}, []string{"is empty"}},
		{"sap contRep clash", map[string]string{"repos.d/Z1.yaml": "id: Z1\nbackend: local\nlocal: { path: a }\nsap: {}\n"}, []string{"CFG_CONTREP_DUPLICATE"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, _ := stateFixture(t, tc.files)
			_, err := Load(cfgPath)
			if err == nil {
				t.Fatal("expected an error")
			}
			text := apperr.Format(err)
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("error lacks %q:\n%s", w, text)
				}
			}
		})
	}
}

func TestSecretFileMustNotBeReadableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	cfgPath, state := stateFixture(t, map[string]string{
		"repos.d/ZH.yaml":          "id: ZH\nsiteUrl: https://x.sharepoint.com/sites/a\ntenantId: t\nclientId: c\nclientSecretFile: secrets/ZH/client-secret\n",
		"secrets/ZH/client-secret": "s3cret\n",
	})
	if err := os.Chmod(filepath.Join(state, "secrets", "ZH", "client-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(apperr.Format(err), "must be 600") {
		t.Fatalf("a world-readable secret must be refused, got %v", err)
	}
	if strings.Contains(apperr.Format(err), "s3cret") {
		t.Fatal("the secret must never appear in an error")
	}
}

func TestSecretFileLinkOutOfStateDirIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath, state := stateFixture(t, map[string]string{
		"repos.d/ZH.yaml": "id: ZH\nsiteUrl: https://x.sharepoint.com/sites/a\ntenantId: t\nclientId: c\nclientSecretFile: secrets/ZH/client-secret\n",
	})
	if err := os.MkdirAll(filepath.Join(state, "secrets", "ZH"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(state, "secrets", "ZH", "client-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfgPath); err == nil || !strings.Contains(apperr.Format(err), "CFG_REPO_SECRET_UNREADABLE") {
		t.Fatalf("a link leaving the state directory must be refused, got %v", err)
	}
}

func TestNoStateDirKeepsStaticBehaviour(t *testing.T) {
	p := writeCfg(t, `
server: { interfaces: [rest] }
graph: { tenantId: t, clientId: c, clientSecret: s }
auth: { disabled: true }
repositories:
  - { id: Local, backend: local, local: { path: x } }
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(apperr.Format(err), "needs server.stateDir") {
		t.Fatalf("local repository without a state directory must be refused, got %v", err)
	}
}

func TestManagedRepositoryMayNotReuseAStaticID(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, ReposDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, ReposDir, "AD.yaml"), []byte("id: AD\nbackend: local\nlocal: { path: AD }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := writeCfg(t, "server: { interfaces: [rest], stateDir: \""+filepath.ToSlash(state)+"\" }\ngraph: { tenantId: t, clientId: c, clientSecret: s }\nauth: { disabled: true }\n"+
		"repositories:\n  - { id: AD, siteUrl: \"https://x.sharepoint.com/sites/a\" }\n")
	if _, err := Load(p); err == nil || !strings.Contains(apperr.Format(err), "CFG_REPO_ID_DUPLICATE") {
		t.Fatalf("a managed repository must not shadow a static one, got %v", err)
	}
}
