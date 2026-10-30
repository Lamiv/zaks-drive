// Package config loads adapter configuration from a YAML file with ${ENV}
// expansion, then applies well-known environment overrides for secrets.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/safelog"
)

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	Graph        GraphConfig        `yaml:"graph"`
	Transfer     TransferConfig     `yaml:"transfer"`
	Auth         AuthConfig         `yaml:"auth"`
	Repositories []RepositoryConfig `yaml:"repositories"`
	// ContentServer configures the SAP Content Server HTTP interface.
	ContentServer ContentServerConfig `yaml:"contentServer"`
	Log           LogConfig           `yaml:"log"`

	// emptyRefs holds the ${VAR} names the YAML file references whose
	// environment variable is empty or unset; used to explain validation
	// errors ("SHAREPOINT_SITE_URL is empty") instead of a bare "missing".
	emptyRefs map[string]bool
	// repoErrs are problems found while reading the state directory; Validate reports them.
	repoErrs []error
	// Fallback is set when the repository definitions in the state directory
	// did not validate and the last state that started is served instead; it
	// holds the reason.
	Fallback string
}

type ContentServerConfig struct {
	// AllowedNetworks restricts callers to these CIDRs (empty = any).
	AllowedNetworks []string `yaml:"allowedNetworks"`
	// ClockSkew tolerated when checking the signed URL expiration.
	ClockSkew time.Duration `yaml:"clockSkew"`
	// AutoActivateCertificates activates certificates received via putCert
	// immediately (trust on first use). Keep false in production.
	AutoActivateCertificates bool                      `yaml:"autoActivateCertificates"`
	Repositories             []ContentRepositoryConfig `yaml:"repositories"`
}

type ContentRepositoryConfig struct {
	// ContRep is the SAP content repository ID from transaction OAC0.
	ContRep     string `yaml:"contRep"`
	Description string `yaml:"description"`
	// Repository references a storage repository (library + root path).
	Repository string `yaml:"repository"`
	// Folder below the repository root; defaults to "ContentServer/<contRep>".
	Folder string `yaml:"folder"`
	// Signature: required (default) | optional | none. Must match OAC0.
	Signature string `yaml:"signature"`
}

type ServerConfig struct {
	// Interfaces to start: "rest", "contentrepo". Admin (metrics/health) always runs.
	Interfaces      []string      `yaml:"interfaces"`
	RESTAddr        string        `yaml:"restAddr"`
	ContentRepoAddr string        `yaml:"contentRepoAddr"`
	AdminAddr       string        `yaml:"adminAddr"`
	MaxInFlight     int           `yaml:"maxInFlight"` // per listener; excess requests get 503
	ReadHeaderTO    time.Duration `yaml:"readHeaderTimeout"`
	IdleTimeout     time.Duration `yaml:"idleTimeout"`
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"`
	// DrainTimeout bounds how long a restart for a repository change waits for
	// running requests (maintenance mode refuses new ones meanwhile).
	DrainTimeout time.Duration `yaml:"drainTimeout"`
	CORSOrigins  []string      `yaml:"corsOrigins"`
	TLSCertFile  string        `yaml:"tlsCertFile"`
	TLSKeyFile   string        `yaml:"tlsKeyFile"`
	EnablePprof  bool          `yaml:"enablePprof"`
	// AdminToken, when set, is required (Bearer) for mutating admin
	// endpoints such as certificate activation.
	AdminToken string `yaml:"adminToken"`
	// StateDir holds repository definitions (repos.d), their secrets and the
	// data of local repositories. Empty disables managed repositories.
	StateDir string `yaml:"stateDir"`
}

type GraphConfig struct {
	TenantID     string `yaml:"tenantId"`
	ClientID     string `yaml:"clientId"`
	ClientSecret string `yaml:"clientSecret"`
	// AuthorityURL defaults to https://login.microsoftonline.com.
	AuthorityURL string `yaml:"authorityUrl"`
	// BaseURL defaults to https://graph.microsoft.com/v1.0.
	BaseURL string `yaml:"baseUrl"`
	Scope   string `yaml:"scope"`
	// MaxConcurrency caps simultaneous Graph calls across the process to
	// stay below SharePoint throttling thresholds.
	MaxConcurrency  int           `yaml:"maxConcurrency"`
	MaxRetries      int           `yaml:"maxRetries"`
	MaxIdleConns    int           `yaml:"maxIdleConnsPerHost"`
	MaxRetryBackoff time.Duration `yaml:"maxRetryBackoff"`
	// MetadataCacheTTL caches item metadata (incl. pre-authenticated
	// download URLs, valid ~1h) to make repeated reads one hop. 0 disables.
	MetadataCacheTTL time.Duration `yaml:"metadataCacheTtl"`
}

