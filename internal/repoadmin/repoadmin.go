// Package repoadmin creates, changes and detaches the repositories that live
// in the state directory (repos.d/<ID>.yaml plus secrets/<ID>/client-secret).
//
// It is the one place that writes those files. The server's admin API and the
// one-off container used when the server is down both call it, so the files
// are always created by the server's own user and checked by the server's
// own loader: every change is written, validated by loading the whole
// configuration, and rolled back if the result is not valid.
package repoadmin

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/config"
)

// Spec is the settings of one repository, as written to repos.d. It holds no
// secret: the client secret is a separate file.
type Spec struct {
	ID          string `json:"id" yaml:"id"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Backend     string `json:"backend,omitempty" yaml:"backend,omitempty"`
	ReadOnly    bool   `json:"readOnly,omitempty" yaml:"readOnly,omitempty"`

	Local *LocalSpec `json:"local,omitempty" yaml:"local,omitempty"`

	SiteURL   string `json:"siteUrl,omitempty" yaml:"siteUrl,omitempty"`
	UserID    string `json:"userId,omitempty" yaml:"userId,omitempty"`
	SiteID    string `json:"siteId,omitempty" yaml:"siteId,omitempty"`
	DriveID   string `json:"driveId,omitempty" yaml:"driveId,omitempty"`
	DriveName string `json:"driveName,omitempty" yaml:"driveName,omitempty"`
	RootPath  string `json:"rootPath,omitempty" yaml:"rootPath,omitempty"`

	TenantID string `json:"tenantId,omitempty" yaml:"tenantId,omitempty"`
	ClientID string `json:"clientId,omitempty" yaml:"clientId,omitempty"`
	// ClientSecretFile is derived from the ID; callers never set it.
	ClientSecretFile string `json:"-" yaml:"clientSecretFile,omitempty"`

	SAP *SAPSpec `json:"sap,omitempty" yaml:"sap,omitempty"`
}

type LocalSpec struct {
	Path     string `json:"path,omitempty" yaml:"path,omitempty"`
	Versions *int   `json:"versions,omitempty" yaml:"versions,omitempty"`
}

type SAPSpec struct {
	ContRep   string `json:"contRep,omitempty" yaml:"contRep,omitempty"`
	Signature string `json:"signature,omitempty" yaml:"signature,omitempty"`
}

// View is a repository as shown to an administrator: its settings, where it
// comes from, and whether a secret is stored (never the secret itself).
type View struct {
	Spec
	Source    string `json:"source"` // "static" (config.yaml) or "managed"
	SecretSet bool   `json:"secretSet"`
}

// Manager changes the managed repositories under StateDir.
type Manager struct {
	StateDir string
	// Load reads and validates the whole configuration, including the files
	// this manager writes (config.Load of the server's config file).
	Load func() (config.Config, error)
	// Actor is recorded in the audit log: "api", "cli" or "one-off".
	Actor string

	mu  sync.Mutex
	now func() time.Time
}

func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func secretPath(id string) string   { return path.Join(config.SecretsDir, id, "client-secret") }
func repoPath(id string) string     { return path.Join(config.ReposDir, id+".yaml") }
func detachedPath(id string) string { return path.Join(config.DetachedDir, id+".yaml") }

const (
	auditFile   = "audit.log"
	lockFile    = ".lock"
	lockTimeout = 5 * time.Second
	lockStale   = time.Minute
	maxSecret   = 4096
)

// open returns the state directory, or the coded error that explains why
// managed repositories are unavailable.
func (m *Manager) open() (*os.Root, error) {
	if m.StateDir == "" {
		return nil, apperr.New(apperr.AdminStateUnavailable, "server.stateDir (STATE_DIR) is not set")
	}
	if err := os.MkdirAll(m.StateDir, 0o700); err != nil {
		return nil, apperr.New(apperr.AdminStateUnavailable, "cannot create %s: %v", m.StateDir, err).Wrap(err)
	}
	root, err := os.OpenRoot(m.StateDir)
	if err != nil {
		return nil, apperr.New(apperr.AdminStateUnavailable, "cannot open %s: %v", m.StateDir, err).Wrap(err)
	}
	for _, d := range []string{config.ReposDir, config.SecretsDir, config.DataDir, config.DetachedDir} {
		if err := root.MkdirAll(d, 0o700); err != nil {
			root.Close()
			return nil, apperr.New(apperr.AdminStateUnavailable, "cannot create %s in the state directory (is it writable by the server?): %v", d, err).Wrap(err)
		}
	}
	return root, nil
}

// lock serialises writers, also across processes (the server and a one-off
// container may both be running): an exclusively created lock file, removed
// when stale.
func lock(root *os.Root) (func(), error) {
	deadline := time.Now().Add(lockTimeout)
	for {
		f, err := root.OpenFile(lockFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			f.Close()
			return func() { _ = root.Remove(lockFile) }, nil
		}
		if st, serr := root.Stat(lockFile); serr == nil && time.Since(st.ModTime()) > lockStale {
			_ = root.Remove(lockFile)
			continue
		}
		if time.Now().After(deadline) {
			return nil, apperr.New(apperr.AdminStateUnavailable, "another change to the repositories is in progress (waited %s)", lockTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func writeAtomic(root *os.Root, name string, data []byte, perm os.FileMode) error {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	tmp := name + ".tmp-" + hex.EncodeToString(suffix[:])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

// readIf returns the file's content, or nil when it does not exist.
func readIf(root *os.Root, name string) ([]byte, error) {
	b, err := root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// restore puts a file back to a previous state (nil = absent).
func restore(root *os.Root, name string, prev []byte, perm os.FileMode) {
	if prev == nil {
		_ = root.Remove(name)
		return
	}
	_ = writeAtomic(root, name, prev, perm)
}

// ---- reading ----

func specOf(r config.RepositoryConfig) Spec {
	s := Spec{
		ID: r.ID, Description: r.Description, Backend: r.Backend, ReadOnly: r.ReadOnly,
		SiteURL: r.SiteURL, UserID: r.UserID, SiteID: r.SiteID, DriveID: r.DriveID, DriveName: r.DriveName, RootPath: r.RootPath,
		TenantID: r.TenantID, ClientID: r.ClientID, ClientSecretFile: r.ClientSecretFile,
	}
	if r.Backend == config.BackendLocal {
		s.Local = &LocalSpec{Path: r.Local.Path, Versions: r.Local.Versions}
	}
	if r.SAP != nil {
		s.SAP = &SAPSpec{ContRep: r.SAP.ContRep, Signature: r.SAP.Signature}
	}
	return s
}

// List returns every repository, static and managed. When the configuration
// does not validate it still returns what could be read, with the error.
func (m *Manager) List() ([]View, error) {
	cfg, err := m.Load()
	out := make([]View, 0, len(cfg.Repositories))
	for _, r := range cfg.Repositories {
		out = append(out, View{Spec: specOf(r), Source: r.Source, SecretSet: r.ClientSecret != ""})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

// Get returns one repository.
func (m *Manager) Get(id string) (View, error) {
	views, err := m.List()
	for _, v := range views {
		if v.ID == id {
			return v, nil
		}
	}
	if err != nil {
		return View{}, err
	}
	return View{}, apperr.New(apperr.AdminRepoNotFound, "there is no repository %q", id)
}

// ---- writing ----

func validSecret(secret string) error {
	secret = strings.TrimSpace(secret)
	switch {
	case secret == "":
		return apperr.New(apperr.AdminRepoInvalid, "the client secret is empty")
	case len(secret) > maxSecret:
		return apperr.New(apperr.AdminRepoInvalid, "the client secret is longer than %d characters", maxSecret)
	case strings.ContainsAny(secret, "\r\n\x00"):
		return apperr.New(apperr.AdminRepoInvalid, "the client secret must be a single line")
	}
	return nil
}

func (s Spec) validateShape() error {
	if !config.ValidManagedID(s.ID) {
		return apperr.New(apperr.CfgRepoIDInvalid, "repository id %q must be exactly 2 characters, A-Z or 0-9", s.ID)
	}
	if s.Backend == "" {
		return apperr.New(apperr.AdminRepoInvalid, "the backend is required: sharepoint or local")
	}
	return nil
}

func marshal(s Spec) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// Add creates a managed repository. secret is the Microsoft client secret of
// a repository with its own application ("" otherwise).
func (m *Manager) Add(spec Spec, secret string) (View, error) {
	return m.write(spec, &secret, true)
}

// Update replaces the settings of a managed repository. secret: nil keeps the
// stored secret, otherwise it is replaced.
func (m *Manager) Update(spec Spec, secret *string) (View, error) {
	return m.write(spec, secret, false)
}

func (m *Manager) write(spec Spec, secret *string, create bool) (View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := spec.validateShape(); err != nil {
		return View{}, err
	}
	root, err := m.open()
	if err != nil {
		return View{}, err
	}
	defer root.Close()
	unlock, err := lock(root)
	if err != nil {
		return View{}, err
	}
	defer unlock()

	prevYAML, err := readIf(root, repoPath(spec.ID))
	if err != nil {
		return View{}, err
	}
	switch {
	case create && prevYAML != nil:
		return View{}, apperr.New(apperr.AdminRepoExists, "repository %q already exists", spec.ID)
	case !create && prevYAML == nil:
		return View{}, m.notManaged(spec.ID)
	}
	if !create {
		// The storage type is chosen once, when the repository is created. Changing it would point the
		// repository at an empty store while its documents stay where they are, so it is refused here
		// (the command, the API and the page all come through this check).
		var prev Spec
		if err := yaml.Unmarshal(prevYAML, &prev); err == nil {
			old := firstNonEmpty(prev.Backend, config.BackendSharePoint)
			if firstNonEmpty(spec.Backend, config.BackendSharePoint) != old {
				return View{}, apperr.New(apperr.AdminRepoInvalid,
					"the backend of repository %q is %s and cannot be changed: documents are not moved between a local folder and SharePoint. Detach it and add a new repository with the other backend, then copy the documents with the REST API", spec.ID, old)
			}
		}
	}
	if create {
		// the ID may also belong to config.yaml
		if cfg, _ := m.Load(); hasID(cfg, spec.ID) {
			return View{}, apperr.New(apperr.AdminRepoExists, "repository %q already exists", spec.ID)
		}
	}

	own := spec.TenantID != "" || spec.ClientID != ""
	spec.ClientSecretFile = ""
	if own {
		spec.ClientSecretFile = secretPath(spec.ID)
	}
	prevSecret, err := readIf(root, secretPath(spec.ID))
	if err != nil {
		return View{}, err
	}
	if secret != nil && *secret != "" {
		if !own {
			return View{}, apperr.New(apperr.AdminRepoInvalid, "a client secret was given but tenantId and clientId are not set")
		}
		if err := validSecret(*secret); err != nil {
			return View{}, err
		}
	}

	body, err := marshal(spec)
	if err != nil {
		return View{}, err
	}
	if err := writeAtomic(root, repoPath(spec.ID), body, 0o600); err != nil {
		return View{}, apperr.New(apperr.AdminStateUnavailable, "cannot write the repository definition: %v", err).Wrap(err)
	}
	rollback := func() {
		restore(root, repoPath(spec.ID), prevYAML, 0o600)
		restore(root, secretPath(spec.ID), prevSecret, 0o600)
		if prevSecret == nil {
			_ = root.Remove(path.Dir(secretPath(spec.ID)))
		}
	}
	switch {
	case !own:
		_ = root.Remove(secretPath(spec.ID))
		_ = root.Remove(path.Dir(secretPath(spec.ID)))
	case secret != nil && *secret != "":
		if err := root.MkdirAll(path.Dir(secretPath(spec.ID)), 0o700); err != nil {
			rollback()
			return View{}, apperr.New(apperr.AdminStateUnavailable, "cannot create the secret folder: %v", err).Wrap(err)
		}
		if err := writeAtomic(root, secretPath(spec.ID), []byte(strings.TrimSpace(*secret)+"\n"), 0o600); err != nil {
			rollback()
			return View{}, apperr.New(apperr.AdminStateUnavailable, "cannot write the secret: %v", err).Wrap(err)
		}
	}

	// The server's own loader has the last word.
	if _, err := m.Load(); err != nil {
		rollback()
		return View{}, rejected(err)
	}
	view, err := m.viewOf(spec.ID)
	if err != nil {
		rollback()
		return View{}, err
	}
	action := "update"
	if create {
		action = "add"
	}
	_ = root.Remove(detachedPath(spec.ID))
	m.audit(root, action, spec.ID, fmt.Sprintf("backend=%s secret=%s", spec.Backend, secretState(secret, view.SecretSet)))
	return view, nil
}

func secretState(given *string, set bool) string {
	switch {
	case given != nil && *given != "":
		return "replaced"
	case set:
		return "kept"
	}
	return "none"
}

func (m *Manager) viewOf(id string) (View, error) {
	cfg, err := m.Load()
	if err != nil {
		return View{}, rejected(err)
	}
	for _, r := range cfg.Repositories {
		if r.ID == id {
			return View{Spec: specOf(r), Source: r.Source, SecretSet: r.ClientSecret != ""}, nil
		}
	}
	return View{}, apperr.New(apperr.AdminRepoNotFound, "there is no repository %q", id)
}

func hasID(cfg config.Config, id string) bool {
	for _, r := range cfg.Repositories {
		if r.ID == id {
			return true
		}
	}
	return false
}

// notManaged explains why id cannot be changed: it is in config.yaml, or absent.
func (m *Manager) notManaged(id string) error {
	if cfg, _ := m.Load(); hasID(cfg, id) {
		return apperr.New(apperr.AdminRepoStatic, "repository %q is defined in config.yaml", id)
	}
	return apperr.New(apperr.AdminRepoNotFound, "there is no managed repository %q", id)
}

// rejected wraps a validation error for the caller; the loader's detail is kept.
func rejected(err error) error {
	if _, ok := apperr.As(err); ok && !isJoin(err) {
		return err
	}
	return apperr.New(apperr.AdminRepoInvalid, "%s", strings.TrimSpace(apperr.Format(err))).Wrap(err)
}

func isJoin(err error) bool {
	_, ok := err.(interface{ Unwrap() []error })
	return ok
}

// Detach stops serving a repository and removes its definition and secret.
// Documents are not touched. A repository exposed to SAP needs allowInUse.
func (m *Manager) Detach(id string, allowInUse bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !config.ValidManagedID(id) {
		return apperr.New(apperr.CfgRepoIDInvalid, "repository id %q must be exactly 2 characters, A-Z or 0-9", id)
	}
	root, err := m.open()
	if err != nil {
		return err
	}
	defer root.Close()
	unlock, err := lock(root)
	if err != nil {
		return err
	}
	defer unlock()

	prevYAML, err := readIf(root, repoPath(id))
	if err != nil {
		return err
	}
	if prevYAML == nil {
		return m.notManaged(id)
	}
	var spec Spec
	if err := yaml.Unmarshal(prevYAML, &spec); err == nil && spec.SAP != nil && !allowInUse {
		return apperr.New(apperr.AdminRepoInUse, "repository %q is exposed to SAP as content repository %q", id, firstNonEmpty(spec.SAP.ContRep, id))
	}
	// Keep the definition (it holds no secret) so the repository can be added
	// again and, for a local one, its folder can be found for a purge.
	_ = writeAtomic(root, detachedPath(id), prevYAML, 0o600)
	if err := root.Remove(repoPath(id)); err != nil {
		return apperr.New(apperr.AdminStateUnavailable, "cannot remove the definition: %v", err).Wrap(err)
	}
	_ = root.Remove(secretPath(id))
	_ = root.Remove(path.Dir(secretPath(id)))
	m.audit(root, "detach", id, fmt.Sprintf("inUse=%v allowInUse=%v", spec.SAP != nil, allowInUse))
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- state fingerprint and last-good snapshot ----

// Fingerprint identifies the current managed state (definitions and the
// content of secrets, hashed). The server remembers the value it started
// with; a different value now means "changes pending restart".
func (m *Manager) Fingerprint() (string, error) {
	root, err := m.open()
	if err != nil {
		return "", err
	}
	defer root.Close()
	h := sha256.New()
	add := func(dir string, recurse bool) error {
		var walk func(d string) error
		walk = func(d string) error {
			f, err := root.Open(d)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			entries, err := f.ReadDir(-1)
			f.Close()
			if err != nil {
				return err
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, e := range entries {
				name := path.Join(d, e.Name())
				if strings.Contains(e.Name(), ".tmp-") {
					continue
				}
				if e.IsDir() {
					if recurse {
						if err := walk(name); err != nil {
							return err
						}
					}
					continue
				}
				b, err := root.ReadFile(name)
				if err != nil {
					return err
				}
				fmt.Fprintf(h, "%s\x00%d\x00", name, len(b))
				h.Write(b)
			}
			return nil
		}
		return walk(dir)
	}
	if err := add(config.ReposDir, false); err != nil {
		return "", err
	}
	if err := add(config.SecretsDir, true); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)[:12]), nil
}

// LastGoodDir is where Snapshot keeps the definitions of the last state that
// started successfully, relative to the state directory.
const LastGoodDir = config.LastGoodDir

// Snapshot copies repos.d to last-good/repos.d (definitions only; secrets
// stay where they are). The server calls it after a successful start so a
// later broken change can fall back to it.
func (m *Manager) Snapshot() error {
	root, err := m.open()
	if err != nil {
		return err
	}
	defer root.Close()
	dst := path.Join(LastGoodDir, config.ReposDir)
	_ = root.RemoveAll(dst)
	if err := root.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	f, err := root.Open(config.ReposDir)
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := root.ReadFile(path.Join(config.ReposDir, e.Name()))
		if err != nil {
			return err
		}
		if err := writeAtomic(root, path.Join(dst, e.Name()), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// ---- audit log ----

type auditEntry struct {
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	ID     string    `json:"id"`
	Detail string    `json:"detail,omitempty"`
}

// audit appends one line to audit.log: who changed which repository and
// when. Never a secret, never document content.
func (m *Manager) audit(root *os.Root, action, id, detail string) {
	line, err := json.Marshal(auditEntry{Time: m.clock().UTC(), Actor: firstNonEmpty(m.Actor, "unknown"), Action: action, ID: id, Detail: detail})
	if err != nil {
		return
	}
	f, err := root.OpenFile(auditFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// Audit records an action taken outside this package (for example a purge).
func (m *Manager) Audit(action, id, detail string) {
	root, err := m.open()
	if err != nil {
		return
	}
	defer root.Close()
	m.audit(root, action, id, detail)
}

// AuditLog returns the last n entries, newest last.
func (m *Manager) AuditLog(n int) ([]byte, error) {
	root, err := m.open()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(auditFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return append(bytes.Join(lines, []byte("\n")), '\n'), nil
}
