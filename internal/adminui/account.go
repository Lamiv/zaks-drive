package adminui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// The admin account: one user, "admin", one role. Its password is stored only
// as an Argon2id hash in <state dir>/admin.json (mode 600).
//
// Until the password has been changed, the well-known pair admin/admin works,
// but only to change the password: a session opened that way can do nothing
// else (see Sessions). A reset, by creating an empty file NO_ADMIN in the
// state directory and restarting the server, returns to that state. The
// server deletes NO_ADMIN once a new password is set. Whoever can write to the
// state directory already has the keys of the whole deployment (the secrets
// are in the same volume), so the reset adds no new trust boundary.
const (
	User           = "admin"
	defaultPass    = "admin"
	AccountFile    = "admin.json"
	ResetFile      = "NO_ADMIN"
	MinPasswordLen = 12
	MaxPasswordLen = 128

	// Argon2id parameters from the OWASP password storage guidance (19 MiB, 2 passes, 1 lane).
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
)

// Errors a caller maps to API answers.
var (
	ErrUnavailable   = errors.New("no state directory: the admin account cannot be stored")
	ErrWrongPassword = errors.New("the current password is not correct")
)

// PasswordError says why a new password was refused.
type PasswordError struct{ Reason string }

func (e *PasswordError) Error() string { return e.Reason }

type record struct {
	Version   int       `json:"version"`
	Algo      string    `json:"algo"`
	Memory    uint32    `json:"m"`
	Time      uint32    `json:"t"`
	Threads   uint8     `json:"p"`
	Salt      string    `json:"salt"`
	Hash      string    `json:"hash"`
	ChangedAt time.Time `json:"changedAt"`
}

// Account is the stored credential of the admin user.
type Account struct {
	mu      sync.Mutex
	dir     string
	rec     *record // nil: the default credentials are active
	corrupt bool    // admin.json exists but is unreadable: nobody can log in until a reset
	sem     chan struct{}
	now     func() time.Time
}

// OpenAccount reads the account from the state directory ("" = none: the
// account is unavailable and the UI cannot be used).
func OpenAccount(dir string) (*Account, error) {
	a := &Account{dir: dir, sem: make(chan struct{}, 2), now: time.Now}
	if dir == "" {
		return a, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return a, nil // nothing created yet: the default state
		}
		return a, err
	}
	defer root.Close()
	b, err := root.ReadFile(AccountFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		a.corrupt = true
	default:
		var r record
		if json.Unmarshal(b, &r) != nil || r.Algo != "argon2id" || r.Salt == "" || r.Hash == "" {
			a.corrupt = true
		} else {
			a.rec = &r
		}
	}
	return a, nil
}

// Available reports whether the account can be stored (a state directory exists).
func (a *Account) Available() bool { return a.dir != "" }

// IsDefault reports whether the well-known admin/admin pair is still active.
func (a *Account) IsDefault() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dir != "" && a.rec == nil && !a.corrupt
}

// Locked reports a corrupt record: logins are refused until a reset.
func (a *Account) Locked() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.corrupt
}

