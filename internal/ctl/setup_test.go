package ctl

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func setupEnv(t *testing.T) (*env, string, string) {
	t.Helper()
	e := newEnv(t)
	target := filepath.Join(t.TempDir(), "zd")
	secretFile := filepath.Join(t.TempDir(), "secret")
	must(t, os.WriteFile(secretFile, []byte(secClient+"\n"), 0o600))
	return e, target, secretFile
}

func setupArgs(target, secretFile string, extra ...string) []string {
	return append([]string{"setup", "--non-interactive", "--no-build", "--dir", target,
		"--site-url", "https://contoso.sharepoint.com/sites/SAP_DOCS", "--tenant-id", guidA, "--client-id", guidB,
		"--client-secret-file", secretFile}, extra...)
}

func TestSetupNonInteractiveWritesSecureValidFiles(t *testing.T) {
	e, target, secretFile := setupEnv(t)
	if rc := e.main(setupArgs(target, secretFile, "--content-repo", "Z1", "--content-repo", "Z2:optional", "--allowed-network", "10.20.0.0/16")...); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	envB, err := os.ReadFile(filepath.Join(target, ".env"))
	must(t, err)
	cfgB, err := os.ReadFile(filepath.Join(target, "config.yaml"))
	must(t, err)
	if !strings.Contains(string(envB), "GRAPH_CLIENT_SECRET="+secClient) && !strings.Contains(string(envB), "GRAPH_CLIENT_SECRET='"+secClient+"'") {
		t.Errorf("the secret must be in .env:\n%s", envB)
	}
	if strings.Contains(string(cfgB), secClient) || strings.Contains(string(cfgB), "SYNTH") {
		t.Error("config.yaml must not contain any secret")
	}
	// Generated API keys are random, long, and different.
	envMap, _ := ReadEnv(filepath.Join(target, ".env"))
	if len(envMap["ADAPTER_API_KEY_S4"]) != 64 || envMap["ADAPTER_API_KEY_S4"] == envMap["ADAPTER_API_KEY_FIORI"] || envMap["ADMIN_TOKEN"] == "" {
		t.Errorf("keys: %v", envMap)
	}
	// Nothing sensitive in what was printed.
	for _, s := range []string{secClient, envMap["ADAPTER_API_KEY_S4"], envMap["ADMIN_TOKEN"]} {
		if strings.Contains(e.both(), s) {
			t.Errorf("setup printed a secret: %q", s)
		}
	}
	for _, w := range []string{"secret: set", "not shown", "Z1 (signature: required)", "Z2 (signature: optional)", "reverse proxy"} {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("summary lacks %q:\n%s", w, e.out.String())
		}
	}
	if runtime.GOOS != "windows" {
		for name, want := range map[string]os.FileMode{".env": 0o600, "config.yaml": 0o644} {
			if st, _ := os.Stat(filepath.Join(target, name)); st.Mode().Perm() != want {
				t.Errorf("%s mode %v, want %v", name, st.Mode().Perm(), want)
			}
		}
	}
	// The generated configuration loads with the server's loader and has what was asked for.
	cfg, err := loadConfig(newDeployment(target))
	if err != nil {
		t.Fatalf("generated config invalid: %v", err)
	}
	if len(cfg.ContentServer.Repositories) != 2 || cfg.ContentServer.Repositories[1].Signature != "optional" || len(cfg.ContentServer.AllowedNetworks) != 1 {
		t.Errorf("%+v", cfg.ContentServer)
	}
	if st := newDeployment(target).loadState(); st.Current != "0.1.0" {
		t.Errorf("setup must record the installed version: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(target, "docker-compose.yml")); err != nil {
		t.Error("a release compose file must be written")
	}
	// And the deployment is usable by the other commands.
	if rc := e.main("config", "validate", "--dir", target); rc != ExitOK {
		t.Errorf("config validate: %d %s", rc, e.both())
	}
}

