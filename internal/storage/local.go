package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

const (
	// versionsDir holds the previous versions of files, at the repository root.
	versionsDir = ".versions"
	// tempPrefix marks files being written; they are never listed.
	tempPrefix = ".zaks-tmp-"

	maxNameBytes = 255
	listDefault  = 200
	// searchScanLimit bounds how many entries one search looks at.
	searchScanLimit = 50000
	lockStripes     = 256
)

// localBackend stores a repository in a folder of the server's disk.
//
// Every operation goes through os.Root, so a path can never leave the
// folder, and links inside the store are refused. IDs are opaque encodings of
// the repository-relative path (there is no rename or move, so an ID cannot
// go stale). Writes go to a temporary file in the target folder, are synced
// and renamed into place. The previous contents of a replaced file are kept
// as versions below .versions/, newest first, up to `versions` of them.
type localBackend struct {
	root     *os.Root
	eng      *transfer.Engine
	versions int
	locks    [lockStripes]sync.Mutex
}

// newLocalBackend opens (creating it when missing) the folder dir. versions
// is the number of previous versions to keep per file.
func newLocalBackend(dir string, versions int, eng *transfer.Engine) (*localBackend, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	return &localBackend{root: root, eng: eng, versions: versions}, nil
}

func (b *localBackend) Kind() string { return "local" }

func (b *localBackend) Capabilities() Caps {
	return Caps{Versions: true}
}

func (b *localBackend) Ready(ctx context.Context) error {
	st, err := b.root.Stat(".")
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("the repository folder is not a directory")
	}
	return nil
}

// CheckPath rejects names the file system cannot hold or that the backend
// keeps for itself.
func (b *localBackend) CheckPath(rel string) error {
	for _, seg := range strings.Split(strings.Trim(rel, "/"), "/") {
		if len(seg) > maxNameBytes {
			return apperr.New(apperr.NameTooLong, "the name is %d bytes long; a local repository allows %d (%.40q...)", len(seg), maxNameBytes, seg).Wrap(ErrInvalid)
		}
	}
	return nil
}

// ---- identifiers, locking, path safety ----

func encodeID(rel string) string { return base64.RawURLEncoding.EncodeToString([]byte("/" + rel)) }

// decodeID returns the repository-relative path an ID stands for.
func decodeID(id string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(raw) == 0 || raw[0] != '/' {
		return "", false
	}
	rel := string(raw[1:])
	clean, err := CleanPath(rel)
	if err != nil || clean != rel {
		return "", false
	}
	return rel, true
}

func (b *localBackend) lock(rel string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(path.Dir(rel)))
	m := &b.locks[h.Sum32()%lockStripes]
	m.Lock()
	return m.Unlock
}

// reservedName reports whether rel is, or lies in, something the backend
// keeps for itself.
func reservedName(rel string) bool {
	if rel == versionsDir || strings.HasPrefix(rel, versionsDir+"/") {
		return true
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, tempPrefix) {
			return true
		}
	}
	return false
}

func notFoundLocal(rel string) error {
	return apperr.New(apperr.DocNotFound, "no file or folder %q in this repository", rel).Wrap(ErrNotFound)
}

// lstat returns the entry at rel, refusing links anywhere on the way.
func (b *localBackend) lstat(rel string) (fs.FileInfo, error) {
	if rel == "" {
		return b.root.Lstat(".")
	}
	if reservedName(rel) {
		return nil, notFoundLocal(rel)
	}
	cur := ""
	var st fs.FileInfo
	for _, seg := range strings.Split(rel, "/") {
		cur = path.Join(cur, seg)
		var err error
		if st, err = b.root.Lstat(cur); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, notFoundLocal(rel)
			}
			return nil, err
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return nil, notFoundLocal(rel)
		}
	}
	return st, nil
}

func (b *localBackend) etag(st fs.FileInfo) string {
	return fmt.Sprintf(`"%x-%x"`, st.ModTime().UnixNano(), st.Size())
}

