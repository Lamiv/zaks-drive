// Package storage is the interface-neutral document service shared by the
// Document REST API and the SAP Content Repository interface. It maps
// logical repositories to SharePoint libraries/folders, enforces that every
// item accessed lies inside its repository root, and caches item metadata.
package storage

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/safelog"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

var (
	ErrNotFound = errors.New("not found")
	ErrReadOnly = errors.New("repository is read-only")
	ErrInvalid  = errors.New("invalid request")
	// ErrReserved: the path belongs to another interface (the SAP content
	// server) and is not reachable through this repository handle.
	ErrReserved = errors.New("path is reserved for the SAP content server")
)

type Repository struct {
	ID       string
	DriveID  string
	RootPath string // drive-relative, no leading/trailing slash; "" = drive root
	ReadOnly bool
	// WebURL is the browser URL of the library, for logs and diagnostics.
	WebURL  string
	backend Backend
	// reserved holds repository-relative folders owned by the SAP content
	// server. They are invisible to, and immutable through, this handle.
	reserved []string
}

// Kind names the backend that stores the repository ("sharepoint").
func (r *Repository) Kind() string { return r.backend.Kind() }

// Capabilities says what the repository's backend supports.
func (r *Repository) Capabilities() Caps { return r.backend.Capabilities() }

// Unrestricted returns a handle on the same library without reserved-path
// restrictions, for the interface that owns those paths.
func (r *Repository) Unrestricted() *Repository {
	cp := *r
	cp.reserved = nil
	return &cp
}