func TestSetupRefusesToOverwriteWithoutForceAndBacksUpWithIt(t *testing.T) {
	e, target, secretFile := setupEnv(t)
	if rc := e.main(setupArgs(target, secretFile)...); rc != ExitOK {
		t.Fatalf("first run: exit %d\n%s", rc, e.both())
	}
	first, _ := ReadEnv(filepath.Join(target, ".env"))
	if rc := e.main(setupArgs(target, secretFile)...); rc != ExitFail || !strings.Contains(e.err.String(), "--force") {
		t.Fatalf("second run without --force: exit %d\n%s", rc, e.err.String())
	}
	if rc := e.main(setupArgs(target, secretFile, "--force", "--rest-port", "8181")...); rc != ExitOK {
		t.Fatalf("with --force: exit %d\n%s", rc, e.both())
	}
	second, _ := ReadEnv(filepath.Join(target, ".env"))
	if second["ADAPTER_API_KEY_S4"] != first["ADAPTER_API_KEY_S4"] || second["ADMIN_TOKEN"] != first["ADMIN_TOKEN"] {
		t.Error("reconfiguring must keep the existing API keys and admin token")
	}
	if second["REST_PORT"] != "8181" {
		t.Errorf("the new port must be written: %v", second["REST_PORT"])
	}
	baks, _ := filepath.Glob(filepath.Join(target, ".env.bak-*"))
	if len(baks) != 1 {
		t.Fatalf("a backup of .env must be made: %v", baks)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(baks[0]); st.Mode().Perm()&0o077 != 0 {
			t.Errorf("the .env backup must not be readable by others: %v", st.Mode().Perm())
		}
	}
}

func TestSetupValidatesAnswers(t *testing.T) {
	cases := []struct {
		name  string
		extra []string
		want  string
	}{
		{"bad tenant", []string{"--tenant-id", "contoso"}, "tenant ID must be a GUID"},
		{"bad client", []string{"--client-id", "nope"}, "client ID must be a GUID"},
		{"bad bind", []string{"--bind", "localhost"}, "not an IP address"},
		{"port range", []string{"--rest-port", "70000"}, "not between 1 and 65535"},
		{"same ports", []string{"--rest-port", "9000", "--cs-port", "9000"}, "three different ports"},
		{"interface", []string{"--interfaces", "rest,cmis"}, `"cmis" is unknown`},
		{"log level", []string{"--log-level", "loud"}, "log level"},
		{"content repo id", []string{"--content-repo", "TOOLONG"}, "1 or 2 letters"},
		{"signature", []string{"--content-repo", "Z1:maybe"}, "signature mode"},
		{"network", []string{"--allowed-network", "not-a-net"}, "not an IP address or CIDR"},
		{"duplicate repo", []string{"--content-repo", "Z1", "--content-repo", "Z1"}, "listed twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, target, secretFile := setupEnv(t)
			if rc := e.main(setupArgs(target, secretFile, tc.extra...)...); rc != ExitUsage {
				t.Fatalf("exit %d\n%s", rc, e.both())
			}
			if !strings.Contains(e.err.String(), tc.want) {
				t.Errorf("message lacks %q:\n%s", tc.want, e.err.String())
			}
			if _, err := os.Stat(filepath.Join(target, ".env")); err == nil {
				t.Error("nothing may be written when the answers are invalid")
			}
		})
	}
}

