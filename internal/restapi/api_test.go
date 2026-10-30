package restapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lamiv/zaks-drive/internal/auth"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/graphmock"
	"github.com/Lamiv/zaks-drive/internal/observability"
	"github.com/Lamiv/zaks-drive/internal/restapi"
	"github.com/Lamiv/zaks-drive/internal/storage"
	"github.com/Lamiv/zaks-drive/internal/storage/storagetest"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

type env struct {
	url    string
	seed   graphmock.Seeder
	client *http.Client
}

func setup(t *testing.T, mode string) *env { return setupWith(t, mode, false) }

// setupWith can withhold the download URL, as real Graph does when it is
// mixed into a $select list, to exercise the /content redirect fallback.
func setupWith(t *testing.T, mode string, omitDownloadURL bool) *env {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mh, err := graphmock.New(graphmock.Options{DataDir: t.TempDir(), Drives: []string{"drive1"}, OmitDownloadURL: omitDownloadURL})
	if err != nil {
		t.Fatal(err)
	}
	mock := httptest.NewServer(mh)
	t.Cleanup(mock.Close)

	tokens := auth.NewClientCredentials(mock.URL, "tenant", "client", "secret", "https://graph.microsoft.com/.default", mock.Client())
	gc := graph.New(graph.Options{BaseURL: mock.URL + "/v1.0", MaxConcurrency: 16, MaxRetries: 3, MaxRetryBackoff: 200 * time.Millisecond}, tokens, http.DefaultTransport, log)
	eng := transfer.New(gc, transfer.Options{SimpleUploadMax: 1 << 20, ChunkSize: 320 << 10, MemoryBudget: 8 << 20, MaxUpload: 50 << 20, SpoolDir: t.TempDir()})
	repoCfgs := []config.RepositoryConfig{
		{ID: "DMS", DriveID: "drive1", RootPath: "SAP/DMS"},
		{ID: "RO", DriveID: "drive1", RootPath: "SAP/RO", ReadOnly: true},
	}
	var seeder graphmock.Seeder = mh.(graphmock.Seeder)
	dmsDir, roDir := t.TempDir(), t.TempDir()
	if storagetest.Local() { // the same suite over the local backend
		repoCfgs = []config.RepositoryConfig{
			{ID: "DMS", Backend: config.BackendLocal, LocalRoot: dmsDir},
			{ID: "RO", Backend: config.BackendLocal, LocalRoot: roDir, ReadOnly: true},
		}
	}
	svc, err := storage.NewService(context.Background(), gc, eng, repoCfgs, time.Minute, log)
	if err != nil {
		t.Fatal(err)
	}
	if storagetest.Local() {
		seeder = storagetest.Seeder{Svc: svc, Repos: map[string]storagetest.Repo{"SAP/DMS": {ID: "DMS", Dir: dmsDir}, "SAP/RO": {ID: "RO", Dir: roDir}}}
	}
	t.Cleanup(svc.Close)
	// As in production: the SAP content server folder is reserved.
	if err := svc.ReservePath("DMS", "ContentServer/Z1"); err != nil {
		t.Fatal(err)
	}
	authn, err := auth.NewAuthenticator(config.AuthConfig{APIKeys: []config.APIKeyConfig{
		{Name: "rw", Key: "rw-key", Permissions: []string{"read", "write", "delete"}},
		{Name: "ro", Key: "ro-key", Permissions: []string{"read"}},
		{Name: "other", Key: "other-key", Repositories: []string{"RO"}, Permissions: []string{"read"}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := restapi.New(svc, authn, restapi.Options{DownloadMode: mode, MaxUpload: 50 << 20}, log)
	srv := httptest.NewServer(observability.Middleware("rest", 100, log, api.Handler()))
	t.Cleanup(srv.Close)
	return &env{
		url:  srv.URL + "/api/v1",
		seed: seeder,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

func (e *env) do(t *testing.T, method, path, key string, body io.Reader, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, e.url+path, body)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func expect(t *testing.T, resp *http.Response, code int) {
	t.Helper()
	if resp.StatusCode != code {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s: status %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, code, b)
	}
}

func decodeDoc(t *testing.T, resp *http.Response) restapi.Document {
	t.Helper()
	var d restapi.Document
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestSmallUploadDownloadRangeAndConditional(t *testing.T) {
	e := setup(t, "proxy")
	data := []byte("hello sharepoint from S/4HANA")
	resp := e.do(t, "POST", "/repositories/DMS/documents?folder=invoices/2026&fileName=inv.txt", "rw-key", bytes.NewReader(data), nil)
	expect(t, resp, 201)
	doc := decodeDoc(t, resp)
	if doc.Path != "invoices/2026/inv.txt" || doc.Size != int64(len(data)) || doc.MimeType != "text/plain" {
		t.Fatalf("unexpected doc %+v", doc)
	}

	resp = e.do(t, "GET", "/repositories/DMS/documents/"+doc.ID+"/content", "ro-key", nil, nil)
	expect(t, resp, 200)
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: %q", got)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" || !strings.Contains(resp.Header.Get("Content-Disposition"), "inv.txt") {
		t.Fatalf("missing headers: %v", resp.Header)
	}

	resp = e.do(t, "GET", "/repositories/DMS/documents/"+doc.ID+"/content", "ro-key", nil, map[string]string{"If-None-Match": etag})
	expect(t, resp, 304)

	resp = e.do(t, "GET", "/repositories/DMS/documents/"+doc.ID+"/content", "ro-key", nil, map[string]string{"Range": "bytes=6-15"})
	expect(t, resp, 206)
	got, _ = io.ReadAll(resp.Body)
	if string(got) != string(data[6:16]) {
		t.Fatalf("range mismatch: %q", got)
	}

	resp = e.do(t, "HEAD", "/repositories/DMS/documents/"+doc.ID+"/content", "ro-key", nil, nil)
	expect(t, resp, 200)
	if resp.ContentLength != int64(len(data)) {
		t.Fatalf("HEAD content-length %d", resp.ContentLength)
	}
}

func TestLargeChunkedUpload(t *testing.T) {
	e := setup(t, "proxy")
	data := randBytes(3<<20 + 12345) // > SimpleUploadMax, not a chunk multiple
	resp := e.do(t, "POST", "/repositories/DMS/documents?fileName=big.bin", "rw-key", bytes.NewReader(data), nil)
	expect(t, resp, 201)
	doc := decodeDoc(t, resp)
	if doc.Size != int64(len(data)) {
		t.Fatalf("size %d", doc.Size)
	}
	resp = e.do(t, "GET", "/repositories/DMS/documents/"+doc.ID+"/content", "rw-key", nil, nil)
	expect(t, resp, 200)
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, data) {
		t.Fatal("large content mismatch")
	}

	// Replace content via session (new version).
	data2 := randBytes(2 << 20)
	resp = e.do(t, "PUT", "/repositories/DMS/documents/"+doc.ID+"/content", "rw-key", bytes.NewReader(data2), nil)
	expect(t, resp, 200)
	resp = e.do(t, "GET", "/repositories/DMS/documents/"+doc.ID+"/content", "rw-key", nil, nil)
	got, _ = io.ReadAll(resp.Body)
	if !bytes.Equal(got, data2) {
		t.Fatal("replaced content mismatch")
	}
}

func multipartBody(t *testing.T, fields map[string]string, filename string, data []byte) (io.Reader, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(data)
	_ = mw.Close()
	// Hide length so the adapter sees an unknown-size part stream.
	return io.MultiReader(&buf), mw.FormDataContentType()
}

func TestMultipartUploads(t *testing.T) {
	e := setup(t, "proxy")
	for _, size := range []int{1000, 2<<20 + 77} { // in-memory and spooled paths
		data := randBytes(size)
		body, ct := multipartBody(t, map[string]string{"folder": "mp", "conflict": "replace"}, fmt.Sprintf("photo-%d.png", size), data)
		resp := e.do(t, "POST", "/repositories/DMS/documents", "rw-key", body, map[string]string{"Content-Type": ct})
		expect(t, resp, 201)
		doc := decodeDoc(t, resp)
		if doc.Size != int64(size) || doc.Path != fmt.Sprintf("mp/photo-%d.png", size) {
			t.Fatalf("unexpected doc %+v", doc)
		}
		resp = e.do(t, "GET", "/repositories/DMS/documents/"+doc.ID+"/content?disposition=inline", "rw-key", nil, nil)
		got, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(got, data) {
			t.Fatalf("multipart content mismatch for %d", size)
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") {
			t.Fatal("expected inline disposition")
		}
	}
}

func TestBusinessObjectAttachments(t *testing.T) {
	e := setup(t, "proxy")
	resp := e.do(t, "GET", "/repositories/DMS/objects/BUS2081/5105600001/documents", "rw-key", nil, nil)
	expect(t, resp, 200)
	var page struct{ Value []restapi.Document }
	_ = json.NewDecoder(resp.Body).Decode(&page)
	if len(page.Value) != 0 {
		t.Fatalf("expected empty list, got %d", len(page.Value))
	}
	for i := range 3 {
		resp = e.do(t, "POST", "/repositories/DMS/objects/BUS2081/5105600001/documents", "rw-key",
			strings.NewReader("x"), map[string]string{"Slug": fmt.Sprintf("scan%%20%d.pdf", i)})
		expect(t, resp, 201)
	}
	resp = e.do(t, "GET", "/repositories/DMS/objects/BUS2081/5105600001/documents?top=2", "rw-key", nil, nil)
	expect(t, resp, 200)
	var p1 struct {
		Value      []restapi.Document
		NextCursor string
	}
	_ = json.NewDecoder(resp.Body).Decode(&p1)
	if len(p1.Value) != 2 || p1.NextCursor == "" || p1.Value[0].Name != "scan 0.pdf" {
		t.Fatalf("page 1: %+v", p1)
	}
	resp = e.do(t, "GET", "/repositories/DMS/objects/BUS2081/5105600001/documents?top=2&cursor="+p1.NextCursor, "rw-key", nil, nil)
	expect(t, resp, 200)
	var p2 struct{ Value []restapi.Document }
	_ = json.NewDecoder(resp.Body).Decode(&p2)
	if len(p2.Value) != 1 {
		t.Fatalf("page 2: %+v", p2)
	}
}

func TestScopeAndAuthorization(t *testing.T) {
	e := setup(t, "proxy")
	outside := e.seed.AddFile("drive1", "HR/salaries.xlsx", []byte("secret"))
	ro := e.seed.AddFile("drive1", "SAP/RO/manual.pdf", []byte("pdf"))

	expect(t, e.do(t, "GET", "/repositories/DMS/documents/"+outside, "rw-key", nil, nil), 404)
	expect(t, e.do(t, "GET", "/repositories/DMS/documents/"+outside+"/content", "rw-key", nil, nil), 404)
	expect(t, e.do(t, "DELETE", "/repositories/DMS/documents/"+outside, "rw-key", nil, nil), 404)

	expect(t, e.do(t, "GET", "/repositories/DMS/children", "", nil, nil), 401)
	expect(t, e.do(t, "GET", "/repositories/DMS/children", "wrong", nil, nil), 401)
	expect(t, e.do(t, "POST", "/repositories/DMS/documents?fileName=a.txt", "ro-key", strings.NewReader("a"), nil), 403)
	expect(t, e.do(t, "GET", "/repositories/DMS/children", "other-key", nil, nil), 404) // repo hidden
	expect(t, e.do(t, "GET", "/repositories/NOPE/children", "rw-key", nil, nil), 404)
	expect(t, e.do(t, "GET", "/repositories/RO/documents/"+ro, "other-key", nil, nil), 200)
	expect(t, e.do(t, "POST", "/repositories/RO/documents?fileName=a.txt", "rw-key", strings.NewReader("a"), nil), 403)

	expect(t, e.do(t, "POST", "/repositories/DMS/documents?folder=../HR&fileName=x.txt", "rw-key", strings.NewReader("a"), nil), 400)
	expect(t, e.do(t, "POST", "/repositories/DMS/documents?fileName=a:b.txt", "rw-key", strings.NewReader("a"), nil), 400)

	resp := e.do(t, "GET", "/repositories", "other-key", nil, nil)
	var repos struct{ Value []struct{ ID string } }
	_ = json.NewDecoder(resp.Body).Decode(&repos)
	if len(repos.Value) != 1 || repos.Value[0].ID != "RO" {
		t.Fatalf("repository listing leaked: %+v", repos)
	}
}

func TestConflictDeleteSearchFolders(t *testing.T) {
	e := setup(t, "proxy")
	expect(t, e.do(t, "POST", "/repositories/DMS/documents?fileName=c.txt", "rw-key", strings.NewReader("1"), nil), 201)
	expect(t, e.do(t, "POST", "/repositories/DMS/documents?fileName=c.txt&conflict=fail", "rw-key", strings.NewReader("2"), nil), 409)
	resp := e.do(t, "POST", "/repositories/DMS/documents?fileName=c.txt", "rw-key", strings.NewReader("3"), nil)
	expect(t, resp, 201)
	if d := decodeDoc(t, resp); d.Name != "c 1.txt" {
		t.Fatalf("rename produced %q", d.Name)
	}

	resp = e.do(t, "GET", "/repositories/DMS/search?q=c%201", "rw-key", nil, nil)
	expect(t, resp, 200)
	var found struct{ Value []restapi.Document }
	_ = json.NewDecoder(resp.Body).Decode(&found)
	if len(found.Value) != 1 {
		t.Fatalf("search returned %d", len(found.Value))
	}
	id := found.Value[0].ID
	expect(t, e.do(t, "DELETE", "/repositories/DMS/documents/"+id, "ro-key", nil, nil), 403)
	expect(t, e.do(t, "DELETE", "/repositories/DMS/documents/"+id, "rw-key", nil, nil), 204)
	expect(t, e.do(t, "GET", "/repositories/DMS/documents/"+id, "rw-key", nil, nil), 404)

	resp = e.do(t, "POST", "/repositories/DMS/folders", "rw-key", strings.NewReader(`{"path":"a/b/c"}`), nil)
	expect(t, resp, 201)
	if d := decodeDoc(t, resp); !d.IsFolder || d.Path != "a/b/c" {
		t.Fatalf("folder %+v", d)
	}
	expect(t, e.do(t, "POST", "/repositories/DMS/folders", "rw-key", strings.NewReader(`{"path":"a/b/c"}`), nil), 201) // idempotent
	resp = e.do(t, "GET", "/repositories/DMS/lookup?path=a/b", "rw-key", nil, nil)
	expect(t, resp, 200)
}

func TestRedirectModeAndThumbnail(t *testing.T) {
	if storagetest.Local() {
		t.Skip("a local repository has no pre-authenticated URL to redirect to and no thumbnails; see TestLocalNoThumbnailsButACatalogueCode")
	}
	e := setup(t, "redirect")
	resp := e.do(t, "POST", "/repositories/DMS/documents?fileName=img.png", "rw-key", strings.NewReader("png"), nil)
	expect(t, resp, 201)
	doc := decodeDoc(t, resp)
	resp = e.do(t, "GET", "/repositories/DMS/documents/"+doc.ID+"/content", "rw-key", nil, nil)
	expect(t, resp, 302)
	if !strings.Contains(resp.Header.Get("Location"), "/download/") {
		t.Fatalf("bad redirect %q", resp.Header.Get("Location"))
	}
	resp = e.do(t, "GET", "/repositories/DMS/documents/"+doc.ID+"/thumbnail?size=small", "rw-key", nil, nil)
	expect(t, resp, 200)
	if resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("thumbnail content-type %q", resp.Header.Get("Content-Type"))
	}
}

func TestConcurrentMixedLoad(t *testing.T) {
	e := setup(t, "proxy")
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			size := 50_000
			if i%5 == 0 {
				size = 1<<20 + 500_000
			}
			data := randBytes(size)
			req, _ := http.NewRequest("POST", fmt.Sprintf("%s/repositories/DMS/documents?folder=load&fileName=f%d.bin", e.url, i), bytes.NewReader(data))
			req.Header.Set("X-API-Key", "rw-key")
			resp, err := e.client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			var d restapi.Document
			_ = json.NewDecoder(resp.Body).Decode(&d)
			resp.Body.Close()
			if resp.StatusCode != 201 {
				errs <- fmt.Errorf("upload %d: %d", i, resp.StatusCode)
				return
			}
			req, _ = http.NewRequest("GET", e.url+"/repositories/DMS/documents/"+d.ID+"/content", nil)
			req.Header.Set("X-API-Key", "rw-key")
			resp, err = e.client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !bytes.Equal(got, data) {
				errs <- fmt.Errorf("download %d mismatch", i)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Regression: against real SharePoint the item came back without a download
// URL (the annotation was sent inside $select), so every download failed
// while the emulator, which always supplied one, hid the problem.
func TestDownloadWithoutDownloadURLUsesContentRedirect(t *testing.T) {
	e := setupWith(t, "proxy", true)
	data := []byte("0123456789abcdefghij")
	resp := e.do(t, "POST", "/repositories/DMS/documents?fileName=r.bin", "rw-key", bytes.NewReader(data), nil)
	expect(t, resp, 201)
	doc := decodeDoc(t, resp)
	resp = e.do(t, "GET", "/repositories/DMS/documents/"+doc.ID+"/content", "rw-key", nil, nil)
	expect(t, resp, 200)
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, data) {
		t.Fatalf("content %q", got)
	}
	resp = e.do(t, "GET", "/repositories/DMS/documents/"+doc.ID+"/content", "rw-key", nil, map[string]string{"Range": "bytes=5-9"})
	expect(t, resp, 206)
	got, _ = io.ReadAll(resp.Body)
	if string(got) != "56789" {
		t.Fatalf("range %q", got)
	}
}
