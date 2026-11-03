package ctl

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"

	"errors"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Lamiv/zaks-drive/internal/config"
)

// ---- command parsing and help ----

func TestHelpListsEveryCommandAndExitCodes(t *testing.T) {
	e := newEnv(t)
	if rc := e.main("help"); rc != ExitOK {
		t.Fatalf("help exit %d", rc)
	}
	for _, want := range []string{"setup", "config", "start", "stop", "restart", "status", "logs", "doctor", "check", "update", "rollback", "support", "version", "Exit codes", "--dir"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, e.out.String())
		}
	}
	if !strings.Contains(e.out.String(), "zaks-drive-server") {
		t.Error("help must say that the server is the separate zaks-drive-server binary")
	}
}

func TestNoArgumentsAndUnknownCommandAreUsageErrors(t *testing.T) {
	e := newEnv(t)
	if rc := e.main(); rc != ExitUsage {
		t.Errorf("no args: exit %d, want %d", rc, ExitUsage)
	}
	if rc := e.main("frobnicate"); rc != ExitUsage || !strings.Contains(e.err.String(), `unknown command "frobnicate"`) {
		t.Errorf("unknown: exit %d, %q", rc, e.err.String())
	}
	if rc := e.main("--version"); rc != ExitOK || !strings.Contains(e.out.String(), "0.1.0") {
		t.Errorf("--version: exit %d, %q", rc, e.out.String())
	}
}

func TestBadFlagsAreUsageErrors(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{{"start", "--nope"}, {"logs", "--tail", "x"}, {"logs", "--level", "loud", "--dir", e.dir}, {"logs", "--iface", "sap", "--dir", e.dir}, {"logs", "--since", "yesterday", "--dir", e.dir}, {"config"}} {
		if rc := e.main(args...); rc != ExitUsage {
			t.Errorf("%v: exit %d, want %d (%s)", args, rc, ExitUsage, e.err.String())
		}
	}
}

// ---- deployment discovery ----

func TestLocateOrderAndNotFound(t *testing.T) {
	e := newEnv(t)
	d, err := e.app.locate(e.dir)
	if err != nil || d.Dir != e.dir {
		t.Fatalf("explicit dir: %v %v", d, err)
	}
	e.app.Getenv = func(k string) string {
		if k == "ZAKS_DRIVE_DIR" {
			return e.dir
		}
		return ""
	}
	if d, err := e.app.locate(""); err != nil || d.Dir != e.dir {
		t.Fatalf("ZAKS_DRIVE_DIR: %v %v", d, err)
	}
	e.app.Getenv = func(string) string { return "" }
	empty := t.TempDir()
	_, err = e.app.locate(empty)
	var nf *DeploymentNotFoundError
	if !errors.As(err, &nf) || len(nf.Searched) != 1 {
		t.Fatalf("want DeploymentNotFoundError, got %v", err)
	}
	if rc := e.main("status", "--dir", empty); rc != ExitNoDeployment {
		t.Errorf("status without deployment: exit %d", rc)
	}
	if !strings.Contains(e.err.String(), "zaks-drive setup") {
		t.Errorf("the message must point to setup: %s", e.err.String())
	}
}

// ---- Docker prerequisites ----

