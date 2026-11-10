package contentrepo

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lamiv/zaks-drive/internal/auth"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/graphmock"
	"github.com/Lamiv/zaks-drive/internal/storage"
	"github.com/Lamiv/zaks-drive/internal/storage/storagetest"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

// ---- test signer: emulates SAP SSF producing a detached PKCS#7 secKey ----

type sapSigner struct {
	key  *rsa.PrivateKey
	cert *x509.Certificate
}

func newSigner(t *testing.T, cn string) *sapSigner {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &sapSigner{key: key, cert: cert}
}

func mustMarshal(v any) []byte {
	b, err := asn1.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func (s *sapSigner) sign(msg []byte) []byte {
	digest := sha256.Sum256(msg)
	sigBytes, _ := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	alg := pkix.AlgorithmIdentifier{Algorithm: oidSHA256, Parameters: asn1.NullRawValue}
	ias := mustMarshal(struct {
		Issuer asn1.RawValue
		Serial *big.Int
	}{asn1.RawValue{FullBytes: s.cert.RawIssuer}, s.cert.SerialNumber})
	si := mustMarshal(struct {
		Version   int
		SID       asn1.RawValue
		DigestAlg pkix.AlgorithmIdentifier
		EncAlg    pkix.AlgorithmIdentifier
		Sig       []byte
	}{1, asn1.RawValue{FullBytes: ias}, alg, pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}, Parameters: asn1.NullRawValue}, sigBytes})
	set := func(content []byte) asn1.RawValue {
		return asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true, Bytes: content}
	}
	sd := mustMarshal(struct {
		Version     int
		DigestAlgs  asn1.RawValue
		ContentInfo struct{ CT asn1.ObjectIdentifier }
		Certs       asn1.RawValue
		Signers     asn1.RawValue
	}{
		1, set(mustMarshal(alg)),
		struct{ CT asn1.ObjectIdentifier }{asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}},
		asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: s.cert.Raw},
		set(si),
	})
	return mustMarshal(struct {
		CT      asn1.ObjectIdentifier
		Content asn1.RawValue
	}{oidSignedData, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: sd}})
}

// ---- harness ----

type csEnv struct {
	t      *testing.T
	url    string
	admin  string
	signer *sapSigner
	srv    *Server
	svc    *storage.Service
	cfg    config.ContentServerConfig
	seed   graphmock.Seeder
}