func (b *localBackend) item(rel string, st fs.FileInfo, count bool) Item {
	it := Item{
		ID: encodeID(rel), Name: path.Base(rel), Path: rel, PathKnown: true,
		Size: st.Size(), Created: st.ModTime(), Modified: st.ModTime(), ETag: b.etag(st),
	}
	if rel == "" {
		it.Name = ""
	}
	if st.IsDir() {
		it.Folder, it.Size = true, 0
		if count {
			it.ChildCount = b.countChildren(rel)
		}
		return it
	}
	it.File, it.CTag = true, it.ETag
	it.MimeType = mediaType(mime.TypeByExtension(strings.ToLower(path.Ext(rel))))
	if it.MimeType == "" {
		it.MimeType = "application/octet-stream"
	}
	return it
}

func (b *localBackend) readDir(rel string) ([]fs.DirEntry, error) {
	name := rel
	if name == "" {
		name = "."
	}
	d, err := b.root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, notFoundLocal(rel)
		}
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, e := range entries {
		child := path.Join(rel, e.Name())
		if reservedName(child) || e.Type()&fs.ModeSymlink != 0 || (!e.IsDir() && !e.Type().IsRegular()) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (b *localBackend) countChildren(rel string) int {
	entries, err := b.readDir(rel)
	if err != nil {
		return 0
	}
	return len(entries)
}

// ---- reads ----

func (b *localBackend) Stat(ctx context.Context, id string) (*Item, error) {
	rel, ok := decodeID(id)
	if !ok {
		return nil, notFoundLocal(id)
	}
	return b.StatPath(ctx, rel)
}

func (b *localBackend) StatPath(ctx context.Context, rel string) (*Item, error) {
	st, err := b.lstat(rel)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() && !st.Mode().IsRegular() {
		return nil, notFoundLocal(rel)
	}
	it := b.item(rel, st, true)
	return &it, nil
}

func offsetToken(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(token)
	if err != nil || n < 0 {
		return 0, apperr.New(apperr.CursorInvalid, "the cursor was not issued by this server for this listing").Wrap(ErrInvalid)
	}
	return n, nil
}

func nextToken(offset, page, total int) string {
	if offset+page >= total {
		return ""
	}
	return strconv.Itoa(offset + page)
}

func (b *localBackend) List(ctx context.Context, rel string, top int, token string) (*BackendPage, error) {
	if st, err := b.lstat(rel); err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, notFoundLocal(rel)
	}
	off, err := offsetToken(token)
	if err != nil {
		return nil, err
	}
	if top <= 0 {
		top = listDefault
	}
	entries, err := b.readDir(rel)
	if err != nil {
		return nil, err
	}
	pg := &BackendPage{Items: []Item{}}
	if off >= len(entries) {
		return pg, nil
	}
	end := min(off+top, len(entries))
	for _, e := range entries[off:end] {
		info, err := e.Info()
		if err != nil {
			continue // removed meanwhile
		}
		pg.Items = append(pg.Items, b.item(path.Join(rel, e.Name()), info, true))
	}
	pg.Next = nextToken(off, top, len(entries))
	return pg, nil
}

// Search matches file and folder names (case-insensitively); there is no
// content search. It looks at most searchScanLimit entries deep.
func (b *localBackend) Search(ctx context.Context, query string, top int, token string) (*BackendPage, error) {
	off, err := offsetToken(token)
	if err != nil {
		return nil, err
	}
	if top <= 0 {
		top = listDefault
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var hits []string
	scanned := 0
	var walk func(rel string) error
	walk = func(rel string) error {
		entries, err := b.readDir(rel)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if scanned++; scanned > searchScanLimit {
				return nil
			}
			child := path.Join(rel, e.Name())
			if strings.Contains(strings.ToLower(e.Name()), q) {
				hits = append(hits, child)
			}
			if e.IsDir() {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	pg := &BackendPage{Items: []Item{}}
	if off >= len(hits) {
		return pg, nil
	}
	end := min(off+top, len(hits))
	for _, rel := range hits[off:end] {
		if st, err := b.lstat(rel); err == nil {
			pg.Items = append(pg.Items, b.item(rel, st, false))
		}
	}
	pg.Next = nextToken(off, top, len(hits))
	return pg, nil
}

// Open serves the file, or one byte range of it, as an HTTP response.
func (b *localBackend) Open(ctx context.Context, it *Item, rangeHdr string, _ func(context.Context) (*Item, error)) (*http.Response, error) {
	rel, ok := decodeID(it.ID)
	if !ok {
		return nil, notFoundLocal(it.ID)
	}
	st, err := b.lstat(rel)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, apperr.New(apperr.ItemIsFolder, "item %s is a folder and has no content", it.ID).Wrap(ErrInvalid)
	}
	f, err := b.root.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, notFoundLocal(rel)
		}
		return nil, err
	}
	size := st.Size()
	h := http.Header{}
	h.Set("Accept-Ranges", "bytes")
	h.Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
	resp := &http.Response{StatusCode: http.StatusOK, Header: h}

	from, to, partial, satisfiable := parseRange(rangeHdr, size)
	switch {
	case !satisfiable:
		f.Close()
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		h.Set("Content-Length", "0")
		resp.StatusCode, resp.ContentLength, resp.Body = http.StatusRequestedRangeNotSatisfiable, 0, http.NoBody
		return resp, nil
	case partial:
		resp.StatusCode = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, size))
	}
	n := to - from + 1
	if size == 0 {
		n = 0
	}
	h.Set("Content-Length", strconv.FormatInt(n, 10))
	resp.ContentLength = n
	resp.Body = readCloser{Reader: io.NewSectionReader(f, from, n), Closer: f}
	return resp, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// parseRange understands a single "bytes=a-b", "bytes=a-" or "bytes=-n"
