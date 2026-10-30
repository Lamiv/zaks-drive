package contentrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/observability"
	"github.com/Lamiv/zaks-drive/internal/safelog"
	"github.com/Lamiv/zaks-drive/internal/storage"
)

// Behaviour follows "SAP Content Server HTTP 4.5 Interface" (ABAP Platform
// 2025, help.sap.com topic 4d0551f0eaa85c4be10000000a42189e); see
// docs/content-server-http-spec-notes.md for the extracted tables.

func ymd(t time.Time) string { return t.UTC().Format("2006-01-02") }
func hms(t time.Time) string { return t.UTC().Format("15:04:05") }

// asciiLine renders key="value";... CRLF with embedded quotes doubled.
func asciiLine(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString(kv[i])
		b.WriteString(`="`)
		b.WriteString(strings.ReplaceAll(kv[i+1], `"`, `""`))
		b.WriteString(`";`)
	}
	b.WriteString("\r\n")
	return b.String()
}

// parseContentType splits "type; charset=x; version=y" leniently (SAP may
// send empty parameter values, which mime.ParseMediaType rejects).
func parseContentType(v string) (mediaType, charset, version string) {
	parts := strings.Split(v, ";")
	mediaType = strings.TrimSpace(parts[0])
	for _, p := range parts[1:] {
		k, val, _ := strings.Cut(strings.TrimSpace(p), "=")
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "charset":
			charset = val
		case "version":
			version = val
		}
	}
	return
}

// contentTypeHeader returns the component's Content-Type exactly as SAP
// stored it, or "" when SAP sent none (the header is then omitted). Only a
// file that has no metadata (not written through this interface) falls back
// to the type SharePoint reports.
func (c *component) contentTypeHeader() string {
	if !c.hasMeta {
		return c.contentType()
	}
	if c.meta.Raw != "" {
		return c.meta.Raw
	}
	if c.meta.ContentType == "" {
		return ""
	}
	// Metadata written by earlier versions: rebuild from the parsed parts.
	ct := c.meta.ContentType
	if c.meta.Charset != "" {
		ct += "; charset=" + c.meta.Charset
	}
	if c.meta.Version != "" {
		ct += "; version=" + c.meta.Version
	}
	return ct
}

func metaFromRequest(contentType string, now time.Time, prev *compMeta) *compMeta {
	m := &compMeta{Created: now, Modified: now}
	if prev != nil {
		*m = *prev
		m.Modified = now
	}
	if contentType != "" {
		m.Raw = strings.TrimSpace(contentType)
		m.ContentType, m.Charset, m.Version = parseContentType(contentType)
	}
	return m
}

func setDocHeaders(h http.Header, cr *contentRep, d *document, pv string) {
	created, modified := d.created(), d.modified()
	h.Set("X-dateC", ymd(created))
	h.Set("X-timeC", hms(created))
	h.Set("X-dateM", ymd(modified))
	h.Set("X-timeM", hms(modified))
	// info names them X-contentRep/X-numberComps, docGet X-contRep/X-numComps.
	h.Set("X-contentRep", cr.id)
	h.Set("X-contRep", cr.id)
	n := strconv.Itoa(len(d.comps))
	h.Set("X-numberComps", n)
	h.Set("X-numComps", n)
	h.Set("X-docId", d.id)
	h.Set("X-docStatus", "online")
	h.Set("X-pVersion", pv)
}

func compPartHeader(c *component, pv string, bodyLen int64) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	if ct := c.contentTypeHeader(); ct != "" {
		h.Set("Content-Type", ct)
	}
	h.Set("Content-Length", strconv.FormatInt(bodyLen, 10))
	h.Set("X-compId", c.id)
	h.Set("X-Content-Length", strconv.FormatInt(c.item.Size, 10))
	h.Set("X-compDateC", ymd(c.meta.Created))
	h.Set("X-compTimeC", hms(c.meta.Created))
	h.Set("X-compDateM", ymd(c.meta.Modified))
	h.Set("X-compTimeM", hms(c.meta.Modified))
	h.Set("X-compStatus", "online")
	h.Set("X-pVersion", pv)
	return h
}

// loadDoc wraps docStore.load and maps "not found".
func (s *Server) loadDoc(r *http.Request, cr *contentRep, docID string) (*document, error) {
	d, err := s.docs.load(r.Context(), cr, docID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, apperr.New(apperr.CSDocNotFound, "document %q does not exist in content repository %s", docID, cr.id).Wrap(storage.ErrNotFound)
	}
	return d, err
}

func (s *Server) readCommon(w http.ResponseWriter, r *http.Request, p params, cr *contentRep) (*document, error) {
	if err := requireMethod(r, http.MethodGet, http.MethodHead); err != nil {
		return nil, err
	}
	if err := requireParam(p, "docId"); err != nil {
		return nil, err
	}
	if err := s.authBefore(r, p, cr, 'r'); err != nil {
		return nil, err
	}
	d, err := s.loadDoc(r, cr, p.get("docId"))
	if err != nil {
		return nil, err
	}
	return d, s.authAfter(r, p, cr, 'r', d.docProt())
}