func TestSetupRejectsASecretIDAndMissingSecret(t *testing.T) {
	e, target, _ := setupEnv(t)
	idFile := filepath.Join(t.TempDir(), "id")
	must(t, os.WriteFile(idFile, []byte(guidA), 0o600))
	if rc := e.main(setupArgs(target, idFile)...); rc != ExitUsage || !strings.Contains(e.err.String(), "secret ID") {
		t.Fatalf("exit %d\n%s", rc, e.err.String())
	}
	args := []string{"setup", "--non-interactive", "--no-build", "--dir", target, "--site-url", "https://contoso.sharepoint.com/sites/SAP_DOCS", "--tenant-id", guidA, "--client-id", guidB}
	if rc := e.main(args...); rc != ExitUsage || !strings.Contains(e.err.String(), "client secret is required") {
		t.Fatalf("exit %d\n%s", rc, e.err.String())
	}
	e.app.Getenv = func(k string) string {
		if k == "ZAKS_DRIVE_CLIENT_SECRET" {
			return secClient
		}
		return ""
	}
	if rc := e.main(args...); rc != ExitOK {
		t.Fatalf("the secret from the environment must work: exit %d\n%s", rc, e.both())
	}
}

func TestSetupRefusesValuesThatCouldBreakTheEnvFile(t *testing.T) {
	e, target, secretFile := setupEnv(t)
	if rc := e.main(setupArgs(target, secretFile, "--root-path", "a'b")...); rc != ExitUsage || !strings.Contains(e.err.String(), "single quote") {
		t.Fatalf("exit %d\n%s", rc, e.err.String())
	}
	if rc := e.main(setupArgs(target, secretFile, "--library", "Docs\nINJECTED=1")...); rc != ExitUsage || !strings.Contains(e.err.String(), "line breaks") {
		t.Fatalf("exit %d\n%s", rc, e.err.String())
	}
}

func TestSetupRejectsASiteURLThatPointsAtALibraryPage(t *testing.T) {
	e, target, secretFile := setupEnv(t)
	args := setupArgs(target, secretFile)
	for i, v := range args {
		if v == "https://contoso.sharepoint.com/sites/SAP_DOCS" {
			args[i] = "https://contoso.sharepoint.com/sites/SAP_DOCS/Shared%20Documents/Forms/AllItems.aspx"
		}
	}
	if rc := e.main(args...); rc != ExitFail || !strings.Contains(e.err.String(), "CFG_SITE_URL_INVALID") {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	if _, err := os.Stat(filepath.Join(target, ".env")); err == nil {
		t.Error("an invalid configuration must not be written")
	}
}

func TestSetupInteractiveWithScriptedAnswers(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(t.TempDir(), "zd")
	answers := strings.Join([]string{
		target,                              // deployment directory
		"sharepoint",                        // where documents are stored first
		"127.0.0.1", "8181", "8191", "9191", // bind, rest, cs, admin
		guidA, guidB, // tenant, client (the secret comes from ReadSecret)
		"https://contoso.sharepoint.com/sites/SAP_DOCS", "Documents", "SAP_DMS",
		"y", "Z1,Z7:none", "10.0.0.0/8", // SAP interface, repositories, networks
		"y", "debug", // TLS, log level
	}, "\n") + "\n"
	e.app.In = strings.NewReader(answers)
	e.app.Interactive = func() bool { return true }
	asked := ""
	e.app.ReadSecret = func(prompt string) (string, error) { asked = prompt; return secClient, nil }
	if rc := e.main("setup", "--no-build"); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	if !strings.Contains(asked, "hidden") {
		t.Errorf("the secret prompt must say input is hidden: %q", asked)
	}
	if strings.Contains(e.both(), secClient) {
		t.Error("the typed secret must never be echoed")
	}
	envMap, _ := ReadEnv(filepath.Join(target, ".env"))
	if envMap["BIND_ADDRESS"] != "127.0.0.1" || envMap["REST_PORT"] != "8181" || envMap["ADMIN_PORT"] != "9191" || envMap["LOG_LEVEL"] != "debug" || envMap["GRAPH_CLIENT_SECRET"] != secClient {
		t.Errorf("%v", envMap)
	}
	cfg, err := loadConfig(newDeployment(target))
	if err != nil {
		t.Fatalf("invalid generated config: %v", err)
	}
	if len(cfg.ContentServer.Repositories) != 2 || cfg.ContentServer.Repositories[1].Signature != "none" || cfg.Server.TLSCertFile == "" {
		t.Errorf("%+v / tls=%q", cfg.ContentServer.Repositories, cfg.Server.TLSCertFile)
	}
	if !strings.Contains(e.out.String(), "signature none disables SAP signature checks") {
		t.Errorf("the summary must warn about signature none:\n%s", e.out.String())
	}
}

func TestSetupInteractiveNeedsATerminal(t *testing.T) {
	e := newEnv(t)
	if rc := e.main("setup", "--dir", filepath.Join(t.TempDir(), "x")); rc != ExitUsage || !strings.Contains(e.err.String(), "--non-interactive") {
		t.Fatalf("exit %d\n%s", rc, e.err.String())
	}
}

func TestSetupBuildsTheImageFromAReleasePayload(t *testing.T) {
	e, target, secretFile := setupEnv(t)
	share := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(share, "0.1.0", "deploy"), 0o755))
	must(t, os.WriteFile(filepath.Join(share, "0.1.0", "zaks-drive-server"), []byte("bin"), 0o755))
	e.app.Getenv = func(k string) string {
		if k == "ZAKS_DRIVE_SHARE" {
			return share
		}
		return ""
	}
	base := e.run.handler
	e.run.handler = func(c call) (Result, error) {
		if strings.HasPrefix(c.String(), "docker image inspect") {
			return Result{ExitCode: 1}, nil // not built yet
		}
		return base(c)
	}
	args := setupArgs(target, secretFile)
	for i, v := range args {
		if v == "--no-build" {
			args = append(args[:i], args[i+1:]...)
			break
		}
	}
	if rc := e.main(args...); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	want := "docker build -f " + filepath.Join(share, "0.1.0", "deploy", "Dockerfile.release") + " --build-arg VERSION=0.1.0 -t zaks-drive:0.1.0 " + filepath.Join(share, "0.1.0")
	if !e.run.has(want) {
		t.Errorf("missing %q in %v", want, e.run.all())
	}
}

