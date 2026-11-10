package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Lamiv/zaks-drive/internal/graph"
)

// ErrOutsideRoot is returned by a backend for an item that exists in the
// underlying store but is not inside the repository's root folder.
var ErrOutsideRoot = errors.New("item is outside the repository root")

// Conflict says what an upload does when the target name already exists.
type Conflict string

const (
	ConflictFail    Conflict = "fail"
	ConflictReplace Conflict = "replace"
	ConflictRename  Conflict = "rename"
)

// Item is a document or folder, independent of the backend that stores it.
type Item struct {
	ID         string
	Name       string
	ETag       string
	CTag       string // content tag; empty when the backend has none
	Size       int64
	Created    time.Time
	Modified   time.Time
	CreatedBy  string
	ModifiedBy string
	WebURL     string

	Folder     bool
	ChildCount int
	File       bool // a document with content (not a folder or another kind of item)

	MimeType     string
	QuickXorHash string
	SHA256Hash   string

	// Path is relative to the repository root, without a leading slash; the
	// root itself is "". PathKnown is false when the backend did not say
	// where the item lives (such items are never filtered by location).
	Path      string
	PathKnown bool

	// DirectURL is a short-lived, pre-authenticated URL the client may be
	// redirected to. Empty when the backend has none.
	DirectURL string
}

// Version is one stored revision of a file.
type Version struct {
	ID         string
	Size       int64
	Modified   time.Time
	ModifiedBy string
}

// BackendPage is one page of a listing; Next is an opaque, backend-specific
// continuation token ("" on the last page). Service wraps it in a signed cursor.
type BackendPage struct {
	Items []Item
	Next  string
}

// Caps tells callers what a backend can do, so they do not assume SharePoint.
type Caps struct {
	Versions         bool
	Thumbnails       bool
	RedirectDownload bool
	// MetadataCache says item metadata may be cached briefly; a local
	// backend reads its own disk and must not serve stale entries.
	MetadataCache bool
}

// Backend stores the documents of one repository. All paths are relative to
// the repository root and already validated by Service (CleanPath). Errors
// for a missing item wrap ErrNotFound.
type Backend interface {
	Kind() string
	Capabilities() Caps
	// Ready reports whether the store is reachable.
	Ready(ctx context.Context) error
	// CheckPath rejects a path the store cannot hold, before any bytes are sent.
	CheckPath(rel string) error

	Stat(ctx context.Context, id string) (*Item, error)
	StatPath(ctx context.Context, rel string) (*Item, error)
	List(ctx context.Context, rel string, top int, token string) (*BackendPage, error)
	Search(ctx context.Context, query string, top int, token string) (*BackendPage, error)

	// Create stores body at rel. size is -1 when unknown.
	Create(ctx context.Context, rel string, body io.Reader, size int64, conflict Conflict, ifMatch string) (*Item, error)
	Replace(ctx context.Context, id, ifMatch string, body io.Reader, size int64) (*Item, error)
	Delete(ctx context.Context, id, ifMatch string) error
	// CreateFolder creates rel and any missing parents.
	CreateFolder(ctx context.Context, rel string) (*Item, error)
	// EnsureFolder creates rel; with parentKnown only the last segment is
	// created, which keeps hot paths to one request.
	EnsureFolder(ctx context.Context, rel string, parentKnown bool) error

	Versions(ctx context.Context, id string) ([]Version, error)
	// Open starts a content read (optionally one byte range). refresh
	// returns fresh metadata when a cached direct URL has expired. The caller
	// closes the body.
	Open(ctx context.Context, it *Item, rangeHdr string, refresh func(context.Context) (*Item, error)) (*http.Response, error)
	Thumbnail(ctx context.Context, id, size string) (*http.Response, error)
}

// Neutral reasons a write can be refused. Backends return errors that wrap
// them (the SharePoint backend returns Graph errors, which IsConflict and
// IsPrecondition also recognise).
var (
	ErrConflict     = errors.New("an item with that name already exists")
	ErrPrecondition = errors.New("the item changed since it was read")
)

// IsConflict reports whether err means "name already exists".
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) || graph.IsConflict(err) }

// IsPrecondition reports whether err means "If-Match did not match".
func IsPrecondition(err error) bool {
	return errors.Is(err, ErrPrecondition) || graph.StatusOf(err) == http.StatusPreconditionFailed
}
