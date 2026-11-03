package adminui

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newAccount(t *testing.T) (*Account, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	a, err := OpenAccount(dir)
	if err != nil {
		t.Fatal(err)
	}
	return a, dir
}

const goodPass = "correct horse battery staple"

func TestFirstRunAcceptsOnlyTheDefaultPair(t *testing.T) {
	a, _ := newAccount(t)
	if !a.IsDefault() {
		t.Fatal("a new account starts with the default credentials")
	}
	for _, c := range [][2]string{{"admin", "admin"}} {
		if !a.Verify(c[0], c[1]) {
			t.Errorf("%v must work on first run", c)
		}
	}
	for _, c := range [][2]string{{"admin", "Admin"}, {"Admin", "admin"}, {"root", "admin"}, {"admin", ""}, {"", ""}, {"admin", "admin "}, {"admin", strings.Repeat("a", 5000)}, {strings.Repeat("a", 100), "admin"}} {
		if a.Verify(c[0], c[1]) {
			t.Errorf("%q must not log in", c)
		}
	}
}

func TestWithoutAStateDirectoryNobodyCanLogIn(t *testing.T) {
	a, _ := OpenAccount("")
	if a.Available() || a.Verify("admin", "admin") || a.IsDefault() {
		t.Fatal("without a place to store the password the default pair must not work")
	}
	if _, err := a.SetPassword("admin", goodPass); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestSettingAPasswordReplacesTheDefaultAndSurvivesRestart(t *testing.T) {
	a, dir := newAccount(t)
	if _, err := a.SetPassword("wrong", goodPass); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("a wrong current password: %v", err)
	}
	if left, err := a.SetPassword("admin", goodPass); err != nil || left {
		t.Fatalf("%v %v", err, left)
	}
	if a.IsDefault() || a.Verify("admin", "admin") {
		t.Fatal("the default pair must stop working at once")
	}
	if !a.Verify("admin", goodPass) || a.Verify("admin", goodPass+"x") || a.Verify("root", goodPass) {
		t.Fatal("the new password (and only it) must work")
	}

	b, err := OpenAccount(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b.IsDefault() || !b.Verify("admin", goodPass) || b.Verify("admin", "admin") {
		t.Fatal("the password must survive a restart")
	}

	raw, _ := os.ReadFile(filepath.Join(dir, AccountFile))
	if strings.Contains(string(raw), goodPass) || !strings.Contains(string(raw), `"algo":"argon2id"`) {
		t.Fatalf("only a salted Argon2id hash may be stored: %s", raw)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(dir, AccountFile)); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", st.Mode().Perm())
		}
	}
	// two accounts with the same password get different hashes (random salt)
	c, _ := newAccount(t)
	_, _ = c.SetPassword("admin", goodPass)
	rawC, _ := os.ReadFile(filepath.Join(c.dir, AccountFile))
	if string(rawC) == string(raw) {
		t.Fatal("the salt must be random")
	}
}

