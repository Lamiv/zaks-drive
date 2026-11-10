package ctl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lamiv/zaks-drive/deploy"
)

// Synthetic sensitive values used across the tests. None may ever appear in output.
const (
	secClient = "SYNTH~client.Secret-VALUE-81f3a9"
	secAPI    = "SYNTH-API-KEY-c0ffee1234567890abcdef"
	secAdmin  = "SYNTH-ADMIN-TOKEN-0987654321fedcba"
	guidA     = "11111111-2222-3333-4444-555555555555"
	guidB     = "66666666-7777-8888-9999-aaaaaaaaaaaa"
)

type call struct {
	Name string
	Args []string
	Dir  string
	Env  []string
	// Stdin is what the process was given on standard input.
	Stdin string
}

func (c call) String() string { return c.Name + " " + strings.Join(c.Args, " ") }

// fakeRunner records every process start and answers from a handler. Nothing is executed.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []call
	handler func(c call) (Result, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args []string, opts RunOpts) (Result, error) {
	c := call{Name: name, Args: args, Dir: opts.Dir, Env: opts.Env}
	if opts.Stdin != nil {
		b, _ := io.ReadAll(opts.Stdin)
		c.Stdin = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	res := Result{}
	var err error
	if f.handler != nil {
		res, err = f.handler(c)
	}
	// Like ExecRunner: when a writer is supplied the output goes there and the Result is empty.
	if opts.Stdout != nil {
		_, _ = opts.Stdout.Write([]byte(res.Stdout))
		res.Stdout = ""
	}
	if opts.Stderr != nil {
		_, _ = opts.Stderr.Write([]byte(res.Stderr))
		res.Stderr = ""
	}
	return res, err
}

func (f *fakeRunner) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.String())
	}
	return out
}