// ApplyReset handles a NO_ADMIN file at start-up. A file newer than the last
// password change returns the account to the default state (the old hash is
// removed) and stays until a new password is set. A file older than the last
// change is a leftover (its deletion failed) and is ignored and removed.
func (a *Account) ApplyReset() (reset bool, err error) {
	if a.dir == "" {
		return false, nil
	}
	root, err := os.OpenRoot(a.dir)
	if err != nil {
		return false, nil
	}
	defer root.Close()
	st, err := root.Lstat(ResetFile)
	if err != nil {
		return false, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rec != nil && !st.ModTime().After(a.rec.ChangedAt) {
		_ = root.Remove(ResetFile) // stale: the password was changed after it was created
		return false, nil
	}
	if a.rec == nil && !a.corrupt {
		return true, nil // already in the default state; the file stays until the password is set
	}
	if err := root.Remove(AccountFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	a.rec, a.corrupt = nil, false
	return true, nil
}

func derive(pw string, salt []byte, m, t uint32, p uint8) []byte {
	return argon2.IDKey([]byte(pw), salt, t, m, p, argonKeyLen)
}

// Verify checks a login. It does comparable work whether or not the user name
// is right, and in constant time for the secret itself.
func (a *Account) Verify(user, pass string) bool {
	if len(user) > 64 || len(pass) > 4*MaxPasswordLen {
		return false
	}
	a.mu.Lock()
	rec, corrupt, dir := a.rec, a.corrupt, a.dir
	a.mu.Unlock()
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(User)) == 1
	switch {
	case dir == "" || corrupt:
		return false
	case rec == nil:
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(defaultPass)) == 1
		return userOK && passOK
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(rec.Salt)
	want, err2 := base64.RawStdEncoding.DecodeString(rec.Hash)
	if err1 != nil || err2 != nil {
		return false
	}
	a.sem <- struct{}{} // at most two hashes at once: a flood cannot exhaust memory
	got := derive(pass, salt, rec.Memory, rec.Time, rec.Threads)
	<-a.sem
	return subtle.ConstantTimeCompare(got, want) == 1 && userOK
}

var commonPasswords = map[string]bool{
	"password1234": true, "passwordpassword": true, "administrator": true, "administrator1": true, "changemechangeme": true,
	"123456789012": true, "1234567890123": true, "qwertyuiop12": true, "qwertyqwerty": true, "iloveyouiloveyou": true,
	"zaksdrive1234": true, "zaksdrivezaksdrive": true,
}

// ValidatePassword applies the rules for a new password. Length is what
// counts (no composition rules, so passphrases are welcome); the obvious
// choices are refused.
func ValidatePassword(next, current string) error {
	n := utf8.RuneCountInString(next)
	lower := strings.ToLower(strings.TrimSpace(next))
	switch {
	case !utf8.ValidString(next) || strings.ContainsAny(next, "\x00\r\n"):
		return &PasswordError{"the password contains characters that are not allowed"}
	case n < MinPasswordLen:
		return &PasswordError{fmt.Sprintf("the password must have at least %d characters; a few words make a good passphrase", MinPasswordLen)}
	case n > MaxPasswordLen:
		return &PasswordError{fmt.Sprintf("the password may have at most %d characters", MaxPasswordLen)}
	case strings.TrimSpace(next) == "":
		return &PasswordError{"the password is empty"}
	case next == current:
		return &PasswordError{"the new password must differ from the current one"}
	case commonPasswords[lower] || strings.Contains(lower, "admin") && n < 16 || strings.Count(lower, string(lower[0])) == len(lower):
		return &PasswordError{"that password is too easy to guess"}
	}
	return nil
}

// SetPassword verifies the current password and stores the new one. When it
// succeeds it removes the NO_ADMIN file; resetFileLeft reports a file that
// could not be removed (the stale-file rule makes that harmless).
func (a *Account) SetPassword(current, next string) (resetFileLeft bool, err error) {
	if a.dir == "" {
		return false, ErrUnavailable
	}
	if !a.Verify(User, current) {
		return false, ErrWrongPassword
	}
	if err := ValidatePassword(next, current); err != nil {
		return false, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return false, err
	}
	a.sem <- struct{}{}
	h := derive(next, salt, argonMemory, argonTime, argonThreads)
	<-a.sem
	rec := &record{Version: 1, Algo: "argon2id", Memory: argonMemory, Time: argonTime, Threads: argonThreads,
		Salt: base64.RawStdEncoding.EncodeToString(salt), Hash: base64.RawStdEncoding.EncodeToString(h), ChangedAt: a.now().UTC()}
	b, _ := json.Marshal(rec)

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return false, err
	}
	root, err := os.OpenRoot(a.dir)
	if err != nil {
		return false, err
	}
	defer root.Close()
	if err := writeFileAtomic(root, AccountFile, b); err != nil {
		return false, err
	}
	a.rec, a.corrupt = rec, false
	if err := root.Remove(ResetFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return false, nil
}

// RequestReset creates the NO_ADMIN file (used by the one-off command of the
// management tool). The next start of the server applies it.
func RequestReset(dir string) error {
	if dir == "" {
		return ErrUnavailable
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return writeFileAtomic(root, ResetFile, nil)
}

// writeFileAtomic writes through a temporary file in the same folder and a rename.
func writeFileAtomic(root *os.Root, name string, data []byte) error {
	var sfx [6]byte
	_, _ = rand.Read(sfx[:])
	tmp := fmt.Sprintf("%s.tmp-%x", name, sfx)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = root.Remove(tmp)
		return werr
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}