// range. No header (or one it does not understand) means the whole file.
func parseRange(h string, size int64) (from, to int64, partial, satisfiable bool) {
	whole := func() (int64, int64, bool, bool) { return 0, max(size-1, 0), false, true }
	spec, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return whole()
	}
	a, z, ok := strings.Cut(spec, "-")
	if !ok {
		return whole()
	}
	switch {
	case a == "":
		n, err := strconv.ParseInt(z, 10, 64)
		if err != nil || n <= 0 {
			return whole()
		}
		if n > size {
			n = size
		}
		if size == 0 {
			return 0, 0, false, false
		}
		return size - n, size - 1, true, true
	default:
		lo, err := strconv.ParseInt(a, 10, 64)
		if err != nil || lo < 0 {
			return whole()
		}
		hi := size - 1
		if z != "" {
			v, err := strconv.ParseInt(z, 10, 64)
			if err != nil || v < lo {
				return whole()
			}
			hi = min(v, size-1)
		}
		if lo >= size {
			return 0, 0, false, false
		}
		return lo, hi, true, true
	}
}

func (b *localBackend) Thumbnail(ctx context.Context, id, size string) (*http.Response, error) {
	return nil, apperr.New(apperr.ThumbnailUnavailable, "repository storage is a local folder").Wrap(ErrNotFound)
}

// ---- writes ----

func (b *localBackend) checkETag(st fs.FileInfo, ifMatch string) error {
	if ifMatch == "" {
		return nil
	}
	norm := func(s string) string { return strings.Trim(strings.TrimPrefix(strings.TrimSpace(s), "W/"), `"`) }
	if norm(ifMatch) != norm(b.etag(st)) {
		return apperr.New(apperr.ETagMismatch, "the If-Match value is not the document's current ETag").Wrap(ErrPrecondition)
	}
	return nil
}

func nameConflict(rel string) error {
	return apperr.New(apperr.NameConflict, "a file or folder named %q already exists", path.Base(rel)).Wrap(ErrConflict)
}

// uniqueName returns rel with " 1", " 2", ... added before the extension
// until it is free (the way SharePoint renames on conflict).
func (b *localBackend) uniqueName(rel string) (string, error) {
	ext := path.Ext(rel)
	base := strings.TrimSuffix(rel, ext)
	for i := 1; i < 10000; i++ {
		cand := fmt.Sprintf("%s %d%s", base, i, ext)
		if _, err := b.root.Lstat(cand); errors.Is(err, fs.ErrNotExist) {
			return cand, nil
		}
	}
	return "", nameConflict(rel)
}