type TransferConfig struct {
	// Files at or below this size use a single PUT; larger use an upload session.
	SimpleUploadMax int64 `yaml:"simpleUploadMaxBytes"`
	// Chunk size for upload sessions; rounded down to a multiple of 320 KiB.
	ChunkSize int64 `yaml:"chunkSizeBytes"`
	// MemoryBudget bounds the total bytes held in transfer buffers process-wide.
	MemoryBudget int64  `yaml:"memoryBudgetBytes"`
	MaxUpload    int64  `yaml:"maxUploadBytes"`
	SpoolDir     string `yaml:"spoolDir"`
	// DownloadMode: "proxy" streams bytes through the adapter, "redirect"
	// returns a 302 to the short-lived pre-authenticated SharePoint URL.
	DownloadMode string `yaml:"downloadMode"`
}

type AuthConfig struct {
	APIKeys []APIKeyConfig `yaml:"apiKeys"`
	JWT     *JWTConfig     `yaml:"jwt"`
	// Disabled turns off inbound auth entirely (local dev only).
	Disabled bool `yaml:"disabled"`
}

type APIKeyConfig struct {
	Name string `yaml:"name"`
	// SHA256 is the hex-encoded SHA-256 of the key. Key holds a plaintext key
	// (allowed for convenience, e.g. injected via ${ENV}).
	SHA256       string   `yaml:"sha256"`
	Key          string   `yaml:"key"`
	Repositories []string `yaml:"repositories"` // empty or "*" = all
	Permissions  []string `yaml:"permissions"`  // read, write, delete
}

type JWTConfig struct {
	Issuer   string `yaml:"issuer"`
	Audience string `yaml:"audience"`
	JWKSURL  string `yaml:"jwksUrl"`
	// RolesClaim holds permission strings, e.g. "roles" (Entra) or "scope".
	RolesClaim string `yaml:"rolesClaim"`
	// RolePrefix is stripped from role values: "Documents.Read" -> "read".
	RolePrefix string `yaml:"rolePrefix"`
}

type RepositoryConfig struct {
	ID string `yaml:"id"`
	// One of DriveID, UserID, SiteID (+DriveName) or SiteURL identifies the library.
	DriveID string `yaml:"driveId"`
	// UserID is a user principal name or object ID whose OneDrive is used
	// (resolved through /users/{id}/drive). Works with the Files.ReadWrite.All
	// application permission; the site lookup needs Sites.* permissions.
	UserID    string `yaml:"userId"`
	SiteID    string `yaml:"siteId"`
	SiteURL   string `yaml:"siteUrl"`
	DriveName string `yaml:"driveName"`
	RootPath  string `yaml:"rootPath"`
	ReadOnly  bool   `yaml:"readOnly"`

	Description string `yaml:"description"`
	// Backend is "sharepoint" (default) or "local".
	Backend string `yaml:"backend"`
	// Local holds the settings of a local-folder repository.
	Local LocalConfig `yaml:"local"`
	// TenantID, ClientID and ClientSecretFile name an own Microsoft
	// application for this repository. Omitted: the global graph.* values.
	// The secret is read from a file below secrets/ in the state directory.
	TenantID         string `yaml:"tenantId"`
	ClientID         string `yaml:"clientId"`
	ClientSecretFile string `yaml:"clientSecretFile"`
	// SAP exposes the repository as an SAP content repository.
	SAP *RepositorySAPConfig `yaml:"sap"`

	// Filled in by the loader, never read from YAML.
	Source       string `yaml:"-"` // "static" (config.yaml) or "managed" (repos.d)
	LocalRoot    string `yaml:"-"` // absolute folder of a local repository
	ClientSecret string `yaml:"-"` // content of ClientSecretFile
}

// LocalConfig configures a repository stored in a folder on the server.
type LocalConfig struct {
	// Path is relative to the data folder of the state directory.
	Path string `yaml:"path"`
	// Versions is how many previous versions are kept per file (0 to 99,
	// default 5; 0 keeps none).
	Versions *int `yaml:"versions"`
}