func TestMissingDockerDependenciesAreExplained(t *testing.T) {
	cases := []struct {
		name    string
		handler func(c call) (Result, error)
		want    []string
	}{
		{"docker not installed", func(c call) (Result, error) { return Result{}, ErrNotInstalled }, []string{"Docker is not installed", "does not install Docker"}},
		{"permission denied", func(c call) (Result, error) {
			return Result{ExitCode: 1, Stderr: "permission denied while trying to connect to the Docker daemon socket"}, nil
		}, []string{"may not use the Docker daemon", "docker\" group"}},
		{"daemon not running", func(c call) (Result, error) {
			return Result{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"}, nil
		}, []string{"not running", "systemctl start docker"}},
		{"compose plugin missing", func(c call) (Result, error) {
			if strings.HasPrefix(c.String(), "docker compose version") {
				return Result{ExitCode: 1, Stderr: "docker: 'compose' is not a docker command"}, nil
			}
			return Result{Stdout: "27.1.0"}, nil
		}, []string{"Compose v2 is not available", "docker-compose-plugin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.run.handler = tc.handler
			for _, cmd := range []string{"start", "stop", "status", "restart"} {
				if rc := e.main(cmd, "--dir", e.dir); rc != ExitPrereq {
					t.Errorf("%s: exit %d, want %d", cmd, rc, ExitPrereq)
				}
				for _, w := range tc.want {
					if !strings.Contains(e.err.String(), w) {
						t.Errorf("%s: message lacks %q: %s", cmd, w, e.err.String())
					}
				}
			}
			for _, c := range e.run.all() {
				if strings.Contains(c, "compose -f") && strings.Contains(c, " up ") {
					t.Errorf("nothing may be started when Docker is unusable: %s", c)
				}
			}
		})
	}
}

// ---- lifecycle ----

func TestStartBuildsExactCommandAndReportsEndpoints(t *testing.T) {
	e := newEnv(t)
	if rc := e.main("start", "--dir", e.dir, "--timeout", "5s"); rc != ExitOK {
		t.Fatalf("start exit %d\n%s", rc, e.both())
	}
	want := "docker compose -f " + filepath.Join(e.dir, "docker-compose.yml") + " up -d"
	if !e.run.has(want) {
		t.Fatalf("missing %q in %v", want, e.run.all())
	}
	for _, c := range e.run.calls {
		if strings.Contains(c.String(), " up -d") {
			if c.Dir != e.dir {
				t.Errorf("compose must run in the deployment directory, ran in %q", c.Dir)
			}
			found := false
			for _, kv := range c.Env {
				if kv == "ZAKS_DRIVE_VERSION=0.1.0" {
					found = true
				}
			}
			if !found {
				t.Errorf("the installed version must select the image tag, env=%v", c.Env)
			}
		}
	}
	for _, w := range []string{"ZAKS Drive is ready", "REST API", "SAP Content Server", "localhost only"} {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, e.out.String())
		}
	}
}

func TestStartInSourceModeBuilds(t *testing.T) {
	e := newEnv(t)
	src := "services:\n  adapter:\n    build:\n      context: ..\n    image: zaks-drive:dev\n"
	must(t, os.WriteFile(filepath.Join(e.dir, "docker-compose.yml"), []byte(src), 0o644))
	if rc := e.main("start", "--dir", e.dir, "--timeout", "5s"); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	if !e.run.has(" up -d --build") {
		t.Errorf("source mode must build: %v", e.run.all())
	}
}

