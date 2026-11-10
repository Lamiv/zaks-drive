package repoadmin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Lamiv/zaks-drive/internal/config"
)

// detachedLocal creates a local repository with two documents, then detaches it.
func detachedLocal(t *testing.T, m *Manager, state, id string) string {
	t.Helper()
	if _, err := m.Add(local(id, id), ""); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "data", id)
	for name, body := range map[string]string{"a.txt": "12345", "sub/b.txt": "1234567890"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Detach(id, false); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPurgeHappyPathNeedsPlanTypedIDAndPhrase(t *testing.T) {
	m, state := newManager(t)
	dir := detachedLocal(t, m, state, "AD")

	if specs, err := m.Detached(); err != nil || len(specs) != 1 || specs[0].ID != "AD" {
		t.Fatalf("detached records: %+v %v", specs, err)
	}
	plan, err := m.PurgePlan("AD")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Files != 2 || plan.Folders != 1 || plan.Bytes != 15 || plan.Path != "AD" || !strings.HasPrefix(plan.Challenge, "PURGE-AD-") ||
		time.Until(plan.ExpiresAt) > PurgeValidity || time.Until(plan.ExpiresAt) < PurgeValidity-time.Minute {
		t.Fatalf("plan: %+v", plan)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal("planning must not delete anything")
	}

	if _, err := m.Purge("AD", "AD", plan.Challenge); err != nil {
		t.Fatalf("purge with the right phrase: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the folder is still there")
	}
	if specs, _ := m.Detached(); len(specs) != 0 {
		t.Fatalf("the detached record must go with the data: %+v", specs)
	}
	audit, _ := m.AuditLog(0)
	if !strings.Contains(string(audit), `"action":"purge"`) || !strings.Contains(string(audit), "files=2") || !strings.Contains(string(audit), `"actor":"test"`) {
		t.Fatalf("a purge must be audited: %s", audit)
	}
	// replay: the same phrase cannot delete again (the folder is gone and so is the record)
	if _, err := m.Purge("AD", "AD", plan.Challenge); err == nil {
		t.Fatal("a used phrase must not work again")
	}
}

func TestPurgeRefusals(t *testing.T) {
	m, state := newManager(t)
	detachedLocal(t, m, state, "AD")
	detachedLocal(t, m, state, "AE")
	plan, err := m.PurgePlan("AD")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := m.PurgePlan("AE")

	cases := map[string]struct {
		id, typed, phrase string
		want              string
	}{
		"wrong typed id":         {"AD", "AE", plan.Challenge, "ADMIN_PURGE_CHALLENGE"},
		"typed id in lower case": {"AD", "ad", plan.Challenge, "ADMIN_PURGE_CHALLENGE"},
		"empty phrase":           {"AD", "AD", "", "ADMIN_PURGE_CHALLENGE"},
		"phrase of another repo": {"AD", "AD", other.Challenge, "ADMIN_PURGE_CHALLENGE"},
		"guessed mac":            {"AD", "AD", plan.Challenge[:len(plan.Challenge)-12] + "000000000000", "ADMIN_PURGE_CHALLENGE"},
		"truncated":              {"AD", "AD", plan.Challenge[:20], "ADMIN_PURGE_CHALLENGE"},
		"no such record":         {"ZZ", "ZZ", plan.Challenge, "ADMIN_REPO_NOT_FOUND"},
		"invalid id":             {"../x", "../x", plan.Challenge, "CFG_REPO_ID_INVALID"},
	}
	for name, c := range cases {
		if _, err := m.Purge(c.id, c.typed, c.phrase); code(err) != c.want {
			t.Errorf("%s: %v (want %s)", name, err, c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "data", "AD", "a.txt")); err != nil {
		t.Fatal("a refused purge must delete nothing")
	}
}

func TestPurgeChallengeExpiresAndIsBoundToTheFolderState(t *testing.T) {
	m, state := newManager(t)
	dir := detachedLocal(t, m, state, "AD")
	now := time.Now()
	m.now = func() time.Time { return now }
	plan, err := m.PurgePlan("AD")
	if err != nil {
		t.Fatal(err)
	}

	// a document appears after the plan: the plan no longer describes the folder
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("late"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Purge("AD", "AD", plan.Challenge); code(err) != "ADMIN_PURGE_CHALLENGE" {
		t.Fatalf("a changed folder must invalidate the plan: %v", err)
	}
	plan, _ = m.PurgePlan("AD")

	m.now = func() time.Time { return now.Add(PurgeValidity + 2*time.Minute) }
	if _, err := m.Purge("AD", "AD", plan.Challenge); code(err) != "ADMIN_PURGE_CHALLENGE" {
		t.Fatalf("an expired phrase must be refused: %v", err)
	}
	m.now = func() time.Time { return now.Add(PurgeValidity - 2*time.Minute) }
	if _, err := m.Purge("AD", "AD", plan.Challenge); err != nil {
		t.Fatalf("a phrase inside its validity must work: %v", err)
	}
}

func TestPurgeOnlyDetachedLocalRepositories(t *testing.T) {
	m, state := newManager(t)

	// still attached
	if _, err := m.Add(local("AD", "AD"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PurgePlan("AD"); code(err) != "ADMIN_PURGE_ATTACHED" {
		t.Fatalf("plan of an attached repository: %v", err)
	}
	if _, err := m.Purge("AD", "AD", "PURGE-AD-0-000000000000"); code(err) != "ADMIN_PURGE_ATTACHED" {
		t.Fatalf("purge of an attached repository: %v", err)
	}

	// SharePoint content is never deleted by us
	sp := Spec{ID: "ZH", Backend: config.BackendSharePoint, SiteURL: "https://contoso.sharepoint.com/sites/hr"}
	if _, err := m.Add(sp, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Detach("ZH", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PurgePlan("ZH"); code(err) != "ADMIN_PURGE_UNSUPPORTED" {
		t.Fatalf("SharePoint repository: %v", err)
	}

	// another repository re-uses the folder of a detached one
	if err := m.Detach("AD", false); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "data", "AD"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(local("AE", "AD"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PurgePlan("AD"); code(err) != "ADMIN_PURGE_ATTACHED" {
		t.Fatalf("a folder still used by another repository: %v", err)
	}
}

func TestPurgeRefusesAForgedRecordPointingOutsideTheDataFolder(t *testing.T) {
	m, state := newManager(t)
	outside := filepath.Join(filepath.Dir(state), "precious")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(local("AD", "AD"), ""); err != nil { // creates the state layout
		t.Fatal(err)
	}
	if err := m.Detach("AD", false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../../precious", "/", ".", "..", "a/../..", "C:/x"} {
		forged := "id: AD\nbackend: local\nlocal:\n  path: \"" + p + "\"\n"
		if err := os.WriteFile(filepath.Join(state, "detached", "AD.yaml"), []byte(forged), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := m.PurgePlan("AD"); err == nil {
			t.Errorf("a record with path %q was accepted", p)
		}
		if _, err := m.Purge("AD", "AD", "PURGE-AD-zzzzzz-000000000000"); err == nil {
			t.Errorf("a purge with path %q was accepted", p)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatal("something outside the data folder was deleted")
	}
}

func TestPurgeRemovesLinksWithoutFollowingThem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	m, state := newManager(t)
	dir := detachedLocal(t, m, state, "AD")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	plan, err := m.PurgePlan("AD")
	if err != nil || plan.Links != 1 || plan.Files != 2 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if _, err := m.Purge("AD", "AD", plan.Challenge); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatal("a purge followed a link out of the repository folder")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the folder is still there")
	}
}

func TestReAddingARepositoryClearsTheDetachedRecordButKeepsDocuments(t *testing.T) {
	m, state := newManager(t)
	detachedLocal(t, m, state, "AD")
	if specs, _ := m.Detached(); len(specs) != 1 {
		t.Fatalf("%+v", specs)
	}
	if _, err := m.Add(local("AD", "AD"), ""); err != nil {
		t.Fatal(err)
	}
	if specs, _ := m.Detached(); len(specs) != 0 {
		t.Fatalf("a repository that is attached again is no longer detached: %+v", specs)
	}
	if _, err := os.Stat(filepath.Join(state, "data", "AD", "a.txt")); err != nil {
		t.Fatal("detaching and re-adding must keep the documents")
	}
}
