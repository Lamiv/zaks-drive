package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

func newTestLocal(t *testing.T, versions int) (*localBackend, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "AD")
	eng := transfer.New(nil, transfer.Options{SimpleUploadMax: 1 << 20, ChunkSize: 320 << 10, MemoryBudget: 8 << 20, MaxUpload: 1 << 20, SpoolDir: t.TempDir()})
	b, err := newLocalBackend(dir, versions, eng)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.root.Close() })
	return b, dir
}

func put(t *testing.T, b *localBackend, rel, body string, c Conflict) *Item {
	t.Helper()
	it, err := b.Create(context.Background(), rel, strings.NewReader(body), int64(len(body)), c, "")
	if err != nil {
		t.Fatalf("create %s: %v", rel, err)
	}
	return it
}

func read(t *testing.T, b *localBackend, it *Item, rng string) (*http.Response, string) {
	t.Helper()
	resp, err := b.Open(context.Background(), it, rng, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func code(err error) string {
	if e, ok := apperr.As(err); ok {
		return e.Code
	}
	return ""
}

func TestLocalCreateStatListOpen(t *testing.T) {
	b, _ := newTestLocal(t, 5)
	ctx := context.Background()
	it := put(t, b, "docs/2026/a.pdf", "hello", ConflictFail)
	if !it.File || it.Folder || it.Size != 5 || it.Path != "docs/2026/a.pdf" || it.Name != "a.pdf" || it.MimeType != "application/pdf" || it.ETag == "" {
		t.Fatalf("item: %+v", it)
	}
	got, err := b.Stat(ctx, it.ID)
	if err != nil || got.ID != it.ID || got.ETag != it.ETag {
		t.Fatalf("stat by id: %+v %v", got, err)
	}
	if byPath, err := b.StatPath(ctx, "docs/2026/a.pdf"); err != nil || byPath.ID != it.ID {
		t.Fatalf("stat by path: %+v %v", byPath, err)
	}
	dir, err := b.StatPath(ctx, "docs")
	if err != nil || !dir.Folder || dir.ChildCount != 1 {
		t.Fatalf("folder: %+v %v", dir, err)
	}
	if _, body := read(t, b, it, ""); body != "hello" {
		t.Fatalf("content %q", body)
	}
	resp, body := read(t, b, it, "bytes=1-3")
	if resp.StatusCode != http.StatusPartialContent || body != "ell" || resp.Header.Get("Content-Range") != "bytes 1-3/5" {
		t.Fatalf("range: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Range"))
	}
	if _, body := read(t, b, it, "bytes=-2"); body != "lo" {
		t.Fatalf("suffix range %q", body)
	}
	if _, body := read(t, b, it, "bytes=3-"); body != "lo" {
		t.Fatalf("open range %q", body)
	}
	if resp, _ := read(t, b, it, "bytes=9-"); resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unsatisfiable range answered %d", resp.StatusCode)
	}
}

func TestLocalConflictModes(t *testing.T) {
	b, _ := newTestLocal(t, 5)
	ctx := context.Background()
	first := put(t, b, "a.txt", "one", ConflictFail)
	if _, err := b.Create(ctx, "a.txt", strings.NewReader("x"), 1, ConflictFail, ""); !IsConflict(err) || code(err) != "NAME_CONFLICT" {
		t.Fatalf("fail mode on an existing name: %v", err)
	}
	renamed := put(t, b, "a.txt", "two", ConflictRename)
	if renamed.Name != "a 1.txt" || renamed.ID == first.ID {
		t.Fatalf("rename mode gave %q", renamed.Name)
	}
	replaced := put(t, b, "a.txt", "three", ConflictReplace)
	if replaced.ID != first.ID || replaced.Size != 5 {
		t.Fatalf("replace mode: %+v", replaced)
	}
	if _, body := read(t, b, replaced, ""); body != "three" {
		t.Fatalf("content after replace %q", body)
	}
	// a file in the way of a folder is a conflict, not an internal error
	if _, err := b.Create(ctx, "a.txt/inside.txt", strings.NewReader("x"), 1, ConflictFail, ""); !IsConflict(err) {
		t.Fatalf("file used as folder: %v", err)
	}
	if _, err := b.CreateFolder(ctx, "a.txt/sub"); !IsConflict(err) {
		t.Fatalf("folder below a file: %v", err)
	}
}

