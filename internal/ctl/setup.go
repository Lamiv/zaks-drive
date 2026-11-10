package ctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Lamiv/zaks-drive/deploy"
	"github.com/Lamiv/zaks-drive/internal/config"
)

// Answers are the choices that make up a deployment.
type Answers struct {
	Dir        string
	Bind       string
	RESTPort   int
	CSPort     int
	AdminPort  int
	Interfaces []string
	SiteURL    string
	Library    string
	RootPath   string
	// Storage is where documents go first: "sharepoint" (a Microsoft application and a site) or
	// "local" (a folder on this server; no Microsoft account is needed).
	Storage         string
	TenantID        string
	ClientID        string
	ClientSecret    string
	ContReps        []ContRepAnswer
	AllowedNetworks []string
	TLS             bool
	LogLevel        string
	// Generated or kept from an existing .env.
	APIKeyS4, APIKeyFiori, AdminToken string
}

// ContRepAnswer maps an SAP content repository ID (OAC0) to a signature mode.
type ContRepAnswer struct{ ID, Signature string }

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

var guidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const defaultDir = "/opt/zaks-drive"

func (a *App) cmdSetup(ctx context.Context, args []string) int {
	fset := flag.NewFlagSet("setup", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory (default "+defaultDir+")")
	nonInteractive := fset.Bool("non-interactive", false, "do not prompt; take everything from flags and environment")
	force := fset.Bool("force", false, "replace an existing configuration (backups are kept)")
	bind := fset.String("bind", "", "host address for the REST and SAP ports (default 0.0.0.0)")
	rest := fset.Int("rest-port", 0, "host port of the REST API (default 8080)")
	cs := fset.Int("cs-port", 0, "host port of the SAP Content Server interface (default 8090)")
	admin := fset.Int("admin-port", 0, "host port of the admin/health endpoint, bound to 127.0.0.1 (default 9090)")
	ifaces := fset.String("interfaces", "", "comma-separated: rest, contentrepo (default both)")
	storage := fset.String("storage", "", "where documents are stored first: local (a folder on this server, no Microsoft account needed) or sharepoint (default; asked when interactive)")
	site := fset.String("site-url", "", "SharePoint site address, e.g. https://contoso.sharepoint.com/sites/SAP_DOCS")
	library := fset.String("library", "", "document library display name (default Documents)")
	root := fset.String("root-path", "", "folder inside the library (default SAP_DMS)")
	tenant := fset.String("tenant-id", "", "Microsoft Entra directory (tenant) ID")
	client := fset.String("client-id", "", "Microsoft Entra application (client) ID")
	secretFile := fset.String("client-secret-file", "", "file holding the client secret VALUE (or set ZAKS_DRIVE_CLIENT_SECRET, or type it when prompted)")
	var contReps, networks stringList
	fset.Var(&contReps, "content-repo", "SAP content repository ID[:required|optional|none]; repeatable (default Z1:required)")
	fset.Var(&networks, "allowed-network", "CIDR or IP allowed to call the SAP interface; repeatable (default: any)")
	tls := fset.Bool("tls", false, "ZAKS Drive terminates TLS itself (put tls.crt and tls.key into <dir>/tls)")
	logLevel := fset.String("log-level", "", "debug, info, warn or error (default info)")
	noBuild := fset.Bool("no-build", false, "do not build the image from the release payload")
	version := fset.String("version", "", "release version to install (default: the version of this command)")
	if err := fset.Parse(args); err != nil {
		return ExitUsage
	}
	interactive := !*nonInteractive
	if interactive && (a.Interactive == nil || !a.Interactive()) {
		return a.fail(ExitUsage, "setup needs a terminal to ask questions", "run with --non-interactive and the flags (see: zaks-drive setup -h)")
	}

	target := *dir
	if target == "" {
		target = a.env("ZAKS_DRIVE_DIR")
	}
	if target == "" {
		target = defaultDir
		if interactive {
			target = a.ask("Deployment directory", defaultDir)
		}
	}
	target, _ = filepath.Abs(target)
	d := newDeployment(target)

	existing, envErr := ReadEnv(d.EnvFile)
	hadEnv := envErr == nil
	if _, err := os.Stat(d.ConfigFile); (hadEnv || err == nil) && !*force {
		return a.fail(ExitFail, "a configuration already exists in "+target,
			"use --force to reconfigure it (the old files are backed up, existing secrets are kept unless you change them)")
	}

	ans := Answers{Dir: target}
	pick := func(flagVal, envKey, def string) string {
		if flagVal != "" {
			return flagVal
		}
		if v := existing[envKey]; v != "" {
			return v
		}
		return def
	}
	portOf := func(flagVal int, key string, def int) int {
		if flagVal != 0 {
			return flagVal
		}
		return envPort(existing, key, def)
	}
	ans.Bind = pick(*bind, "BIND_ADDRESS", "0.0.0.0")
	ans.RESTPort = portOf(*rest, "REST_PORT", 8080)
	ans.CSPort = portOf(*cs, "CS_PORT", 8090)
	ans.AdminPort = portOf(*admin, "ADMIN_PORT", 9090)
	ans.SiteURL = pick(*site, "SHAREPOINT_SITE_URL", "")
	ans.Library = pick(*library, "SHAREPOINT_LIBRARY", "Documents")
	ans.RootPath = pick(*root, "SHAREPOINT_ROOT_PATH", "SAP_DMS")
	ans.TenantID = pick(*tenant, "GRAPH_TENANT_ID", "")
	ans.ClientID = pick(*client, "GRAPH_CLIENT_ID", "")
	ans.LogLevel = pick(*logLevel, "LOG_LEVEL", "info")
	ans.TLS = *tls
	// Where documents go first. Without a Microsoft account a local folder is the way to start; a
	// SharePoint repository (with its own Entra application if you like) can be added at any time
	// with `zaks-drive repo add`.
	microsoftGiven := *site != "" || *tenant != "" || *client != "" || *secretFile != "" || a.env("ZAKS_DRIVE_CLIENT_SECRET") != "" ||
		existing["GRAPH_TENANT_ID"] != "" || existing["SHAREPOINT_SITE_URL"] != ""
	ans.Storage = *storage
	if ans.Storage == "" {
		ans.Storage = "sharepoint"
		if interactive {
			def := "local"
			if microsoftGiven {
				def = "sharepoint"
			}
			a.printf("Where will documents be stored first?\n  local       a folder on this server (no Microsoft account needed; good for trying it out)\n  sharepoint  SharePoint Online or OneDrive for Business (needs a Microsoft Entra application)\nYou can add repositories of either kind later with: zaks-drive repo add\n")
			ans.Storage = strings.ToLower(strings.TrimSpace(a.ask("Storage", def)))
		}
	}
	if ans.Storage != "local" && ans.Storage != "sharepoint" {
		return a.fail(ExitUsage, fmt.Sprintf("--storage %q is not known", ans.Storage), "use local or sharepoint")
	}
	if ans.Storage == "local" && (*site != "" || *tenant != "" || *client != "" || *secretFile != "" || *library != "" || *root != "") {
		return a.fail(ExitUsage, "the SharePoint and Microsoft options do not apply with --storage local",
			"leave them out, or use --storage sharepoint; a SharePoint repository can be added later with: zaks-drive repo add")
	}
	ans.Interfaces = splitCSV(*ifaces)
	if len(ans.Interfaces) == 0 {
		ans.Interfaces = []string{"rest", "contentrepo"}
		if ans.Storage == "local" {
			ans.Interfaces = []string{"rest"} // SAP is added per repository (repo add --sap); enable it with --interfaces rest,contentrepo
		}
	}
	for _, c := range contReps {
		id, sig, _ := strings.Cut(c, ":")
		if sig == "" {
			sig = "required"
		}
		ans.ContReps = append(ans.ContReps, ContRepAnswer{ID: strings.TrimSpace(id), Signature: strings.TrimSpace(sig)})
	}
	if len(ans.ContReps) == 0 {
		ans.ContReps = []ContRepAnswer{{"Z1", "required"}}
	}
	ans.AllowedNetworks = append(ans.AllowedNetworks, networks...)

	// The client secret: a file, the environment, a prompt, or the existing .env value.
	haveSecret := existing["GRAPH_CLIENT_SECRET"] != ""
	switch {
	case *secretFile != "":
		b, err := os.ReadFile(*secretFile)
		if err != nil {
			return a.fail(ExitUsage, "cannot read --client-secret-file: "+err.Error(), "")
		}
		ans.ClientSecret = strings.TrimSpace(string(b))
	case a.env("ZAKS_DRIVE_CLIENT_SECRET") != "":
		ans.ClientSecret = strings.TrimSpace(a.env("ZAKS_DRIVE_CLIENT_SECRET"))
	}

	if interactive {
		a.printf("ZAKS Drive setup. Press Enter to accept the value in brackets. Nothing is sent anywhere.\n\n")
		ans.Bind = a.ask("Address the REST and SAP ports listen on (0.0.0.0 = all interfaces)", ans.Bind)
		ans.RESTPort = atoiOr(a.ask("REST API host port", strconv.Itoa(ans.RESTPort)), ans.RESTPort)
		ans.CSPort = atoiOr(a.ask("SAP Content Server host port", strconv.Itoa(ans.CSPort)), ans.CSPort)
		ans.AdminPort = atoiOr(a.ask("Admin/health host port (always bound to 127.0.0.1)", strconv.Itoa(ans.AdminPort)), ans.AdminPort)
		if ans.Storage == "sharepoint" {
			a.printf("\nMicrosoft 365 (see docs/microsoft-365/entra-app-registration.md)\n")
			ans.TenantID = a.ask("Directory (tenant) ID", ans.TenantID)
			ans.ClientID = a.ask("Application (client) ID", ans.ClientID)
			if ans.ClientSecret == "" {
				s, err := a.askSecret("Client secret VALUE (not the secret ID; input is hidden)", haveSecret)
				if err != nil {
					return a.fail(ExitUsage, "could not read the client secret: "+err.Error(), "")
				}
				ans.ClientSecret = s
			}
			a.printf("\nWhere documents are stored\n")
			ans.SiteURL = a.ask("SharePoint site address (https://<tenant>.sharepoint.com/sites/<site>)", ans.SiteURL)
			ans.Library = a.ask("Document library display name (empty = the site's default library)", ans.Library)
			ans.RootPath = a.ask("Folder inside the library (created automatically)", ans.RootPath)
		}
		a.printf("\nSAP Content Server interface\n")
		sapDefault := "y"
		if ans.Storage == "local" {
			sapDefault = "n" // a local folder is easiest to try through the REST API first
		}
		sap := a.ask("Enable the SAP Content Server interface? (y/n)", sapDefault)
		if strings.HasPrefix(strings.ToLower(sap), "n") {
			ans.Interfaces = []string{"rest"}
		} else {
			ans.Interfaces = []string{"rest", "contentrepo"}
			if ans.Storage == "sharepoint" {
				ids := a.ask("SAP content repository IDs created in OAC0 (comma-separated)", joinContReps(ans.ContReps))
				ans.ContReps = parseContReps(ids)
			}
			nets := a.ask("Networks allowed to call it (comma-separated CIDR, empty = any; recommended: your SAP servers)", strings.Join(ans.AllowedNetworks, ","))
			ans.AllowedNetworks = splitCSV(nets)
		}
		a.printf("\nTLS and logging\n")
		tlsAns := a.ask("Terminate TLS inside ZAKS Drive? (n = use a reverse proxy; y = you will supply tls.crt and tls.key)", "n")
		ans.TLS = strings.HasPrefix(strings.ToLower(tlsAns), "y")
		ans.LogLevel = a.ask("Log level (debug, info, warn, error)", ans.LogLevel)
	}
	if ans.ClientSecret == "" {
		ans.ClientSecret = existing["GRAPH_CLIENT_SECRET"]
	}
	ans.APIKeyS4 = firstNonEmptyStr(existing["ADAPTER_API_KEY_S4"], randomHex())
	ans.APIKeyFiori = firstNonEmptyStr(existing["ADAPTER_API_KEY_FIORI"], randomHex())
	ans.AdminToken = firstNonEmptyStr(existing["ADMIN_TOKEN"], randomHex())

	if problems := validateAnswers(ans); len(problems) > 0 {
		a.errorf("zaks-drive: the answers are not valid:\n")
		for _, p := range problems {
			a.errorf("  - %s\n", p)
		}
		return ExitUsage
	}

	envText, err := renderEnv(ans)
	if err != nil {
		return a.fail(ExitUsage, err.Error(), "")
	}
	cfgText := renderConfig(ans)

	// Validate the candidate files with the server's own loader before touching the deployment.
	if rc := a.validateCandidate(envText, cfgText); rc != ExitOK {
		return rc
	}

	// Write the files.
	if err := os.MkdirAll(target, 0o750); err != nil {
		return a.fail(ExitFail, "cannot create "+target+": "+err.Error(), "choose another directory with --dir, or create it with the right owner first")
	}
	now := a.Now()
	for _, f := range []string{d.EnvFile, d.ConfigFile} {
		if bak, err := backupFile(f, now); err != nil {
			return a.fail(ExitFail, "cannot back up "+f+": "+err.Error(), "")
		} else if bak != "" {
			a.printf("Backed up %s to %s\n", f, bak)
		}
	}
	if err := writeFileAtomic(d.EnvFile, envText, 0o600); err != nil {
		return a.fail(ExitFail, "cannot write .env: "+err.Error(), "")
	}
	// config.yaml holds no secrets (it references .env), and the container runs as a non-root user
	// that must be able to read the bind-mounted file, so it is world-readable.
	if err := writeFileAtomic(d.ConfigFile, cfgText, 0o644); err != nil {
		return a.fail(ExitFail, "cannot write config.yaml: "+err.Error(), "")
	}
	if _, err := os.Stat(d.ComposeFile); errors.Is(err, fs.ErrNotExist) {
		if err := writeFileAtomic(d.ComposeFile, deploy.ComposeRelease, 0o644); err != nil {
			return a.fail(ExitFail, "cannot write docker-compose.yml: "+err.Error(), "")
		}
	}
	_ = os.MkdirAll(filepath.Join(target, "tls"), 0o750)

	// Record the version and build the image.
	ver := *version
	if ver == "" {
		ver = a.Version
	}
	built := ""
	if !d.SourceMode() && validVersion(ver) {
		st := d.loadState()
		if st.Current != ver {
			st.Current = ver
			st.History = append(st.History, StateEvent{At: now, Action: "setup", Version: ver})
			_ = d.saveState(st)
		}
		if !*noBuild {
			if a.imageExists(ctx, "zaks-drive:"+ver) {
				built = "image zaks-drive:" + ver + " already exists"
			} else if dirP, ok := a.payloadDir(ver); ok {
				if _, perr := a.checkDocker(ctx); perr != nil {
					built = "not built (Docker is not usable yet: " + perr.Error() + ")"
				} else if res, err := a.buildImage(ctx, dirP, ver); err != nil || res.ExitCode != 0 {
					built = "BUILD FAILED: " + oneLine(trimLines(res.Stderr, 3))
				} else {
					built = "image zaks-drive:" + ver + " built from the release payload"
				}
			} else {
				built = "not built: no release payload for " + ver + " found (install with install.sh, or use zaks-drive update --version " + ver + ")"
			}
		}
	}

	a.printSummary(ans, built)
	return ExitOK
}

// validateCandidate writes the candidate files to a private temporary directory and validates
// them with the server's configuration loader. The directory is removed afterwards.
func (a *App) validateCandidate(envText, cfgText []byte) int {
	tmp, err := os.MkdirTemp("", "zaks-drive-setup-")
	if err != nil {
		return a.fail(ExitFail, "cannot create a temporary directory: "+err.Error(), "")
	}
	defer os.RemoveAll(tmp)
	td := newDeployment(tmp)
	if err := os.WriteFile(td.EnvFile, envText, 0o600); err != nil {
		return a.fail(ExitFail, err.Error(), "")
	}
	if err := os.WriteFile(td.ConfigFile, cfgText, 0o600); err != nil {
		return a.fail(ExitFail, err.Error(), "")
	}
	// The compose file tells the loader that the deployment has a state directory (repositories may come from there).
	if err := os.WriteFile(td.ComposeFile, deploy.ComposeRelease, 0o600); err != nil {
		return a.fail(ExitFail, err.Error(), "")
	}
	if _, problems := a.validateDeployment(td); len(problems) > 0 {
		a.errorf("zaks-drive: the configuration would not be valid, so nothing was written:\n")
		for _, p := range problems {
			a.errorf("  %s\n", p)
		}
		return ExitFail
	}
	return ExitOK
}

func (a *App) printSummary(ans Answers, built string) {
	a.printf("\nConfiguration written to %s (secrets are not shown):\n", ans.Dir)
	a.printf("  .env           mode 600  (Microsoft credentials, API keys, admin token)\n")
	a.printf("  config.yaml    mode 644  (no secrets; the container must be able to read it)\n")
	a.printf("  Listens on     %s: REST %d, SAP %d; admin 127.0.0.1:%d\n", ans.Bind, ans.RESTPort, ans.CSPort, ans.AdminPort)
	a.printf("  Interfaces     %s\n", strings.Join(ans.Interfaces, ", "))
	if ans.Storage == "local" {
		a.printf("  Storage        local folder (no Microsoft account configured; add SharePoint repositories later with: zaks-drive repo add)\n")
	} else {
		a.printf("  Tenant         %s\n", ans.TenantID)
		a.printf("  Client         %s (secret: set, %d characters, not shown)\n", ans.ClientID, len(ans.ClientSecret))
		a.printf("  SharePoint     %s, library %q, folder %q\n", ans.SiteURL, ans.Library, ans.RootPath)
	}
	if contains(ans.Interfaces, "contentrepo") {
		for _, c := range ans.ContReps {
			if ans.Storage == "local" {
				break // the SAP repositories of a local deployment are created with: zaks-drive repo add ID --sap
			}
			a.printf("  SAP repository %s (signature: %s)\n", c.ID, c.Signature)
			if c.Signature == "none" {
				a.printf("    WARNING: signature none disables SAP signature checks; use it for tests only\n")
			}
		}
		if len(ans.AllowedNetworks) == 0 {
			a.printf("  WARNING: no allowed networks set: any host that can reach port %d may call the SAP interface\n", ans.CSPort)
		}
	}
	if ans.TLS {
		a.printf("  TLS            ZAKS Drive terminates TLS: put tls.crt and tls.key into %s\n", filepath.Join(ans.Dir, "tls"))
	} else {
		a.printf("  TLS            not terminated here: put a TLS reverse proxy in front for production\n")
	}
	a.printf("  API keys       generated (3 values in .env: ADAPTER_API_KEY_S4, ADAPTER_API_KEY_FIORI, ADMIN_TOKEN)\n")
	if built != "" {
		a.printf("  Image          %s\n", built)
	}
	if ans.Storage == "local" {
		a.printf("\nNext (no Microsoft account needed):\n  zaks-drive start --dir %s\n  zaks-drive repo add AD --backend local --yes --dir %s     # your first repository: a folder on this server\n", ans.Dir, ans.Dir)
		a.printf("  then open the admin page (first login admin / admin, you will choose a new password) or try the REST API.\nLater, add SharePoint repositories (each may have its own Microsoft application): zaks-drive repo add ZH --backend sharepoint ...\n")
		return
	}
	a.printf("\nNext: zaks-drive --help shows the commands. Start with:\n  zaks-drive start --dir %s\n  zaks-drive check --dir %s   (tests Microsoft access)\n", ans.Dir, ans.Dir)
}

// ---- validation and rendering ----

func validateAnswers(ans Answers) []string {
	var p []string
	if net.ParseIP(ans.Bind) == nil {
		p = append(p, fmt.Sprintf("the bind address %q is not an IP address (use 0.0.0.0 or a host interface address)", ans.Bind))
	}
	for name, port := range map[string]int{"REST": ans.RESTPort, "SAP": ans.CSPort, "admin": ans.AdminPort} {
		if port < 1 || port > 65535 {
			p = append(p, fmt.Sprintf("the %s port %d is not between 1 and 65535", name, port))
		}
	}
	if ans.RESTPort == ans.CSPort || ans.RESTPort == ans.AdminPort || ans.CSPort == ans.AdminPort {
		p = append(p, "the REST, SAP and admin ports must be three different ports")
	}
	for _, i := range ans.Interfaces {
		if i != "rest" && i != "contentrepo" {
			p = append(p, fmt.Sprintf("the interface %q is unknown (use rest and/or contentrepo)", i))
		}
	}
	if ans.Storage != "local" { // a local folder needs no Microsoft account
		if ans.SiteURL == "" {
			p = append(p, "the SharePoint site address is required (--site-url)")
		}
		if !guidRe.MatchString(ans.TenantID) {
			p = append(p, "the tenant ID must be a GUID like 00000000-0000-0000-0000-000000000000 (--tenant-id)")
		}
		if !guidRe.MatchString(ans.ClientID) {
			p = append(p, "the client ID must be a GUID like 00000000-0000-0000-0000-000000000000 (--client-id)")
		}
		if ans.ClientSecret == "" {
			p = append(p, "the client secret is required (--client-secret-file FILE, or ZAKS_DRIVE_CLIENT_SECRET, or type it when prompted)")
		} else if guidRe.MatchString(ans.ClientSecret) {
			p = append(p, "the client secret looks like a secret ID (a GUID): use the secret VALUE shown once when the secret was created")
		}
		for name, v := range map[string]string{"site address": ans.SiteURL, "library": ans.Library, "folder": ans.RootPath, "tenant ID": ans.TenantID, "client ID": ans.ClientID, "client secret": ans.ClientSecret} {
			if strings.ContainsAny(v, "\r\n\x00") {
				p = append(p, "the "+name+" must not contain line breaks")
			}
			if strings.Contains(v, "'") {
				p = append(p, "the "+name+" must not contain a single quote")
			}
		}
	}
	if ans.LogLevel != "debug" && ans.LogLevel != "info" && ans.LogLevel != "warn" && ans.LogLevel != "error" {
		p = append(p, fmt.Sprintf("the log level %q is unknown (use debug, info, warn or error)", ans.LogLevel))
	}
	seen := map[string]bool{}
	for _, c := range ans.ContReps {
		if len(c.ID) < 1 || len(c.ID) > 2 || !regexp.MustCompile(`^[A-Za-z0-9]+$`).MatchString(c.ID) {
			p = append(p, fmt.Sprintf("the SAP content repository ID %q must be 1 or 2 letters or digits (the ID created in OAC0)", c.ID))
		}
		if seen[c.ID] {
			p = append(p, fmt.Sprintf("the SAP content repository %q is listed twice", c.ID))
		}
		seen[c.ID] = true
		if c.Signature != "required" && c.Signature != "optional" && c.Signature != "none" {
			p = append(p, fmt.Sprintf("the signature mode %q of %s is unknown (use required, optional or none)", c.Signature, c.ID))
		}
	}
	for _, n := range ans.AllowedNetworks {
		if _, err := config.ParseNetwork(n); err != nil {
			p = append(p, fmt.Sprintf("the allowed network %q is not an IP address or CIDR", n))
		}
	}
	return p
}

var envSafeRe = regexp.MustCompile(`^[A-Za-z0-9._~:/@+=,-]*$`)

func envValue(v string) string {
	if envSafeRe.MatchString(v) {
		return v
	}
	return "'" + v + "'" // single quotes: no $ interpolation, no comment handling
}

// renderEnv writes the .env file. Values with unusual characters are single-quoted.
func renderEnv(ans Answers) ([]byte, error) {
	var b strings.Builder
	b.WriteString("# Written by zaks-drive setup. This file holds secrets: keep it at mode 600 and never commit it.\n")
	b.WriteString("# After editing it: zaks-drive restart\n\n")
	if ans.Storage == "local" {
		b.WriteString("# No Microsoft account is configured: documents are stored in a folder on this server.\n")
		b.WriteString("# SharePoint repositories can be added later with: zaks-drive repo add <id> --backend sharepoint ...\n")
		b.WriteString("# (each may bring its own Microsoft application). Fill these three only if you want a shared global one:\n")
		b.WriteString("GRAPH_TENANT_ID=\nGRAPH_CLIENT_ID=\nGRAPH_CLIENT_SECRET=\n\n")
	} else {
		b.WriteString("# Microsoft Entra application\n")
		fmt.Fprintf(&b, "GRAPH_TENANT_ID=%s\nGRAPH_CLIENT_ID=%s\nGRAPH_CLIENT_SECRET=%s\n\n", envValue(ans.TenantID), envValue(ans.ClientID), envValue(ans.ClientSecret))
		b.WriteString("# Where documents are stored in SharePoint\n")
		fmt.Fprintf(&b, "SHAREPOINT_SITE_URL=%s\nSHAREPOINT_LIBRARY=%s\nSHAREPOINT_ROOT_PATH=%s\n\n", envValue(ans.SiteURL), envValue(ans.Library), envValue(ans.RootPath))
	}
	b.WriteString("# Access to ZAKS Drive itself\n")
	fmt.Fprintf(&b, "ADAPTER_API_KEY_S4=%s\nADAPTER_API_KEY_FIORI=%s\nADMIN_TOKEN=%s\n\n", ans.APIKeyS4, ans.APIKeyFiori, ans.AdminToken)
	b.WriteString("# Host ports and address (the admin port is always bound to 127.0.0.1)\n")
	fmt.Fprintf(&b, "BIND_ADDRESS=%s\nREST_PORT=%d\nCS_PORT=%d\nADMIN_PORT=%d\n", ans.Bind, ans.RESTPort, ans.CSPort, ans.AdminPort)
	if ans.LogLevel != "info" {
		fmt.Fprintf(&b, "\nLOG_LEVEL=%s\n", ans.LogLevel)
	}
	return []byte(b.String()), nil
}

var (
	ifacesRe  = regexp.MustCompile(`(?m)^(  interfaces: )\[[^\]]*\]`)
	tlsCertRe = regexp.MustCompile(`(?m)^  # (tlsCertFile: .*)$`)
	tlsKeyRe  = regexp.MustCompile(`(?m)^  # (tlsKeyFile: .*)$`)
	netsRe    = regexp.MustCompile(`(?m)^(  allowedNetworks: )\[\]`)
	logLvlRe  = regexp.MustCompile(`(?m)^(  level: )\w+`)

	// Parts of the example configuration that a local-first deployment leaves out (tolerant of CRLF checkouts).
	staticRepoRe = regexp.MustCompile(`\r?\nrepositories:\r?\n  - id: DMS\r?\n    siteUrl: \$\{SHAREPOINT_SITE_URL\}\r?\n    driveName: \$\{SHAREPOINT_LIBRARY\}\r?\n    rootPath: \$\{SHAREPOINT_ROOT_PATH\}\r?\n`)
	fioriReposRe = regexp.MustCompile(`(key: \$\{ADAPTER_API_KEY_FIORI\}[^\n]*\r?\n      repositories: )\[DMS\]`)
	csReposRe    = regexp.MustCompile(`(?s)(\n  autoActivateCertificates: false\r?\n  )repositories:.*?(\r?\n\r?\nlog:)`)
)

// renderConfig starts from the commented example configuration and changes only what the
// answers decide, so the generated file keeps the format and comments users know.
func renderConfig(ans Answers) []byte {
	s := string(deploy.ConfigExample)
	s = ifacesRe.ReplaceAllString(s, "${1}["+strings.Join(ans.Interfaces, ", ")+"]")
	if ans.TLS {
		s = tlsCertRe.ReplaceAllString(s, "  $1")
		s = tlsKeyRe.ReplaceAllString(s, "  $1")
	}
	if len(ans.AllowedNetworks) > 0 {
		q := make([]string, len(ans.AllowedNetworks))
		for i, n := range ans.AllowedNetworks {
			q[i] = strconv.Quote(n)
		}
		s = netsRe.ReplaceAllString(s, "${1}["+strings.Join(q, ", ")+"]")
	}
	s = logLvlRe.ReplaceAllString(s, "${1}"+ans.LogLevel)

	if ans.Storage == "local" {
		// No static SharePoint repository and no SAP repositories: they are created with `zaks-drive repo add`
		// (stored in the state volume), and the read-only key is not tied to a repository that does not exist.
		s = staticRepoRe.ReplaceAllString(s, "\nrepositories: []   # none yet: add one with `zaks-drive repo add` (a local folder or SharePoint)\n")
		s = fioriReposRe.ReplaceAllString(s, `${1}["*"]`)
		s = csReposRe.ReplaceAllString(s, "${1}repositories: []   # SAP content repositories come from `zaks-drive repo add <id> --sap`${2}")
		return []byte(s)
	}

	// Replace the content repositories list.
	start := strings.Index(s, "contentServer:")
	if start >= 0 {
		if rel := strings.Index(s[start:], "\n  repositories:\n"); rel >= 0 {
			from := start + rel + len("\n  repositories:\n")
			end := strings.Index(s[from:], "\n\nlog:")
			if end < 0 {
				end = len(s) - from
			}
			var b strings.Builder
			for _, c := range ans.ContReps {
				fmt.Fprintf(&b, "    - contRep: %s               # content repository ID created in OAC0\n", c.ID)
				fmt.Fprintf(&b, "      description: SAP documents in SharePoint (%s)\n", c.ID)
				b.WriteString("      repository: DMS\n")
				fmt.Fprintf(&b, "      folder: ContentServer/%s\n", c.ID)
				fmt.Fprintf(&b, "      signature: %s\n", c.Signature)
			}
			s = s[:from] + strings.TrimRight(b.String(), "\n") + s[from+end:]
		}
	}
	return []byte(s)
}

func parseContReps(csv string) []ContRepAnswer {
	var out []ContRepAnswer
	for _, p := range splitCSV(csv) {
		id, sig, _ := strings.Cut(p, ":")
		if sig == "" {
			sig = "required"
		}
		out = append(out, ContRepAnswer{ID: strings.TrimSpace(id), Signature: strings.TrimSpace(sig)})
	}
	return out
}

func joinContReps(cs []ContRepAnswer) string {
	var p []string
	for _, c := range cs {
		if c.Signature == "required" {
			p = append(p, c.ID)
		} else {
			p = append(p, c.ID+":"+c.Signature)
		}
	}
	return strings.Join(p, ",")
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func randomHex() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
