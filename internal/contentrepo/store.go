package contentrepo

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/safelog"
	"github.com/Lamiv/zaks-drive/internal/storage"
)

// Storage layout in SharePoint, below the repository root:
//
//	<folder>/<shard>/<docId>/<compId>      one file per component
//	<folder>/<shard>/<docId>/~sapdoc.json  sidecar: content types, docProt, timestamps
//	<folder>/~certs/<hash>.json            certificates received via putCert
//
// shard is the first byte (hex) of SHA-1(docId); it spreads documents over
// 256 folders so no SharePoint folder grows unbounded.

const sidecarName = "~sapdoc.json"

// Sentinels kept for errors.Is; the errors carry catalogue codes.
var (
	errDocExists  = errors.New("document already exists")
	errCompExists = errors.New("component already exists")
	errBusy       = errors.New("concurrent modification; retry")
)

func docExists(cr *contentRep, docID string) error {
	return apperr.New(apperr.CSDocExists, "document %q already exists in content repository %s", docID, cr.id).Wrap(errDocExists)
}

func compExists(docID, compID string) error {
	return apperr.New(apperr.CSCompExists, "component %q of document %q already exists", compID, docID).Wrap(errCompExists)
}

func busy(docID string) error {
	return apperr.New(apperr.CSBusy, "document %q is being modified by other requests; gave up after repeated conflicts", docID).Wrap(errBusy)
}

// compNames lists the component IDs, for error messages.
func (d *document) compNames() string {
	if len(d.comps) == 0 {
		return "none"
	}
	names := make([]string, len(d.comps))
	for i, c := range d.comps {
		names[i] = c.id
	}
	return strings.Join(names, ", ")
}

type contentRep struct {
	id     string
	cfg    config.ContentRepositoryConfig
	repo   *storage.Repository
	folder string // repository-relative

	shardsMu sync.Mutex
	shards   map[string]bool // shard folders known to exist
	baseOK   bool
}

type compMeta struct {
	// Raw is the Content-Type exactly as SAP sent it on create/update. It is
	// returned unchanged: SAP's own test (RSCMSTH2) compares it literally,
	// so "text/plain" must not come back as "text/plain; charset=".
	Raw         string    `json:"raw,omitempty"`
	ContentType string    `json:"contentType,omitempty"`
	Charset     string    `json:"charset,omitempty"`
	Version     string    `json:"version,omitempty"`
	Created     time.Time `json:"created"`
	Modified    time.Time `json:"modified"`
}

type docMeta struct {
	DocID      string               `json:"docId"`
	ContRep    string               `json:"contRep"`
	DocProt    string               `json:"docProt,omitempty"`
	Created    time.Time            `json:"created"`
	Modified   time.Time            `json:"modified"`
	Components map[string]*compMeta `json:"components"`
}

type component struct {
	id      string
	item    storage.Item
	meta    compMeta
	hasMeta bool // the sidecar describes this component (it was written through this interface)
}

func (c *component) contentType() string {
	if c.meta.ContentType != "" {
		return c.meta.ContentType
	}
	if c.item.MimeType != "" {
		return c.item.MimeType
	}
	return "application/octet-stream"
}

type document struct {
	id      string
	meta    *docMeta
	sidecar *storage.Item
	comps   []*component // sorted by compId
}

func (d *document) comp(id string) *component {
	for _, c := range d.comps {
		if c.id == id {
			return c
		}
	}
	return nil
}

func (d *document) created() time.Time {
	if !d.meta.Created.IsZero() {
		return d.meta.Created
	}
	var t time.Time
	for _, c := range d.comps {
		if t.IsZero() || c.meta.Created.Before(t) {
			t = c.meta.Created
		}
	}
	return t
}

func (d *document) modified() time.Time {
	t := d.meta.Modified
	for _, c := range d.comps {
		if c.meta.Modified.After(t) {
			t = c.meta.Modified
		}
	}
	return t
}

// ---- name encoding ----

