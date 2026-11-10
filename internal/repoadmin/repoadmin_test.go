package repoadmin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/config"
)

func newManager(t *testing.T) (*Manager, string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	body := "server: { interfaces: [rest, contentrepo], stateDir: \"" + filepath.ToSlash(state) + "\" }\n" +
		"graph: { tenantId: t, clientId: c, clientSecret: s }\nauth: { disabled: true }\n" +
		"repositories:\n  - { id: DMS, siteUrl: \"https://contoso.sharepoint.com/sites/x\" }\n" +
		"contentServer:\n  repositories:\n    - { contRep: Z1, repository: DMS }\n"
	if err := os.WriteFile(cfgFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Manager{StateDir: state, Actor: "test", Load: func() (config.Config, error) { return config.Load(cfgFile) }}, state
}

func local(id, path string) Spec {
	return Spec{ID: id, Backend: config.BackendLocal, Local: &LocalSpec{Path: path}}
}

func code(err error) string {
	if e, ok := apperr.As(err); ok {
		return e.Code
	}
	return ""
}

func TestAddListUpdateDetachLocalRepository(t *testing.T) {
	m, state := newManager(t)
	v, err := m.Add(Spec{ID: "AD", Description: "Archive", Backend: config.BackendLocal, Local: &LocalSpec{Path: "AD"}, SAP: &SAPSpec{}}, "")
	if err != nil {
		t.Fatal(apperr.Format(err))
	}
	if v.Source != config.SourceManaged || v.Backend != config.BackendLocal || v.SecretSet {
		t.Fatalf("view: %+v", v)
	}
	views, err := m.List()
	if err != nil || len(views) != 2 || views[0].ID != "AD" || views[1].ID != "DMS" || views[1].Source != config.SourceStatic {
		t.Fatalf("list: %+v %v", views, err)
	}

	three := 3
	if _, err := m.Update(Spec{ID: "AD", Backend: config.BackendLocal, Local: &LocalSpec{Path: "AD", Versions: &three}, SAP: &SAPSpec{Signature: "optional"}}, nil); err != nil {
		t.Fatal(apperr.Format(err))
	}
	got, _ := m.Get("AD")
	if got.Local == nil || got.Local.Versions == nil || *got.Local.Versions != 3 || got.SAP.Signature != "optional" {
		t.Fatalf("update not stored: %+v", got)
	}

	if err := m.Detach("AD", false); code(err) != "ADMIN_REPO_IN_USE" {
		t.Fatalf("a repository exposed to SAP needs the explicit confirmation: %v", err)
	}
	if err := m.Detach("AD", true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get("AD"); code(err) != "ADMIN_REPO_NOT_FOUND" {
		t.Fatalf("after detach: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "repos.d", "AD.yaml")); err == nil {
		t.Fatal("the definition is still there")
	}
}

func TestDetachKeepsTheDocuments(t *testing.T) {
	m, state := newManager(t)
	if _, err := m.Add(local("AD", "AD"), ""); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(state, "data", "AD", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(doc), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(doc, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Detach("AD", false); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(doc); err != nil || string(b) != "precious" {
		t.Fatalf("detach must never touch documents: %q %v", b, err)
	}
}

func TestInvalidChangeIsRolledBackCompletely(t *testing.T) {
	m, state := newManager(t)
	if _, err := m.Add(local("AD", "AD"), ""); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(state, "repos.d", "AD.yaml"))

	hundred := 100
	_, err := m.Update(Spec{ID: "AD", Backend: config.BackendLocal, Local: &LocalSpec{Path: "AD", Versions: &hundred}}, nil)
	if code(err) != "CFG_REPO_LOCAL_INVALID" && code(err) != "ADMIN_REPO_INVALID" {
		t.Fatalf("invalid update: %v", err)
	}
	if !strings.Contains(apperr.Format(err), "0 to 99") {
		t.Errorf("the reason must be shown: %s", apperr.Format(err))
	}
	if after, _ := os.ReadFile(filepath.Join(state, "repos.d", "AD.yaml")); string(after) != string(before) {
		t.Fatalf("a rejected update changed the file:\n%s\nwas:\n%s", after, before)
	}

	// a rejected add leaves no file and no secret folder behind
	if _, err := m.Add(Spec{ID: "ZH", Backend: config.BackendSharePoint, SiteURL: "https://contoso.sharepoint.com/sites/x/Forms/AllItems.aspx",
		TenantID: "t", ClientID: "c"}, "s3cret"); err == nil {
		t.Fatal("expected a rejection")
	}
	for _, p := range []string{"repos.d/ZH.yaml", "secrets/ZH/client-secret", "secrets/ZH"} {
		if _, err := os.Stat(filepath.Join(state, filepath.FromSlash(p))); err == nil {
			t.Errorf("%s was left behind", p)
		}
	}
}

func TestSecretIsStoredSeparatelyAndNeverEchoed(t *testing.T) {
	m, state := newManager(t)
	spec := Spec{ID: "ZH", Backend: config.BackendSharePoint, SiteURL: "https://contoso.sharepoint.com/sites/hr", DriveName: "Documents",
		RootPath: "SAP_HR", TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "66666666-7777-8888-9999-aaaaaaaaaaaa"}
	v, err := m.Add(spec, "  correct-horse-battery  ")
	if err != nil {
		t.Fatal(apperr.Format(err))
	}
	if !v.SecretSet {
		t.Fatal("the view must say a secret is set")
	}
	yamlBytes, _ := os.ReadFile(filepath.Join(state, "repos.d", "ZH.yaml"))
	if strings.Contains(string(yamlBytes), "correct-horse") || !strings.Contains(string(yamlBytes), "clientSecretFile: secrets/ZH/client-secret") {
		t.Fatalf("the definition must name the secret file and never hold the secret:\n%s", yamlBytes)
	}
	sec, err := os.ReadFile(filepath.Join(state, "secrets", "ZH", "client-secret"))
	if err != nil || string(sec) != "correct-horse-battery\n" {
		t.Fatalf("secret file: %q %v", sec, err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(state, "secrets", "ZH", "client-secret"))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("secret mode %o", st.Mode().Perm())
		}
	}
	audit, _ := m.AuditLog(0)
	if strings.Contains(string(audit), "correct-horse") || !strings.Contains(string(audit), `"action":"add"`) || !strings.Contains(string(audit), `"actor":"test"`) {
		t.Fatalf("audit line wrong or leaking: %s", audit)
	}
	out, _ := m.Get("ZH")
	if b := mustJSON(t, out); strings.Contains(b, "correct-horse") {
		t.Fatalf("the view leaked the secret: %s", b)
	}

	// keep the secret on update, replace it on request, drop it with the own application
	if _, err := m.Update(spec, nil); err != nil {
		t.Fatal(apperr.Format(err))
	}
	if b, _ := os.ReadFile(filepath.Join(state, "secrets", "ZH", "client-secret")); string(b) != "correct-horse-battery\n" {
		t.Fatalf("update without a secret must keep it: %q", b)
	}
	rotated := "rotated-secret"
	if _, err := m.Update(spec, &rotated); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "secrets", "ZH", "client-secret")); string(b) != "rotated-secret\n" {
		t.Fatalf("secret not rotated: %q", b)
	}
	spec.TenantID, spec.ClientID = "", ""
	if _, err := m.Update(spec, nil); err != nil {
		t.Fatal(apperr.Format(err))
	}
	if _, err := os.Stat(filepath.Join(state, "secrets", "ZH", "client-secret")); err == nil {
		t.Fatal("a repository that returns to the global application must not keep a secret on disk")
	}

	// detaching removes the secret too
	spec.TenantID, spec.ClientID = "t", "c"
	if _, err := m.Update(spec, &rotated); err != nil {
		t.Fatal(err)
	}
	if err := m.Detach("ZH", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "secrets", "ZH")); err == nil {
		t.Fatal("the secret of a detached repository must be removed")
	}
}