func TestLocalETagsChangeOnEveryWriteAndGuardWrites(t *testing.T) {
	b, _ := newTestLocal(t, 5)
	ctx := context.Background()
	it := put(t, b, "a.txt", "aaaa", ConflictFail)
	seen := map[string]bool{it.ETag: true}
	cur := it
	for i := 0; i < 20; i++ { // same size, in quick succession: the ETag must still change
		next, err := b.Replace(ctx, it.ID, cur.ETag, strings.NewReader("bbbb"), 4)
		if err != nil {
			t.Fatalf("replace %d with the current etag: %v", i, err)
		}
		if seen[next.ETag] {
			t.Fatalf("ETag %s repeated after write %d", next.ETag, i)
		}
		seen[next.ETag] = true
		cur = next
	}
	stale := it.ETag
	if _, err := b.Replace(ctx, it.ID, stale, strings.NewReader("c"), 1); !IsPrecondition(err) || code(err) != "ETAG_MISMATCH" {
		t.Fatalf("a stale ETag must be refused with 412 semantics: %v", err)
	}
	if err := b.Delete(ctx, it.ID, stale); !IsPrecondition(err) {
		t.Fatalf("delete with a stale ETag: %v", err)
	}
	if _, err := b.Create(ctx, "missing.txt", strings.NewReader("x"), 1, ConflictReplace, `"abc"`); !IsPrecondition(err) {
		t.Fatalf("conditional write of a missing file: %v", err)
	}
	if _, err := b.Replace(ctx, it.ID, `W/`+cur.ETag, strings.NewReader("ok"), 2); err != nil {
		t.Fatalf("weak form of the current ETag: %v", err)
	}
}

