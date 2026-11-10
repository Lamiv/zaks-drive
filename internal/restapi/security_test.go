package restapi_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Lamiv/zaks-drive/internal/restapi"
	"github.com/Lamiv/zaks-drive/internal/storage/storagetest"
)

// Regression: paging cursors used to be a base64 Graph URL checked only for
// the Graph base prefix, so a caller could page through any drive or folder
// the adapter's app registration can reach.
func TestCursorCannotBeForgedOrReused(t *testing.T) {
	e := setup(t, "proxy")
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		expect(t, e.do(t, "POST", "/repositories/DMS/documents?folder=f1&fileName="+name, "rw-key", strings.NewReader("x"), nil), 201)
	}
	e.seed.AddFile("drive1", "HR/salaries.xlsx", []byte("secret"))
	e.seed.AddFile("drive1", "HR/bonus.xlsx", []byte("secret"))

	resp := e.do(t, "GET", "/repositories/DMS/children?path=f1&top=1", "ro-key", nil, nil)
	expect(t, resp, 200)
	var page struct {
		Value      []restapi.Document
		NextCursor string
	}
	_ = json.NewDecoder(resp.Body).Decode(&page)
	if page.NextCursor == "" {
		t.Fatal("expected a cursor")
	}
	// Valid cursor on its own folder works.
	expect(t, e.do(t, "GET", "/repositories/DMS/children?path=f1&top=1&cursor="+page.NextCursor, "ro-key", nil, nil), 200)
	// Same cursor on another folder or as a search cursor is rejected.
	expect(t, e.do(t, "GET", "/repositories/DMS/children?path=other&top=1&cursor="+page.NextCursor, "ro-key", nil, nil), 400)
	expect(t, e.do(t, "GET", "/repositories/DMS/search?q=a&cursor="+page.NextCursor, "ro-key", nil, nil), 400)

	// Forged cursor pointing at a folder outside the repository.
	link, _ := base64.RawURLEncoding.DecodeString(strings.SplitN(page.NextCursor, ".", 2)[0])
	evil := strings.Replace(string(link), "SAP/DMS/f1", "HR", 1)
	if storagetest.Local() {
		evil = "999" // a local cursor is a paging offset, not a link
	}
	forged := base64.RawURLEncoding.EncodeToString([]byte(evil))
	for _, c := range []string{forged, forged + "." + strings.SplitN(page.NextCursor, ".", 2)[1], forged + ".AAAA"} {
		resp := e.do(t, "GET", "/repositories/DMS/children?path=f1&cursor="+c, "ro-key", nil, nil)
		expect(t, resp, 400)
	}
}

// Regression: the SAP content server's folder (documents, sidecars and
// certificate records) was reachable and writable through the REST API,
// letting a REST write key plant an "active" certificate record.
func TestContentServerFolderIsReserved(t *testing.T) {
	e := setup(t, "proxy")
	docID := e.seed.AddFile("drive1", "SAP/DMS/ContentServer/Z1/ab/DOC1/data", []byte("sap content"))
	certID := e.seed.AddFile("drive1", "SAP/DMS/ContentServer/Z1/~certs/abc.json", []byte(`{"active":true}`))
	e.seed.AddFile("drive1", "SAP/DMS/ContentServer/readme.txt", []byte("visible"))

	for _, id := range []string{docID, certID} {
		expect(t, e.do(t, "GET", "/repositories/DMS/documents/"+id, "rw-key", nil, nil), 404)
		expect(t, e.do(t, "GET", "/repositories/DMS/documents/"+id+"/content", "rw-key", nil, nil), 404)
		expect(t, e.do(t, "PUT", "/repositories/DMS/documents/"+id+"/content", "rw-key", strings.NewReader("evil"), nil), 404)
		expect(t, e.do(t, "DELETE", "/repositories/DMS/documents/"+id, "rw-key", nil, nil), 404)
	}
	expect(t, e.do(t, "GET", "/repositories/DMS/children?path=ContentServer/Z1", "rw-key", nil, nil), 404)
	expect(t, e.do(t, "GET", "/repositories/DMS/children?path=ContentServer/Z1/~certs", "rw-key", nil, nil), 404)
	expect(t, e.do(t, "GET", "/repositories/DMS/lookup?path=ContentServer/Z1/~certs/abc.json", "rw-key", nil, nil), 404)
	expect(t, e.do(t, "POST", "/repositories/DMS/documents?folder=ContentServer/Z1/~certs&fileName=forged.json", "rw-key", strings.NewReader("{}"), nil), 403)
	expect(t, e.do(t, "POST", "/repositories/DMS/documents?folder=ContentServer&fileName=Z1", "rw-key", strings.NewReader("{}"), nil), 403)
	expect(t, e.do(t, "POST", "/repositories/DMS/folders", "rw-key", strings.NewReader(`{"path":"ContentServer/Z1/x"}`), nil), 403)

	// The parent folder is visible, but the reserved child is hidden and the
	// parent cannot be deleted (that would delete the SAP documents).
	resp := e.do(t, "GET", "/repositories/DMS/children?path=ContentServer", "rw-key", nil, nil)
	expect(t, resp, 200)
	var page struct{ Value []restapi.Document }
	_ = json.NewDecoder(resp.Body).Decode(&page)
	if len(page.Value) != 1 || page.Value[0].Name != "readme.txt" {
		t.Fatalf("listing leaked reserved folder: %+v", page.Value)
	}
	resp = e.do(t, "GET", "/repositories/DMS/lookup?path=ContentServer", "rw-key", nil, nil)
	expect(t, resp, 200)
	var parent restapi.Document
	_ = json.NewDecoder(resp.Body).Decode(&parent)
	expect(t, e.do(t, "DELETE", "/repositories/DMS/documents/"+parent.ID, "rw-key", nil, nil), 403)

	resp = e.do(t, "GET", "/repositories/DMS/search?q=abc", "rw-key", nil, nil)
	expect(t, resp, 200)
	_ = json.NewDecoder(resp.Body).Decode(&page)
	if len(page.Value) != 0 {
		t.Fatalf("search leaked reserved items: %+v", page.Value)
	}
}