func TestStartRefusesAnInvalidConfigurationBeforeTouchingDocker(t *testing.T) {
	e := newEnv(t)
	cfg := filepath.Join(e.dir, "config.yaml")
	b, _ := os.ReadFile(cfg)
	must(t, os.WriteFile(cfg, []byte(strings.Replace(string(b), "contRep: Z1", "contRep: TOOLONG", 1)), 0o644))
	if rc := e.main("start", "--dir", e.dir); rc != ExitNoDeployment {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	if !strings.Contains(e.err.String(), "CFG_CONTREP_INVALID") && !strings.Contains(e.err.String(), "contRep") {
		t.Errorf("the problem must be named: %s", e.err.String())
	}
	if e.run.has(" up ") {
		t.Error("compose up must not run with an invalid configuration")
	}
}

func TestStartFailureClassification(t *testing.T) {
	e := newEnv(t)
	base := healthyDocker("running")
	e.run.handler = func(c call) (Result, error) {
		if strings.Contains(c.String(), " up -d") {
			return Result{ExitCode: 1, Stderr: "Error: Bind for 0.0.0.0:9090 failed: port is already allocated"}, nil
		}
		return base(c)
	}
	if rc := e.main("start", "--dir", e.dir); rc != ExitFail {
		t.Fatalf("exit %d", rc)
	}
	if !strings.Contains(e.err.String(), "ADMIN_PORT") || !strings.Contains(e.err.String(), "Cockpit") {
		t.Errorf("a port conflict must name the variables: %s", e.err.String())
	}
}

func TestStartTimesOutWhenNotReadyAndShowsTheReason(t *testing.T) {
	e := newEnv(t)
	e.readyStatus, e.readyBody = 503, "not ready: SharePoint check failed: SP_ACCESS_DENIED"
	if rc := e.main("start", "--dir", e.dir, "--timeout", "100ms"); rc != ExitUnhealthy {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	if !strings.Contains(e.err.String(), "SP_ACCESS_DENIED") || !strings.Contains(e.err.String(), "zaks-drive doctor") {
		t.Errorf("the readiness reason and the next step must be shown: %s", e.err.String())
	}
}

func TestStartFailsFastWhenTheContainerExits(t *testing.T) {
	e := newEnv(t)
	e.useFake(func(f *dockerFake) { f.state = "exited" })
	e.readyStatus = 503
	if rc := e.main("start", "--dir", e.dir, "--timeout", "10s"); rc != ExitUnhealthy {
		t.Fatalf("exit %d", rc)
	}
	if !strings.Contains(e.err.String(), "stopped") {
		t.Errorf("must say the container stopped: %s", e.err.String())
	}
}

func TestStopNeverRemovesContainersOrVolumes(t *testing.T) {
	e := newEnv(t)
	for _, cmd := range [][]string{{"stop", "--dir", e.dir}, {"restart", "--dir", e.dir, "--timeout", "2s"}, {"start", "--dir", e.dir, "--timeout", "2s"}} {
		if rc := e.main(cmd...); rc != ExitOK {
			t.Fatalf("%v exit %d\n%s", cmd, rc, e.both())
		}
	}
	for _, c := range e.run.all() {
		for _, bad := range []string{" down", " rm ", " -v", "--volumes", "prune", "volume ", "system ", " kill", "--remove-orphans"} {
			if strings.Contains(c, bad) {
				t.Errorf("a destructive operation was issued: %s", c)
			}
		}
		if strings.HasPrefix(c, "docker compose") && !strings.Contains(c, "-f "+filepath.Join(e.dir, "docker-compose.yml")) && !strings.HasPrefix(c, "docker compose version") {
			t.Errorf("compose must always get the deployment's file explicitly: %s", c)
		}
	}
	if !e.run.has(" stop") {
		t.Error("stop must call docker compose stop")
	}
}

func TestOnlyManagedContainerIDsAreInspected(t *testing.T) {
	e := newEnv(t)
	if rc := e.main("status", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	for _, c := range e.run.all() {
		if strings.HasPrefix(c, "docker inspect") && !strings.HasSuffix(c, "abc123def456") {
			t.Errorf("inspect must name only the IDs that compose returned: %s", c)
		}
	}
	for _, c := range e.run.all() {
		if strings.HasPrefix(c, "docker ps") || strings.HasPrefix(c, "docker container ls") {
			t.Errorf("never list the host's containers: %s", c)
		}
	}
}

// ---- status ----

func TestStatusReadyAndJSON(t *testing.T) {
	e := newEnv(t)
	if rc := e.main("status", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	for _, w := range []string{"zd-adapter-1: running, healthy", "0 restarts", "Ready        yes", "0.1.0"} {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("status lacks %q:\n%s", w, e.out.String())
		}
	}
	if rc := e.main("status", "--dir", e.dir, "--json"); rc != ExitOK {
		t.Fatalf("json exit %d", rc)
	}
	for _, w := range []string{`"ready": true`, `"serverVersion": "0.1.0"`, `"service": "adapter"`} {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("json lacks %q:\n%s", w, e.out.String())
		}
	}
}

func TestStatusWhenNotStartedAndNotReady(t *testing.T) {
	e := newEnv(t)
	base := healthyDocker("running")
	e.run.handler = func(c call) (Result, error) {
		if strings.Contains(c.String(), " ps --all") {
			return Result{Stdout: ""}, nil
		}
		return base(c)
	}
	if rc := e.main("status", "--dir", e.dir); rc != ExitUnhealthy || !strings.Contains(e.out.String(), "not started") {
		t.Fatalf("not started: exit %d\n%s", rc, e.out.String())
	}
	e.run.handler = base
	e.readyStatus, e.readyBody = 503, "not ready: token roles NONE"
	if rc := e.main("status", "--dir", e.dir); rc != ExitUnhealthy || !strings.Contains(e.out.String(), "NO: not ready") {
		t.Fatalf("not ready: exit %d\n%s", rc, e.out.String())
	}
}

func TestParseComposePSAcceptsBothShapes(t *testing.T) {
	lines := `{"ID":"a","Name":"x-1","Service":"adapter","State":"running"}` + "\n" + `{"ID":"b","Name":"y-1","Service":"other","State":"exited","ExitCode":2}`
	array := `[{"ID":"a","Name":"x-1","Service":"adapter","State":"running"},{"ID":"b","Name":"y-1","Service":"other","State":"exited","ExitCode":2}]`
	for name, in := range map[string]string{"lines": lines, "array": array} {
		cs, err := parseComposePS(in)
		if err != nil || len(cs) != 2 || cs[1].ExitCode != 2 || cs[0].Service != "adapter" {
			t.Errorf("%s: %v %+v", name, err, cs)
		}
	}
	if cs, err := parseComposePS(""); err != nil || cs != nil {
		t.Errorf("empty: %v %v", cs, err)
	}
	if _, err := parseComposePS("not json"); err == nil {
		t.Error("garbage must be an error")
	}
}

// ---- version ----

func TestVersionShowsCLIAndServer(t *testing.T) {
	e := newEnv(t)
	if rc := e.main("version", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("exit %d", rc)
	}
	for _, w := range []string{"zaks-drive (management command)  0.1.0", "running server                   0.1.0", "installed release"} {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("lacks %q:\n%s", w, e.out.String())
		}
	}
}

// ---- config validate ----

func TestConfigValidate(t *testing.T) {
	e := newEnv(t)
	if rc := e.main("config", "validate", "--dir", e.dir); rc != ExitOK {
		t.Fatalf("valid config: exit %d\n%s", rc, e.both())
	}
	if !strings.Contains(e.out.String(), "is valid") || !strings.Contains(e.out.String(), "Microsoft and SAP were not contacted") {
		t.Errorf("%s", e.out.String())
	}
	if len(e.run.calls) != 0 {
		t.Errorf("validation must not start any process: %v", e.run.all())
	}

	// A bad site URL is reported with its error code.
	envPath := filepath.Join(e.dir, ".env")
	b, _ := os.ReadFile(envPath)
	bad := strings.Replace(string(b), "https://contoso.sharepoint.com/sites/SAP_DOCS", "https://contoso.sharepoint.com/sites/SAP_DOCS/Forms/AllItems.aspx", 1)
	must(t, os.WriteFile(envPath, []byte(bad), 0o600))
	if rc := e.main("config", "validate", "--dir", e.dir); rc != ExitFail || !strings.Contains(e.err.String(), "CFG_SITE_URL_INVALID") {
		t.Fatalf("exit %d\n%s", rc, e.err.String())
	}
	if strings.Contains(e.both(), secClient) {
		t.Error("the client secret must not appear in validation output")
	}

	// Missing files.
	empty := t.TempDir()
	must(t, os.WriteFile(filepath.Join(empty, "docker-compose.yml"), []byte("services: {}\n"), 0o644))
	if rc := e.main("config", "validate", "--dir", empty); rc != ExitFail || !strings.Contains(e.err.String(), ".env is missing") {
		t.Fatalf("exit %d\n%s", rc, e.err.String())
	}
}

// The configuration is evaluated as the container would see it: only .env, never the shell's environment.
func TestConfigValidateIgnoresTheProcessEnvironmentAndRestoresIt(t *testing.T) {
	e := newEnv(t)
	envPath := filepath.Join(e.dir, ".env")
	b, _ := os.ReadFile(envPath)
	var kept []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, "GRAPH_TENANT_ID=") {
			kept = append(kept, l)
		}
	}
	must(t, os.WriteFile(envPath, []byte(strings.Join(kept, "\n")), 0o600))
	t.Setenv("GRAPH_TENANT_ID", guidA) // present in the shell, absent in .env: the container would not have it
	t.Setenv("ZD_TEST_SENTINEL", "keep-me")
	if rc := e.main("config", "validate", "--dir", e.dir); rc != ExitFail {
		t.Fatalf("a variable only present in the shell must not make the config valid; exit %d\n%s", rc, e.both())
	}
	if os.Getenv("ZD_TEST_SENTINEL") != "keep-me" || os.Getenv("GRAPH_TENANT_ID") != guidA {
		t.Error("the process environment must be restored")
	}
	if os.Getenv("GRAPH_CLIENT_SECRET") != "" {
		t.Error("secrets from .env must not stay in the process environment")
	}
}