func TestLocalVersionsAreKeptAndPruned(t *testing.T) {
	b, dir := newTestLocal(t, 3)
	ctx := context.Background()
	it := put(t, b, "a.txt", "v0", ConflictFail)
	for i := 1; i <= 5; i++ {
		if _, err := b.Replace(ctx, it.ID, "", strings.NewReader(fmt.Sprintf("v%d", i)), 2); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := b.Versions(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 4 || vs[0].ID != "current" { // current + 3 kept
		t.Fatalf("versions: %+v", vs)
	}
	for i := 1; i < len(vs); i++ {
		if vs[i].Size != 2 || vs[i].Modified.After(vs[i-1].Modified) {
			t.Fatalf("versions are not newest first: %+v", vs)
		}
	}
	// version files are invisible to listings and cannot be addressed
	pg, err := b.List(ctx, "", 100, "")
	if err != nil || len(pg.Items) != 1 || pg.Items[0].Name != "a.txt" {
		t.Fatalf("listing shows %+v (%v)", pg.Items, err)
	}
	if _, err := b.StatPath(ctx, ".versions"); !errors.Is(err, ErrNotFound) {
		t.Fatalf(".versions must not be reachable: %v", err)
	}
	// deleting the file removes its versions
	if err := b.Delete(ctx, it.ID, ""); err != nil {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(filepath.Join(dir, versionsDir))
	if len(left) != 0 {
		t.Fatalf("version folders left behind: %v", left)
	}
}

func TestLocalZeroVersionsKeepsNothing(t *testing.T) {
	b, _ := newTestLocal(t, 0)
	ctx := context.Background()
	it := put(t, b, "a.txt", "v0", ConflictFail)
	if _, err := b.Replace(ctx, it.ID, "", strings.NewReader("v1"), 2); err != nil {
		t.Fatal(err)
	}
	vs, _ := b.Versions(ctx, it.ID)
	if len(vs) != 1 {
		t.Fatalf("versions: %+v", vs)
	}
}

func TestLocalDeleteFolderRemovesVersionsToo(t *testing.T) {
	b, dir := newTestLocal(t, 5)
	ctx := context.Background()
	it := put(t, b, "d/e/a.txt", "1", ConflictFail)
	if _, err := b.Replace(ctx, it.ID, "", strings.NewReader("2"), 1); err != nil {
		t.Fatal(err)
	}
	d, _ := b.StatPath(ctx, "d")
	if err := b.Delete(ctx, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.StatPath(ctx, "d/e/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("file survived: %v", err)
	}
	left, _ := os.ReadDir(filepath.Join(dir, versionsDir))
	if len(left) != 0 {
		t.Fatalf("version folders left behind: %v", left)
	}
	if err := b.Delete(ctx, encodeID(""), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the repository root must not be deletable: %v", err)
	}
}

func TestLocalPagingAndSearch(t *testing.T) {
	b, _ := newTestLocal(t, 5)
	ctx := context.Background()
	for _, n := range []string{"c.txt", "a.txt", "b.txt", "Report-2026.pdf"} {
		put(t, b, n, "x", ConflictFail)
	}
	put(t, b, "sub/report-old.pdf", "x", ConflictFail)
	var names []string
	token := ""
	for pages := 0; ; pages++ {
		pg, err := b.List(ctx, "", 2, token)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range pg.Items {
			names = append(names, it.Name)
		}
		if pg.Next == "" {
			break
		}
		if pages > 5 {
			t.Fatal("paging does not end")
		}
		token = pg.Next
	}
	if strings.Join(names, ",") != "Report-2026.pdf,a.txt,b.txt,c.txt,sub" {
		t.Fatalf("names %v", names)
	}
	if _, err := b.List(ctx, "", 2, "not-a-number"); code(err) != "CURSOR_INVALID" {
		t.Fatalf("bad token: %v", err)
	}
	hits, err := b.Search(ctx, "REPORT", 10, "")
	if err != nil || len(hits.Items) != 2 {
		t.Fatalf("search: %+v %v", hits, err)
	}
	if _, err := b.List(ctx, "a.txt", 10, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("listing a file: %v", err)
	}
}

func TestLocalRefusesEscapesAndReservedNames(t *testing.T) {
	b, dir := newTestLocal(t, 5)
	ctx := context.Background()
	secret := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		base64.RawURLEncoding.EncodeToString([]byte("/../outside.txt")),
		base64.RawURLEncoding.EncodeToString([]byte("/a/../../outside.txt")),
		base64.RawURLEncoding.EncodeToString([]byte("../outside.txt")),
		base64.RawURLEncoding.EncodeToString([]byte("/" + dir + "/../outside.txt")),
		"!!not base64!!", "",
	} {
		if _, err := b.Stat(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("id %q must not resolve: %v", id, err)
		}
		if err := b.Delete(ctx, id, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete of id %q: %v", id, err)
		}
	}
	for _, name := range []string{".versions", ".versions/x", "d/.zaks-tmp-1"} {
		if _, err := b.Create(ctx, name, strings.NewReader("x"), 1, ConflictFail, ""); err == nil {
			t.Errorf("creating %q must be refused", name)
		}
		if _, err := b.CreateFolder(ctx, name); err == nil {
			t.Errorf("creating folder %q must be refused", name)
		}
	}
	if b, err := os.ReadFile(secret); err != nil || string(b) != "top secret" {
		t.Fatalf("a file outside the repository was touched: %q %v", b, err)
	}
}

func TestLocalIgnoresLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	b, dir := newTestLocal(t, 5)
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	put(t, b, "real/inner.txt", "ok", ConflictFail)
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "file-link")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"escape", "escape/secret.txt", "alias", "alias/inner.txt", "file-link"} {
		if _, err := b.StatPath(ctx, rel); !errors.Is(err, ErrNotFound) {
			t.Errorf("link %q must not resolve: %v", rel, err)
		}
	}
	pg, err := b.List(ctx, "", 100, "")
	if err != nil || len(pg.Items) != 1 || pg.Items[0].Name != "real" {
		t.Fatalf("links must not be listed: %+v %v", pg, err)
	}
	if _, err := b.Create(ctx, "escape/new.txt", strings.NewReader("x"), 1, ConflictFail, ""); err == nil {
		t.Error("writing through a link must be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("a file was written outside the repository")
	}
}

func TestLocalWriteFailuresLeaveNoTrace(t *testing.T) {
	b, dir := newTestLocal(t, 5)
	ctx := context.Background()
	it := put(t, b, "a.txt", "original", ConflictFail)

	if _, err := b.Replace(ctx, it.ID, "", strings.NewReader("short"), 99); code(err) != "UPLOAD_BODY_INVALID" {
		t.Fatalf("length mismatch: %v", err)
	}
	big := bytes.NewReader(make([]byte, 2<<20))
	if _, err := b.Replace(ctx, it.ID, "", big, -1); !errors.Is(err, transfer.ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	if _, err := b.Create(ctx, "big.bin", io.LimitReader(zero{}, 5<<20), 5<<20, ConflictFail, ""); !errors.Is(err, transfer.ErrTooLarge) {
		t.Fatalf("announced size too large: %v", err)
	}
	if _, body := read(t, b, it, ""); body != "original" {
		t.Fatalf("a failed write changed the file: %q", body)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) || e.Name() == "big.bin" {
			t.Fatalf("leftover %s", e.Name())
		}
	}
}

type zero struct{}

func (zero) Read(p []byte) (int, error) { return len(p), nil }

func TestLocalUnknownSizeUpload(t *testing.T) {
	b, _ := newTestLocal(t, 5)
	it, err := b.Create(context.Background(), "s.bin", io.MultiReader(strings.NewReader("abc"), strings.NewReader("def")), -1, ConflictFail, "")
	if err != nil || it.Size != 6 {
		t.Fatalf("%+v %v", it, err)
	}
}

func TestLocalNoThumbnailsButACatalogueCode(t *testing.T) {
	b, _ := newTestLocal(t, 5)
	_, err := b.Thumbnail(context.Background(), "x", "small")
	if code(err) != "THUMBNAIL_UNAVAILABLE" || !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if c := b.Capabilities(); c.Thumbnails || c.RedirectDownload || c.MetadataCache {
		t.Fatalf("capabilities overstate the backend: %+v", c)
	}
}

func TestLocalConcurrentCreatesOfOneNameHaveOneWinner(t *testing.T) {
	b, _ := newTestLocal(t, 5)
	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := b.Create(context.Background(), "d/one.txt", strings.NewReader(fmt.Sprint(i)), -1, ConflictFail, "")
			switch {
			case err == nil:
				wins.Add(1)
			case IsConflict(err):
				conflicts.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 15 {
		t.Fatalf("%d winners, %d conflicts", wins.Load(), conflicts.Load())
	}
}

func TestLocalConcurrentConditionalReplacesHaveOneWinner(t *testing.T) {
	b, _ := newTestLocal(t, 5)
	it := put(t, b, "a.txt", "start", ConflictFail)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := b.Replace(context.Background(), it.ID, it.ETag, strings.NewReader(fmt.Sprintf("w%02d", i)), 3)
			switch {
			case err == nil:
				wins.Add(1)
			case IsPrecondition(err):
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d writers passed the same If-Match", wins.Load())
	}
}

func TestLocalNameLimits(t *testing.T) {
	b, _ := newTestLocal(t, 5)
	if err := b.CheckPath("a/" + strings.Repeat("é", 200) + ".txt"); code(err) != "NAME_TOO_LONG" {
		t.Fatalf("a name over 255 bytes: %v", err)
	}
	if err := b.CheckPath("a/" + strings.Repeat("x", 255)); err != nil {
		t.Fatalf("255 bytes are fine: %v", err)
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		h              string
		size           int64
		from, to       int64
		partial, valid bool
	}{
		{"", 10, 0, 9, false, true},
		{"bytes=0-", 10, 0, 9, true, true},
		{"bytes=2-4", 10, 2, 4, true, true},
		{"bytes=2-100", 10, 2, 9, true, true},
		{"bytes=-3", 10, 7, 9, true, true},
		{"bytes=-100", 10, 0, 9, true, true},
		{"bytes=10-", 10, 0, 0, false, false},
		{"bytes=0-1,5-6", 10, 0, 9, false, true}, // several ranges: whole file
		{"bytes=5-2", 10, 0, 9, false, true},     // malformed: whole file
		{"items=1-2", 10, 0, 9, false, true},
		{"bytes=0-", 0, 0, 0, false, false},
	}
	for _, c := range cases {
		from, to, partial, ok := parseRange(c.h, c.size)
		if ok != c.valid || (ok && (from != c.from || to != c.to || partial != c.partial)) {
			t.Errorf("parseRange(%q,%d) = %d,%d,%v,%v; want %d,%d,%v,%v", c.h, c.size, from, to, partial, ok, c.from, c.to, c.partial, c.valid)
		}
	}
}