// ---- info ----

func (s *Server) info(w http.ResponseWriter, r *http.Request, p params, cr *contentRep, pv string) error {
	d, err := s.readCommon(w, r, p, cr)
	if err != nil {
		return err
	}
	comps := d.comps
	if id := p.get("compId"); id != "" {
		c := d.comp(id)
		if c == nil {
			return apperr.New(apperr.CSCompNotFound, "document %q has no component %q (components: %s)", d.id, id, d.compNames())
		}
		comps = []*component{c}
	}
	setDocHeaders(w.Header(), cr, d, pv)

	if strings.EqualFold(p.get("resultAs"), "html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var b strings.Builder
		fmt.Fprintf(&b, "<html><body><h3>Document %s (%s)</h3><table border=1><tr><th>compId</th><th>Content-Type</th><th>Size</th><th>Created</th><th>Modified</th></tr>", html.EscapeString(d.id), html.EscapeString(cr.id))
		for _, c := range comps {
			fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>%d</td><td>%s %s</td><td>%s %s</td></tr>", html.EscapeString(c.id), html.EscapeString(c.contentTypeHeader()), c.item.Size,
				ymd(c.meta.Created), hms(c.meta.Created), ymd(c.meta.Modified), hms(c.meta.Modified))
		}
		b.WriteString("</table></body></html>")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, b.String())
		}
		return nil
	}

	// Default (resultAs=ascii): multipart/form-data, one empty part per
	// component; Content-Length 0, component size in X-Content-Length.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, c := range comps {
		if _, err := mw.CreatePart(compPartHeader(c, pv, 0)); err != nil {
			return err
		}
	}
	_ = mw.Close()
	w.Header().Set("Content-Type", "multipart/form-data; boundary="+mw.Boundary())
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
	return nil
}

// ---- get ----

func (s *Server) get(w http.ResponseWriter, r *http.Request, p params, cr *contentRep, pv string) error {
	d, err := s.readCommon(w, r, p, cr)
	if err != nil {
		return err
	}
	var c *component
	if id := p.get("compId"); id != "" {
		c = d.comp(id)
	} else if c = d.comp("data"); c == nil {
		c = d.comp("data1")
	}
	if c == nil {
		return apperr.New(apperr.CSCompNotFound, "document %q has no component %q (components: %s); without compId the component `data`, else `data1`, is returned", d.id, p.get("compId"), d.compNames())
	}
	from, to, err := offsets(p, c.item.Size)
	if err != nil {
		return err
	}
	length := int64(0)
	if c.item.Size > 0 {
		length = to - from + 1
	}
	h := w.Header()
	if ct := c.contentTypeHeader(); ct != "" {
		h.Set("Content-Type", ct)
	} else {
		h["Content-Type"] = nil // none stored: send none (and stop Go from sniffing one)
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	h.Set("X-compId", c.id)
	h.Set("X-Content-Length", strconv.FormatInt(c.item.Size, 10))
	h.Set("X-compDateC", ymd(c.meta.Created))
	h.Set("X-compTimeC", hms(c.meta.Created))
	h.Set("X-compDateM", ymd(c.meta.Modified))
	h.Set("X-compTimeM", hms(c.meta.Modified))
	h.Set("X-compStatus", "online")
	h.Set("X-docId", d.id)
	h.Set("X-contRep", cr.id)
	if r.Method == http.MethodHead || length == 0 {
		w.WriteHeader(http.StatusOK)
		return nil
	}
	rng := ""
	if from != 0 || to != c.item.Size-1 {
		rng = fmt.Sprintf("bytes=%d-%d", from, to)
	}
	resp, err := s.svc.Open(r.Context(), cr.repo, &c.item, rng)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	w.WriteHeader(http.StatusOK)
	if _, err := s.svc.Engine().Copy(r.Context(), w, resp.Body); err != nil && r.Context().Err() == nil {
		s.log.Warn("get interrupted", "contRep", cr.id, "docId", d.id, "compId", c.id, safelog.Err(err))
	}
	return nil
}

// offsets resolves fromOffset/toOffset (defaults 0 and -1 = end of
// component) to an inclusive byte range.
func offsets(p params, size int64) (from, to int64, err error) {
	from, to = 0, -1
	if v := p.get("fromOffset"); v != "" {
		if from, err = strconv.ParseInt(v, 10, 64); err != nil || from < 0 {
			return 0, 0, apperr.New(apperr.CSOffsetInvalid, "fromOffset %q is not a number >= 0", v)
		}
	}
	if v := p.get("toOffset"); v != "" {
		if to, err = strconv.ParseInt(v, 10, 64); err != nil || to < -1 {
			return 0, 0, apperr.New(apperr.CSOffsetInvalid, "toOffset %q is not a number >= -1", v)
		}
	}
	if size == 0 {
		return 0, -1, nil
	}
	if to == -1 || to >= size {
		to = size - 1
	}
	if from >= size || from > to {
		return 0, 0, apperr.New(apperr.CSOffsetInvalid, "the range %d-%d is outside the component (size %d bytes)", from, to, size)
	}
	return from, to, nil
}

// ---- docGet ----

func (s *Server) docGet(w http.ResponseWriter, r *http.Request, p params, cr *contentRep, pv string) error {
	d, err := s.readCommon(w, r, p, cr)
	if err != nil {
		return err
	}
	setDocHeaders(w.Header(), cr, d, pv)
	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", "multipart/form-data; boundary="+mw.Boundary())
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}
	for _, c := range d.comps {
		// Open before writing the part header so a failure does not leave a
		// half-written part.
		var body io.ReadCloser = io.NopCloser(strings.NewReader(""))
		if c.item.Size > 0 {
			resp, err := s.svc.Open(r.Context(), cr.repo, &c.item, "")
			if err != nil {
				s.log.Error("docGet: component unavailable", "docId", d.id, "compId", c.id, safelog.Err(err))
				return nil // headers already sent; abort the stream
			}
			body = resp.Body
		}
		pw, err := mw.CreatePart(compPartHeader(c, pv, c.item.Size))
		if err == nil {
			_, err = s.svc.Engine().Copy(r.Context(), pw, body)
		}
		body.Close()
		if err != nil {
			return nil
		}
	}
	_ = mw.Close()
	return nil
}