// ---- deployment files ----

func TestReadEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	must(t, os.WriteFile(p, []byte("# c\nA=1\nB='two words'\nC=\"x#y\"\nexport D=4\n\nbad line\nE=\n"), 0o600))
	m, err := ReadEnv(p)
	if err != nil || m["A"] != "1" || m["B"] != "two words" || m["C"] != "x#y" || m["D"] != "4" || m["E"] != "" {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := ReadEnv(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("a missing file must be an error")
	}
}

func TestStateRoundTripAndPermissions(t *testing.T) {
	d := newDeployment(t.TempDir())
	must(t, d.saveState(State{Current: "1.2.3", Previous: "1.2.2"}))
	if s := d.loadState(); s.Current != "1.2.3" || s.Previous != "1.2.2" {
		t.Fatalf("%+v", s)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(d.statePath())
		if st.Mode().Perm() != 0o600 {
			t.Errorf("state mode %v", st.Mode().Perm())
		}
	}
}

func TestEnvPortsAndBind(t *testing.T) {
	e := newEnv(t)
	d := newDeployment(e.dir)
	p := d.ports()
	if p.Bind != "0.0.0.0" || p.REST != 8080 || p.CS != e.fake.csPort || p.Admin != e.adminPort {
		t.Errorf("%+v", p)
	}
	m := map[string]string{"X": "99999", "Y": "abc", "Z": "70"}
	if envPort(m, "X", 1) != 1 || envPort(m, "Y", 2) != 2 || envPort(m, "Z", 3) != 70 || envPort(m, "none", 4) != 4 {
		t.Error("envPort must reject out-of-range and non-numeric values")
	}
}

