package storage

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

// maxSharePointPath is SharePoint's limit for the decoded path after the
// domain, including site, library and all folders.
const maxSharePointPath = 400

// sharePointBackend stores a repository in a SharePoint document library or
// OneDrive through Microsoft Graph, below the folder root ("" = drive root).
type sharePointBackend struct {
	g      *graph.Client
	eng    *transfer.Engine
	drive  string
	root   string
	webURL string
}

func newSharePointBackend(g *graph.Client, eng *transfer.Engine, drive, root, webURL string) *sharePointBackend {
	return &sharePointBackend{g: g, eng: eng, drive: drive, root: root, webURL: webURL}
}

func (b *sharePointBackend) Kind() string { return "sharepoint" }

func (b *sharePointBackend) Capabilities() Caps {
	return Caps{Versions: true, Thumbnails: true, RedirectDownload: true}
}

func (b *sharePointBackend) Ready(ctx context.Context) error { return b.g.Ping(ctx, b.drive) }

// abs converts a repository-relative path to a drive-relative one.
func (b *sharePointBackend) abs(rel string) string {
	return strings.Trim(b.root+"/"+rel, "/")
}

// rel converts a drive path ("/root/sub/x.pdf") to a repository-relative
// one; ok is false when the path is outside the repository.
func (b *sharePointBackend) rel(drivePath string) (string, bool) {
	p := strings.Trim(drivePath, "/")
	if b.root == "" {
		return p, true
	}
	if p == b.root {
		return "", true
	}
	if strings.HasPrefix(p, b.root+"/") {
		return p[len(b.root)+1:], true
	}
	return "", false
}

// CheckPath rejects a path SharePoint would refuse, with a clear message,
// before any bytes are sent.
func (b *sharePointBackend) CheckPath(rel string) error {
	prefix := 40 // typical /sites/<site>/<library> when the URL is unknown
	if u, err := url.Parse(b.webURL); err == nil && u.Path != "" {
		prefix = len(u.Path)
	}
	total := prefix + 1 + len(b.abs(rel))
	if total > maxSharePointPath {
		return apperr.New(apperr.PathTooLong, "the path would be about %d characters; SharePoint allows %d (library path %d + root folder %q + %q)",
			total, maxSharePointPath, prefix, b.root, rel).Wrap(ErrInvalid)
	}
	return nil
}

// toItem adapts a Graph item. inside is false when it lies outside the root.
func (b *sharePointBackend) toItem(d *graph.DriveItem) (it *Item, inside bool) {
	rel, inside := b.rel(d.Path())
	it = &Item{
		ID: d.ID, Name: d.Name, ETag: d.ETag, CTag: d.CTag, Size: d.Size,
		Created: d.CreatedDateTime, Modified: d.LastModifiedDateTime,
		CreatedBy: d.CreatedBy.Name(), ModifiedBy: d.LastModifiedBy.Name(),
		WebURL: d.WebURL, DirectURL: d.DownloadURL,
		Path:      rel,
		PathKnown: d.ParentReference != nil && d.ParentReference.Path != "",
	}
	if d.Folder != nil {
		it.Folder, it.ChildCount = true, d.Folder.ChildCount
	}
	if d.File != nil {
		it.File, it.MimeType = true, d.File.MimeType
		if h := d.File.Hashes; h != nil {
			it.QuickXorHash, it.SHA256Hash = h.QuickXorHash, h.SHA256Hash
		}
	}
	return it, inside
}

func (b *sharePointBackend) Stat(ctx context.Context, id string) (*Item, error) {
	d, err := b.g.GetItem(ctx, b.drive, id)
	if err != nil {
		return nil, mapErr(err)
	}
	it, inside := b.toItem(d)
	if !inside {
		return nil, ErrOutsideRoot
	}
	return it, nil
}

func (b *sharePointBackend) StatPath(ctx context.Context, rel string) (*Item, error) {
	d, err := b.g.GetItemByPath(ctx, b.drive, b.abs(rel))
	if err != nil {
		return nil, mapErr(err)
	}
	it, _ := b.toItem(d)
	return it, nil
}