func TestPasswordRules(t *testing.T) {
	good := []string{goodPass, "a-long-enough-secret-1", "ΩmegaΩmegaΩmega", strings.Repeat("ab", 20)}
	for _, p := range good {
		if err := ValidatePassword(p, "old"); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
	bad := map[string]string{
		"short":                          "at least 12",
		"admin":                          "at least 12",
		"administrator":                  "easy to guess",
		"AdminAdmin12":                   "easy to guess",
		"aaaaaaaaaaaaaaa":                "easy to guess",
		"            ":                   "",
		"password1234":                   "easy to guess",
		strings.Repeat("x", 200):         "at most",
		"line\nbreak is not allowed....": "not allowed",
		"nul\x00byte is not allowed....": "not allowed",
	}
	for p, want := range bad {
		err := ValidatePassword(p, "old")
		var pe *PasswordError
		if !errors.As(err, &pe) || !strings.Contains(pe.Reason, want) {
			t.Errorf("%q: %v (want %q)", p, err, want)
		}
	}
	if err := ValidatePassword(goodPass, goodPass); err == nil {
		t.Error("the new password must differ from the current one")
	}
	if _, err := func() (bool, error) { a, _ := newAccount(t); return a.SetPassword("admin", "admin") }(); err == nil {
		t.Error("changing admin to admin is no change")
	}
}

func TestResetFileFlow(t *testing.T) {
	a, dir := newAccount(t)
	_, _ = a.SetPassword("admin", goodPass)
	time.Sleep(20 * time.Millisecond)

	if reset, err := a.ApplyReset(); reset || err != nil {
		t.Fatalf("without a NO_ADMIN file nothing happens: %v %v", reset, err)
	}
	if err := RequestReset(dir); err != nil {
		t.Fatal(err)
	}
	// a restart: the file is newer than the password change
	b, _ := OpenAccount(dir)
	reset, err := b.ApplyReset()
	if err != nil || !reset {
		t.Fatalf("a new NO_ADMIN must reset: %v %v", reset, err)
	}
	if !b.IsDefault() || b.Verify("admin", goodPass) || !b.Verify("admin", "admin") {
		t.Fatal("after the reset only the default pair works")
	}
	if _, err := os.Stat(filepath.Join(dir, AccountFile)); err == nil {
		t.Fatal("the old hash must be gone")
	}
	if _, err := os.Stat(filepath.Join(dir, ResetFile)); err != nil {
		t.Fatal("NO_ADMIN stays until a new password is set")
	}
	// another restart before a password is set: still the default state, file still there
	c, _ := OpenAccount(dir)
	if reset, _ := c.ApplyReset(); !reset || !c.IsDefault() {
		t.Fatal("a second restart keeps the default state")
	}
	if _, err := os.Stat(filepath.Join(dir, ResetFile)); err != nil {
		t.Fatal("the file must still be there")
	}
	// setting the password deletes the file
	if left, err := c.SetPassword("admin", "another long passphrase 42"); err != nil || left {
		t.Fatalf("%v %v", err, left)
	}
	if _, err := os.Stat(filepath.Join(dir, ResetFile)); err == nil {
		t.Fatal("the server must delete NO_ADMIN once a password is set")
	}
	d, _ := OpenAccount(dir)
	if reset, _ := d.ApplyReset(); reset || d.IsDefault() {
		t.Fatal("no file, no reset")
	}
}

func TestStaleResetFileIsIgnoredAndRemoved(t *testing.T) {
	a, dir := newAccount(t)
	if err := RequestReset(dir); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(dir, ResetFile), old, old)
	_, _ = a.SetPassword("admin", goodPass) // changed after the file was made; deletion normally removes it
	_ = RequestReset(dir)                   // simulate a failed deletion: a file that exists again ...
	_ = os.Chtimes(filepath.Join(dir, ResetFile), old, old)
	b, _ := OpenAccount(dir)
	if reset, _ := b.ApplyReset(); reset || b.IsDefault() || !b.Verify("admin", goodPass) {
		t.Fatal("a leftover file older than the last change must not reset the password again")
	}
	if _, err := os.Stat(filepath.Join(dir, ResetFile)); err == nil {
		t.Fatal("the stale file should be cleaned up")
	}
}

func TestCorruptRecordFailsClosedUntilReset(t *testing.T) {
	a, dir := newAccount(t)
	_, _ = a.SetPassword("admin", goodPass)
	if err := os.WriteFile(filepath.Join(dir, AccountFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := OpenAccount(dir)
	if !b.Locked() || b.IsDefault() || b.Verify("admin", "admin") || b.Verify("admin", goodPass) {
		t.Fatal("a corrupt record must lock the login, never fall back to admin/admin")
	}
	if _, err := b.SetPassword("admin", "some other long passphrase"); err == nil {
		t.Fatal("no password can be set while locked")
	}
	time.Sleep(20 * time.Millisecond)
	_ = RequestReset(dir)
	c, _ := OpenAccount(dir)
	if reset, err := c.ApplyReset(); !reset || err != nil || !c.IsDefault() || c.Locked() {
		t.Fatalf("a reset recovers a locked account: %v %v", reset, err)
	}
}

func TestTamperedHashParametersAreNotTrusted(t *testing.T) {
	a, dir := newAccount(t)
	_, _ = a.SetPassword("admin", goodPass)
	raw, _ := os.ReadFile(filepath.Join(dir, AccountFile))
	// weakening the stored cost cannot make a wrong password pass
	weak := strings.Replace(string(raw), `"algo":"argon2id"`, `"algo":"md5"`, 1)
	_ = os.WriteFile(filepath.Join(dir, AccountFile), []byte(weak), 0o600)
	b, _ := OpenAccount(dir)
	if b.Verify("admin", goodPass) || b.Verify("admin", "admin") {
		t.Fatal("an unknown algorithm must lock the account")
	}
}