// RepositorySAPConfig is the SAP content repository side of a repository.
type RepositorySAPConfig struct {
	// ContRep defaults to the repository ID.
	ContRep   string `yaml:"contRep"`
	Signature string `yaml:"signature"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // json | text
}

func Default() Config {
	return Config{
		Server: ServerConfig{
			Interfaces:      []string{"rest"},
			RESTAddr:        ":8080",
			ContentRepoAddr: ":8090",
			AdminAddr:       ":9090",
			MaxInFlight:     1000,
			ReadHeaderTO:    10 * time.Second,
			IdleTimeout:     120 * time.Second,
			ShutdownTimeout: 30 * time.Second,
			DrainTimeout:    30 * time.Second,
		},
		Graph: GraphConfig{
			AuthorityURL:     "https://login.microsoftonline.com",
			BaseURL:          "https://graph.microsoft.com/v1.0",
			Scope:            "https://graph.microsoft.com/.default",
			MaxConcurrency:   64,
			MaxRetries:       5,
			MaxIdleConns:     256,
			MaxRetryBackoff:  60 * time.Second,
			MetadataCacheTTL: 60 * time.Second,
		},
		Transfer: TransferConfig{
			SimpleUploadMax: 4 << 20,
			ChunkSize:       5 * 1024 * 1024, // 16 x 320 KiB
			MemoryBudget:    512 << 20,
			MaxUpload:       2 << 30,
			SpoolDir:        os.TempDir(),
			DownloadMode:    "proxy",
		},
		Log: LogConfig{Level: "info", Format: "json"},
	}
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads the YAML file at path (optional; "" skips) and applies env overrides.
func Load(path string) (Config, error) {
	cfg, err := load(path, ReposDir)
	if err == nil || cfg.Server.StateDir == "" {
		return cfg, err
	}
	// A changed repository definition that no longer validates must not take
	// the server down: fall back to the definitions of the last state that
	// started, and say so. Only when that snapshot exists and itself loads.
	lastGood := LastGoodDir + "/" + ReposDir
	if st, serr := os.Stat(filepath.Join(cfg.Server.StateDir, filepath.FromSlash(lastGood))); serr == nil && st.IsDir() {
		if fb, ferr := load(path, lastGood); ferr == nil {
			fb.Fallback = strings.TrimSpace(apperr.Format(err))
			return fb, nil
		}
	}
	return cfg, err
}

func load(path, reposDir string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			switch {
			case errors.Is(err, fs.ErrNotExist):
				return cfg, apperr.New(apperr.CfgFileNotFound, "configuration file %q does not exist", path).Wrap(err)
			case errors.Is(err, fs.ErrPermission):
				return cfg, apperr.New(apperr.CfgFileUnreadable, "cannot read %q: permission denied", path).Wrap(err)
			}
			return cfg, apperr.New(apperr.CfgFileUnreadable, "cannot read %q: %v", path, err).Wrap(err)
		}
		cfg.emptyRefs = map[string]bool{}
		for _, m := range envRef.FindAllStringSubmatch(string(raw), -1) {
			if os.Getenv(m[1]) == "" {
				cfg.emptyRefs[m[1]] = true
			}
		}
		expanded := os.ExpandEnv(string(raw))
		if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
			return cfg, apperr.New(apperr.CfgYAMLInvalid, "%q is not valid YAML: %s", path, safelog.OneLine(safelog.RedactValues(err.Error()))).Wrap(err)
		}
	}
	applyEnv(&cfg)
	cfg.loadManaged(reposDir)
	normalize(&cfg)
	for i := range cfg.Repositories {
		cfg.repoErrs = append(cfg.repoErrs, cfg.resolveRepository(&cfg.Repositories[i])...)
	}
	return cfg, cfg.Validate()
}

func applyEnv(c *Config) {
	str := func(name string, dst *string) {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			*dst = v
		}
	}
	str("ADMIN_TOKEN", &c.Server.AdminToken)
	str("STATE_DIR", &c.Server.StateDir)
	str("GRAPH_TENANT_ID", &c.Graph.TenantID)
	str("GRAPH_CLIENT_ID", &c.Graph.ClientID)
	str("GRAPH_CLIENT_SECRET", &c.Graph.ClientSecret)
	str("GRAPH_AUTHORITY_URL", &c.Graph.AuthorityURL)
	str("GRAPH_BASE_URL", &c.Graph.BaseURL)
	str("LOG_LEVEL", &c.Log.Level)
	str("DOWNLOAD_MODE", &c.Transfer.DownloadMode)
	if v := os.Getenv("ADAPTER_INTERFACES"); v != "" {
		c.Server.Interfaces = splitList(v)
	}
	if v := os.Getenv("GRAPH_MAX_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Graph.MaxConcurrency = n
		}
	}
	if v := os.Getenv("METADATA_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Graph.MetadataCacheTTL = d
		}
	}
	if os.Getenv("AUTH_DISABLED") == "true" {
		c.Auth.Disabled = true
	}
}

func normalize(c *Config) {
	const frag = 320 * 1024
	if c.Transfer.ChunkSize < frag {
		c.Transfer.ChunkSize = frag
	}
	c.Transfer.ChunkSize -= c.Transfer.ChunkSize % frag
	if c.Transfer.MemoryBudget < c.Transfer.ChunkSize {
		c.Transfer.MemoryBudget = c.Transfer.ChunkSize
	}
	if c.Transfer.SimpleUploadMax > c.Transfer.MemoryBudget {
		c.Transfer.SimpleUploadMax = c.Transfer.MemoryBudget
	}
	if c.ContentServer.ClockSkew == 0 {
		c.ContentServer.ClockSkew = 5 * time.Minute
	}
	for i := range c.Repositories {
		r := &c.Repositories[i]
		if r.Backend == "" {
			r.Backend = BackendSharePoint
		}
		if r.Source == "" {
			r.Source = SourceStatic
		}
	}
	c.expandSAP()
	for i := range c.ContentServer.Repositories {
		r := &c.ContentServer.Repositories[i]
		if r.Folder == "" {
			r.Folder = "ContentServer/" + r.ContRep
		}
		r.Folder = strings.Trim(r.Folder, "/")
		if r.Signature == "" {
			r.Signature = "required"
		}
	}
	c.Graph.BaseURL = strings.TrimRight(c.Graph.BaseURL, "/")
	c.Graph.AuthorityURL = strings.TrimRight(c.Graph.AuthorityURL, "/")
	for i := range c.Repositories {
		c.Repositories[i].RootPath = strings.Trim(c.Repositories[i].RootPath, "/")
	}
}

// emptyNote explains which referenced environment variables are empty.
func (c Config) emptyNote(names ...string) string {
	var hit []string
	for _, n := range names {
		if c.emptyRefs[n] {
			hit = append(hit, "${"+n+"}")
		}
	}
	if len(hit) == 0 {
		return ""
	}
	return " (config.yaml references " + strings.Join(hit, ", ") + ", which is empty or not set: define it in deploy/.env and recreate the container)"
}

// ParseNetwork parses an IP address or CIDR from contentServer.allowedNetworks.
func ParseNetwork(s string) (*net.IPNet, error) {
	if !strings.Contains(s, "/") {
		if strings.Contains(s, ":") {
			s += "/128"
		} else {
			s += "/32"
		}
	}
	_, n, err := net.ParseCIDR(s)
	return n, err
}

var knownInterfaces = map[string]bool{"rest": true, "contentrepo": true}

// Validate checks the configuration. Every problem is reported at once, with
// the setting, the environment variable behind it and how to fix it.
func (c Config) Validate() error {
	var errs []error
	add := func(e *apperr.Error) { errs = append(errs, e) }

	errs = append(errs, c.repoErrs...)

	var missing []string
	for _, f := range []struct{ val, key, env string }{
		{c.Graph.TenantID, "graph.tenantId", "GRAPH_TENANT_ID"},
		{c.Graph.ClientID, "graph.clientId", "GRAPH_CLIENT_ID"},
		{c.Graph.ClientSecret, "graph.clientSecret", "GRAPH_CLIENT_SECRET"},
	} {
		if f.val == "" {
			missing = append(missing, f.key+" ("+f.env+")")
		}
	}
	if len(missing) > 0 && c.usesGlobalGraph() {
		add(apperr.New(apperr.CfgGraphCredentials, "empty: %s%s", strings.Join(missing, ", "),
			c.emptyNote("GRAPH_TENANT_ID", "GRAPH_CLIENT_ID", "GRAPH_CLIENT_SECRET")))
	}

	for _, i := range c.Server.Interfaces {
		if !knownInterfaces[strings.ToLower(i)] {
			add(apperr.New(apperr.CfgInterfaceUnknown, "server.interfaces contains %q; allowed values are rest and contentrepo", i))
		}
	}
	if len(c.Server.Interfaces) == 0 {
		add(apperr.New(apperr.CfgNoInterfaces, "server.interfaces is empty (check also the ADAPTER_INTERFACES variable)"))
	}

	// With a state directory the first repository is added through zaks-drive repo add,
	// so a server without any repository yet may start (and reports not ready).
	if len(c.Repositories) == 0 && c.Server.StateDir == "" {
		add(apperr.New(apperr.CfgNoRepositories, "the `repositories:` list is empty"))
	}
	seen := map[string]bool{}
	for i, r := range c.Repositories {
		name := r.ID
		if name == "" {
			name = fmt.Sprintf("#%d", i+1)
			add(apperr.New(apperr.CfgRepoIDMissing, "repositories entry %s has no `id`", name))
		}
		if r.ID != "" && seen[r.ID] {
			add(apperr.New(apperr.CfgRepoIDDuplicate, "repository id %q is used more than once", r.ID))
		}
		seen[r.ID] = true
		c.validateRepository(name, r, add)
	}

	if c.Server.Enabled("contentrepo") && len(c.ContentServer.Repositories) == 0 && !(c.Server.StateDir != "" && len(c.Repositories) == 0) {
		add(apperr.New(apperr.CfgCSRepositoriesMissing, "server.interfaces enables contentrepo but contentServer.repositories is empty"))
	}
	seenRep := map[string]bool{}
	for _, cr := range c.ContentServer.Repositories {
		if cr.ContRep == "" || len(cr.ContRep) > 2 {
			add(apperr.New(apperr.CfgContRepInvalid, "contentServer contRep %q must be 1 or 2 characters, the ID of the content repository in SAP transaction OAC0", cr.ContRep))
		}
		if seenRep[cr.ContRep] {
			add(apperr.New(apperr.CfgContRepDuplicate, "contRep %q is configured more than once", cr.ContRep))
		}
		seenRep[cr.ContRep] = true
		if !seen[cr.Repository] {
			add(apperr.New(apperr.CfgContRepRepository, "contRep %q uses repository %q, which is not in `repositories:` (available: %s)",
				cr.ContRep, cr.Repository, joinKeys(seen)))
		}
		switch cr.Signature {
		case "required", "optional", "none":
		default:
			add(apperr.New(apperr.CfgSignatureInvalid, "contRep %q has signature %q; use required, optional or none", cr.ContRep, cr.Signature))
		}
	}
	for _, n := range c.ContentServer.AllowedNetworks {
		if _, err := ParseNetwork(n); err != nil {
			add(apperr.New(apperr.CfgNetworkInvalid, "contentServer.allowedNetworks entry %q is not an IP address or CIDR: %v", n, err))
		}
	}

	if c.Transfer.DownloadMode != "proxy" && c.Transfer.DownloadMode != "redirect" {
		add(apperr.New(apperr.CfgDownloadMode, "transfer.downloadMode is %q; use proxy or redirect (or the DOWNLOAD_MODE variable)", c.Transfer.DownloadMode))
	}

	if c.Server.Enabled("rest") && !c.Auth.Disabled {
		if len(c.Auth.APIKeys) == 0 && c.Auth.JWT == nil {
			add(apperr.New(apperr.CfgAuthMissing, "the rest interface is enabled but auth has no apiKeys and no jwt"))
		}
		for _, k := range c.Auth.APIKeys {
			if k.Key == "" && len(k.SHA256) != 64 {
				add(apperr.New(apperr.CfgAPIKeyInvalid, "auth.apiKeys entry %q has no key and no 64-character sha256%s", k.Name,
					c.emptyNote("ADAPTER_API_KEY_S4", "ADAPTER_API_KEY_FIORI")))
			}
		}
	}
	return errors.Join(errs...)
}

// siteURLProblem explains why s cannot be a SharePoint site URL; "" if fine.
func siteURLProblem(s string) string {
	u, err := url.Parse(s)
	switch {
	case err != nil || u.Host == "":
		return "is not a URL; use https://<tenant>.sharepoint.com/sites/<site>"
	case u.Scheme != "https" && u.Scheme != "http":
		return "must start with https://"
	case strings.Contains(strings.ToLower(u.Path), "/forms/") || strings.HasSuffix(strings.ToLower(u.Path), ".aspx"):
		return "is a page address inside a library; use the site address only (https://<tenant>.sharepoint.com/sites/<site>) and put the library name into driveName / SHAREPOINT_LIBRARY"
	}
	return ""
}

func joinKeys(m map[string]bool) string {
	var out []string
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return "none"
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func (s ServerConfig) Enabled(name string) bool {
	for _, i := range s.Interfaces {
		if strings.EqualFold(i, name) {
			return true
		}
	}
	return false
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
