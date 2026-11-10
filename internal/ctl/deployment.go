package ctl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Deployment is one ZAKS Drive installation: a directory holding docker-compose.yml, .env
// (secrets) and config.yaml. The layout is the same as the repository's deploy/ directory.
type Deployment struct {
	Dir         string
	ComposeFile string
	EnvFile     string
	ConfigFile  string
}

func newDeployment(dir string) *Deployment {
	return &Deployment{
		Dir:         dir,
		ComposeFile: filepath.Join(dir, "docker-compose.yml"),
		EnvFile:     filepath.Join(dir, ".env"),
		ConfigFile:  filepath.Join(dir, "config.yaml"),
	}
}

// DeploymentNotFoundError lists where the command looked.
type DeploymentNotFoundError struct{ Searched []string }

func (e *DeploymentNotFoundError) Error() string {
	return "no ZAKS Drive deployment found (looked for docker-compose.yml in: " + strings.Join(e.Searched, ", ") + ")"
}

// locate finds the deployment: --dir, $ZAKS_DRIVE_DIR, the current directory, ./deploy, /opt/zaks-drive.
func (a *App) locate(dirFlag string) (*Deployment, error) {
	var candidates []string
	if dirFlag != "" {
		candidates = []string{dirFlag}
	} else {
		if d := a.env("ZAKS_DRIVE_DIR"); d != "" {
			candidates = append(candidates, d)
		}
		candidates = append(candidates, ".", "deploy", "/opt/zaks-drive")
	}
	var searched []string
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		searched = append(searched, abs)
		if st, err := os.Stat(filepath.Join(abs, "docker-compose.yml")); err == nil && !st.IsDir() {
			return newDeployment(abs), nil
		}
	}
	return nil, &DeploymentNotFoundError{Searched: searched}
}

// SourceMode reports whether the compose file builds the image from a source checkout
// (the repository's deploy/ directory) instead of running a released image.
func (d *Deployment) SourceMode() bool {
	b, err := os.ReadFile(d.ComposeFile)
	if err != nil {
		return false
	}
	return bytes.Contains(b, []byte("\n    build:"))
}

// ---- .env ----

// ReadEnv parses a .env file: KEY=VALUE lines, # comments, optional single or double quotes.
// Values are not expanded. A missing file is reported as an error the caller can test with
// errors.Is(err, fs.ErrNotExist).
func ReadEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

// envPort returns the integer value of key in env, or def.
func envPort(env map[string]string, key string, def int) int {
	if v, ok := env[key]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return def
}

// Ports are the host ports of the deployment, as compose will publish them.
type Ports struct {
	REST, CS, Admin int
	Bind            string
}

func (d *Deployment) ports() Ports {
	env, _ := ReadEnv(d.EnvFile)
	p := Ports{
		REST:  envPort(env, "REST_PORT", 8080),
		CS:    envPort(env, "CS_PORT", 8090),
		Admin: envPort(env, "ADMIN_PORT", 9090),
		Bind:  env["BIND_ADDRESS"],
	}
	if p.Bind == "" {
		p.Bind = "0.0.0.0"
	}
	return p
}

// secretValues returns the values of .env entries that are secrets, so the sanitizer can mask
// them by value in addition to masking by pattern.
func (d *Deployment) secretValues() []string {
	env, _ := ReadEnv(d.EnvFile)
	var out []string
	for k, v := range env {
		if len(v) < 6 {
			continue
		}
		u := strings.ToUpper(k)
		if strings.Contains(u, "SECRET") || strings.Contains(u, "TOKEN") || strings.Contains(u, "KEY") || strings.Contains(u, "PASSWORD") {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// ---- state ----

// State records what the command installed, for updates and rollback. It lives in
// <dir>/.zaks-drive/state.json and contains no secrets.
type State struct {
	Current  string       `json:"current"`  // version whose image runs now
	Previous string       `json:"previous"` // version to return to with rollback
	History  []StateEvent `json:"history"`
	Source   string       `json:"source,omitempty"`
}

// StateEvent is one entry of State.History.
type StateEvent struct {
	At      time.Time `json:"at"`
	Action  string    `json:"action"` // setup, update, rollback
	Version string    `json:"version"`
}

func (d *Deployment) statePath() string { return filepath.Join(d.Dir, ".zaks-drive", "state.json") }

func (d *Deployment) loadState() State {
	var s State
	if b, err := os.ReadFile(d.statePath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (d *Deployment) saveState(s State) error {
	if len(s.History) > 50 {
		s.History = s.History[len(s.History)-50:]
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.statePath()), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(d.statePath(), append(b, '\n'), 0o600)
}

// ---- files ----

// writeFileAtomic writes data to a temporary file in the same directory (created with mode 0600)
// and renames it over path, so a reader never sees a half-written file and a secret file is
// never briefly world-readable.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// backupFile copies path to path.bak-<timestamp> with the same mode, if it exists.
func backupFile(path string, now time.Time) (string, error) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	dst := fmt.Sprintf("%s.bak-%s", path, now.UTC().Format("20060102T150405Z"))
	return dst, writeFileAtomic(dst, b, st.Mode().Perm()&0o644|0o600)
}

// hasState reports whether the compose file gives the server a state
// directory (the volume that holds managed repositories).
func (d *Deployment) hasState() bool {
	b, err := os.ReadFile(d.ComposeFile)
	return err == nil && bytes.Contains(b, []byte("STATE_DIR"))
}