// hidden reports whether rel is a reserved folder or lies inside one.
func (r *Repository) hidden(rel string) bool {
	for _, p := range r.reserved {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// containsReserved reports whether deleting folder rel would also delete a
// reserved folder.
func (r *Repository) containsReserved(rel string) bool {
	for _, p := range r.reserved {
		if rel == "" || strings.HasPrefix(p, rel+"/") {
			return true
		}
	}
	return false
}

// visible reports whether an item the backend returned may be exposed. The
// backend already confined it to the repository root; this hides the
// reserved SAP folders.
func (r *Repository) visible(it *Item) bool {
	return !it.PathKnown || !r.hidden(it.Path)
}

// abs converts a repository-relative path to a drive-relative path.
func (r *Repository) abs(rel string) (string, error) {
	clean, err := CleanPath(rel)
	if err != nil {
		return "", err
	}
	return strings.Trim(r.RootPath+"/"+clean, "/"), nil
}

// CleanPath validates a relative path: no "..", no empty or dot segments,
// no characters SharePoint rejects.
func CleanPath(p string) (string, error) {
	p = strings.Trim(strings.ReplaceAll(p, "\\", "/"), "/")
	if p == "" {
		return "", nil
	}
	for _, seg := range strings.Split(p, "/") {
		if err := ValidName(seg); err != nil {
			return "", err
		}
	}
	return path.Clean(p), nil
}

func ValidName(name string) error {
	const bad = "\"*:<>?/\\|\x00"
	switch {
	case name == "" || name == "." || name == "..":
		return apperr.New(apperr.NameInvalid, "the name %q is not allowed (empty, \".\" and \"..\" are reserved)", name).Wrap(ErrInvalid)
	case len(name) > 255:
		return apperr.New(apperr.NameTooLong, "the name is %d characters long; the limit is 255 (%.40q...)", len(name), name).Wrap(ErrInvalid)
	case strings.ContainsAny(name, bad):
		var found []string
		for _, r := range name {
			if strings.ContainsRune(bad, r) && !strings.Contains(strings.Join(found, ""), string(r)) {
				found = append(found, string(r))
			}
		}
		return apperr.New(apperr.NameInvalid, "the name %q contains characters SharePoint does not allow: %s", name, strings.Join(found, " ")).Wrap(ErrInvalid)
	case strings.HasSuffix(name, "."):
		return apperr.New(apperr.NameInvalid, "the name %q ends with a dot, which SharePoint does not allow", name).Wrap(ErrInvalid)
	case strings.HasPrefix(name, " ") || strings.HasSuffix(name, " "):
		return apperr.New(apperr.NameInvalid, "the name %q starts or ends with a space, which SharePoint does not allow", name).Wrap(ErrInvalid)
	}
	return nil
}

// notFound builds the coded "not found" error.
func notFound(format string, args ...any) error {
	return apperr.New(apperr.DocNotFound, format, args...).Wrap(ErrNotFound)
}

func readOnly(r *Repository) error {
	return apperr.New(apperr.RepoReadOnly, "repository %q is read-only (readOnly: true in config.yaml)", r.ID).Wrap(ErrReadOnly)
}

func reserved(r *Repository, rel string) error {
	return apperr.New(apperr.PathReserved, "%q is inside the SAP content server folder of repository %q", rel, r.ID).Wrap(ErrReserved)
}

type Service struct {
	eng       *transfer.Engine
	repos     map[string]*Repository
	order     []string
	cache     *itemCache
	log       *slog.Logger
	cursorKey []byte
}

func NewService(ctx context.Context, g *graph.Client, eng *transfer.Engine, cfgs []config.RepositoryConfig, cacheTTL time.Duration, log *slog.Logger) (*Service, error) {
	s := &Service{eng: eng, repos: map[string]*Repository{}, cache: newItemCache(cacheTTL, 20000), log: log}
	s.cursorKey = make([]byte, 32)
	_, _ = rand.Read(s.cursorKey)
	for _, rc := range cfgs {
		drive := rc.DriveID
		if drive == "" {
			var err error
			if drive, err = g.ResolveDrive(ctx, rc.UserID, rc.SiteID, rc.SiteURL, rc.DriveName); err != nil {
				return nil, fmt.Errorf("repository %s: resolve drive: %w", rc.ID, err)
			}
			log.Info("resolved repository drive", "repository", rc.ID, "drive_id", drive)
		}
		repo := &Repository{ID: rc.ID, DriveID: drive, RootPath: rc.RootPath, ReadOnly: rc.ReadOnly}
		if _, web, err := g.DriveInfo(ctx, drive); err == nil {
			repo.WebURL = web
		}
		repo.backend = newSharePointBackend(g, eng, drive, rc.RootPath, repo.WebURL)
		log.Info("repository ready: documents are stored here", "repository", rc.ID, "library_url", repo.WebURL, "root_path", rc.RootPath)
		s.repos[rc.ID] = repo
		s.order = append(s.order, rc.ID)
	}
	return s, nil
}

func (s *Service) Engine() *transfer.Engine { return s.eng }

// SetCursorKey sets the key that authenticates paging cursors. Instances
// behind one load balancer must share it; the default is random per process.
func (s *Service) SetCursorKey(key []byte) { s.cursorKey = key }

// ReservePath hides folder rel of repository id from the shared handle
// (used by the REST API). Call before serving requests.
func (s *Service) ReservePath(id, rel string) error {
	r, err := s.Repository(id)
	if err != nil {
		return err
	}
	clean, err := CleanPath(rel)
	if err != nil {
		return err
	}
	if clean == "" {
		return fmt.Errorf("%w: cannot reserve the repository root", ErrInvalid)
	}
	r.reserved = append(r.reserved, clean)
	return nil
}

func (s *Service) Repository(id string) (*Repository, error) {
	if r, ok := s.repos[id]; ok {
		return r, nil
	}
	return nil, apperr.New(apperr.RepoNotFound, "repository %q does not exist or this credential may not use it", id).Wrap(ErrNotFound)
}

func (s *Service) Repositories() []*Repository {
	out := make([]*Repository, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.repos[id])
	}
	return out
}

// mapErr converts Graph "not found" into ErrNotFound for callers.
func mapErr(err error) error {
	if graph.IsNotFound(err) {
		return apperr.New(apperr.DocNotFound, "SharePoint reports that the item or folder does not exist (%v)", err).Wrap(ErrNotFound)
	}
	return err
}

// Get returns item metadata (including a fresh-enough download URL) for an
// item inside repo. Results are cached briefly.
func (s *Service) Get(ctx context.Context, repo *Repository, id string) (*Item, error) {
	if it := s.cache.get(repo.DriveID, id); it != nil {
		if repo.visible(it) {
			return it, nil
		}
		return nil, notFound("item %s is not inside repository %q", id, repo.ID)
	}
	it, err := repo.backend.Stat(ctx, id)
	if errors.Is(err, ErrOutsideRoot) {
		return nil, notFound("item %s exists in the library but is not inside repository %q (root folder %q)", id, repo.ID, repo.RootPath)
	}
	if err != nil {
		return nil, err
	}
	if !repo.visible(it) {
		return nil, notFound("item %s exists in the library but is not inside repository %q (root folder %q)", id, repo.ID, repo.RootPath)
	}
	s.cache.put(repo.DriveID, it)
	return it, nil
}

// Refresh bypasses the cache.
func (s *Service) Refresh(ctx context.Context, repo *Repository, id string) (*Item, error) {
	s.cache.drop(repo.DriveID, id)
	return s.Get(ctx, repo, id)
}

func (s *Service) GetByPath(ctx context.Context, repo *Repository, rel string) (*Item, error) {
	clean, err := CleanPath(rel)
	if err != nil {
		return nil, err
	}
	if repo.hidden(clean) {
		return nil, notFound("%q does not exist in repository %q", rel, repo.ID)
	}
	it, err := repo.backend.StatPath(ctx, clean)
	if err != nil {
		return nil, err
	}
	s.cache.put(repo.DriveID, it)
	return it, nil
}

type Page struct {
	Items  []Item
	Cursor string
}

// Cursors wrap the backend's continuation token. They are authenticated with
// an HMAC bound to the repository, drive and listing scope (folder or
// query), so a caller cannot substitute a link to another drive, site or
// folder.

func (s *Service) cursorMAC(repo *Repository, scope, link string) []byte {
	m := hmac.New(sha256.New, s.cursorKey)
	for _, part := range []string{repo.ID, repo.DriveID, scope, link} {
		m.Write([]byte(part))
		m.Write([]byte{0})
	}
	return m.Sum(nil)[:16]
}

func (s *Service) encodeCursor(repo *Repository, scope, link string) string {
	if link == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(link)) + "." + base64.RawURLEncoding.EncodeToString(s.cursorMAC(repo, scope, link))
}