// encodeName maps an SAP docId/compId to a SharePoint-safe file name,
// reversibly. Characters SharePoint rejects (and '%', '~', '#') become %XX;
// so do a leading/trailing space and a trailing dot. A leading '~' is always
// escaped, which keeps "~sapdoc.json" and "~certs" out of the SAP namespace.
func encodeName(s string) (string, error) {
	if s == "" {
		return "", apperr.New(apperr.CSIDInvalid, "the document or component ID is empty").Wrap(storage.ErrInvalid)
	}
	lower := strings.ToLower(s)
	escUnderscore := strings.Contains(lower, "_vti_")
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		esc := c < 0x20 || c == 0x7f || strings.IndexByte("\"*:<>?/\\|%~#", c) >= 0 ||
			(c == ' ' && (i == 0 || i == len(s)-1)) ||
			(c == '.' && i == len(s)-1) ||
			(c == '_' && escUnderscore)
		if esc {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	if b.Len() > 255 {
		return "", apperr.New(apperr.CSIDInvalid, "the identifier %.40q... is %d characters after encoding; the limit is 255", s, b.Len()).Wrap(storage.ErrInvalid)
	}
	return b.String(), nil
}

func decodeName(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := hex.DecodeString(s[i+1 : i+3]); err == nil {
				b.WriteByte(v[0])
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func shardOf(docID string) string {
	sum := sha1.Sum([]byte(docID))
	return hex.EncodeToString(sum[:1])
}

func (cr *contentRep) docFolder(docID string) (string, error) {
	enc, err := encodeName(docID)
	if err != nil {
		return "", err
	}
	return cr.folder + "/" + shardOf(docID) + "/" + enc, nil
}

// ---- document cache ----

type docCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]docCacheEntry
}

type docCacheEntry struct {
	doc     *document
	expires time.Time
}

func (c *docCache) get(key string) *document {
	if c.ttl <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Now().After(e.expires) {
		delete(c.m, key)
		return nil
	}
	return e.doc
}

func (c *docCache) put(key string, d *document) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 20000 {
		for k := range c.m {
			delete(c.m, k)
			if len(c.m) < 18000 {
				break
			}
		}
	}
	c.m[key] = docCacheEntry{doc: d, expires: time.Now().Add(c.ttl)}
}