// page adapts a Graph page, dropping items outside the root whose location
// is known (defence in depth when a service widens a folder-scoped search).
func (b *sharePointBackend) page(pg *graph.Page) *BackendPage {
	out := &BackendPage{Items: make([]Item, 0, len(pg.Items)), Next: pg.NextLink}
	for i := range pg.Items {
		it, inside := b.toItem(&pg.Items[i])
		if it.PathKnown && !inside {
			continue
		}
		out.Items = append(out.Items, *it)
	}
	return out
}

func (b *sharePointBackend) List(ctx context.Context, rel string, top int, token string) (*BackendPage, error) {
	pg, err := b.g.ListChildren(ctx, b.drive, b.abs(rel), top, token)
	if err != nil {
		return nil, mapErr(err)
	}
	return b.page(pg), nil
}

func (b *sharePointBackend) Search(ctx context.Context, query string, top int, token string) (*BackendPage, error) {
	pg, err := b.g.Search(ctx, b.drive, b.root, query, top, token)
	if err != nil {
		return nil, mapErr(err)
	}
	return b.page(pg), nil
}

func (b *sharePointBackend) Create(ctx context.Context, rel string, body io.Reader, size int64, conflict Conflict, ifMatch string) (*Item, error) {
	t := transfer.Target{Drive: b.drive, Path: b.abs(rel), Conflict: graph.ConflictBehavior(conflict), IfMatch: ifMatch}
	d, err := b.eng.Upload(ctx, t, body, size)
	if err != nil {
		return nil, mapErr(err)
	}
	it, _ := b.toItem(d)
	return it, nil
}

func (b *sharePointBackend) Replace(ctx context.Context, id, ifMatch string, body io.Reader, size int64) (*Item, error) {
	d, err := b.eng.Upload(ctx, transfer.Target{Drive: b.drive, ItemID: id, IfMatch: ifMatch}, body, size)
	if err != nil {
		return nil, mapErr(err)
	}
	it, _ := b.toItem(d)
	return it, nil
}

func (b *sharePointBackend) Delete(ctx context.Context, id, ifMatch string) error {
	return mapErr(b.g.Delete(ctx, b.drive, id, ifMatch))
}

func (b *sharePointBackend) CreateFolder(ctx context.Context, rel string) (*Item, error) {
	d, err := b.g.CreateFolderPath(ctx, b.drive, b.abs(rel))
	if err != nil {
		return nil, mapErr(err)
	}
	it, _ := b.toItem(d)
	return it, nil
}

func (b *sharePointBackend) EnsureFolder(ctx context.Context, rel string, parentKnown bool) error {
	p := b.abs(rel)
	if !parentKnown {
		_, err := b.g.CreateFolderPath(ctx, b.drive, p)
		return mapErr(err)
	}
	parent, name := path.Split(p)
	return mapErr(b.g.EnsureChildFolder(ctx, b.drive, strings.TrimSuffix(parent, "/"), name))
}

func (b *sharePointBackend) Versions(ctx context.Context, id string) ([]Version, error) {
	vs, err := b.g.Versions(ctx, b.drive, id)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]Version, 0, len(vs))
	for _, v := range vs {
		out = append(out, Version{ID: v.ID, Size: v.Size, Modified: v.LastModifiedDateTime, ModifiedBy: v.LastModifiedBy.Name()})
	}
	return out, nil
}

func (b *sharePointBackend) Open(ctx context.Context, it *Item, rangeHdr string, refresh func(context.Context) (*Item, error)) (*http.Response, error) {
	refreshURL := func() (string, error) {
		fresh, err := refresh(ctx)
		if err != nil {
			return "", err
		}
		return fresh.DirectURL, nil
	}
	u := it.DirectURL
	if u == "" {
		var err error
		if u, err = refreshURL(); err != nil {
			return nil, err
		}
	}
	if u == "" {
		// No pre-authenticated URL available: stream through /content.
		resp, err := b.g.OpenContent(ctx, b.drive, it.ID, rangeHdr)
		return resp, mapErr(err)
	}
	resp, err := b.eng.Open(ctx, u, rangeHdr, refreshURL)
	return resp, mapErr(err)
}

func (b *sharePointBackend) Thumbnail(ctx context.Context, id, size string) (*http.Response, error) {
	resp, err := b.g.OpenThumbnail(ctx, b.drive, id, size)
	return resp, mapErr(err)
}
