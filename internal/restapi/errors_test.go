package restapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Lamiv/zaks-drive/internal/storage/storagetest"
)

type problemBody struct {
	Type, Title, Detail, Code, Hint, RequestID string
	Status                                     int
}

func problemOf(t *testing.T, body io.Reader) problemBody {
	t.Helper()
	var p problemBody
	raw, _ := io.ReadAll(body)
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	return p
}

// Every error answers with a code, what happened, and how to fix it.
func TestErrorsCarryCodeDetailAndHint(t *testing.T) {
	e := setup(t, "proxy")
	cases := []struct {
		name, method, path, key, body string
		hdr                           map[string]string
		status                        int
		code                          string
		detail                        []string // substrings the detail must contain
	}{
		{"no credentials", "GET", "/repositories", "", "", nil, 401, "AUTH_MISSING_CREDENTIALS", []string{"no X-API-Key"}},
		{"wrong key", "GET", "/repositories", "nope", "", nil, 401, "AUTH_INVALID_API_KEY", []string{"X-API-Key header does not match"}},
		{"bad scheme", "GET", "/repositories", "", "", map[string]string{"Authorization": "Basic abc"}, 401, "AUTH_MISSING_CREDENTIALS", []string{"supported scheme", "Bearer"}},
		{"read-only key cannot write", "POST", "/repositories/DMS/documents?fileName=a.txt", "ro-key", "x", nil, 403, "AUTH_PERMISSION_DENIED", []string{"apikey:ro", `"write"`, "granted: read"}},
		{"unknown repository lists the visible ones", "GET", "/repositories/NOPE/children", "rw-key", "", nil, 404, "REPO_NOT_FOUND", []string{`"NOPE"`, "DMS"}},
		{"read-only repository", "POST", "/repositories/RO/documents?fileName=a.txt", "rw-key", "x", nil, 403, "REPO_READ_ONLY", []string{`"RO"`, "readOnly"}},
		{"invalid characters", "POST", "/repositories/DMS/documents?fileName=a:b.txt", "rw-key", "x", nil, 400, "NAME_INVALID", []string{`"a:b.txt"`, ":"}},
		{"trailing dot", "POST", "/repositories/DMS/documents?fileName=a.", "rw-key", "x", nil, 400, "NAME_INVALID", []string{"ends with a dot"}},
		{"name too long", "POST", "/repositories/DMS/documents?fileName=" + strings.Repeat("a", 300), "rw-key", "x", nil, 400, "NAME_TOO_LONG", []string{"300 characters", "255"}},
		{"path too long", "POST", "/repositories/DMS/documents?fileName=a.txt&folder=" + strings.Repeat("d/", 220), "rw-key", "x", nil, 400, "PATH_TOO_LONG", []string{"400"}},
		{"parent folder segment", "POST", "/repositories/DMS/documents?fileName=a.txt&folder=../x", "rw-key", "x", nil, 400, "NAME_INVALID", []string{`".."`}},
		{"no file name", "POST", "/repositories/DMS/documents", "rw-key", "x", nil, 400, "UPLOAD_FILENAME_REQUIRED", []string{"fileName"}},
		{"bad conflict", "POST", "/repositories/DMS/documents?fileName=a.txt&conflict=x", "rw-key", "x", nil, 400, "UPLOAD_CONFLICT_INVALID", []string{`"x"`, "rename, replace or fail"}},
		{"bad cursor", "GET", "/repositories/DMS/children?cursor=abc.def", "rw-key", "", nil, 400, "CURSOR_INVALID", nil},
		{"empty search", "GET", "/repositories/DMS/search?q=", "rw-key", "", nil, 400, "QUERY_EMPTY", nil},
		{"bad thumbnail size", "GET", "/repositories/DMS/documents/x/thumbnail?size=huge", "rw-key", "", nil, 400, "THUMBNAIL_SIZE_INVALID", []string{`"huge"`}},
		{"folder body", "POST", "/repositories/DMS/folders", "rw-key", "not json", nil, 400, "REQUEST_BODY_INVALID", []string{"path"}},
		{"missing document", "GET", "/repositories/DMS/documents/01DOESNOTEXIST", "rw-key", "", nil, 404, "DOC_NOT_FOUND", nil},
		{"SAP folder is reserved", "POST", "/repositories/DMS/documents?fileName=x.txt&folder=ContentServer/Z1", "rw-key", "x", nil, 403, "PATH_RESERVED", []string{"SAP content server"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "path too long" && storagetest.Local() {
				t.Skip("the 400-character path limit is SharePoint's; a local repository only limits each name to 255 bytes")
			}
			hdr := c.hdr
			resp := e.do(t, c.method, c.path, c.key, strings.NewReader(c.body), hdr)
			if resp.StatusCode != c.status {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, c.status, b)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("content type %q", ct)
			}
			p := problemOf(t, resp.Body)
			if p.Code != c.code {
				t.Fatalf("code %q, want %q (detail: %s)", p.Code, c.code, p.Detail)
			}
			for _, want := range c.detail {
				if !strings.Contains(p.Detail, want) {
					t.Errorf("detail %q lacks %q", p.Detail, want)
				}
			}
			if p.Hint == "" || p.Title == "" || p.Status != c.status {
				t.Errorf("hint/title/status missing: %+v", p)
			}
			if !strings.HasSuffix(p.Type, "docs/error-codes.md#"+strings.ToLower(c.code)) {
				t.Errorf("type %q does not link to the code", p.Type)
			}
			if p.RequestID == "" {
				t.Error("requestId missing")
			}
		})
	}
}

func TestUploadTooLargeNamesTheLimit(t *testing.T) {
	e := setup(t, "proxy") // limit 50 MiB; a declared length above it is refused before reading
	req, _ := http_NewRequest(e.url+"/repositories/DMS/documents?fileName=big.bin", "rw-key", 60<<20)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	p := problemOf(t, resp.Body)
	if resp.StatusCode != 413 || p.Code != "UPLOAD_TOO_LARGE" || !strings.Contains(p.Detail, "52428800") {
		t.Fatalf("%d %+v", resp.StatusCode, p)
	}
}

// http_NewRequest builds a POST that declares a body of n bytes without
// sending them (the server must refuse on the declared length).
func http_NewRequest(url, key string, n int64) (*http.Request, error) {
	req, err := http.NewRequest("POST", url, io.LimitReader(zeroReader{}, n))
	if err != nil {
		return nil, err
	}
	req.ContentLength = n
	req.Header.Set("X-API-Key", key)
	return req, nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