func (c *docCache) drop(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

// ---- document store ----

type docStore struct {
	svc   *storage.Service
	cache *docCache
	locks keyedMutex
}

func cacheKey(cr *contentRep, docID string) string { return cr.id + "|" + docID }

// keyedMutex serialises modifications of one document inside this process.
// Across instances, sidecar writes are additionally guarded by If-Match.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedEntry{}
	}
	e := k.m[key]
	if e == nil {
		e = &keyedEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// lock serialises writers of one document (in this process).
func (s *docStore) lock(cr *contentRep, docID string) func() {
	return s.locks.lock(cacheKey(cr, docID))
}

func (m *docMeta) clone() *docMeta {
	cp := *m
	cp.Components = make(map[string]*compMeta, len(m.Components))
	for k, v := range m.Components {
		c := *v
		cp.Components[k] = &c
	}
	return &cp
}

// clone returns a deep copy so callers may modify it without affecting the
// cached instance shared with concurrent requests.
func (d *document) clone() *document {
	cp := *d
	cp.meta = d.meta.clone()
	if d.sidecar != nil {
		sc := *d.sidecar
		cp.sidecar = &sc
	}
	cp.comps = make([]*component, len(d.comps))
	for i, c := range d.comps {
		cc := *c
		cp.comps[i] = &cc
	}
	return &cp
}

// docProt is the document's security level. A document with components but
// no sidecar was not written through this interface (or its metadata is
// missing); the spec gives such documents the highest level.
func (d *document) docProt() string {
	if d.sidecar == nil && len(d.comps) > 0 {
		return "rcud"
	}
	return d.meta.DocProt
}

// errMetaUnavailable maps to 409 "administrative data inaccessible".
var errMetaUnavailable = errors.New("document administration data (~sapdoc.json) is unreadable")

// load reads a document: one folder listing plus the sidecar. The result is
// a private copy.
func (s *docStore) load(ctx context.Context, cr *contentRep, docID string) (*document, error) {
	key := cacheKey(cr, docID)
	if d := s.cache.get(key); d != nil {
		return d.clone(), nil
	}
	d, err := s.loadFresh(ctx, cr, docID)
	if err != nil {
		return nil, err
	}
	s.cache.put(key, d.clone())
	return d, nil
}

func (s *docStore) loadFresh(ctx context.Context, cr *contentRep, docID string) (*document, error) {
	folder, err := cr.docFolder(docID)
	if err != nil {
		return nil, err
	}
	items, err := s.svc.ListAll(ctx, cr.repo, folder)
	if err != nil {
		return nil, err
	}
	d := &document{id: docID, meta: &docMeta{DocID: docID, ContRep: cr.id, Components: map[string]*compMeta{}}}
	for i := range items {
		if items[i].Name == sidecarName {
			d.sidecar = &items[i]
		}
	}
	if d.sidecar != nil {
		m, err := s.readSidecar(ctx, cr, d.sidecar)
		if err != nil {
			// Fail closed: without the sidecar the document's protection
			// level (docProt) and content types are unknown.
			return nil, apperr.New(apperr.CSMetaUnreadable, "the metadata file of document %q cannot be read: %v", docID, err).Wrap(errMetaUnavailable)
		}
		d.meta = m
	}
	present := map[string]bool{}
	for i := range items {
		it := items[i]
		if it.Folder || it.Name == sidecarName {
			continue
		}
		id := decodeName(it.Name)
		present[id] = true
		c := &component{id: id, item: it, meta: compMeta{Created: it.Created, Modified: it.Modified}}
		if m := d.meta.Components[id]; m != nil {
			c.meta = *m
			c.hasMeta = true
		}
		d.comps = append(d.comps, c)
	}
	// The listing is the source of truth for which components exist; drop
	// sidecar entries left behind by an interrupted delete.
	for id := range d.meta.Components {
		if !present[id] {
			delete(d.meta.Components, id)
		}
	}
	sort.Slice(d.comps, func(i, j int) bool { return d.comps[i].id < d.comps[j].id })
	if d.meta.Created.IsZero() {
		d.meta.Created = d.created()
	}
	return d, nil
}

func (s *docStore) readSidecar(ctx context.Context, cr *contentRep, it *storage.Item) (*docMeta, error) {
	resp, err := s.svc.Open(ctx, cr.repo, it, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m docMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&m); err != nil {
		return nil, err
	}
	if m.Components == nil {
		m.Components = map[string]*compMeta{}
	}
	return &m, nil
}

// saveMeta applies fn to the document's metadata and writes the sidecar
// with optimistic concurrency: an existing sidecar is replaced only if its
// eTag is unchanged (If-Match), a new one only if none exists yet. On a
// conflict the document is reloaded and fn re-applied, so concurrent
// writers on other instances do not lose each other's changes. Retries use
// jittered backoff; after 12 collisions the request fails with 409.
//
// d is the caller's current view (nil = document not found); fn receives
// isNew=true when no sidecar exists yet.
func (s *docStore) saveMeta(ctx context.Context, cr *contentRep, docID string, d *document, fn func(m *docMeta, isNew bool)) error {
	defer s.invalidate(cr, docID)
	folder, err := cr.docFolder(docID)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 12; attempt++ {
		if attempt > 0 {
			// Jittered backoff keeps colliding writers from retrying in lockstep.
			wait := time.Duration(attempt) * 10 * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait/2 + time.Duration(rand.Int64N(int64(wait)))):
			}
			d, err = s.loadFresh(ctx, cr, docID)
			if errors.Is(err, storage.ErrNotFound) {
				d = nil
			} else if err != nil {
				return err
			}
		}
		var m *docMeta
		isNew := d == nil || d.sidecar == nil
		if d != nil {
			m = d.meta.clone()
			if d.sidecar == nil && len(d.comps) > 0 {
				m.DocProt = "rcud" // keep the fail-closed level of a legacy document
			}
		} else {
			m = &docMeta{DocID: docID, ContRep: cr.id, Components: map[string]*compMeta{}}
		}
		fn(m, isNew)
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		req := storage.UploadRequest{Folder: folder, Name: sidecarName, Body: bytes.NewReader(b), Size: int64(len(b))}
		if isNew {
			req.Conflict = storage.ConflictFail
		} else {
			req.Conflict, req.IfMatch = storage.ConflictReplace, d.sidecar.ETag
		}
		_, err = s.svc.Upload(ctx, cr.repo, req)
		if err == nil {
			return nil
		}
		if !graph.IsConflict(err) && graph.StatusOf(err) != http.StatusPreconditionFailed {
			return err
		}
	}
	return busy(docID)
}