func (s *Service) decodeCursor(repo *Repository, scope, c string) (string, error) {
	if c == "" {
		return "", nil
	}
	bad := apperr.New(apperr.CursorInvalid, "the cursor was not issued by this server for this listing").Wrap(ErrInvalid)
	linkPart, macPart, ok := strings.Cut(c, ".")
	if !ok {
		return "", bad
	}
	link, err1 := base64.RawURLEncoding.DecodeString(linkPart)
	mac, err2 := base64.RawURLEncoding.DecodeString(macPart)
	if err1 != nil || err2 != nil || !hmac.Equal(mac, s.cursorMAC(repo, scope, string(link))) {
		return "", bad
	}
	return string(link), nil
}

// visibleItems keeps only items outside reserved folders (defence in depth
// for listings and search results).
func visibleItems(repo *Repository, items []Item) []Item {
	out := items[:0]
	for _, it := range items {
		if !repo.visible(&it) {
			continue
		}
		out = append(out, it)
	}
	return out
}

func (s *Service) List(ctx context.Context, repo *Repository, rel string, top int, cursor string) (*Page, error) {
	clean, err := CleanPath(rel)
	if err != nil {
		return nil, err
	}
	if repo.hidden(clean) {
		return nil, notFound("no such item in repository %q", repo.ID)
	}
	p, _ := repo.abs(rel)
	scope := "list:" + p
	next, err := s.decodeCursor(repo, scope, cursor)
	if err != nil {
		return nil, err
	}
	pg, err := repo.backend.List(ctx, clean, top, next)
	if err != nil {
		return nil, err
	}
	return &Page{Items: visibleItems(repo, pg.Items), Cursor: s.encodeCursor(repo, scope, pg.Next)}, nil
}