func TestRenderConfigVariants(t *testing.T) {
	ans := validAnswers(t.TempDir())
	ans.Interfaces = []string{"rest"}
	ans.TLS = true
	ans.AllowedNetworks = []string{"10.1.0.0/16", "192.168.5.7"}
	ans.LogLevel = "warn"
	ans.ContReps = []ContRepAnswer{{"Z1", "required"}, {"A9", "optional"}}
	cfg := string(renderConfig(ans))
	for _, want := range []string{"interfaces: [rest]", "  tlsCertFile: /etc/adapter/tls/tls.crt", `allowedNetworks: ["10.1.0.0/16", "192.168.5.7"]`, "level: warn", "contRep: A9", "signature: optional", "folder: ContentServer/A9"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("rendered config lacks %q", want)
		}
	}
	if strings.Count(cfg, "contRep:") != 2 || strings.Contains(cfg, "ATTACHMENTS") {
		t.Errorf("the repository list must be replaced, not appended to:\n%s", cfg)
	}
}

func TestEnvValueQuoting(t *testing.T) {
	for in, want := range map[string]string{"plain-Value_1.2~x": "plain-Value_1.2~x", "has space": "'has space'", "a$b": "'a$b'", "x#y": "'x#y'", "": ""} {
		if got := envValue(in); got != want {
			t.Errorf("envValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImageBuildCarriesTheReleaseRevisionOnlyWhenItIsAValidCommit(t *testing.T) {
	e := newEnv(t)
	share := t.TempDir()
	dir := filepath.Join(share, "0.1.0")
	must(t, os.MkdirAll(filepath.Join(dir, "deploy"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "zaks-drive-server"), []byte("bin"), 0o755))
	e.app.Getenv = func(k string) string {
		if k == "ZAKS_DRIVE_SHARE" {
			return share
		}
		return ""
	}
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for name, tc := range map[string]struct {
		json string
		want bool
	}{"valid": {`{"commit":"` + sha + `"}`, true}, "short": {`{"commit":"abc123"}`, false}, "injection": {`{"commit":"` + sha + ` --privileged"}`, false}, "garbage": {`not json`, false}} {
		must(t, os.WriteFile(filepath.Join(dir, "release.json"), []byte(tc.json), 0o644))
		e.run.calls = nil
		if _, err := e.app.buildImage(nil2(), dir, "0.1.0"); err != nil {
			t.Fatal(err)
		}
		has := e.run.has("--build-arg REVISION=" + sha)
		if has != tc.want {
			t.Errorf("%s: revision build-arg present=%v, want %v: %v", name, has, tc.want, e.run.all())
		}
		if e.run.has("--privileged") {
			t.Errorf("%s: unvalidated text reached a docker argument", name)
		}
	}
}

// ---- local-first setup: no Microsoft account needed ----

func TestSetupLocalFirstNeedsNoMicrosoftAccount(t *testing.T) {
	e, target, _ := setupEnv(t)
	rc := e.main("setup", "--non-interactive", "--no-build", "--storage", "local", "--dir", target)
	if rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	envMap, err := ReadEnv(filepath.Join(target, ".env"))
	must(t, err)
	for _, k := range []string{"GRAPH_TENANT_ID", "GRAPH_CLIENT_ID", "GRAPH_CLIENT_SECRET"} {
		if v, ok := envMap[k]; !ok || v != "" {
			t.Errorf("%s must be present and empty in a local-first deployment, got %q", k, v)
		}
	}
	if _, ok := envMap["SHAREPOINT_SITE_URL"]; ok {
		t.Error("no SharePoint location belongs in a local-first .env")
	}
	for _, k := range []string{"ADAPTER_API_KEY_S4", "ADAPTER_API_KEY_FIORI", "ADMIN_TOKEN"} {
		if len(envMap[k]) < 32 {
			t.Errorf("%s must still be generated", k)
		}
	}
	cfg, err := loadConfig(newDeployment(target))
	if err != nil {
		t.Fatalf("the generated configuration must load: %v", err)
	}
	if len(cfg.Repositories) != 0 || len(cfg.ContentServer.Repositories) != 0 || !reflect.DeepEqual(cfg.Server.Interfaces, []string{"rest"}) {
		t.Fatalf("repositories=%v sap=%v interfaces=%v", cfg.Repositories, cfg.ContentServer.Repositories, cfg.Server.Interfaces)
	}
	for _, k := range cfg.Auth.APIKeys {
		for _, r := range k.Repositories {
			if r == "DMS" {
				t.Errorf("key %s is still tied to a repository that does not exist", k.Name)
			}
		}
	}
	out := e.out.String()
	for _, want := range []string{"Storage        local folder", "zaks-drive repo add AD --backend local", "no Microsoft account needed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Tenant") || strings.Contains(out, "zaks-drive check") {
		t.Errorf("a local-first summary must not talk about Microsoft:\n%s", out)
	}
	if info, err := os.Stat(filepath.Join(target, ".env")); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf(".env must still be mode 600")
	}
}

func TestSetupLocalFirstWithSAPInterfaceIsValid(t *testing.T) {
	e, target, _ := setupEnv(t)
	if rc := e.main("setup", "--non-interactive", "--no-build", "--storage", "local", "--interfaces", "rest,contentrepo", "--allowed-network", "10.0.0.0/8", "--dir", target); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	cfg, err := loadConfig(newDeployment(target))
	if err != nil || !cfg.Server.Enabled("contentrepo") || len(cfg.ContentServer.AllowedNetworks) != 1 {
		t.Fatalf("%v %+v", err, cfg.ContentServer)
	}
}

func TestSetupLocalRefusesMicrosoftOptionsAndUnknownStorage(t *testing.T) {
	e, target, secretFile := setupEnv(t)
	for _, extra := range [][]string{{"--site-url", "https://x.sharepoint.com/sites/a"}, {"--tenant-id", guidA}, {"--client-secret-file", secretFile}, {"--library", "Docs"}, {"--root-path", "X"}} {
		args := append([]string{"setup", "--non-interactive", "--no-build", "--storage", "local", "--dir", target}, extra...)
		if rc := e.main(args...); rc != ExitUsage || !strings.Contains(e.err.String(), "do not apply with --storage local") {
			t.Errorf("%v: exit %d\n%s", extra, rc, e.err.String())
		}
	}
	if rc := e.main("setup", "--non-interactive", "--no-build", "--storage", "nas", "--dir", target); rc != ExitUsage || !strings.Contains(e.err.String(), "use local or sharepoint") {
		t.Errorf("unknown storage: %d\n%s", rc, e.err.String())
	}
	if _, err := os.Stat(filepath.Join(target, ".env")); err == nil {
		t.Fatal("a refused setup wrote files")
	}
}

func TestSetupWithoutAStorageChoiceStillRequiresSharePointWhenNotInteractive(t *testing.T) {
	e, target, _ := setupEnv(t)
	if rc := e.main("setup", "--non-interactive", "--no-build", "--dir", target); rc != ExitUsage || !strings.Contains(e.err.String(), "--site-url") {
		t.Fatalf("the default is unchanged for scripts: %d\n%s", rc, e.err.String())
	}
}

func TestSetupInteractiveLocalAsksNothingAboutMicrosoft(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(t.TempDir(), "zd")
	e.app.In = strings.NewReader(strings.Join([]string{target, "", "127.0.0.1", "8181", "8191", "9191", "n", "n", "info"}, "\n") + "\n") // dir, storage (Enter = local), ports, SAP no, TLS no, log level
	e.app.Interactive = func() bool { return true }
	asked := false
	e.app.ReadSecret = func(string) (string, error) { asked = true; return "", nil }
	if rc := e.main("setup", "--no-build"); rc != ExitOK {
		t.Fatalf("exit %d\n%s", rc, e.both())
	}
	if asked || strings.Contains(e.out.String(), "Directory (tenant) ID") || !strings.Contains(e.out.String(), "Storage [local]") {
		t.Fatalf("with nothing Microsoft known the default is local and no Microsoft question is asked:\n%s", e.out.String())
	}
	if cfg, err := loadConfig(newDeployment(target)); err != nil || len(cfg.Repositories) != 0 {
		t.Fatalf("%v", err)
	}
}

// A server that has no repository yet is running, not broken: start, status and doctor say so.
func TestStartAndDoctorTreatAMissingFirstRepositoryAsANormalState(t *testing.T) {
	e := newEnv(t)
	e.readyStatus = 503
	e.readyBody = "not ready: no repository is configured yet; add one with `zaks-drive repo add`"
	if rc := e.main("start", "--dir", e.dir, "--timeout", "5s"); rc != ExitOK {
		t.Fatalf("start must succeed: %d\n%s", rc, e.both())
	}
	for _, want := range []string{"running, but no repository is configured yet", "zaks-drive repo add AD --backend local --yes", "--backend sharepoint"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("start output lacks %q:\n%s", want, e.out.String())
		}
	}
	if strings.Contains(e.out.String(), "ZAKS Drive is ready.") {
		t.Error("it must not claim to be ready")
	}
	f, _ := e.doctor(t)
	wantStatus(t, f, "readiness", StatusWarn)
	if !strings.Contains(f["readiness"].Fix, "zaks-drive repo add") {
		t.Errorf("the fix must name the command: %s", f["readiness"].Fix)
	}
	// any other 503 is still a failure
	e.readyBody = "not ready: repository check failed: Microsoft rejected the credentials"
	if rc := e.main("start", "--dir", e.dir, "--timeout", "1s"); rc != ExitUnhealthy {
		t.Fatalf("a real failure must stay a failure: %d", rc)
	}
}