// Backward compatibility: the configuration formats of the existing deployment are still valid.
func TestExistingDeployExampleFilesStillLoad(t *testing.T) {
	d := newDeployment(t.TempDir())
	ans := validAnswers(d.Dir)
	envText, _ := renderEnv(ans)
	must(t, os.WriteFile(d.EnvFile, envText, 0o600))
	// The unmodified example file from the repository must load with the generated .env.
	example, err := os.ReadFile("../../deploy/config.example.yaml")
	must(t, err)
	must(t, os.WriteFile(d.ConfigFile, example, 0o644))
	cfg, err := loadConfig(d)
	if err != nil {
		t.Fatalf("the unchanged config.example.yaml must stay valid: %v", err)
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0].ID != "DMS" {
		t.Errorf("%+v", cfg.Repositories)
	}
	var _ config.Config = cfg
}

// The compose file written for released deployments must not drift from the repository's own
// deploy/docker-compose.yml on anything that matters for security or compatibility.
func TestReleaseComposeMatchesDevelopmentCompose(t *testing.T) {
	load := func(path string) map[string]any {
		b, err := os.ReadFile(path)
		must(t, err)
		var doc struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		must(t, yaml.Unmarshal(b, &doc))
		svc := doc.Services["adapter"]
		if svc == nil {
			t.Fatalf("%s has no service named adapter (the service name is a compatibility promise)", path)
		}
		return svc
	}
	dev, rel := load("../../deploy/docker-compose.yml"), load("../../deploy/docker-compose.release.yml")
	for _, key := range []string{"ports", "read_only", "security_opt", "tmpfs", "ulimits", "healthcheck", "mem_limit", "restart", "logging", "env_file", "environment"} {
		if !reflect.DeepEqual(dev[key], rel[key]) {
			t.Errorf("service.adapter.%s differs:\n dev:     %v\n release: %v", key, dev[key], rel[key])
		}
	}
	if _, ok := rel["build"]; ok {
		t.Error("the release compose file must not build from source")
	}
}