// ---- create ----

func (s *Server) create(w http.ResponseWriter, r *http.Request, p params, cr *contentRep, pv string) error {
	if err := requireParam(p, "docId"); err != nil {
		return err
	}
	if err := s.authBefore(r, p, cr, 'c'); err != nil {
		return err
	}
	if err := s.authAfter(r, p, cr, 'c', p.get("docProt")); err != nil {
		return err
	}
	switch r.Method {
	case http.MethodPut:
		return s.createPut(w, r, p, cr)
	case http.MethodPost:
		return s.createPost(w, r, p, cr)
	}
	return apperr.New(apperr.CSMethodNotAllowed, "create needs PUT (one component) or POST multipart/form-data, not %s", r.Method)
}

func newDocMeta(cr *contentRep, docID, docProt string, now time.Time) *docMeta {
	return &docMeta{DocID: docID, ContRep: cr.id, DocProt: docProt, Created: now, Modified: now, Components: map[string]*compMeta{}}
}

func (s *Server) createPut(w http.ResponseWriter, r *http.Request, p params, cr *contentRep) error {
	if err := requireParam(p, "compId"); err != nil {
		return err
	}
	docID, compID := p.get("docId"), p.get("compId")
	defer s.docs.lock(cr, docID)()
	now := s.now().UTC()
	// Look up existing document metadata while the component uploads; the
	// upload itself fails atomically if the component already exists.
	type loaded struct {
		d   *document
		err error
	}
	ch := make(chan loaded, 1)
	go func() {
		d, err := s.docs.load(r.Context(), cr, docID)
		ch <- loaded{d, err}
	}()
	item, err := s.docs.putComponent(r.Context(), cr, docID, compID, r.Body, requestLength(r, p), true)
	l := <-ch
	if err != nil {
		return err
	}
	if l.err != nil && !errors.Is(l.err, storage.ErrNotFound) {
		s.removeAfterFailedCreate(r, cr, docID, item)
		return l.err
	}
	cm := metaFromRequest(firstNonEmpty(r.Header.Get("Content-Type"), p.get("Content-Type")), now, nil)
	if cs := p.get("charset"); cs != "" && cm.Charset == "" {
		cm.Charset = cs
		if cm.Raw != "" {
			cm.Raw += "; charset=" + cs
		}
	}
	if v := p.get("version"); v != "" && cm.Version == "" {
		cm.Version = v
		if cm.Raw != "" {
			cm.Raw += "; version=" + v
		}
	}
	err = s.docs.saveMeta(r.Context(), cr, docID, l.d, func(m *docMeta, isNew bool) {
		if isNew {
			m.Created = now
			if m.DocProt == "" {
				m.DocProt = p.get("docProt")
			}
		}
		m.Modified = now
		c := *cm
		m.Components[compID] = &c
	})
	if err != nil {
		// Without its metadata the component would block SAP's retry with
		// 403 "already exists"; remove it so the create can be repeated.
		s.removeAfterFailedCreate(r, cr, docID, item)
		return err
	}
	w.Header().Set("X-docId", docID)
	w.WriteHeader(http.StatusCreated)
	return nil
}