// ensureShard creates the shard folder once per process so concurrent
// first writes into a shard do not race on implicit folder creation.
func (s *docStore) ensureShard(ctx context.Context, cr *contentRep, docID string) error {
	shard := shardOf(docID)
	cr.shardsMu.Lock()
	known, baseOK := cr.shards[shard], cr.baseOK
	cr.shardsMu.Unlock()
	if known {
		return nil
	}
	if !baseOK {
		if err := s.svc.EnsureFolder(ctx, cr.repo, cr.folder, false); err != nil {
			return err
		}
	}
	if err := s.svc.EnsureFolder(ctx, cr.repo, cr.folder+"/"+shard, true); err != nil {
		return err
	}
	cr.shardsMu.Lock()
	cr.baseOK = true
	cr.shards[shard] = true
	cr.shardsMu.Unlock()
	return nil
}

// putComponent uploads a component. With mustBeNew, an existing component
// yields errCompExists instead of being overwritten.
func (s *docStore) putComponent(ctx context.Context, cr *contentRep, docID, compID string, body io.Reader, size int64, mustBeNew bool) (*storage.Item, error) {
	folder, err := cr.docFolder(docID)
	if err != nil {
		return nil, err
	}
	name, err := encodeName(compID)
	if err != nil {
		return nil, err
	}
	if err := s.ensureShard(ctx, cr, docID); err != nil {
		return nil, err
	}
	conflict := storage.ConflictReplace
	if mustBeNew {
		conflict = storage.ConflictFail
	}
	it, err := s.svc.Upload(ctx, cr.repo, storage.UploadRequest{Folder: folder, Name: name, Conflict: conflict, Body: body, Size: size})
	if graph.IsConflict(err) {
		if _, gerr := s.svc.GetByPath(ctx, cr.repo, folder+"/"+name); gerr == nil {
			return nil, compExists(docID, compID)
		}
		// 409 without an existing file: a parallel request created the
		// document folder at the same moment.
		return nil, busy(docID)
	}
	return it, err
}

func (s *docStore) replaceComponent(ctx context.Context, cr *contentRep, c *component, body io.Reader, size int64) (*storage.Item, error) {
	return s.svc.Replace(ctx, cr.repo, c.item.ID, "", body, size)
}

func (s *docStore) deleteComponent(ctx context.Context, cr *contentRep, c *component) error {
	return s.svc.Delete(ctx, cr.repo, c.item.ID, "")
}

func (s *docStore) deleteDocument(ctx context.Context, cr *contentRep, docID string) error {
	folder, err := cr.docFolder(docID)
	if err != nil {
		return err
	}
	it, err := s.svc.GetByPath(ctx, cr.repo, folder)
	if err != nil {
		return err
	}
	return s.svc.Delete(ctx, cr.repo, it.ID, "")
}

func (s *docStore) createEmpty(ctx context.Context, cr *contentRep, docID string) error {
	folder, err := cr.docFolder(docID)
	if err != nil {
		return err
	}
	if err := s.ensureShard(ctx, cr, docID); err != nil {
		return err
	}
	return s.svc.EnsureFolder(ctx, cr.repo, folder, true)
}

func (s *docStore) invalidate(cr *contentRep, docID string) { s.cache.drop(cacheKey(cr, docID)) }

// The messages of these errors are constants of this program, so they may be logged.
func init() { safelog.Allow(errDocExists, errCompExists, errBusy, errMetaUnavailable) }