// docs/cli.md must mention everything the command offers, so the guide cannot fall behind.
func TestCLIGuideDocumentsEverything(t *testing.T) {
	b, err := os.ReadFile("../../docs/cli.md")
	must(t, err)
	doc := string(b)
	app := &App{Version: "x"}
	for _, c := range app.commands() {
		if !strings.Contains(doc, "zaks-drive "+c.name) && !strings.Contains(doc, "`"+c.name+"`") && !strings.Contains(doc, "`"+c.name+",") {
			t.Errorf("docs/cli.md does not document the command %q", c.name)
		}
	}
	for _, di := range Diagnostics {
		if !strings.Contains(doc, "`"+di.ID+"`") {
			t.Errorf("docs/cli.md does not document the diagnostic %q", di.ID)
		}
	}
	for code, name := range map[int]string{ExitOK: "success", ExitFail: "failed", ExitUsage: "bad command line", ExitPrereq: "Docker", ExitNoDeployment: "no deployment", ExitUnhealthy: "not ready", ExitVerify: "verification", ExitDeclined: "declined"} {
		if !strings.Contains(doc, fmt.Sprintf("| %d | ", code)) {
			t.Errorf("docs/cli.md does not list exit code %d (%s)", code, name)
		}
	}
	for _, v := range []string{"ZAKS_DRIVE_DIR", "ZAKS_DRIVE_SHARE", "ZAKS_DRIVE_VERSION", "ZAKS_DRIVE_CLIENT_SECRET", "ZAKS_DRIVE_BASE_IMAGE", "ZAKS_DRIVE_RELEASE_BASE_URL"} {
		if !strings.Contains(doc, "`"+v+"`") {
			t.Errorf("docs/cli.md does not document %s", v)
		}
	}
	// And the environment variables the code reads are all in that list.
	for _, f := range []string{"app.go", "deployment.go", "payload.go", "release.go", "setup.go", "lifecycle.go", "docker.go"} {
		src, err := os.ReadFile(f)
		must(t, err)
		for _, m := range regexp.MustCompile(`"(ZAKS_DRIVE_[A-Z_]+)"`).FindAllStringSubmatch(string(src), -1) {
			if !strings.Contains(doc, "`"+m[1]+"`") {
				t.Errorf("%s reads %s, which docs/cli.md does not document", f, m[1])
			}
		}
	}
}

// Security review as a test: the command may start only docker (and stty, to hide a typed secret),
// never through a shell, and only runner.go and terminal.go may use os/exec.
func TestOnlyDockerAndSttyCanBeStarted(t *testing.T) {
	entries, err := os.ReadDir(".")
	must(t, err)
	progRe := regexp.MustCompile(`(?:Runner\.Run|\.Run)\(ctx, "([^"]+)"|exec\.Command(?:Context)?\((?:ctx, )?"([^"]+)"`)
	for _, en := range entries {
		name := en.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		must(t, err)
		src := string(b)
		if strings.Contains(src, `"os/exec"`) && name != "runner.go" && name != "terminal.go" {
			t.Errorf("%s imports os/exec; only runner.go and terminal.go may start processes", name)
		}
		for _, m := range progRe.FindAllStringSubmatch(src, -1) {
			prog := m[1] + m[2]
			if prog != "docker" && prog != "stty" {
				t.Errorf("%s starts %q; only docker and stty are allowed", name, prog)
			}
		}
		for _, bad := range []string{`"sh"`, `"bash"`, `"-c"`, `"cmd"`, `"sudo"`, `"su"`} {
			if strings.Contains(src, "Run(ctx, "+bad) || strings.Contains(src, "exec.Command("+bad) {
				t.Errorf("%s runs %s: no shell and no privilege escalation", name, bad)
			}
		}
	}
}

// docs/cli-reference.md lists every flag of every command, so it cannot fall behind: the test asks
// each command for its flags with -h and fails for any that the page does not mention.
func TestCLIReferenceDocumentsEveryFlag(t *testing.T) {
	b, err := os.ReadFile("../../docs/cli-reference.md")
	must(t, err)
	doc := string(b)
	flagLine := regexp.MustCompile(`(?m)^  -([a-z][a-z0-9-]*)`)
	commands := [][]string{
		{"setup"}, {"config", "validate"}, {"start"}, {"stop"}, {"restart"}, {"status"}, {"logs"}, {"doctor"}, {"check"},
		{"update"}, {"rollback"}, {"version"}, {"support", "bundle"}, {"admin", "reset-password"},
		{"repo", "list"}, {"repo", "show"}, {"repo", "add"}, {"repo", "update"}, {"repo", "remove"}, {"repo", "check"}, {"repo", "purge"},
	}
	for _, c := range commands {
		var errOut bytes.Buffer
		app := &App{Out: &bytes.Buffer{}, Err: &errOut, In: strings.NewReader(""), Version: "x", Getenv: func(string) string { return "" }}
		app.Main(append(append([]string{}, c...), "-h"))
		found := flagLine.FindAllStringSubmatch(errOut.String(), -1)
		if len(found) == 0 {
			t.Errorf("%v: no flags found in the -h output; the test cannot check this command:\n%s", c, errOut.String())
		}
		for _, m := range found {
			if !strings.Contains(doc, "--"+m[1]) {
				t.Errorf("docs/cli-reference.md does not document `--%s` of `zaks-drive %s`", m[1], strings.Join(c, " "))
			}
		}
	}
}