func setupCS(t *testing.T) *csEnv {
	t.Helper()
	return setupCSWithLogger(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func setupCSWithLogger(t *testing.T, log *slog.Logger) *csEnv {
	t.Helper()
	mh, err := graphmock.New(graphmock.Options{DataDir: t.TempDir(), Drives: []string{"drive1"}})
	if err != nil {
		t.Fatal(err)
	}
	mock := httptest.NewServer(mh)
	t.Cleanup(mock.Close)
	tokens := auth.NewClientCredentials(mock.URL, "tenant", "client", "secret", "scope", mock.Client())
	gc := graph.New(graph.Options{BaseURL: mock.URL + "/v1.0", MaxConcurrency: 16, MaxRetries: 2, MaxRetryBackoff: 100 * time.Millisecond}, tokens, http.DefaultTransport, log)
	eng := transfer.New(gc, transfer.Options{SimpleUploadMax: 1 << 20, ChunkSize: 320 << 10, MemoryBudget: 8 << 20, MaxUpload: 50 << 20, SpoolDir: t.TempDir()})
	repoCfgs := []config.RepositoryConfig{{ID: "DMS", DriveID: "drive1", RootPath: "SAP_DMS"}}
	var seeder graphmock.Seeder = mh.(graphmock.Seeder)
	dmsDir := t.TempDir()
	if storagetest.Local() { // the same suite over the local backend
		repoCfgs = []config.RepositoryConfig{{ID: "DMS", Backend: config.BackendLocal, LocalRoot: dmsDir}}
	}
	svc, err := storage.NewService(context.Background(), gc, eng, repoCfgs, time.Minute, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	if storagetest.Local() {
		seeder = storagetest.Seeder{Svc: svc, Repos: map[string]storagetest.Repo{"SAP_DMS": {ID: "DMS", Dir: dmsDir}}}
	}
	cfg := config.ContentServerConfig{ClockSkew: time.Minute, Repositories: []config.ContentRepositoryConfig{
		{ContRep: "Z1", Repository: "DMS", Folder: "ContentServer/Z1", Signature: "required", Description: "signed"},
		{ContRep: "Z2", Repository: "DMS", Folder: "ContentServer/Z2", Signature: "none"},
		{ContRep: "Z3", Repository: "DMS", Folder: "ContentServer/Z3", Signature: "optional"},
	}}
	if err := ReserveFolders(svc, cfg); err != nil {
		t.Fatal(err)
	}
	srv, err := New(context.Background(), svc, cfg, time.Minute, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	mux := http.NewServeMux()
	srv.RegisterAdmin(mux, "admintoken")
	as := httptest.NewServer(mux)
	t.Cleanup(as.Close)
	return &csEnv{t: t, url: hs.URL + "/ContentServer/ContentServer.dll", admin: as.URL, signer: newSigner(t, "S4H"), srv: srv,
		svc: svc, cfg: cfg, seed: seeder}
}

// u builds a URL; when signed, accessMode/authId/expiration and secKey are
// added like SAP does. kv are name,value pairs in URL order.
func (e *csEnv) u(cmd string, signed bool, accessMode string, kv ...string) string {
	q := cmd + "&pVersion=0047"
	for i := 0; i+1 < len(kv); i += 2 {
		q += "&" + kv[i] + "=" + url.QueryEscape(kv[i+1])
	}
	if signed {
		exp := time.Now().UTC().Add(time.Hour).Format("20060102150405")
		q += "&accessMode=" + accessMode + "&authId=" + url.QueryEscape("CN=S4H") + "&expiration=" + exp
		msg := signedMessages(cmd, q)[0]
		q += "&secKey=" + url.QueryEscape(base64.StdEncoding.EncodeToString(e.signer.sign(msg)))
	}
	return e.url + "?" + q
}

func (e *csEnv) do(method, u string, body io.Reader, hdr map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest(method, u, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func (e *csEnv) expect(resp *http.Response, body []byte, code int) {
	e.t.Helper()
	if resp.StatusCode != code {
		e.t.Fatalf("%s %s: status %d want %d (%s) %s", resp.Request.Method, resp.Request.URL.RawQuery[:min(60, len(resp.Request.URL.RawQuery))], resp.StatusCode, code, resp.Header.Get("X-ErrorDescription"), body)
	}
}

func (e *csEnv) activate() {
	e.t.Helper()
	resp, b := e.do("PUT", e.u("putCert", false, "", "contRep", "Z1", "authId", "CN=S4H"), bytes.NewReader(e.signer.cert.Raw), nil)
	e.expect(resp, b, 200)
	req, _ := http.NewRequest("POST", e.admin+"/admin/contentserver/certificates/activate", strings.NewReader(`{"contRep":"Z1","authId":"CN=S4H"}`))
	req.Header.Set("Authorization", "Bearer admintoken")
	r2, err := http.DefaultClient.Do(req)
	if err != nil || r2.StatusCode != 200 {
		e.t.Fatalf("activate: %v %v", err, r2.Status)
	}
	r2.Body.Close()
}

func parseMultipart(t *testing.T, resp *http.Response, body []byte) []struct {
	h    textproto.MIMEHeader
	data []byte
} {
	t.Helper()
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("content-type %q", resp.Header.Get("Content-Type"))
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var out []struct {
		h    textproto.MIMEHeader
		data []byte
	}
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		d, _ := io.ReadAll(p)
		out = append(out, struct {
			h    textproto.MIMEHeader
			data []byte
		}{p.Header, d})
	}
}

// ---- tests ----

func TestServerInfo(t *testing.T) {
	e := setupCS(t)
	resp, b := e.do("GET", e.u("serverInfo", false, "", "contRep", "Z1"), nil, nil)
	e.expect(resp, b, 200)
	s := string(b)
	if !strings.HasPrefix(s, `serverStatus="running";`) || !strings.Contains(s, "\r\ncontRep=\"Z1\";contRepDescription=\"signed\";contRepStatus=\"running\";") || !strings.Contains(s, `pVersion="0047";`) {
		t.Fatalf("serverInfo body:\n%s", s)
	}
	resp, b = e.do("GET", e.u("serverInfo", false, "", "contRep", "XX"), nil, nil)
	e.expect(resp, b, 404)
}

func TestSignatureEnforcement(t *testing.T) {
	e := setupCS(t)
	put := func(u string) (*http.Response, []byte) {
		return e.do("PUT", u, strings.NewReader("hello"), map[string]string{"Content-Type": "text/plain"})
	}
	// Unknown certificate, then received but not yet activated.
	resp, b := put(e.u("create", true, "c", "contRep", "Z1", "docId", "D1", "compId", "data"))
	e.expect(resp, b, 401)
	resp, b = e.do("PUT", e.u("putCert", false, "", "contRep", "Z1", "authId", "CN=S4H"), bytes.NewReader([]byte("garbage")), nil)
	e.expect(resp, b, 406)
	resp, b = e.do("PUT", e.u("putCert", false, "", "contRep", "Z1", "authId", "CN=S4H"), bytes.NewReader(e.signer.cert.Raw), nil)
	e.expect(resp, b, 200)
	resp, b = put(e.u("create", true, "c", "contRep", "Z1", "docId", "D1", "compId", "data"))
	e.expect(resp, b, 401)
	if !strings.Contains(resp.Header.Get("X-ErrorDescription"), "not activated") {
		t.Fatalf("error description %q", resp.Header.Get("X-ErrorDescription"))
	}
	e.activate()
	resp, b = put(e.u("create", true, "c", "contRep", "Z1", "docId", "D1", "compId", "data"))
	e.expect(resp, b, 201)

	// No secKey, tampered parameter, wrong access mode, expired, foreign signer.
	resp, b = e.do("GET", e.u("get", false, "", "contRep", "Z1", "docId", "D1"), nil, nil)
	e.expect(resp, b, 401)
	good := e.u("get", true, "r", "contRep", "Z1", "docId", "D1")
	resp, b = e.do("GET", strings.Replace(good, "docId=D1", "docId=D2", 1), nil, nil)
	e.expect(resp, b, 401)
	resp, b = e.do("GET", strings.Replace(good, "?get&", "?delete&", 1), nil, nil)
	e.expect(resp, b, 401) // accessMode r does not permit delete
	expired := e.url + "?get&contRep=Z1&docId=D1&accessMode=r&authId=CN%3DS4H&expiration=20200101000000"
	expired += "&secKey=" + url.QueryEscape(base64.StdEncoding.EncodeToString(e.signer.sign(signedMessages("get", strings.TrimPrefix(expired, e.url+"?"))[0])))
	resp, b = e.do("GET", expired, nil, nil)
	e.expect(resp, b, 401)
	if !strings.Contains(resp.Header.Get("X-ErrorDescription"), "expired") {
		t.Fatalf("expected expiry error, got %q", resp.Header.Get("X-ErrorDescription"))
	}
	other := &csEnv{t: t, url: e.url, signer: newSigner(t, "S4H")}
	resp, b = e.do("GET", other.u("get", true, "r", "contRep", "Z1", "docId", "D1"), nil, nil)
	e.expect(resp, b, 401)

	// compId is not signed for get (per spec), so the same URL reads any component.
	resp, b = e.do("GET", good, nil, nil)
	e.expect(resp, b, 200)
	if string(b) != "hello" {
		t.Fatalf("body %q", b)
	}
	// A get URL signed with accessMode=rd may be reused for delete (spec example).
	rd := e.u("get", true, "rd", "contRep", "Z1", "docId", "D1")
	resp, b = e.do("GET", strings.Replace(rd, "?get&", "?delete&", 1), nil, nil)
	e.expect(resp, b, 200)
	resp, b = e.do("GET", e.u("info", true, "r", "contRep", "Z1", "docId", "D1"), nil, nil)
	e.expect(resp, b, 404)

	// Repository with signature "none" ignores secKey entirely.
	resp, b = put(e.u("create", false, "", "contRep", "Z2", "docId", "D1", "compId", "data"))
	e.expect(resp, b, 201)
}

func TestDocProtOptionalMode(t *testing.T) {
	e := setupCS(t)
	e.activate()
	// Z3 is "optional": docProt=d means reads are open but deletes need a signature.
	resp, b := e.do("PUT", e.u("create", false, "", "contRep", "Z3", "docId", "P1", "compId", "data", "docProt", "d"), strings.NewReader("x"), nil)
	e.expect(resp, b, 201)
	resp, b = e.do("GET", e.u("get", false, "", "contRep", "Z3", "docId", "P1"), nil, nil)
	e.expect(resp, b, 200)
	resp, b = e.do("GET", e.u("delete", false, "", "contRep", "Z3", "docId", "P1"), nil, nil)
	e.expect(resp, b, 401)
}

func TestDocumentLifecycle(t *testing.T) {
	e := setupCS(t)
	e.activate()
	docID := "361A524A3ECB5459E0000800099245EC"

	// create (POST multipart): two components incl. one with characters SharePoint rejects.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	pdf := bytes.Repeat([]byte("%PDF-1.7 "), 200_000) // > simple-upload threshold -> upload session
	for _, c := range []struct {
		id, ct string
		data   []byte
	}{
		{"data", "application/pdf; charset=; version=1.7", pdf},
		{"scan:page?1.tif", "image/tiff", []byte("TIFFDATA")},
	} {
		h := textproto.MIMEHeader{}
		h.Set("X-compId", c.id)
		h.Set("Content-Type", c.ct)
		h.Set("Content-Length", fmt.Sprint(len(c.data)))
		pw, _ := mw.CreatePart(h)
		_, _ = pw.Write(c.data)
	}
	_ = mw.Close()
	ct := map[string]string{"Content-Type": mw.FormDataContentType()}
	resp, b := e.do("POST", e.u("create", true, "c", "contRep", "Z1", "docId", docID), bytes.NewReader(body.Bytes()), ct)
	e.expect(resp, b, 201)
	resp, b = e.do("POST", e.u("create", true, "c", "contRep", "Z1", "docId", docID), bytes.NewReader(body.Bytes()), ct)
	e.expect(resp, b, 403) // document already exists

	// info: document headers + one empty part per component.
	resp, b = e.do("GET", e.u("info", true, "r", "contRep", "Z1", "docId", docID), nil, nil)
	e.expect(resp, b, 200)
	for k, want := range map[string]string{"X-numberComps": "2", "X-contentRep": "Z1", "X-docId": docID, "X-docStatus": "online", "X-pVersion": "0047"} {
		if got := resp.Header.Get(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	if _, err := time.Parse("2006-01-02", resp.Header.Get("X-dateC")); err != nil {
		t.Fatalf("X-dateC %q", resp.Header.Get("X-dateC"))
	}
	parts := parseMultipart(t, resp, b)
	if len(parts) != 2 || parts[0].h.Get("X-compId") != "data" || parts[0].h.Get("Content-Length") != "0" ||
		parts[0].h.Get("X-Content-Length") != fmt.Sprint(len(pdf)) || parts[0].h.Get("Content-Type") != "application/pdf; charset=; version=1.7" ||
		parts[1].h.Get("X-compId") != "scan:page?1.tif" || len(parts[0].data) != 0 {
		t.Fatalf("info parts: %+v", parts)
	}

	// get: default component "data", explicit compId, offsets (inclusive).
	resp, b = e.do("GET", e.u("get", true, "r", "contRep", "Z1", "docId", docID), nil, nil)
	e.expect(resp, b, 200)
	if !bytes.Equal(b, pdf) || resp.Header.Get("Content-Type") != "application/pdf; charset=; version=1.7" {
		t.Fatalf("get data: %d bytes, ct %q", len(b), resp.Header.Get("Content-Type"))
	}
	resp, b = e.do("GET", e.u("get", true, "r", "contRep", "Z1", "docId", docID, "compId", "scan:page?1.tif", "fromOffset", "2", "toOffset", "5"), nil, nil)
	e.expect(resp, b, 200)
	if string(b) != "FFDA" {
		t.Fatalf("range %q", b)
	}

	// docGet: multipart with content.
	resp, b = e.do("GET", e.u("docGet", true, "r", "contRep", "Z1", "docId", docID), nil, nil)
	e.expect(resp, b, 200)
	parts = parseMultipart(t, resp, b)
	if len(parts) != 2 || !bytes.Equal(parts[0].data, pdf) || string(parts[1].data) != "TIFFDATA" || parts[1].h.Get("Content-Length") != "8" {
		t.Fatalf("docGet parts mismatch")
	}

	// update PUT (overwrite one component), append, search.
	resp, b = e.do("PUT", e.u("update", true, "u", "contRep", "Z1", "docId", docID, "compId", "note"), strings.NewReader("first line;"), map[string]string{"Content-Type": "application/x-note"})
	e.expect(resp, b, 200)
	resp, b = e.do("PUT", e.u("append", true, "u", "contRep", "Z1", "docId", docID, "compId", "note"), strings.NewReader(" second LINE;"), nil)
	e.expect(resp, b, 200)
	resp, b = e.do("GET", e.u("get", true, "r", "contRep", "Z1", "docId", docID, "compId", "note"), nil, nil)
	if string(b) != "first line; second LINE;" || resp.Header.Get("Content-Type") != "application/x-note" {
		t.Fatalf("after append %q %q", b, resp.Header.Get("Content-Type"))
	}
	resp, b = e.do("GET", e.u("search", true, "r", "contRep", "Z1", "docId", docID, "compId", "note", "pattern", "line", "numResults", "5"), nil, nil)
	e.expect(resp, b, 200)
	if string(b) != "2;6;19;" {
		t.Fatalf("search %q", b)
	}
	resp, b = e.do("GET", e.u("search", true, "r", "contRep", "Z1", "docId", docID, "compId", "note", "pattern", "line", "caseSensitive", "y", "numResults", "5"), nil, nil)
	if string(b) != "1;6;" {
		t.Fatalf("case-sensitive search %q", b)
	}

	// update POST replaces the document: components not sent are deleted.
	body.Reset()
	mw = multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("X-compId", "data")
	h.Set("Content-Type", "application/pdf")
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write([]byte("v2"))
	_ = mw.Close()
	resp, b = e.do("POST", e.u("update", true, "u", "contRep", "Z1", "docId", docID), bytes.NewReader(body.Bytes()), map[string]string{"Content-Type": mw.FormDataContentType()})
	e.expect(resp, b, 200)
	resp, b = e.do("GET", e.u("info", true, "r", "contRep", "Z1", "docId", docID), nil, nil)
	if resp.Header.Get("X-numberComps") != "1" {
		t.Fatalf("after update POST: %s comps", resp.Header.Get("X-numberComps"))
	}

	// delete component, then the document.
	resp, b = e.do("GET", e.u("delete", true, "d", "contRep", "Z1", "docId", docID, "compId", "data"), nil, nil)
	e.expect(resp, b, 200)
	resp, b = e.do("GET", e.u("info", true, "r", "contRep", "Z1", "docId", docID), nil, nil)
	e.expect(resp, b, 200)
	if resp.Header.Get("X-numberComps") != "0" {
		t.Fatalf("comps %s", resp.Header.Get("X-numberComps"))
	}
	resp, b = e.do("GET", e.u("delete", true, "d", "contRep", "Z1", "docId", docID), nil, nil)
	e.expect(resp, b, 200)
	resp, b = e.do("GET", e.u("get", true, "r", "contRep", "Z1", "docId", docID), nil, nil)
	e.expect(resp, b, 404)
}

func TestCreatePutConflictsAndEmptyDocument(t *testing.T) {
	e := setupCS(t)
	u := e.u("create", false, "", "contRep", "Z2", "docId", "E1", "compId", "data")
	resp, b := e.do("PUT", u, strings.NewReader("a"), nil)
	e.expect(resp, b, 201)
	resp, b = e.do("PUT", u, strings.NewReader("b"), nil)
	e.expect(resp, b, 403) // component already exists

	// POST create with zero components creates an empty document.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.Close()
	resp, b = e.do("POST", e.u("create", false, "", "contRep", "Z2", "docId", "EMPTY"), &body, map[string]string{"Content-Type": mw.FormDataContentType()})
	e.expect(resp, b, 201)
	resp, b = e.do("GET", e.u("info", false, "", "contRep", "Z2", "docId", "EMPTY"), nil, nil)
	e.expect(resp, b, 200)
	if resp.Header.Get("X-numberComps") != "0" {
		t.Fatalf("empty doc comps %s", resp.Header.Get("X-numberComps"))
	}
	resp, b = e.do("PUT", e.u("update", false, "", "contRep", "Z2", "docId", "MISSING", "compId", "data"), strings.NewReader("x"), nil)
	e.expect(resp, b, 404)
	resp, b = e.do("GET", e.u("attrSearch", false, "", "contRep", "Z2", "docId", "E1", "pattern", "1+2+x"), nil, nil)
	e.expect(resp, b, 501)
}

func TestMCreate(t *testing.T) {
	e := setupCS(t)
	resp, b := e.do("PUT", e.u("create", false, "", "contRep", "Z2", "docId", "M2", "compId", "data"), strings.NewReader("exists"), nil)
	e.expect(resp, b, 201)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, p := range [][2]string{{"M1", "data"}, {"M1", "note"}, {"M2", "data"}, {"M3", "data"}} {
		h := textproto.MIMEHeader{}
		h.Set("X-docId", p[0])
		h.Set("X-compId", p[1])
		h.Set("Content-Type", "text/plain")
		pw, _ := mw.CreatePart(h)
		_, _ = pw.Write([]byte(p[0] + "/" + p[1]))
	}
	_ = mw.Close()
	resp, b = e.do("POST", e.u("mCreate", false, "", "contRep", "Z2", "docId", "M1"), &body, map[string]string{"Content-Type": mw.FormDataContentType()})
	e.expect(resp, b, 250)
	want := "docId=\"M1\";retCode=\"201\";\r\ndocId=\"M2\";retCode=\"403\";errorDescription=\"document already exists\";\r\ndocId=\"M3\";retCode=\"201\";\r\n"
	if string(b) != want {
		t.Fatalf("mCreate body:\n%q", b)
	}
	resp, b = e.do("GET", e.u("info", false, "", "contRep", "Z2", "docId", "M1"), nil, nil)
	if resp.Header.Get("X-numberComps") != "2" {
		t.Fatalf("M1 comps %s", resp.Header.Get("X-numberComps"))
	}
}

// Regression: handlers mutated the cached document shared between requests
// (a concurrent map write crashes the process) and sidecar writes were
// unconditional (concurrent writers lost each other's metadata). Two server
// instances with separate caches and locks model two adapter replicas.
func TestConcurrentUpdatesAcrossInstances(t *testing.T) {
	e := setupCS(t)
	srv2, err := New(context.Background(), e.svc, e.cfg, time.Minute, "test2", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	hs2 := httptest.NewServer(srv2.Handler())
	t.Cleanup(hs2.Close)
	urls := []string{e.url, hs2.URL + "/ContentServer/ContentServer.dll"}

	resp, b := e.do("PUT", e.u("create", false, "", "contRep", "Z2", "docId", "CONC", "compId", "data"), strings.NewReader("base"), map[string]string{"Content-Type": "text/plain"})
	e.expect(resp, b, 201)
	// Warm both caches so each instance starts from the same sidecar eTag.
	for _, u := range urls {
		r, _ := http.Get(u + "?info&pVersion=0047&contRep=Z2&docId=CONC")
		r.Body.Close()
	}

	const n = 12
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := fmt.Sprintf("%s?update&pVersion=0047&contRep=Z2&docId=CONC&compId=c%02d", urls[i%2], i)
			req, _ := http.NewRequest("PUT", u, strings.NewReader(fmt.Sprintf("v%d", i)))
			req.Header.Set("Content-Type", fmt.Sprintf("application/x-test%02d", i))
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			r.Body.Close()
			if r.StatusCode != 200 {
				errs <- fmt.Errorf("update %d: %d %s", i, r.StatusCode, r.Header.Get("X-ErrorDescription"))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	resp, b = e.do("GET", e.u("info", false, "", "contRep", "Z2", "docId", "CONC"), nil, nil)
	e.expect(resp, b, 200)
	types := map[string]string{}
	for _, part := range parseMultipart(t, resp, b) {
		types[part.h.Get("X-compId")] = part.h.Get("Content-Type")
	}
	if len(types) != n+1 {
		t.Fatalf("want %d components, got %d: %v", n+1, len(types), types)
	}
	for i := range n {
		if want := fmt.Sprintf("application/x-test%02d", i); types[fmt.Sprintf("c%02d", i)] != want {
			t.Errorf("c%02d: metadata lost, Content-Type %q", i, types[fmt.Sprintf("c%02d", i)])
		}
	}
}

// docPath returns the SharePoint path of a component as the adapter stores it.
func docPath(contRep, docID, comp string) string {
	enc, _ := encodeName(docID)
	return "SAP_DMS/ContentServer/" + contRep + "/" + shardOf(docID) + "/" + enc + "/" + comp
}

// Regression: a document without readable metadata used to be served with
// docProt "" (unprotected) in optional-signature mode.
func TestMissingOrCorruptSidecarFailsClosed(t *testing.T) {
	e := setupCS(t)
	// Component without sidecar (e.g. copied in by hand): highest protection.
	e.seed.AddFile("drive1", docPath("Z3", "LEGACY", "data"), []byte("legacy"))
	resp, b := e.do("GET", e.u("get", false, "", "contRep", "Z3", "docId", "LEGACY"), nil, nil)
	e.expect(resp, b, 401)

	// Corrupt sidecar: the administration data is inaccessible -> 409.
	e.seed.AddFile("drive1", docPath("Z2", "BROKEN", "data"), []byte("x"))
	e.seed.AddFile("drive1", docPath("Z2", "BROKEN", sidecarName), []byte("{not json"))
	resp, b = e.do("GET", e.u("info", false, "", "contRep", "Z2", "docId", "BROKEN"), nil, nil)
	e.expect(resp, b, 409)
}

// The REST API must not see the content server's folder, while the content
// server itself keeps full access to it.
func TestContentServerUnaffectedByReservation(t *testing.T) {
	e := setupCS(t)
	resp, b := e.do("PUT", e.u("create", false, "", "contRep", "Z2", "docId", "R1", "compId", "data"), strings.NewReader("ok"), nil)
	e.expect(resp, b, 201)
	repo, _ := e.svc.Repository("DMS")
	if _, err := e.svc.GetByPath(context.Background(), repo, "ContentServer/Z2"); err == nil {
		t.Fatal("reserved folder visible through the shared handle")
	}
	if _, err := e.svc.GetByPath(context.Background(), repo.Unrestricted(), "ContentServer/Z2"); err != nil {
		t.Fatalf("unrestricted handle: %v", err)
	}
}

// Real SAP systems and the SAP spec produce multipart bodies that Go's
// parser or a strict reading of the spec would reject. Observed on S/4HANA:
// "HTTP error: 400 Bad Request part without X-compId" in CCMS for a create.
func TestMultipartBodiesFromSAP(t *testing.T) {
	e := setupCS(t)
	const b = "KoZIhvcNAQcB"
	post := func(cmd, doc, body string) (*http.Response, []byte) {
		return e.do("POST", e.u(cmd, false, "", "contRep", "Z2", "docId", doc), strings.NewReader(body),
			map[string]string{"Content-Type": "multipart/form-data; boundary=" + b})
	}
	comps := func(doc string) []string {
		resp, body := e.do("GET", e.u("info", false, "", "contRep", "Z2", "docId", doc), nil, nil)
		e.expect(resp, body, 200)
		var out []string
		for _, p := range parseMultipart(t, resp, body) {
			out = append(out, p.h.Get("X-compId"))
		}
		return out
	}
	count := func(doc string) string {
		resp, body := e.do("GET", e.u("info", false, "", "contRep", "Z2", "docId", doc), nil, nil)
		e.expect(resp, body, 200)
		return resp.Header.Get("X-numberComps")
	}

	// Zero components exactly as printed in the SAP spec (Go's parser fails on this).
	resp, body := post("create", "EMPTY1", "--"+b+"\r\n--"+b+"--\r\n")
	e.expect(resp, body, 201)
	if count("EMPTY1") != "0" {
		t.Fatalf("spec zero-component create: %s components", count("EMPTY1"))
	}
	// LF-only variant and a completely empty body.
	resp, body = post("create", "EMPTY2", "--"+b+"\n--"+b+"--\n")
	e.expect(resp, body, 201)
	resp, body = post("create", "EMPTY3", "")
	e.expect(resp, body, 201)

	// The empty-document marker: a part with no headers and no body.
	resp, body = post("create", "EMPTY4", "--"+b+"\r\n\r\n\r\n--"+b+"--\r\n")
	e.expect(resp, body, 201)
	if count("EMPTY4") != "0" {
		t.Fatalf("empty marker part created a component: %s", count("EMPTY4"))
	}

	// Component named only through Content-Disposition.
	resp, body = post("create", "CDNAME", "--"+b+"\r\nContent-Disposition: form-data; name=\"note\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--"+b+"--\r\n")
	e.expect(resp, body, 201)
	if got := comps("CDNAME"); len(got) != 1 || got[0] != "note" {
		t.Fatalf("Content-Disposition name: %v", got)
	}

	// No identification at all: default component names, in order.
	resp, body = post("create", "NOID", "--"+b+"\r\nContent-Type: text/plain\r\n\r\nfirst\r\n--"+b+"\r\nContent-Type: text/plain\r\n\r\nsecond\r\n--"+b+"--\r\n")
	e.expect(resp, body, 201)
	if got := comps("NOID"); strings.Join(got, ",") != "data,data1" {
		t.Fatalf("default component names: %v", got)
	}
	resp, body = e.do("GET", e.u("get", false, "", "contRep", "Z2", "docId", "NOID"), nil, nil)
	if string(body) != "first" {
		t.Fatalf("get without compId returned %q", body)
	}

	// update POST with zero components replaces the document with nothing.
	resp, body = post("update", "NOID", "--"+b+"\r\n--"+b+"--\r\n")
	e.expect(resp, body, 200)
	if count("NOID") != "0" {
		t.Fatalf("update with zero components left %s components", count("NOID"))
	}

	// Garbage that is not multipart at all still fails with 400.
	resp, body = post("create", "BAD", "this is not multipart")
	if resp.StatusCode != 201 && resp.StatusCode != 400 {
		t.Fatalf("garbage body: %d", resp.StatusCode)
	}
}

func TestRedactSecKey(t *testing.T) {
	got := redactSecKey("get&pVersion=0047&contRep=Z1&secKey=AbC%2B123&docId=D")
	if strings.Contains(got, "AbC") || !strings.Contains(got, "secKey=<redacted>") || !strings.Contains(got, "docId=D") {
		t.Fatalf("redaction: %q", got)
	}
}

// CSADMIN's adminContRep commands are not supported; the answer must be a
// clear 501 (what third-party content servers return), not a generic error.
func TestAdminContRepIsNotImplemented(t *testing.T) {
	e := setupCS(t)
	for _, c := range []struct{ method, q, op string }{
		{"GET", "adminContRep&configGet&pVersion=0046&contRep=Z2", "configget"},
		{"POST", "adminContRep&pVersion=0046&operation=configSet&contRep=Z2", "configSet"},
		{"GET", "adminContRep&pVersion=0047&contRep=Z2&certGet", "certget"},
	} {
		resp, body := e.do(c.method, e.url+"?"+c.q, strings.NewReader("ContRepRoot=/x"), nil)
		e.expect(resp, body, 501)
		if d := resp.Header.Get("X-ErrorDescription"); !strings.Contains(d, "adminContRep "+c.op) || !strings.Contains(d, "config.yaml") {
			t.Errorf("%s: description %q", c.q, d)
		}
	}
}

func TestRefusedRequestsAreLoggedWithReason(t *testing.T) {
	var buf bytes.Buffer
	e := setupCSWithLogger(t, slog.New(slog.NewTextHandler(&buf, nil)))
	resp, body := e.do("GET", e.u("get", false, "", "contRep", "Z2", "docId", "NOPE"), nil, nil)
	e.expect(resp, body, 404)
	out := buf.String()
	for _, want := range []string{"content server request refused", "command=get", "status=404", "contRep=Z2", "docId=NOPE", "code=CS_DOC_NOT_FOUND", "title="} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secKey") {
		t.Error("log must not contain the secKey")
	}
}

// Regression for report RSCMSTH2 on a real S/4HANA system: it creates
// documents and compares the Content-Type that info/get return with what it
// sent, literally. "text/plain" came back as "text/plain; charset=", and a
// component created without a Content-Type came back as
// "application/octet-stream".
func TestContentTypeIsReturnedExactlyAsSent(t *testing.T) {
	e := setupCS(t)
	const b = "KoZIhvcNAQcB"
	part := func(comp, ct string) string {
		h := "--" + b + "\r\nX-compId: " + comp + "\r\n"
		if ct != "" {
			h += "Content-Type: " + ct + "\r\n"
		}
		return h + "\r\nhello\r\n"
	}
	body := part("data1.txt", "text/plain") + part("data2.txt", "text/plain; charset=utf-8") + part("data3.txt", "application/x-alf; charset=") +
		part("data4.txt", "application/pdf; charset=; version=1.7") + part("data5", "") + "--" + b + "--\r\n"
	resp, out := e.do("POST", e.u("create", false, "", "contRep", "Z2", "docId", "CT1"), strings.NewReader(body),
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + b})
	e.expect(resp, out, 201)

	want := map[string]string{"data1.txt": "text/plain", "data2.txt": "text/plain; charset=utf-8", "data3.txt": "application/x-alf; charset=",
		"data4.txt": "application/pdf; charset=; version=1.7", "data5": ""}
	check := func(what string, got map[string]string) {
		for comp, w := range want {
			if got[comp] != w {
				t.Errorf("%s %s: Content-Type %q, want %q", what, comp, got[comp], w)
			}
		}
	}
	partTypes := func(cmd string) map[string]string {
		resp, out := e.do("GET", e.u(cmd, false, "", "contRep", "Z2", "docId", "CT1"), nil, nil)
		e.expect(resp, out, 200)
		got := map[string]string{}
		for _, p := range parseMultipart(t, resp, out) {
			got[p.h.Get("X-compId")] = p.h.Get("Content-Type")
		}
		return got
	}
	check("info", partTypes("info"))
	check("docGet", partTypes("docGet"))

	got := map[string]string{}
	for comp := range want {
		resp, out = e.do("GET", e.u("get", false, "", "contRep", "Z2", "docId", "CT1", "compId", comp), nil, nil)
		e.expect(resp, out, 200)
		got[comp] = resp.Header.Get("Content-Type")
	}
	check("get", got)

	// A zero-byte component without Content-Type: nothing is invented.
	req, _ := http.NewRequest("PUT", e.u("create", false, "", "contRep", "Z2", "docId", "CT0", "compId", "data"), strings.NewReader(""))
	req.Header.Del("Content-Type")
	r0, err := http.DefaultClient.Do(req)
	if err != nil || r0.StatusCode != 201 {
		t.Fatalf("create zero-byte component: %v %v", err, r0)
	}
	r0.Body.Close()
	resp, out = e.do("GET", e.u("get", false, "", "contRep", "Z2", "docId", "CT0", "compId", "data"), nil, nil)
	e.expect(resp, out, 200)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		t.Errorf("zero-byte component without Content-Type: got %q", ct)
	}
}