// removeAfterFailedCreate deletes a component stored by a create whose
// metadata could not be written (best effort, logged on failure).
func (s *Server) removeAfterFailedCreate(r *http.Request, cr *contentRep, docID string, item *storage.Item) {
	if item == nil {
		return
	}
	if err := s.svc.Delete(context.WithoutCancel(r.Context()), cr.repo, item.ID, ""); err != nil {
		s.log.Error("create rollback failed; component left without metadata", "contRep", cr.id, "docId", docID, "item", item.ID, safelog.Err(err))
	}
	s.docs.invalidate(cr, docID)
}

// requestLength returns the body length from the header or, as in some SAP
// examples, from a Content-Length URL parameter; -1 when unknown.
func requestLength(r *http.Request, p params) int64 {
	if r.ContentLength >= 0 {
		return r.ContentLength
	}
	if v, err := strconv.ParseInt(p.get("Content-Length"), 10, 64); err == nil && v >= 0 {
		return v
	}
	return -1
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// partInfo extracts compId (X-compId, else Content-Disposition name),
// docId (mCreate), size and content type from a multipart part.
type partInfo struct {
	docID, compID, contentType string
	size                       int64
	body                       io.Reader
}

func readPartInfo(part *multipart.Part) partInfo {
	pi := partInfo{
		docID:       part.Header.Get("X-docId"),
		compID:      part.Header.Get("X-compId"),
		contentType: part.Header.Get("Content-Type"),
		size:        -1,
	}
	if pi.compID == "" {
		if _, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition")); err == nil {
			pi.compID = params["name"]
		}
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(part.Header.Get("Content-Length")), 10, 64); err == nil && v >= 0 {
		pi.size = v
	}
	return pi
}

// partReader iterates over the component parts of a multipart request.
//
// It is deliberately lenient where real SAP systems and the SAP spec differ:
//   - the spec's zero-component body ("--B" directly followed by "--B--")
//     is accepted (Go's parser rejects it);
//   - a part with no headers and no body is the empty-document marker and is
//     skipped;
//   - a part without X-compId falls back to Content-Disposition name, then to
//     the default component names "data", "data1", "data2", ... (a warning
//     with the part's headers is logged so the real format can be checked).
type partReader struct {
	s     *Server
	r     *http.Request
	cmd   string
	mr    *multipart.Reader
	empty bool
	idx   int
}

func (s *Server) newPartReader(r *http.Request, cmd string) (*partReader, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	boundary := params["boundary"]
	if err != nil || !strings.HasPrefix(mt, "multipart/") || boundary == "" {
		return nil, apperr.New(apperr.CSMultipartRequired, "Content-Type is %q; the %s command needs multipart/form-data with a boundary", r.Header.Get("Content-Type"), cmd)
	}
	br := bufio.NewReaderSize(r.Body, 64<<10)
	head, _ := br.Peek(2*len(boundary) + 16)
	return &partReader{s: s, r: r, cmd: cmd, mr: multipart.NewReader(br, boundary), empty: isEmptyMultipart(string(head), boundary)}, nil
}

// isEmptyMultipart reports whether head starts a multipart body without any
// part: nothing at all, or the opening boundary directly followed by the
// closing boundary.
func isEmptyMultipart(head, boundary string) bool {
	h := strings.TrimLeft(head, "\r\n ")
	if h == "" {
		return true
	}
	delim := "--" + boundary
	if !strings.HasPrefix(h, delim) {
		return false
	}
	rest := strings.TrimLeft(h[len(delim):], " \t")
	rest = strings.TrimPrefix(strings.TrimPrefix(rest, "\r"), "\n")
	return strings.HasPrefix(rest, delim+"--")
}

// next returns the next component part, or io.EOF when there are no more.
func (pr *partReader) next() (partInfo, error) {
	if pr.empty {
		return partInfo{}, io.EOF
	}
	for {
		part, err := pr.mr.NextRawPart()
		if err == io.EOF {
			return partInfo{}, io.EOF
		}
		if err != nil {
			return partInfo{}, apperr.New(apperr.CSMultipartInvalid, "the multipart body of %s could not be parsed: %v", pr.cmd, err)
		}
		pi := readPartInfo(part)
		pi.body = part
		idx := pr.idx
		pr.idx++
		if pi.compID != "" {
			pr.s.log.Debug("multipart part", "command", pr.cmd, "index", idx, "headers", safelog.HeaderValues(part.Header, "Content-Type", "Content-Length", "Content-Disposition", "X-compId", "X-docId", "X-Content-Length", "X-contRep", "X-pVersion"))
			return pi, nil
		}
		// No component ID. A part without headers and body is the marker for
		// an empty document, not a component.
		buf := bufio.NewReader(part)
		if _, perr := buf.Peek(1); perr == io.EOF && len(part.Header) == 0 {
			pr.s.log.Info("empty multipart part ignored (empty document)", "command", pr.cmd, "index", idx)
			continue
		}
		pi.body = buf
		if idx == 0 {
			pi.compID = "data"
		} else {
			pi.compID = "data" + strconv.Itoa(idx)
		}
		pr.s.log.Warn("multipart part without X-compId; using default component name",
			"command", pr.cmd, "index", idx, "compId", pi.compID, "headers", safelog.HeaderValues(part.Header, "Content-Type", "Content-Length", "Content-Disposition", "X-compId", "X-docId", "X-Content-Length", "X-contRep", "X-pVersion"),
			"request_id", observability.RequestID(pr.r.Context()))
		return pi, nil
	}
}

