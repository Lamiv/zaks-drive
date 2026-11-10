package ctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/config"
)

var envMu sync.Mutex

// loadConfig loads the deployment's config.yaml exactly as the container would: ${VAR}
// references and the well-known overrides see only the variables of the deployment's .env
// file, never the environment of the shell that runs this command. The process environment is
// restored before returning, so no secret stays in it and none reaches a child process.
func loadConfig(d *Deployment) (config.Config, error) {
	env, err := ReadEnv(d.EnvFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return config.Config{}, fmt.Errorf("cannot read %s: %w", d.EnvFile, err)
	}
	envMu.Lock()
	defer envMu.Unlock()
	saved := os.Environ()
	os.Clearenv()
	defer func() {
		os.Clearenv()
		for _, kv := range saved {
			if k, v, ok := strings.Cut(kv, "="); ok {
				_ = os.Setenv(k, v)
			}
		}
	}()
	// Managed repositories live in the server's state volume, which this host cannot
	// read. Pointing STATE_DIR at a folder that does not exist tells the loader that
	// repositories may also come from there, without reading anything.
	if d.hasState() {
		_ = os.Setenv("STATE_DIR", filepath.Join(os.TempDir(), "zaks-drive-state-is-in-the-container"))
	}
	for k, v := range env {
		_ = os.Setenv(k, v)
	}
	return config.Load(d.ConfigFile)
}

// validateDeployment checks that the deployment's files exist and that the configuration is
// valid. It returns a list of human-readable problems (empty = valid). It never contacts
// Microsoft or SAP.
func (a *App) validateDeployment(d *Deployment) (cfg config.Config, problems []string) {
	for _, f := range []struct{ path, what string }{{d.EnvFile, ".env"}, {d.ConfigFile, "config.yaml"}} {
		if _, err := os.Stat(f.path); err != nil {
			problems = append(problems, fmt.Sprintf("%s is missing (%s); run: zaks-drive setup --dir %s", f.what, f.path, d.Dir))
		}
	}
	if len(problems) > 0 {
		return cfg, problems
	}
	cfg, err := loadConfig(d)
	if err != nil {
		text := apperr.Format(err)
		for _, line := range strings.Split(strings.TrimSpace(a.sanitizerFor(d).Text(text)), "\n") {
			if strings.TrimSpace(line) != "" {
				problems = append(problems, line)
			}
		}
	}
	return cfg, problems
}

func (a *App) cmdConfig(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] != "validate" {
		fmt.Fprintln(a.Err, "usage: zaks-drive config validate [--dir DIR]")
		return ExitUsage
	}
	fset := flag.NewFlagSet("config validate", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	if err := fset.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	d, err := a.locate(*dir)
	if err != nil {
		return a.fail(ExitNoDeployment, err.Error(), "run: zaks-drive setup, or pass --dir")
	}
	cfg, problems := a.validateDeployment(d)
	if len(problems) > 0 {
		fmt.Fprintf(a.Err, "The configuration in %s is NOT valid:\n", d.Dir)
		for _, p := range problems {
			fmt.Fprintf(a.Err, "  %s\n", p)
		}
		return ExitFail
	}
	a.printf("The configuration in %s is valid.\n", d.Dir)
	a.printf("  interfaces: %s\n", strings.Join(cfg.Server.Interfaces, ", "))
	for _, r := range cfg.Repositories {
		a.printf("  repository: %s\n", r.ID)
	}
	for _, c := range cfg.ContentServer.Repositories {
		a.printf("  SAP content repository: %s (signature: %s)\n", c.ContRep, c.Signature)
	}
	a.printf("This checked the files only. Microsoft and SAP were not contacted (use: zaks-drive check).\n")
	return ExitOK
}