func (f *fakeRunner) has(substr string) bool {
	for _, c := range f.all() {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// dockerFake answers the way a working Docker with one container does. The knobs let a test
// drive each diagnostic into each of its states.
type dockerFake struct {
	state       string // container state
	health      string
	restarts    int
	oom         bool
	memory      int64
	adminPort   int
	restPort    int
	csPort      int
	noContainer bool
}

func (f dockerFake) handler() func(c call) (Result, error) {
	return func(c call) (Result, error) {
		line := c.String()
		switch {
		case strings.HasPrefix(line, "docker version"):
			return Result{Stdout: "27.1.0\n"}, nil
		case strings.HasPrefix(line, "docker compose version"):
			return Result{Stdout: "2.29.0\n"}, nil
		case strings.Contains(line, " ps --all --format json"):
			if f.noContainer {
				return Result{}, nil
			}
			pub := func(host string, port int) string {
				return fmt.Sprintf(`{"URL":"%s","TargetPort":%d,"PublishedPort":%d,"Protocol":"tcp"}`, host, port, port)
			}
			return Result{Stdout: `{"ID":"abc123def456","Name":"zd-adapter-1","Service":"adapter","State":"` + f.state + `","Health":"` + f.health +
				`","ExitCode":0,"Image":"zaks-drive:0.1.0","Publishers":[` + pub("127.0.0.1", f.adminPort) + `,` + pub("0.0.0.0", f.restPort) + `,` + pub("0.0.0.0", f.csPort) + `]}` + "\n"}, nil
		case strings.HasPrefix(line, "docker inspect") && strings.Contains(line, "OOMKilled"):
			return Result{Stdout: fmt.Sprintf("%v %d\n", f.oom, f.memory)}, nil
		case strings.HasPrefix(line, "docker inspect"):
			return Result{Stdout: fmt.Sprintf("abc123def456000000000000000000000000000000000000000000000000abcd %d\n", f.restarts)}, nil
		case strings.Contains(line, "config --images"):
			return Result{Stdout: "zaks-drive:0.1.0\n"}, nil
		case strings.HasPrefix(line, "docker image inspect"):
			return Result{Stdout: "sha256:x\n"}, nil
		}
		return Result{}, nil
	}
}

// healthyDocker is a running, healthy container with the default ports.
func healthyDocker(containerState string) func(c call) (Result, error) {
	return dockerFake{state: containerState, health: "healthy", memory: 1 << 30, adminPort: 9090, restPort: 8080, csPort: 8090}.handler()
}

type env struct {
	app *App
	out *bytes.Buffer
	err *bytes.Buffer
	run *fakeRunner
	dir string
	// admin is the fake admin endpoint (health, readiness, version, certificate list).
	admin       *httptest.Server
	readyStatus int
	readyBody   string
	adminPort   int
	// cs is the fake SAP Content Server port; csStatus, csBody and csHeader are what serverInfo answers.
	cs       *httptest.Server
	csStatus int
	csBody   string
	csHeader map[string]string
	// certs is the JSON of the fake certificate admin endpoint; certStatus its HTTP status.
	certs      string
	certStatus int
	fake       dockerFake
	// repoAPI, when set, answers /admin/api/v1/... on the fake admin endpoint (after the bearer token is checked).
	repoAPI http.Handler
}

// newEnv creates a test App, a valid release-mode deployment in a temp dir, a fake admin endpoint
// and a fake Content Server port whose ports are written into the deployment's .env.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{out: &bytes.Buffer{}, err: &bytes.Buffer{}, run: &fakeRunner{}, dir: t.TempDir(), readyStatus: 200, readyBody: "ready",
		csStatus: 200, csBody: "serverStatus=\"running\";pVersion=\"0047\";\ncontRep=\"Z1\";contRepStatus=\"running\";\n", certStatus: 200, certs: `{"value":[]}`}
	e.admin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/api/v1/") && e.repoAPI != nil {
			if r.Header.Get("Authorization") != "Bearer "+secAdmin {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			e.repoAPI.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/readyz":
			w.WriteHeader(e.readyStatus)
			_, _ = w.Write([]byte(e.readyBody))
		case "/version":
			_, _ = w.Write([]byte("0.1.0"))
		case "/healthz":
			_, _ = w.Write([]byte("ok"))
		case "/admin/contentserver/certificates":
			if r.Header.Get("Authorization") != "Bearer "+secAdmin {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(e.certStatus)
			_, _ = w.Write([]byte(e.certs))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(e.admin.Close)
	u, _ := url.Parse(e.admin.URL)
	e.adminPort, _ = strconv.Atoi(u.Port())

	e.cs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range e.csHeader {
			w.Header().Set(k, v)
		}
		w.WriteHeader(e.csStatus)
		_, _ = w.Write([]byte(e.csBody))
	}))
	t.Cleanup(e.cs.Close)
	cu, _ := url.Parse(e.cs.URL)
	csPort, _ := strconv.Atoi(cu.Port())

	e.fake = dockerFake{state: "running", health: "healthy", memory: 1 << 30, adminPort: e.adminPort, restPort: 8080, csPort: csPort}
	e.run.handler = e.fake.handler()

	ans := validAnswers(e.dir)
	ans.AdminPort = e.adminPort
	ans.CSPort = csPort
	envText, err := renderEnv(ans)
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(e.dir, ".env"), envText, 0o600))
	must(t, os.WriteFile(filepath.Join(e.dir, "config.yaml"), renderConfig(ans), 0o644))
	must(t, os.WriteFile(filepath.Join(e.dir, "docker-compose.yml"), deploy.ComposeRelease, 0o644))
	d := newDeployment(e.dir)
	must(t, d.saveState(State{Current: "0.1.0"}))

	e.app = &App{
		Out: e.out, Err: e.err, In: strings.NewReader(""),
		Runner: e.run, HTTP: &http.Client{Timeout: 3 * time.Second},
		Getenv:       func(string) string { return "" },
		Now:          time.Now,
		Version:      "0.1.0",
		GOOS:         "linux",
		GOARCH:       "amd64",
		Interactive:  func() bool { return false },
		ExePath:      func() (string, error) { return filepath.Join(t.TempDir(), "bin", "zaks-drive"), nil },
		PollInterval: 5 * time.Millisecond,
		DiskFree:     func(string) (int64, error) { return 50 << 30, nil },
		PortBusy:     func(string, int) bool { return false },
	}
	return e
}

// useFake replaces the Docker behaviour with a modified dockerFake.
func (e *env) useFake(mod func(f *dockerFake)) {
	f := e.fake
	mod(&f)
	e.run.handler = f.handler()
}

func validAnswers(dir string) Answers {
	return Answers{
		Dir: dir, Bind: "0.0.0.0", RESTPort: 8080, CSPort: 8090, AdminPort: 9090,
		Interfaces: []string{"rest", "contentrepo"},
		SiteURL:    "https://contoso.sharepoint.com/sites/SAP_DOCS", Library: "Documents", RootPath: "SAP_DMS",
		TenantID: guidA, ClientID: guidB, ClientSecret: secClient,
		ContReps: []ContRepAnswer{{"Z1", "required"}}, LogLevel: "info",
		APIKeyS4: secAPI, APIKeyFiori: secAPI + "-fiori", AdminToken: secAdmin,
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) main(args ...string) int {
	e.out.Reset()
	e.err.Reset()
	return e.app.Main(args)
}

func (e *env) both() string { return e.out.String() + e.err.String() }

func nil2() context.Context { return context.Background() }