func (s *Service) Search(ctx context.Context, repo *Repository, query string, top int, cursor string) (*Page, error) {
	if strings.TrimSpace(query) == "" {
		return nil, apperr.New(apperr.QueryEmpty, "the search text `q` is empty").Wrap(ErrInvalid)
	}
	scope := "search:" + query
	next, err := s.decodeCursor(repo, scope, cursor)
	if err != nil {
		return nil, err
	}
	pg, err := repo.backend.Search(ctx, query, top, next)
	if err != nil {
		return nil, err
	}
	return &Page{Items: visibleItems(repo, pg.Items), Cursor: s.encodeCursor(repo, scope, pg.Next)}, nil
}

// UploadRequest creates a new document in folder (repository-relative).
type UploadRequest struct {
	Folder   string
	Name     string
	Conflict Conflict
	Body     io.Reader
	Size     int64 // -1 if unknown
	// IfMatch makes the write conditional on the current eTag of an
	// existing file (412 otherwise). Small bodies only.
	IfMatch string
}

func (s *Service) Upload(ctx context.Context, repo *Repository, req UploadRequest) (*Item, error) {
	if repo.ReadOnly {
		return nil, readOnly(repo)
	}
	if err := ValidName(req.Name); err != nil {
		return nil, err
	}
	folder, err := CleanPath(req.Folder)
	if err != nil {
		return nil, err
	}
	rel := strings.TrimLeft(folder+"/"+req.Name, "/")
	if repo.hidden(rel) {
		return nil, reserved(repo, rel)
	}
	if err := repo.backend.CheckPath(strings.Trim(req.Folder, "/") + "/" + req.Name); err != nil {
		return nil, err
	}
	if req.Conflict == "" {
		req.Conflict = ConflictRename
	}
	it, err := repo.backend.Create(ctx, rel, req.Body, req.Size, req.Conflict, req.IfMatch)
	if err != nil {
		return nil, err
	}
	s.cache.drop(repo.DriveID, it.ID)
	return it, nil
}

// Replace uploads new content for an existing item (a new version).
func (s *Service) Replace(ctx context.Context, repo *Repository, id, ifMatch string, body io.Reader, size int64) (*Item, error) {
	if repo.ReadOnly {
		return nil, readOnly(repo)
	}
	cur, err := s.Get(ctx, repo, id)
	if err != nil {
		return nil, err
	}
	if cur.Folder {
		return nil, apperr.New(apperr.ItemIsFolder, "item %s is a folder", id).Wrap(ErrInvalid)
	}
	s.cache.drop(repo.DriveID, id)
	return repo.backend.Replace(ctx, id, ifMatch, body, size)
}

func (s *Service) Delete(ctx context.Context, repo *Repository, id, ifMatch string) error {
	if repo.ReadOnly {
		return readOnly(repo)
	}
	it, err := s.Get(ctx, repo, id)
	if err != nil {
		return err
	}
	if it.Folder && repo.containsReserved(it.Path) {
		return reserved(repo, it.Path)
	}
	s.cache.drop(repo.DriveID, id)
	return repo.backend.Delete(ctx, id, ifMatch)
}

func (s *Service) CreateFolder(ctx context.Context, repo *Repository, rel string) (*Item, error) {
	if repo.ReadOnly {
		return nil, readOnly(repo)
	}
	p, err := repo.abs(rel)
	if err != nil {
		return nil, err
	}
	if p == "" {
		return nil, apperr.New(apperr.FolderPathRequired, "the folder path is empty").Wrap(ErrInvalid)
	}
	clean, _ := CleanPath(rel)
	if repo.hidden(clean) {
		return nil, reserved(repo, rel)
	}
	return repo.backend.CreateFolder(ctx, clean)
}

