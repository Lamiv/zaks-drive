package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/safelog"
)

const (
	BackendSharePoint = "sharepoint"
	BackendLocal      = "local"

	SourceStatic  = "static"
	SourceManaged = "managed"

	// MaxLocalVersions is the largest number of previous versions a local
	// repository keeps per file; DefaultLocalVersions applies when unset.
	MaxLocalVersions     = 99
	DefaultLocalVersions = 5

	// Layout of the state directory.
	ReposDir   = "repos.d"
	SecretsDir = "secrets"
	// DetachedDir keeps the secret-free definition of each detached repository, so
	// it can be added again and its local folder found for a purge.
	DetachedDir = "detached"
	DataDir     = "data"
	// LastGoodDir holds a copy of the definitions of the last state that started.
	LastGoodDir = "last-good"

	maxRepoFile   = 64 << 10
	maxSecretFile = 16 << 10
)

// managedID is the shape of a repository ID created through the management
// tools: exactly two characters, like an SAP content repository ID.
var managedID = regexp.MustCompile(`^[A-Z0-9]{2}$`)

// ValidManagedID reports whether id may name a managed repository.
func ValidManagedID(id string) bool { return managedID.MatchString(id) }

// Versions returns the number of previous versions a local repository keeps.
func (l LocalConfig) KeepVersions() int {
	if l.Versions == nil {
		return DefaultLocalVersions
	}
	return *l.Versions
}

// loadManaged reads <stateDir>/repos.d/*.yaml and appends the repositories
// to c.Repositories. Problems are collected in c.repoErrs so Validate can
// report them together with everything else. The files hold no secrets:
// those are separate files below secrets/, read here.
func (c *Config) loadManaged(reposDir string) {
	if c.Server.StateDir == "" {
		return
	}
	root, err := os.OpenRoot(c.Server.StateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return // nothing created yet
		}
		c.repoErrs = append(c.repoErrs, apperr.New(apperr.CfgRepoFileInvalid, "cannot open the state directory %q: %v", c.Server.StateDir, err).Wrap(err))
		return
	}
	defer root.Close()

	dir, err := root.Open(reposDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.repoErrs = append(c.repoErrs, apperr.New(apperr.CfgRepoFileInvalid, "cannot read %s in the state directory: %v", reposDir, err).Wrap(err))
		}
		return
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		c.repoErrs = append(c.repoErrs, apperr.New(apperr.CfgRepoFileInvalid, "cannot list %s: %v", reposDir, err).Wrap(err))
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		bad := func(format string, args ...any) {
			c.repoErrs = append(c.repoErrs, apperr.New(apperr.CfgRepoFileInvalid, "%s/%s: "+format, append([]any{reposDir, name}, args...)...))
		}
		raw, err := readLimited(root, path.Join(reposDir, name), maxRepoFile)
		if err != nil {
			bad("%v", err)
			continue
		}
		var r RepositoryConfig
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&r); err != nil {
			bad("not valid: %s", safelog.OneLine(safelog.RedactValues(err.Error())))
			continue
		}
		if r.ID+".yaml" != name {
			bad("the file must be named %s.yaml after its repository id %q", r.ID, r.ID)
			continue
		}
		r.Source = SourceManaged
		if !managedID.MatchString(r.ID) {
			c.repoErrs = append(c.repoErrs, apperr.New(apperr.CfgRepoIDInvalid, "repository id %q in %s/%s must be exactly 2 characters, A-Z or 0-9", r.ID, ReposDir, name))
			continue
		}
		c.Repositories = append(c.Repositories, r)
	}
}

// resolveRepository fills the loader-owned fields of r (local folder, secret
// content) and reports problems.
func (c *Config) resolveRepository(r *RepositoryConfig) []error {
	var errs []error
	if r.Backend == BackendLocal && c.Server.StateDir != "" && r.Local.Path != "" && cleanRelative(r.Local.Path) != "" {
		r.LocalRoot = filepath.Join(c.Server.StateDir, DataDir, filepath.FromSlash(cleanRelative(r.Local.Path)))
	}
	if r.ClientSecretFile == "" {
		return nil
	}
	rel := cleanRelative(r.ClientSecretFile)
	if c.Server.StateDir == "" || rel == "" || !strings.HasPrefix(rel, SecretsDir+"/") {
		return []error{apperr.New(apperr.CfgRepoSecretUnreadable, "repository %q: clientSecretFile %q must be a path below %s/ in the state directory (server.stateDir)", r.ID, r.ClientSecretFile, SecretsDir)}
	}
	root, err := os.OpenRoot(c.Server.StateDir)
	if err != nil {
		return []error{apperr.New(apperr.CfgRepoSecretUnreadable, "repository %q: cannot open the state directory: %v", r.ID, err).Wrap(err)}
	}
	defer root.Close()
	if runtime.GOOS != "windows" {
		if st, err := root.Stat(rel); err == nil && st.Mode().Perm()&0o077 != 0 {
			return []error{apperr.New(apperr.CfgRepoSecretUnreadable, "repository %q: %s is accessible to other users (mode %o); it must be 600", r.ID, rel, st.Mode().Perm())}
		}
	}
	raw, err := readLimited(root, rel, maxSecretFile)
	if err != nil {
		return []error{apperr.New(apperr.CfgRepoSecretUnreadable, "repository %q: %s: %v", r.ID, rel, err)}
	}
	r.ClientSecret = strings.TrimSpace(string(raw))
	if r.ClientSecret == "" {
		errs = append(errs, apperr.New(apperr.CfgRepoSecretUnreadable, "repository %q: %s is empty", r.ID, rel))
	}
	return errs
}