func mustJSON(t *testing.T, v View) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSecretValidation(t *testing.T) {
	m, _ := newManager(t)
	spec := Spec{ID: "ZH", Backend: config.BackendSharePoint, SiteURL: "https://contoso.sharepoint.com/sites/hr", TenantID: "t", ClientID: "c"}
	for name, secret := range map[string]string{"empty": "", "blank": "   ", "multi-line": "a\nb", "huge": strings.Repeat("x", 5000)} {
		if _, err := m.Add(spec, secret); err == nil {
			t.Errorf("%s secret accepted", name)
		}
	}
	// own application without any secret fails in the loader, not later at start
	if _, err := m.Add(spec, ""); err == nil || !strings.Contains(apperr.Format(err), "secret") {
		t.Fatalf("own application without a secret: %v", err)
	}
	// a secret without an own application makes no sense
	if _, err := m.Add(local("AD", "AD"), "x"); code(err) != "ADMIN_REPO_INVALID" {
		t.Fatalf("secret for a local repository: %v", err)
	}
}

func TestIdsCannotEscapeTheStateDirectory(t *testing.T) {
	m, state := newManager(t)
	outside := filepath.Join(filepath.Dir(state), "pwned")
	for _, id := range []string{"../pwned", "..", "a/b", "A", "ABC", "a1", "", `..\x`, "%2e", "Z\x00"} {
		if _, err := m.Add(Spec{ID: id, Backend: config.BackendLocal, Local: &LocalSpec{Path: "x"}}, ""); err == nil {
			t.Errorf("id %q accepted", id)
		}
		if err := m.Detach(id, true); err == nil {
			t.Errorf("detach of %q accepted", id)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("something was written outside the state directory")
	}
	// a local path that leaves the data folder is rejected by the loader and rolled back
	if _, err := m.Add(local("AD", "../../etc"), ""); err == nil {
		t.Fatal("escaping local path accepted")
	}
	if _, err := os.Stat(filepath.Join(state, "repos.d", "AD.yaml")); err == nil {
		t.Fatal("the rejected definition was kept")
	}
}

func TestDuplicatesAndStaticRepositories(t *testing.T) {
	m, _ := newManager(t)
	if _, err := m.Add(local("AD", "AD"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(local("AD", "AD2"), ""); code(err) != "ADMIN_REPO_EXISTS" {
		t.Fatalf("duplicate add: %v", err)
	}
	if _, err := m.Update(local("ZZ", "ZZ"), nil); code(err) != "ADMIN_REPO_NOT_FOUND" {
		t.Fatalf("update of a missing repository: %v", err)
	}
	// "DMS" comes from config.yaml; its ID is not even a managed ID, and detach says why it cannot be touched
	if err := m.Detach("DM", false); code(err) != "ADMIN_REPO_NOT_FOUND" {
		t.Fatalf("%v", err)
	}
}

func TestStateDirectoryUnavailable(t *testing.T) {
	m := &Manager{Load: func() (config.Config, error) { return config.Config{}, nil }}
	if _, err := m.Add(local("AD", "AD"), ""); code(err) != "ADMIN_STATE_UNAVAILABLE" {
		t.Fatalf("%v", err)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.StateDir = filepath.Join(blocker, "state") // below a file: cannot be created
	if _, err := m.Add(local("AD", "AD"), ""); code(err) != "ADMIN_STATE_UNAVAILABLE" {
		t.Fatalf("%v", err)
	}
}

func TestFingerprintTracksPendingChanges(t *testing.T) {
	m, _ := newManager(t)
	start, err := m.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(local("AD", "AD"), ""); err != nil {
		t.Fatal(err)
	}
	added, _ := m.Fingerprint()
	if added == start {
		t.Fatal("adding a repository must change the fingerprint")
	}
	if again, _ := m.Fingerprint(); again != added {
		t.Fatal("the fingerprint must be stable while nothing changes")
	}
	if err := m.Detach("AD", false); err != nil {
		t.Fatal(err)
	}
	if back, _ := m.Fingerprint(); back != start {
		t.Fatal("returning to the starting state must return to the starting fingerprint")
	}

	// a rotated secret of the same length is a change too
	spec := Spec{ID: "ZH", Backend: config.BackendSharePoint, SiteURL: "https://contoso.sharepoint.com/sites/hr", TenantID: "t", ClientID: "c"}
	if _, err := m.Add(spec, "aaaa"); err != nil {
		t.Fatal(err)
	}
	f1, _ := m.Fingerprint()
	b := "bbbb"
	if _, err := m.Update(spec, &b); err != nil {
		t.Fatal(err)
	}
	if f2, _ := m.Fingerprint(); f2 == f1 {
		t.Fatal("rotating a secret must change the fingerprint")
	}
}

func TestSnapshotKeepsDefinitionsOnly(t *testing.T) {
	m, state := newManager(t)
	spec := Spec{ID: "ZH", Backend: config.BackendSharePoint, SiteURL: "https://contoso.sharepoint.com/sites/hr", TenantID: "t", ClientID: "c"}
	if _, err := m.Add(spec, "topsecret"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(local("AD", "AD"), ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Snapshot(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ZH.yaml", "AD.yaml"} {
		if _, err := os.Stat(filepath.Join(state, LastGoodDir, "repos.d", n)); err != nil {
			t.Errorf("snapshot lacks %s: %v", n, err)
		}
	}
	filepath.WalkDir(filepath.Join(state, LastGoodDir), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "topsecret") {
				t.Errorf("snapshot %s holds a secret", p)
			}
		}
		return nil
	})
	// a later snapshot replaces the earlier one
	if err := m.Detach("AD", false); err != nil {
		t.Fatal(err)
	}
	if err := m.Snapshot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, LastGoodDir, "repos.d", "AD.yaml")); err == nil {
		t.Fatal("the snapshot kept a detached repository")
	}
}

func TestStaleLockIsBrokenButAFreshOneIsRespected(t *testing.T) {
	m, state := newManager(t)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(state, lockFile)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(local("AD", "AD"), ""); err != nil {
		t.Fatalf("a stale lock must not block forever: %v", err)
	}
	if _, err := os.Stat(lockPath); err == nil {
		t.Fatal("the lock was not released")
	}
	// a fresh lock held by someone else makes the change wait, then fail clearly
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := m.Add(local("AE", "AE"), "")
	if code(err) != "ADMIN_STATE_UNAVAILABLE" || !strings.Contains(err.Error(), "in progress") || time.Since(start) < lockTimeout-time.Second {
		t.Fatalf("a live lock must be respected for %s: %v after %s", lockTimeout, err, time.Since(start))
	}
}

func TestAuditLogHasOneLinePerChangeAndNoSecrets(t *testing.T) {
	m, _ := newManager(t)
	spec := Spec{ID: "ZH", Backend: config.BackendSharePoint, SiteURL: "https://contoso.sharepoint.com/sites/hr", TenantID: "tenant-id-value", ClientID: "client-id-value"}
	if _, err := m.Add(spec, "supersecret"); err != nil {
		t.Fatal(err)
	}
	if err := m.Detach("ZH", false); err != nil {
		t.Fatal(err)
	}
	b, err := m.AuditLog(0)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"action":"add"`) || !strings.Contains(lines[1], `"action":"detach"`) {
		t.Fatalf("audit: %s", b)
	}
	for _, s := range []string{"supersecret", "tenant-id-value", "client-id-value", "contoso"} {
		if strings.Contains(string(b), s) {
			t.Errorf("audit log contains %q", s)
		}
	}
	if last, _ := m.AuditLog(1); strings.Count(string(last), "\n") != 1 {
		t.Fatalf("AuditLog(1): %s", last)
	}
}

func TestTheBackendIsChosenOnceAndCannotBeSwitched(t *testing.T) {
	m, state := newManager(t)
	if _, err := m.Add(local("AD", "AD"), ""); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(state, "repos.d", "AD.yaml"))
	sp := Spec{ID: "AD", Backend: config.BackendSharePoint, SiteURL: "https://contoso.sharepoint.com/sites/x"}
	if _, err := m.Update(sp, nil); code(err) != "ADMIN_REPO_INVALID" || !strings.Contains(err.Error(), "cannot be changed") {
		t.Fatalf("switching local to SharePoint must be refused: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(state, "repos.d", "AD.yaml")); string(after) != string(before) {
		t.Fatal("a refused switch changed the file")
	}
	// the other direction, and an unspecified backend (which means SharePoint), are refused too
	if _, err := m.Add(Spec{ID: "ZH", Backend: config.BackendSharePoint, SiteURL: "https://contoso.sharepoint.com/sites/hr"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update(local("ZH", "ZH"), nil); code(err) != "ADMIN_REPO_INVALID" {
		t.Fatalf("switching SharePoint to local must be refused: %v", err)
	}
	// ordinary updates of the same backend still work
	three := 3
	if _, err := m.Update(Spec{ID: "AD", Backend: config.BackendLocal, Local: &LocalSpec{Path: "AD", Versions: &three}}, nil); err != nil {
		t.Fatalf("an update that keeps the backend: %v", err)
	}
}
