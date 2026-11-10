// Package ctl implements the zaks-drive management command: installing, configuring,
// starting, stopping, updating and troubleshooting a ZAKS Drive deployment that runs under
// Docker Compose. The server itself is a separate binary (cmd/adapter, "zaks-drive-server").
//
// Design rules (see docs/cli.md):
//   - Docker is only ever invoked with explicit argument lists, never through a shell, and only
//     against the compose file of the selected deployment.
//   - Normal operation never deletes volumes or containers.
//   - Secrets are never printed. Anything shown or exported passes through the sanitizer, and
//     the support bundle is built from allowlisted fields.
//   - Nothing is sent anywhere unless the user runs a command that is documented to do so
//     (update, doctor --connectivity, check).
package ctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// Exit codes. They are part of the command's contract so scripts can react to them.
const (
	ExitOK           = 0
	ExitFail         = 1 // a check failed or an operation failed
	ExitUsage        = 2 // bad command line
	ExitPrereq       = 3 // Docker or Docker Compose missing or not usable
	ExitNoDeployment = 4 // no deployment found or it is not configured
	ExitUnhealthy    = 5 // started, but did not become ready
	ExitVerify       = 6 // a downloaded artifact failed verification
	ExitDeclined     = 7 // the user (or a missing --yes) declined a confirmation
)

// App carries everything the commands need from the outside world, so tests can replace it.
type App struct {
	Out, Err io.Writer
	In       io.Reader
	Runner   Runner
	HTTP     *http.Client
	Getenv   func(string) string
	Now      func() time.Time
	// Version is the version of this management command.
	Version string
	GOOS    string
	GOARCH  string
	// Interactive reports whether prompting is possible (stdin is a terminal).
	Interactive func() bool
	// ReadSecret reads a secret without echo. nil means: read a line from In (non-terminals).
	ReadSecret func(prompt string) (string, error)
	// ExePath returns the path of this executable (used to find the release payload).
	ExePath func() (string, error)
	// PollInterval is how often readiness is polled (default one second; tests shorten it).
	PollInterval time.Duration
	// DiskFree overrides the free-space lookup (tests).
	DiskFree func(path string) (int64, error)
	// ReleaseBase is where releases are downloaded from (default GitHub); tests and mirrors set it.
	ReleaseBase string
	// DownloadHTTP is the client for release downloads (default: a long timeout).
	DownloadHTTP *http.Client
	// PortBusy overrides the host port probe (tests); the default tries to listen on the port.
	PortBusy func(host string, port int) bool

	inReader *bufio.Reader
	// noRepoYet is set when the server answers "not ready" only because no repository exists yet.
	noRepoYet bool
}

// NewApp returns an App wired to the real process environment.
func NewApp(version string) *App {
	return &App{
		Out:         os.Stdout,
		Err:         os.Stderr,
		In:          os.Stdin,
		Runner:      ExecRunner{},
		HTTP:        &http.Client{Timeout: 15 * time.Second},
		Getenv:      os.Getenv,
		Now:         time.Now,
		Version:     version,
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		Interactive: stdinIsTerminal,
		ReadSecret:  readSecretFromTerminal,
		ExePath:     os.Executable,
	}
}

type command struct {
	name    string
	summary string
	run     func(a *App, ctx context.Context, args []string) int
}

func (a *App) commands() []command {
	return []command{
		{"setup", "create or update a deployment (interactive, or --non-interactive)", (*App).cmdSetup},
		{"config", "config validate: check the deployment's configuration without starting it", (*App).cmdConfig},
		{"start", "start ZAKS Drive and wait until it is ready", (*App).cmdStart},
		{"stop", "stop ZAKS Drive (containers and data are kept)", (*App).cmdStop},
		{"restart", "restart ZAKS Drive and wait until it is ready", (*App).cmdRestart},
		{"status", "show container, health and version information", (*App).cmdStatus},
		{"logs", "show sanitized logs (--follow, --tail N, --since 1h, --level, --iface)", (*App).cmdLogs},
		{"doctor", "diagnose the installation (--json, --connectivity)", (*App).cmdDoctor},
		{"check", "test every SharePoint access the server needs (contacts Microsoft)", (*App).cmdCheck},
		{"admin", "admin reset-password: return the admin UI login to admin/admin (to be changed at once)", (*App).cmdAdmin},
		{"repo", "repo list|show|add|update|remove|check|purge: manage repositories (local folder or SharePoint)", (*App).cmdRepo},
		{"update", "update to a verified release (--check, --version, --yes)", (*App).cmdUpdate},
		{"rollback", "return to the previously installed version", (*App).cmdRollback},
		{"support", "support bundle: collect sanitized diagnostics for a bug report", (*App).cmdSupport},
		{"version", "print the versions of this command and of the running server", (*App).cmdVersion},
		{"help", "show this help", nil},
	}
}

// Main runs the command line args (without the program name) and returns the exit code.
func (a *App) Main(args []string) int {
	ctx := context.Background()
	if len(args) == 0 {
		a.usage(a.Err)
		return ExitUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		a.usage(a.Out)
		return ExitOK
	case "-v", "--version":
		return a.cmdVersion(ctx, nil)
	}
	for _, c := range a.commands() {
		if c.name == args[0] && c.run != nil {
			return c.run(a, ctx, args[1:])
		}
	}
	fmt.Fprintf(a.Err, "zaks-drive: unknown command %q\n\n", args[0])
	a.usage(a.Err)
	return ExitUsage
}

func (a *App) usage(w io.Writer) {
	fmt.Fprintf(w, "ZAKS Drive %s - Open-Source Enterprise Content Gateway\n", a.Version)
	fmt.Fprintln(w, "Management command for a ZAKS Drive deployment (Docker Compose). The server is the separate zaks-drive-server binary.")
	fmt.Fprintln(w, "\nUsage:\n  zaks-drive <command> [flags]\n\nCommands:")
	cmds := a.commands()
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w, `
Common flags (every command that works on a deployment):
  --dir DIR     the deployment directory (default: $ZAKS_DRIVE_DIR, the current directory,
                ./deploy, or /opt/zaks-drive, whichever holds a docker-compose.yml)

Exit codes: 0 ok, 1 failure, 2 usage, 3 Docker missing or unusable, 4 no deployment,
5 not ready, 6 verification failed, 7 declined.

Run "zaks-drive <command> -h" for the flags of a command. Documentation: docs/cli.md`)
}

// ---- small helpers shared by the commands ----

func (a *App) printf(format string, args ...any) { fmt.Fprintf(a.Out, format, args...) }
func (a *App) errorf(format string, args ...any) { fmt.Fprintf(a.Err, format, args...) }

func (a *App) env(key string) string {
	if a.Getenv == nil {
		return ""
	}
	return a.Getenv(key)
}

// fail prints an error with an optional fix and returns the exit code.
func (a *App) fail(code int, msg, fix string) int {
	fmt.Fprintf(a.Err, "zaks-drive: %s\n", msg)
	if fix != "" {
		fmt.Fprintf(a.Err, "  fix: %s\n", fix)
	}
	return code
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func trimLines(s string, max int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return strings.Join(lines, "\n")
}

// marshalJSON indents like json.MarshalIndent but does not escape <, > and & (the output is
// read by people and scripts, never embedded in HTML).
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