func readLimited(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("does not exist")
		}
		return nil, errors.New("cannot be opened (outside the state directory, a link, or no permission)")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, errors.New("cannot be read")
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("is larger than %d bytes", limit)
	}
	return raw, nil
}

// cleanRelative returns p as a clean slash-separated relative path, or ""
// when p is empty, absolute, or leaves its base through "..".
func cleanRelative(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" || strings.HasPrefix(p, "/") || filepath.VolumeName(p) != "" {
		return ""
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return ""
	}
	return c
}

// expandSAP turns the `sap` block of each repository into an entry of
// contentServer.repositories, so the rest of the server sees one list.
func (c *Config) expandSAP() {
	for _, r := range c.Repositories {
		if r.SAP == nil {
			continue
		}
		contRep := r.SAP.ContRep
		if contRep == "" {
			contRep = r.ID
		}
		c.ContentServer.Repositories = append(c.ContentServer.Repositories, ContentRepositoryConfig{
			ContRep:     contRep,
			Description: r.Description,
			Repository:  r.ID,
			Signature:   r.SAP.Signature,
		})
	}
}

// validateRepository checks one repository's backend-specific settings.
func (c Config) validateRepository(name string, r RepositoryConfig, add func(*apperr.Error)) {
	if r.Backend == "" {
		r.Backend = BackendSharePoint
	}
	switch r.Backend {
	case BackendSharePoint:
		if r.DriveID == "" && r.UserID == "" && r.SiteID == "" && r.SiteURL == "" {
			add(apperr.New(apperr.CfgRepoLocationMissing,
				"repository %q has no SharePoint location: siteUrl, userId, siteId and driveId are all empty%s", name,
				c.emptyNote("SHAREPOINT_SITE_URL", "SHAREPOINT_USER_ID", "SP_DRIVE_ID")))
		}
		if r.SiteURL != "" {
			if why := siteURLProblem(r.SiteURL); why != "" {
				add(apperr.New(apperr.CfgSiteURLInvalid, "repository %q: siteUrl %q %s", name, r.SiteURL, why))
			}
		}
		own := 0
		for _, v := range []string{r.TenantID, r.ClientID, r.ClientSecretFile} {
			if v != "" {
				own++
			}
		}
		if own != 0 && own != 3 {
			add(apperr.New(apperr.CfgRepoCredentialsIncomplete, "repository %q sets only some of tenantId, clientId and clientSecretFile; set all three or none", name))
		}
	case BackendLocal:
		if c.Server.StateDir == "" {
			add(apperr.New(apperr.CfgRepoBackendInvalid, "repository %q uses the local backend, which needs server.stateDir (STATE_DIR)", name))
		}
		if cleanRelative(r.Local.Path) == "" {
			add(apperr.New(apperr.CfgRepoLocalInvalid, "repository %q: local.path %q must be a relative folder below the data folder, without \"..\"", name, r.Local.Path))
		}
		if v := r.Local.Versions; v != nil && (*v < 0 || *v > MaxLocalVersions) {
			add(apperr.New(apperr.CfgRepoLocalInvalid, "repository %q: local.versions is %d; use 0 to %d", name, *v, MaxLocalVersions))
		}
		if r.DriveID != "" || r.UserID != "" || r.SiteID != "" || r.SiteURL != "" || r.DriveName != "" ||
			r.TenantID != "" || r.ClientID != "" || r.ClientSecretFile != "" {
			add(apperr.New(apperr.CfgRepoCredentialsIncomplete, "repository %q uses the local backend, so SharePoint location and credential settings do not apply", name))
		}
	default:
		add(apperr.New(apperr.CfgRepoBackendInvalid, "repository %q has backend %q; use sharepoint or local", name, r.Backend))
	}
}

// usesGlobalGraph reports whether any repository needs the global graph.*
// credentials.
func (c Config) usesGlobalGraph() bool {
	if len(c.Repositories) == 0 {
		return c.Server.StateDir == "" // a server waiting for its first repository needs no Microsoft credentials yet
	}
	for _, r := range c.Repositories {
		if r.Backend != BackendLocal && r.ClientSecretFile == "" {
			return true
		}
	}
	return false
}