// EnsureFolder makes sure folder rel exists, creating parents as needed.
// Only the last segment is created with a single call when the parent is
// known to exist (parentKnown), which keeps hot paths to one request.
func (s *Service) EnsureFolder(ctx context.Context, repo *Repository, rel string, parentKnown bool) error {
	if repo.ReadOnly {
		return readOnly(repo)
	}
	clean, err := CleanPath(rel)
	if err != nil {
		return err
	}
	return repo.backend.EnsureFolder(ctx, clean, parentKnown)
}

func (s *Service) Versions(ctx context.Context, repo *Repository, id string) ([]Version, error) {
	if _, err := s.Get(ctx, repo, id); err != nil {
		return nil, err
	}
	return repo.backend.Versions(ctx, id)
}

// Download streams content to w. The caller sets descriptive headers first.
func (s *Service) Download(ctx context.Context, w http.ResponseWriter, repo *Repository, it *Item, rangeHdr string) (transfer.DownloadResult, error) {
	resp, err := s.Open(ctx, repo, it, rangeHdr)
	if err != nil {
		return transfer.DownloadResult{}, err
	}
	res, err := s.eng.StreamOpened(ctx, w, resp)
	return res, mapErr(err)
}

// Open returns the raw content response (optionally a byte range) for it,
// refreshing an expired download URL once. The caller closes the body.
func (s *Service) Open(ctx context.Context, repo *Repository, it *Item, rangeHdr string) (*http.Response, error) {
	return repo.backend.Open(ctx, it, rangeHdr, func(ctx context.Context) (*Item, error) {
		return s.Refresh(ctx, repo, it.ID)
	})
}

// ListAll returns every child of a repository-relative folder.
func (s *Service) ListAll(ctx context.Context, repo *Repository, rel string) ([]Item, error) {
	var all []Item
	cursor := ""
	for {
		pg, err := s.List(ctx, repo, rel, 1000, cursor)
		if err != nil {
			return nil, err
		}
		all = append(all, pg.Items...)
		if pg.Cursor == "" {
			return all, nil
		}
		cursor = pg.Cursor
	}
}

func (s *Service) Thumbnail(ctx context.Context, repo *Repository, id, size string) (*http.Response, error) {
	if _, err := s.Get(ctx, repo, id); err != nil {
		return nil, err
	}
	return repo.backend.Thumbnail(ctx, id, size)
}

// Ping checks connectivity of the first repository's store.
func (s *Service) Ping(ctx context.Context) error {
	for _, id := range s.order {
		return s.repos[id].backend.Ready(ctx)
	}
	return nil
}

// ---- small TTL cache for item metadata ----

type cacheEntry struct {
	item    *Item
	expires time.Time
}

type itemCache struct {
	ttl time.Duration
	max int
	mu  sync.Mutex
	m   map[string]cacheEntry
}

func newItemCache(ttl time.Duration, max int) *itemCache {
	return &itemCache{ttl: ttl, max: max, m: map[string]cacheEntry{}}
}

func (c *itemCache) get(drive, id string) *Item {
	if c.ttl <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[drive+"|"+id]
	if !ok {
		return nil
	}
	if time.Now().After(e.expires) {
		delete(c.m, drive+"|"+id)
		return nil
	}
	return e.item
}

func (c *itemCache) put(drive string, it *Item) {
	if c.ttl <= 0 || it == nil || it.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		// Evict arbitrary entries (map order is random) — cheap and adequate
		// for a short-TTL cache.
		n := c.max / 10
		for k := range c.m {
			delete(c.m, k)
			if n--; n <= 0 {
				break
			}
		}
	}
	c.m[drive+"|"+it.ID] = cacheEntry{item: it, expires: time.Now().Add(c.ttl)}
}

func (c *itemCache) drop(drive, id string) {
	c.mu.Lock()
	delete(c.m, drive+"|"+id)
	c.mu.Unlock()
}

func init() { safelog.Allow(ErrNotFound, ErrReadOnly, ErrInvalid, ErrReserved) }