func (s *Server) createPost(w http.ResponseWriter, r *http.Request, p params, cr *contentRep) error {
	docID := p.get("docId")
	defer s.docs.lock(cr, docID)()
	if _, err := s.docs.load(r.Context(), cr, docID); err == nil {
		return docExists(cr, docID)
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	pr, err := s.newPartReader(r, "create")
	if err != nil {
		return err
	}
	now := s.now().UTC()
	meta := newDocMeta(cr, docID, p.get("docProt"), now)
	rollback := func(cause error) error {
		// "If an error occurs when storing a component, the entire action is canceled."
		if len(meta.Components) > 0 {
			if derr := s.docs.deleteDocument(context.WithoutCancel(r.Context()), cr, docID); derr != nil {
				s.log.Error("create rollback failed", "contRep", cr.id, "docId", docID, safelog.Err(derr))
			}
		}
		s.docs.invalidate(cr, docID)
		return cause
	}
	for {
		pi, err := pr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return rollback(err)
		}
		if meta.Components[pi.compID] != nil {
			return rollback(apperr.New(apperr.CSCompDuplicate, "component %q appears twice in the request", pi.compID))
		}
		if _, err := s.docs.putComponent(r.Context(), cr, docID, pi.compID, pi.body, pi.size, true); err != nil {
			return rollback(err)
		}
		meta.Components[pi.compID] = metaFromRequest(pi.contentType, now, nil)
	}
	if len(meta.Components) == 0 {
		if err := s.docs.createEmpty(r.Context(), cr, docID); err != nil {
			return rollback(err)
		}
	}
	if err := s.docs.saveMeta(r.Context(), cr, docID, nil, func(m *docMeta, isNew bool) {
		*m = *meta.clone()
	}); err != nil {
		if len(meta.Components) == 0 {
			_ = s.docs.deleteDocument(context.WithoutCancel(r.Context()), cr, docID)
		}
		return rollback(err)
	}
	w.Header().Set("X-docId", docID)
	w.WriteHeader(http.StatusCreated)
	return nil
}

// ---- mCreate ----