func (b *localBackend) Create(ctx context.Context, rel string, body io.Reader, size int64, conflict Conflict, ifMatch string) (*Item, error) {
	if rel == "" || reservedName(rel) {
		return nil, apperr.New(apperr.NameInvalid, "the name %q is reserved", path.Base(rel)).Wrap(ErrInvalid)
	}
	if err := b.CheckPath(rel); err != nil {
		return nil, err
	}
	unlock := b.lock(rel)
	defer unlock()

	if dir := path.Dir(rel); dir != "." {
		if err := b.checkAncestors(dir); err != nil {
			return nil, err
		}
		if err := b.root.MkdirAll(dir, 0o700); err != nil {
			return nil, nameConflictOr(err, dir)
		}
	}
	st, err := b.root.Lstat(rel)
	exists := err == nil
	if exists && (st.Mode()&fs.ModeSymlink != 0 || !st.Mode().IsRegular()) {
		return nil, nameConflict(rel)
	}
	if exists {
		switch conflict {
		case ConflictFail:
			return nil, nameConflict(rel)
		case ConflictRename:
			if rel, err = b.uniqueName(rel); err != nil {
				return nil, err
			}
			exists = false
		default:
			if err := b.checkETag(st, ifMatch); err != nil {
				return nil, err
			}
		}
	} else if ifMatch != "" {
		return nil, apperr.New(apperr.ETagMismatch, "the document no longer exists").Wrap(ErrPrecondition)
	}
	var prev fs.FileInfo
	if exists {
		prev = st
	}
	return b.write(ctx, rel, body, size, prev)
}

func (b *localBackend) Replace(ctx context.Context, id, ifMatch string, body io.Reader, size int64) (*Item, error) {
	rel, ok := decodeID(id)
	if !ok || rel == "" {
		return nil, notFoundLocal(id)
	}
	unlock := b.lock(rel)
	defer unlock()
	st, err := b.lstat(rel)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, apperr.New(apperr.ItemIsFolder, "item %s is a folder", id).Wrap(ErrInvalid)
	}
	if err := b.checkETag(st, ifMatch); err != nil {
		return nil, err
	}
	return b.write(ctx, rel, body, size, st)
}

func nameConflictOr(err error, rel string) error {
	if errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrInvalid) {
		return nameConflict(rel)
	}
	return err
}

// write stores body at rel through a temporary file. When replacing, the
// old contents become the newest version. The caller holds the lock.
func (b *localBackend) write(ctx context.Context, rel string, body io.Reader, size int64, prev fs.FileInfo) (*Item, error) {
	if max := b.eng.MaxUpload(); size > max {
		return nil, apperr.New(apperr.UploadTooLarge, "the upload is %d bytes; the limit is %d bytes (transfer.maxUploadBytes)", size, max).Wrap(transfer.ErrTooLarge)
	}
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	tmp := path.Join(path.Dir(rel), tempPrefix+hex.EncodeToString(suffix[:]))
	f, err := b.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			f.Close()
			_ = b.root.Remove(tmp)
		}
	}()

	limit := b.eng.MaxUpload()
	n, err := b.eng.Copy(ctx, f, io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, apperr.New(apperr.UploadTooLarge, "the upload is larger than the limit of %d bytes (transfer.maxUploadBytes)", limit).Wrap(transfer.ErrTooLarge)
	}
	if size >= 0 && n != size {
		return nil, apperr.New(apperr.UploadBodyInvalid, "the body had %d bytes but %d were announced", n, size)
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	// The modification time doubles as the generation of the file (it is part
	// of the ETag), so it must grow with every write even when the file
	// system's clock is coarse.
	mtime := time.Now()
	if prev != nil && !mtime.After(prev.ModTime()) {
		mtime = prev.ModTime().Add(time.Millisecond)
	}
	if err := b.root.Chtimes(tmp, mtime, mtime); err != nil {
		return nil, err
	}
	if prev != nil && b.versions > 0 {
		if err := b.archive(rel); err != nil {
			return nil, err
		}
	}
	if err := b.root.Rename(tmp, rel); err != nil {
		return nil, err
	}
	committed = true
	st, err := b.root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	it := b.item(rel, st, false)
	return &it, nil
}

// ---- versions ----

func versionDir(rel string) string {
	sum := sha256.Sum256([]byte(rel))
	return path.Join(versionsDir, hex.EncodeToString(sum[:16]))
}

// archive keeps the current file as the newest version and prunes the oldest
// ones beyond b.versions. It adds a second name for the same contents (a hard
// link), so the file stays in place until the new one atomically replaces
// it; where links are not possible it moves the file instead.
func (b *localBackend) archive(rel string) error {
	st, err := b.root.Lstat(rel)
	if err != nil {
		return nil
	}
	dir := versionDir(rel)
	if err := b.root.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	target := path.Join(dir, fmt.Sprintf("%020d-%d", st.ModTime().UnixNano(), st.Size()))
	if err := b.root.Link(rel, target); err != nil && !errors.Is(err, fs.ErrExist) {
		if err := b.root.Rename(rel, target); err != nil {
			return err
		}
	}
	old, err := b.versionNames(dir)
	if err != nil {
		return nil
	}
	for i := b.versions; i < len(old); i++ { // old is newest first
		_ = b.root.Remove(path.Join(dir, old[i]))
	}
	return nil
}

