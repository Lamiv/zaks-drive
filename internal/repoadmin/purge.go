package repoadmin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/config"
)

// Purging deletes the documents of a detached local repository. It exists
// only in the one-off command on the Docker host, never in the admin API or
// the UI, and every safeguard is enforced here, not in the caller:
//
//   - the repository must be detached (no definition left, in config.yaml or
//     in repos.d, and no other repository using the same folder);
//   - only the local backend; SharePoint content is never deleted;
//   - a plan first, which counts what would go and issues a challenge phrase
//     that is valid for a short time and bound to exactly that state of the
//     folder (so it cannot be prepared in advance and cannot be replayed
//     after the folder changed);
//   - the exact repository ID typed again, and the phrase;
//   - deletion confined to the repository's own folder (os.Root), never
//     following links, and audited.

// PurgeValidity is how long a plan's challenge phrase stays valid.
const PurgeValidity = 15 * time.Minute

const purgeKeyFile = ".purge-key"

// PurgePlan describes what a purge would delete.
type PurgePlan struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"` // folder below the data directory
	Files     int       `json:"files"`
	Folders   int       `json:"folders"`
	Links     int       `json:"links"` // links inside the folder (removed, never followed)
	Bytes     int64     `json:"bytes"`
	Newest    time.Time `json:"newest,omitempty"`
	Challenge string    `json:"challenge"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// PurgeResult reports what was deleted.
type PurgeResult struct {
	ID      string `json:"id"`
	Files   int    `json:"files"`
	Folders int    `json:"folders"`
	Bytes   int64  `json:"bytes"`
}

// Detached returns the definitions of detached repositories.
func (m *Manager) Detached() ([]Spec, error) {
	root, err := m.open()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(config.DetachedDir)
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	var out []Spec
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := root.ReadFile(path.Join(config.DetachedDir, e.Name()))
		if err != nil {
			continue
		}
		var s Spec
		if yaml.Unmarshal(b, &s) == nil && s.ID != "" {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// purgeTarget finds the detached repository's data folder and refuses
// anything that is not a purgeable, detached local repository.
func (m *Manager) purgeTarget(root *os.Root, id string) (string, error) {
	if !config.ValidManagedID(id) {
		return "", apperr.New(apperr.CfgRepoIDInvalid, "repository id %q must be exactly 2 characters, A-Z or 0-9", id)
	}
	b, err := readIf(root, detachedPath(id))
	if err != nil {
		return "", err
	}
	if b == nil {
		if cfg, _ := m.Load(); hasID(cfg, id) {
			return "", apperr.New(apperr.AdminPurgeAttached, "repository %q is still defined; detach it first", id)
		}
		return "", apperr.New(apperr.AdminRepoNotFound, "there is no detached repository %q (a purge needs the record `zaks-drive repo remove` leaves)", id)
	}
	var s Spec
	if err := yaml.Unmarshal(b, &s); err != nil || s.ID != id {
		return "", apperr.New(apperr.AdminRepoInvalid, "the detached record of %q is not readable", id)
	}
	if s.Backend != config.BackendLocal || s.Local == nil {
		return "", apperr.New(apperr.AdminPurgeUnsupported, "repository %q uses the %s backend", id, firstNonEmpty(s.Backend, "sharepoint"))
	}
	rel := cleanDataPath(s.Local.Path)
	if rel == "" {
		return "", apperr.New(apperr.AdminRepoInvalid, "the detached record of %q has an unusable folder %q", id, s.Local.Path)
	}
	// Not in use: nothing defined under this ID, and no definition (static
	// or managed) uses the same folder.
	cfg, _ := m.Load()
	for _, r := range cfg.Repositories {
		if r.ID == id || (r.Backend == config.BackendLocal && cleanDataPath(r.Local.Path) == rel) {
			return "", apperr.New(apperr.AdminPurgeAttached, "repository %q uses the folder %q; detach it first", r.ID, rel)
		}
	}
	return rel, nil
}

// cleanDataPath returns p as a clean relative path or "".
func cleanDataPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" || strings.HasPrefix(p, "/") {
		return ""
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") || strings.Contains(c, ":") {
		return ""
	}
	return c
}

type scan struct {
	files, folders, links int
	bytes                 int64
	newest                time.Time
}

// scanFolder counts what is below rel without following links.
func scanFolder(data *os.Root, rel string) (scan, error) {
	var s scan
	var walk func(dir string) error
	walk = func(dir string) error {
		f, err := data.Open(dir)
		if err != nil {
			return err
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return err
		}
		for _, e := range entries {
			name := path.Join(dir, e.Name())
			st, err := data.Lstat(name)
			if err != nil {
				continue
			}
			switch {
			case st.Mode()&fs.ModeSymlink != 0:
				s.links++
			case st.IsDir():
				s.folders++
				if err := walk(name); err != nil {
					return err
				}
			default:
				s.files++
				s.bytes += st.Size()
				if st.ModTime().After(s.newest) {
					s.newest = st.ModTime()
				}
			}
		}
		return nil
	}
	if st, err := data.Lstat(rel); err != nil {
		return s, err
	} else if !st.IsDir() {
		return s, errors.New("the repository folder is not a directory")
	}
	return s, walk(rel)
}

func purgeKey(root *os.Root) ([]byte, error) {
	if b, err := readIf(root, purgeKeyFile); err != nil || len(b) >= 32 {
		return b, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := writeAtomic(root, purgeKeyFile, k, 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

// challenge builds the phrase for the current state of the folder:
// PURGE-<ID>-<expiry minutes, base 36>-<mac>. The MAC covers the ID, the
// expiry, the path and the counts, under a secret that never leaves the
// state directory.
func challenge(key []byte, id, rel string, expiryMinutes int64, s scan) string {
	h := hmac.New(sha256.New, key)
	fmt.Fprintf(h, "%s\x00%d\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d", id, expiryMinutes, rel, s.files, s.folders, s.links, s.bytes, s.newest.UnixNano())
	return fmt.Sprintf("PURGE-%s-%s-%s", id, strconv.FormatInt(expiryMinutes, 36), hex.EncodeToString(h.Sum(nil)[:6]))
}

func (m *Manager) dataRoot(root *os.Root) (*os.Root, error) {
	return root.OpenRoot(config.DataDir)
}

// PurgePlan inspects the folder of a detached local repository and returns
// what would be deleted together with the challenge phrase. It changes nothing.
func (m *Manager) PurgePlan(id string) (PurgePlan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := m.open()
	if err != nil {
		return PurgePlan{}, err
	}
	defer root.Close()
	rel, err := m.purgeTarget(root, id)
	if err != nil {
		return PurgePlan{}, err
	}
	data, err := m.dataRoot(root)
	if err != nil {
		return PurgePlan{}, err
	}
	defer data.Close()
	s, err := scanFolder(data, rel)
	if err != nil {
		return PurgePlan{}, apperr.New(apperr.AdminRepoNotFound, "the folder %q of repository %q does not exist (nothing to purge): %v", rel, id, err)
	}
	key, err := purgeKey(root)
	if err != nil {
		return PurgePlan{}, err
	}
	exp := m.clock().Add(PurgeValidity)
	plan := PurgePlan{
		ID: id, Path: rel, Files: s.files, Folders: s.folders, Links: s.links, Bytes: s.bytes, Newest: s.newest, ExpiresAt: exp.UTC(),
		Challenge: challenge(key, id, rel, exp.Unix()/60, s),
	}
	m.audit(root, "purge-plan", id, fmt.Sprintf("files=%d bytes=%d", s.files, s.bytes))
	return plan, nil
}

// Purge deletes the folder of a detached local repository. typedID must be
// the repository ID typed again, and phrase the challenge of a current plan
// of the folder in its present state.
func (m *Manager) Purge(id, typedID, phrase string) (PurgeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := m.open()
	if err != nil {
		return PurgeResult{}, err
	}
	defer root.Close()
	unlock, err := lock(root)
	if err != nil {
		return PurgeResult{}, err
	}
	defer unlock()

	rel, err := m.purgeTarget(root, id)
	if err != nil {
		return PurgeResult{}, err
	}
	badChallenge := apperr.New(apperr.AdminPurgeChallenge, "the typed repository ID or the challenge phrase does not match a current plan for %q (a wrong or expired phrase, or the folder changed since the plan)", id)
	if typedID != id {
		return PurgeResult{}, badChallenge
	}
	// PURGE-<ID>-<expiry>-<mac>
	parts := strings.Split(phrase, "-")
	if len(parts) != 4 || parts[0] != "PURGE" || parts[1] != id {
		return PurgeResult{}, badChallenge
	}
	expiry, err := strconv.ParseInt(parts[2], 36, 64)
	if err != nil {
		return PurgeResult{}, badChallenge
	}
	if m.clock().Unix()/60 > expiry || expiry > m.clock().Add(PurgeValidity).Unix()/60+1 {
		return PurgeResult{}, badChallenge
	}
	data, err := m.dataRoot(root)
	if err != nil {
		return PurgeResult{}, err
	}
	defer data.Close()
	s, err := scanFolder(data, rel)
	if err != nil {
		return PurgeResult{}, apperr.New(apperr.AdminRepoNotFound, "the folder %q of repository %q does not exist (nothing to purge)", rel, id)
	}
	key, err := purgeKey(root)
	if err != nil {
		return PurgeResult{}, err
	}
	if !hmac.Equal([]byte(challenge(key, id, rel, expiry, s)), []byte(phrase)) {
		// A wrong phrase, an expired one, or a folder that is no longer what
		// the plan described: all look the same, on purpose.
		return PurgeResult{}, badChallenge
	}
	if err := data.RemoveAll(rel); err != nil {
		m.audit(root, "purge-failed", id, fmt.Sprintf("files=%d bytes=%d error=%v", s.files, s.bytes, err))
		return PurgeResult{}, apperr.New(apperr.AdminStateUnavailable, "the purge stopped part-way: %v", err).Wrap(err)
	}
	_ = root.Remove(detachedPath(id))
	m.audit(root, "purge", id, fmt.Sprintf("files=%d folders=%d bytes=%d", s.files, s.folders, s.bytes))
	return PurgeResult{ID: id, Files: s.files, Folders: s.folders, Bytes: s.bytes}, nil
}