func (s *Server) mCreate(w http.ResponseWriter, r *http.Request, p params, cr *contentRep) error {
	if err := requireMethod(r, http.MethodPost); err != nil {
		return err
	}
	if err := s.authBefore(r, p, cr, 'c'); err != nil {
		return err
	}
	if err := s.authAfter(r, p, cr, 'c', p.get("docProt")); err != nil {
		return err
	}
	pr, err := s.newPartReader(r, "mCreate")
	if err != nil {
		return err
	}
	type result struct {
		docID, desc string
		code        int
	}
	var results []*result
	var cur *result
	var meta *docMeta
	unlock := func() {}
	defer func() { unlock() }()
	now := s.now().UTC()
	finish := func() {
		defer func() { unlock(); unlock = func() {} }()
		if cur == nil || cur.code != 0 {
			return
		}
		if err := s.docs.saveMeta(r.Context(), cr, cur.docID, nil, func(m *docMeta, isNew bool) {
			*m = *meta.clone()
		}); err != nil {
			cur.code, cur.desc = http.StatusInternalServerError, safelog.Summary(err)
			_ = s.docs.deleteDocument(context.WithoutCancel(r.Context()), cr, cur.docID)
		} else {
			cur.code = http.StatusCreated
		}
		s.docs.invalidate(cr, cur.docID)
	}
	for {
		pi, err := pr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if pi.docID == "" {
			return apperr.New(apperr.CSMCreateDocIDMissing, "a part of the mCreate request has no X-docId header (component %q)", pi.compID)
		}
		if cur == nil || cur.docID != pi.docID {
			finish()
			cur = &result{docID: pi.docID}
			results = append(results, cur)
			unlock = s.docs.lock(cr, pi.docID)
			meta = newDocMeta(cr, pi.docID, p.get("docProt"), now)
			if _, err := s.docs.load(r.Context(), cr, pi.docID); err == nil {
				cur.code, cur.desc = http.StatusForbidden, "document already exists"
			} else if !errors.Is(err, storage.ErrNotFound) {
				cur.code, cur.desc = http.StatusInternalServerError, safelog.Summary(err)
			}
		}
		if cur.code != 0 {
			continue // skip remaining parts of a failed/existing document
		}
		if _, err := s.docs.putComponent(r.Context(), cr, pi.docID, pi.compID, pi.body, pi.size, true); err != nil {
			cur.code, cur.desc = http.StatusInternalServerError, safelog.Summary(err)
			if errors.Is(err, errCompExists) {
				cur.code = http.StatusForbidden
			}
			_ = s.docs.deleteDocument(context.WithoutCancel(r.Context()), cr, pi.docID)
			s.docs.invalidate(cr, pi.docID)
			continue
		}
		meta.Components[pi.compID] = metaFromRequest(pi.contentType, now, nil)
	}
	finish()

	status := http.StatusCreated
	var b strings.Builder
	for _, res := range results {
		switch res.code {
		case http.StatusForbidden:
			if status == http.StatusCreated {
				status = 250 // "missing documents created"
			}
		case http.StatusInternalServerError:
			status = http.StatusInternalServerError
		}
		if res.desc != "" {
			b.WriteString(asciiLine("docId", res.docID, "retCode", strconv.Itoa(res.code), "errorDescription", res.desc))
		} else {
			b.WriteString(asciiLine("docId", res.docID, "retCode", strconv.Itoa(res.code)))
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=us-ascii")
	if status == http.StatusInternalServerError {
		w.Header().Set("X-ErrorDescription", "one or more documents could not be created")
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, b.String())
	return nil
}

// ---- update ----

func (s *Server) update(w http.ResponseWriter, r *http.Request, p params, cr *contentRep, pv string) error {
	if err := requireParam(p, "docId"); err != nil {
		return err
	}
	if err := s.authBefore(r, p, cr, 'u'); err != nil {
		return err
	}
	defer s.docs.lock(cr, p.get("docId"))()
	d, err := s.loadDoc(r, cr, p.get("docId"))
	if err != nil {
		return err
	}
	if err := s.authAfter(r, p, cr, 'u', d.docProt()); err != nil {
		return err
	}
	defer s.docs.invalidate(cr, d.id)
	now := s.now().UTC()
	// Metadata changes are collected and applied in saveMeta so they can be
	// re-applied on top of a concurrent writer's sidecar.
	written := map[string]string{} // compId -> Content-Type sent
	var removed []string

	switch r.Method {
	case http.MethodPut:
		// Create or overwrite a single component of an existing document.
		if err := requireParam(p, "compId"); err != nil {
			return err
		}
		compID := p.get("compId")
		if err := s.writeComponent(r, cr, d, compID, r.Body, requestLength(r, p)); err != nil {
			return err
		}
		written[compID] = r.Header.Get("Content-Type")
	case http.MethodPost:
		// The whole document is replaced: components not transferred are deleted.
		pr, err := s.newPartReader(r, "update")
		if err != nil {
			return err
		}
		sent := map[string]bool{}
		for {
			pi, err := pr.next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if err := s.writeComponent(r, cr, d, pi.compID, pi.body, pi.size); err != nil {
				return err
			}
			sent[pi.compID] = true
			written[pi.compID] = pi.contentType
		}
		for _, c := range d.comps {
			if !sent[c.id] {
				if err := s.docs.deleteComponent(r.Context(), cr, c); err != nil && !errors.Is(err, storage.ErrNotFound) {
					return err
				}
				removed = append(removed, c.id)
			}
		}
	default:
		return apperr.New(apperr.CSMethodNotAllowed, "update needs PUT (one component) or POST multipart/form-data, not %s", r.Method)
	}
	if err := s.docs.saveMeta(r.Context(), cr, d.id, d, func(m *docMeta, isNew bool) {
		m.Modified = now
		for id, ct := range written {
			m.Components[id] = metaFromRequest(ct, now, m.Components[id])
		}
		for _, id := range removed {
			delete(m.Components, id)
		}
	}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// writeComponent overwrites an existing component or creates a new one.
func (s *Server) writeComponent(r *http.Request, cr *contentRep, d *document, compID string, body io.Reader, size int64) error {
	if c := d.comp(compID); c != nil {
		_, err := s.docs.replaceComponent(r.Context(), cr, c, body, size)
		return err
	}
	_, err := s.docs.putComponent(r.Context(), cr, d.id, compID, body, size, false)
	return err
}

// ---- append ----

func (s *Server) appendComp(w http.ResponseWriter, r *http.Request, p params, cr *contentRep, pv string) error {
	if err := requireMethod(r, http.MethodPut); err != nil {
		return err
	}
	if err := requireParam(p, "docId", "compId"); err != nil {
		return err
	}
	if err := s.authBefore(r, p, cr, 'u'); err != nil {
		return err
	}
	defer s.docs.lock(cr, p.get("docId"))()
	d, err := s.loadDoc(r, cr, p.get("docId"))
	if err != nil {
		return err
	}
	if err := s.authAfter(r, p, cr, 'u', d.docProt()); err != nil {
		return err
	}
	c := d.comp(p.get("compId"))
	if c == nil {
		return apperr.New(apperr.CSCompNotFound, "document %q has no component %q (components: %s)", d.id, p.get("compId"), d.compNames())
	}
	defer s.docs.invalidate(cr, d.id)

	// SharePoint has no append: stream existing content followed by the new
	// data into a new version of the same file.
	var existing io.Reader = strings.NewReader("")
	if c.item.Size > 0 {
		resp, err := s.svc.Open(r.Context(), cr.repo, &c.item, "")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		existing = resp.Body
	}
	add := requestLength(r, p)
	total := int64(-1)
	if add >= 0 {
		total = c.item.Size + add
	}
	if _, err := s.docs.replaceComponent(r.Context(), cr, c, io.MultiReader(existing, r.Body), total); err != nil {
		return err
	}
	now := s.now().UTC()
	if err := s.docs.saveMeta(r.Context(), cr, d.id, d, func(m *docMeta, isNew bool) {
		m.Modified = now
		cm := m.Components[c.id]
		if cm == nil {
			cp := c.meta
			cm = &cp
			m.Components[c.id] = cm
		}
		cm.Modified = now
	}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// ---- delete ----

func (s *Server) deleteDoc(w http.ResponseWriter, r *http.Request, p params, cr *contentRep, pv string) error {
	// The spec uses HTTP GET; DELETE is accepted as well.
	if err := requireMethod(r, http.MethodGet, http.MethodDelete); err != nil {
		return err
	}
	if err := requireParam(p, "docId"); err != nil {
		return err
	}
	if err := s.authBefore(r, p, cr, 'd'); err != nil {
		return err
	}
	defer s.docs.lock(cr, p.get("docId"))()
	d, err := s.loadDoc(r, cr, p.get("docId"))
	if err != nil {
		return err
	}
	if err := s.authAfter(r, p, cr, 'd', d.docProt()); err != nil {
		return err
	}
	defer s.docs.invalidate(cr, d.id)
	if id := p.get("compId"); id != "" {
		c := d.comp(id)
		if c == nil {
			return apperr.New(apperr.CSCompNotFound, "document %q has no component %q (components: %s)", d.id, id, d.compNames())
		}
		if err := s.docs.deleteComponent(r.Context(), cr, c); err != nil {
			return err
		}
		// The component is gone; a stale sidecar entry is harmless (pruned on
		// the next load), so a failed metadata write does not fail the delete.
		now := s.now().UTC()
		if err := s.docs.saveMeta(r.Context(), cr, d.id, d, func(m *docMeta, isNew bool) {
			delete(m.Components, id)
			m.Modified = now
		}); err != nil {
			s.log.Warn("component deleted but metadata not updated", "contRep", cr.id, "docId", d.id, "compId", id, safelog.Err(err))
		}
	} else if err := s.docs.deleteDocument(r.Context(), cr, d.id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// ---- search ----

const maxBackwardSearch = 64 << 20

func (s *Server) search(w http.ResponseWriter, r *http.Request, p params, cr *contentRep, pv string) error {
	if err := requireParam(p, "compId", "pattern"); err != nil {
		return err
	}
	d, err := s.readCommon(w, r, p, cr)
	if err != nil {
		return err
	}
	c := d.comp(p.get("compId"))
	if c == nil {
		return apperr.New(apperr.CSCompNotFound, "document %q has no component %q (components: %s)", d.id, p.get("compId"), d.compNames())
	}
	pattern := []byte(p.get("pattern"))
	caseSensitive := strings.EqualFold(p.get("caseSensitive"), "y")
	numResults := 1
	if v := p.get("numResults"); v != "" {
		if numResults, err = strconv.Atoi(v); err != nil || numResults < 1 {
			return apperr.New(apperr.CSNumResultsInvalid, "numResults %q is not a whole number >= 1", p.get("numResults"))
		}
	}
	from, to := int64(0), int64(-1)
	if v := p.get("fromOffset"); v != "" {
		from, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := p.get("toOffset"); v != "" {
		to, _ = strconv.ParseInt(v, 10, 64)
	}
	backward := to >= 0 && from > to
	lo, hi := from, to
	if backward {
		lo, hi = to, from
	}
	if hi < 0 || hi >= c.item.Size {
		hi = c.item.Size - 1
	}
	var hits []int64
	if lo <= hi && c.item.Size > 0 && len(pattern) > 0 {
		if backward && hi-lo+1 > maxBackwardSearch {
			return apperr.New(apperr.CSSearchRangeTooLarge, "a backward search over %d bytes exceeds the %d byte limit", hi-lo+1, maxBackwardSearch)
		}
		resp, err := s.svc.Open(r.Context(), cr.repo, &c.item, fmt.Sprintf("bytes=%d-%d", lo, hi))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if backward {
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			hits = searchBackward(data, lo, pattern, caseSensitive, numResults)
		} else {
			hits, err = searchForward(resp.Body, lo, pattern, caseSensitive, numResults)
			if err != nil {
				return err
			}
		}
	}
	var b strings.Builder
	b.WriteString(strconv.Itoa(len(hits)))
	b.WriteByte(';')
	for _, h := range hits {
		b.WriteString(strconv.FormatInt(h, 10))
		b.WriteByte(';')
	}
	w.Header().Set("Content-Type", "text/plain; charset=us-ascii")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, b.String())
	return nil
}

func foldASCII(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

// searchForward streams rd (starting at absolute offset base) and returns
// the offsets of up to max matches.
func searchForward(rd io.Reader, base int64, pat []byte, caseSensitive bool, max int) ([]int64, error) {
	if !caseSensitive {
		pat = foldASCII(pat)
	}
	var hits []int64
	buf := make([]byte, 0, 1<<20+len(pat))
	chunk := make([]byte, 1<<20)
	bufStart := base // absolute offset of buf[0]
	for {
		n, err := rd.Read(chunk)
		if n > 0 {
			data := chunk[:n]
			if !caseSensitive {
				data = foldASCII(data)
			}
			buf = append(buf, data...)
			searched := 0
			for {
				i := bytes.Index(buf[searched:], pat)
				if i < 0 {
					break
				}
				hits = append(hits, bufStart+int64(searched+i))
				if len(hits) >= max {
					return hits, nil
				}
				searched += i + 1
			}
			// Keep a tail that could start a match spanning the next read.
			keep := min(len(pat)-1, len(buf))
			if searched > len(buf)-keep {
				keep = len(buf) - searched
			}
			drop := len(buf) - keep
			bufStart += int64(drop)
			buf = append(buf[:0], buf[drop:]...)
		}
		if err == io.EOF {
			return hits, nil
		}
		if err != nil {
			return hits, err
		}
	}
}

func searchBackward(data []byte, base int64, pat []byte, caseSensitive bool, max int) []int64 {
	if !caseSensitive {
		data, pat = foldASCII(data), foldASCII(pat)
	}
	var hits []int64
	end := len(data)
	for len(hits) < max {
		i := bytes.LastIndex(data[:end], pat)
		if i < 0 {
			break
		}
		hits = append(hits, base+int64(i))
		end = i + len(pat) - 1
	}
	return hits
}

// ---- putCert ----

func (s *Server) putCert(w http.ResponseWriter, r *http.Request, p params, cr *contentRep) error {
	if err := requireMethod(r, http.MethodPut, http.MethodPost); err != nil {
		return err
	}
	if err := requireParam(p, "authId"); err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		return err
	}
	cert, err := parseCertificateLenient(body)
	if err != nil {
		return apperr.New(apperr.CSCertNotRecognized, "the putCert body (%d bytes) is not an X.509 certificate: %v", len(body), err)
	}
	rec, err := s.certs.put(r.Context(), cr, p.get("authId"), cert, s.cfg.AutoActivateCertificates)
	if err != nil {
		return err
	}
	s.log.Info("certificate received via putCert", "contRep", cr.id, "authId", rec.AuthID,
		"subject", rec.Subject, "fingerprint", rec.Fingerprint, "active", rec.Active)
	w.WriteHeader(http.StatusOK)
	return nil
}

// ---- serverInfo ----

func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request, p params, pv string) {
	ids := s.order
	if id := p.get("contRep"); id != "" {
		if _, ok := s.reps[id]; !ok {
			s.fail(w, r, apperr.New(apperr.CSContRepUnknown, "content repository %q is not configured; configured: %s", id, strings.Join(s.order, ", ")))
			return
		}
		ids = []string{id}
	}
	now := s.now()
	if strings.EqualFold(p.get("resultAs"), "html") {
		var b strings.Builder
		fmt.Fprintf(&b, "<html><body><h3>ZAKS Drive content server %s</h3><p>Status: running, %s %s UTC</p><table border=1><tr><th>contRep</th><th>Description</th><th>Status</th></tr>",
			html.EscapeString(s.version), ymd(now), hms(now))
		for _, id := range ids {
			fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>running</td></tr>", html.EscapeString(id), html.EscapeString(s.reps[id].cfg.Description))
		}
		b.WriteString("</table></body></html>")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, b.String())
		return
	}
	var b strings.Builder
	b.WriteString(asciiLine(
		"serverStatus", "running",
		"serverVendorId", "SharePointAdapter",
		"serverVersion", s.version,
		"serverBuild", s.version,
		"serverTime", hms(now),
		"serverDate", ymd(now),
		"serverStatusDescription", "",
		"pVersion", pv,
	))
	for _, id := range ids {
		b.WriteString(asciiLine(
			"contRep", id,
			"contRepDescription", s.reps[id].cfg.Description,
			"contRepStatus", "running",
			"contRepStatusDescription", "",
			"pVersion", pv,
		))
	}
	w.Header().Set("Content-Type", "text/plain; charset=us-ascii")
	_, _ = io.WriteString(w, b.String())
}