// versionNames lists a version folder, newest first.
func (b *localBackend) versionNames(dir string) ([]string, error) {
	d, err := b.root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

func (b *localBackend) Versions(ctx context.Context, id string) ([]Version, error) {
	rel, ok := decodeID(id)
	if !ok {
		return nil, notFoundLocal(id)
	}
	st, err := b.lstat(rel)
	if err != nil {
		return nil, err
	}
	out := []Version{{ID: "current", Size: st.Size(), Modified: st.ModTime()}}
	if !st.Mode().IsRegular() {
		return nil, apperr.New(apperr.ItemIsFolder, "item %s is a folder", id).Wrap(ErrInvalid)
	}
	names, err := b.versionNames(versionDir(rel))
	if err != nil {
		return out, nil // no earlier versions
	}
	for _, n := range names {
		stamp, sizeText, ok := strings.Cut(n, "-")
		nano, err1 := strconv.ParseInt(stamp, 10, 64)
		sz, err2 := strconv.ParseInt(sizeText, 10, 64)
		if !ok || err1 != nil || err2 != nil {
			continue
		}
		out = append(out, Version{ID: n, Size: sz, Modified: time.Unix(0, nano)})
	}
	return out, nil
}

// ---- deletes and folders ----

func (b *localBackend) Delete(ctx context.Context, id, ifMatch string) error {
	rel, ok := decodeID(id)
	if !ok || rel == "" {
		return notFoundLocal(id)
	}
	unlock := b.lock(rel)
	defer unlock()
	st, err := b.lstat(rel)
	if err != nil {
		return err
	}
	if err := b.checkETag(st, ifMatch); err != nil {
		return err
	}
	if st.IsDir() {
		b.dropVersionsBelow(rel)
		return b.root.RemoveAll(rel)
	}
	_ = b.root.RemoveAll(versionDir(rel))
	return b.root.Remove(rel)
}

// dropVersionsBelow removes the version folders of every file below dir.
func (b *localBackend) dropVersionsBelow(dir string) {
	entries, err := b.readDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		child := path.Join(dir, e.Name())
		if e.IsDir() {
			b.dropVersionsBelow(child)
		} else {
			_ = b.root.RemoveAll(versionDir(child))
		}
	}
}

func (b *localBackend) CreateFolder(ctx context.Context, rel string) (*Item, error) {
	if reservedName(rel) {
		return nil, apperr.New(apperr.NameInvalid, "the name %q is reserved", path.Base(rel)).Wrap(ErrInvalid)
	}
	if err := b.CheckPath(rel); err != nil {
		return nil, err
	}
	if rel != "" {
		unlock := b.lock(rel)
		defer unlock()
		if err := b.checkAncestors(rel); err != nil {
			return nil, err
		}
		if err := b.root.MkdirAll(rel, 0o700); err != nil {
			return nil, nameConflictOr(err, rel)
		}
	}
	return b.StatPath(ctx, rel)
}

func (b *localBackend) EnsureFolder(ctx context.Context, rel string, parentKnown bool) error {
	_, err := b.CreateFolder(ctx, rel)
	return err
}

// checkAncestors reports a conflict when a part of dir is an existing file
// (or a link), which would make creating dir fail.
func (b *localBackend) checkAncestors(dir string) error {
	cur := ""
	for _, seg := range strings.Split(strings.Trim(dir, "/"), "/") {
		if seg == "" || seg == "." {
			continue
		}
		cur = path.Join(cur, seg)
		st, err := b.root.Lstat(cur)
		if err != nil {
			return nil // missing from here on: MkdirAll creates it
		}
		if !st.IsDir() {
			return nameConflict(cur)
		}
	}
	return nil
}

// Close releases the folder handle.
func (b *localBackend) Close() error { return b.root.Close() }

// mediaType drops parameters such as "; charset=utf-8", so the type reads
// the way SharePoint reports it.
func mediaType(v string) string {
	if mt, _, err := mime.ParseMediaType(v); err == nil {
		return mt
	}
	return ""
}
