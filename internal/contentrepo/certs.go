package contentrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/safelog"
	"github.com/Lamiv/zaks-drive/internal/storage"
)

// certRecord is a certificate SAP sent via putCert. It is persisted in
// SharePoint (<contRep folder>/~certs/) so all adapter instances share it.
type certRecord struct {
	ContRep     string     `json:"contRep"`
	AuthID      string     `json:"authId"`
	CertDER     []byte     `json:"certificate"`
	Subject     string     `json:"subject"`
	Issuer      string     `json:"issuer"`
	Algorithm   string     `json:"publicKeyAlgorithm"`
	NotAfter    time.Time  `json:"notAfter"`
	Fingerprint string     `json:"sha256Fingerprint"`
	ReceivedAt  time.Time  `json:"receivedAt"`
	Active      bool       `json:"active"`
	ActivatedAt *time.Time `json:"activatedAt,omitempty"`

	cert *x509.Certificate
}

const certsFolder = "~certs"

type certStore struct {
	svc  *storage.Service
	reps map[string]*contentRep
	log  *slog.Logger

	mu       sync.RWMutex
	recs     map[string]*certRecord // contRep|authId
	loadedAt time.Time
	reloadMu sync.Mutex
}

func newCertStore(svc *storage.Service, reps map[string]*contentRep, log *slog.Logger) *certStore {
	return &certStore{svc: svc, reps: reps, log: log, recs: map[string]*certRecord{}}
}

func certKey(contRep, authID string) string { return contRep + "|" + authID }

func certFileName(authID string) string {
	sum := sha256.Sum256([]byte(authID))
	return hex.EncodeToString(sum[:12]) + ".json"
}

// lookup returns the stored certificate for contRep/authId. authId matching
// falls back to case-insensitive comparison (SAP may vary CN casing).
func (c *certStore) lookup(contRep, authID string) *certRecord {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if r, ok := c.recs[certKey(contRep, authID)]; ok {
		return r
	}
	for _, r := range c.recs {
		if r.ContRep == contRep && strings.EqualFold(r.AuthID, authID) {
			return r
		}
	}
	return nil
}

func (c *certStore) list() []certRecord {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]certRecord, 0, len(c.recs))
	for _, r := range c.recs {
		cp := *r
		cp.CertDER = nil
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ContRep != out[j].ContRep {
			return out[i].ContRep < out[j].ContRep
		}
		return out[i].AuthID < out[j].AuthID
	})
	return out
}

// reload refreshes certificates from SharePoint. When force is false it is
// skipped if the cache is younger than maxAge (keeps signature failures from
// hammering Graph).
func (c *certStore) reload(ctx context.Context, maxAge time.Duration) error {
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()
	c.mu.RLock()
	fresh := time.Since(c.loadedAt) < maxAge
	c.mu.RUnlock()
	if fresh {
		return nil
	}
	recs := map[string]*certRecord{}
	var errs []error
	for _, cr := range c.reps {
		items, err := c.svc.ListAll(ctx, cr.repo, cr.folder+"/"+certsFolder)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for i := range items {
			rec, err := c.read(ctx, cr, &items[i])
			if err != nil {
				c.log.Warn("ignoring unreadable certificate record", "contRep", cr.id, "file", items[i].Name, safelog.Err(err))
				continue
			}
			recs[certKey(rec.ContRep, rec.AuthID)] = rec
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	c.mu.Lock()
	c.recs = recs
	c.loadedAt = time.Now()
	c.mu.Unlock()
	return nil
}

func (c *certStore) read(ctx context.Context, cr *contentRep, it *storage.Item) (*certRecord, error) {
	if it.Folder || !strings.HasSuffix(it.Name, ".json") {
		return nil, errors.New("not a certificate record")
	}
	resp, err := c.svc.Open(ctx, cr.repo, it, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var rec certRecord
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rec); err != nil {
		return nil, err
	}
	if rec.cert, err = x509.ParseCertificate(rec.CertDER); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (c *certStore) write(ctx context.Context, cr *contentRep, rec *certRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	_, err = c.svc.Upload(ctx, cr.repo, storage.UploadRequest{
		Folder:   cr.folder + "/" + certsFolder,
		Name:     certFileName(rec.AuthID),
		Conflict: storage.ConflictReplace,
		Body:     bytes.NewReader(b),
		Size:     int64(len(b)),
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.recs[certKey(rec.ContRep, rec.AuthID)] = rec
	c.mu.Unlock()
	return nil
}

// put stores a certificate received via putCert. Re-sending the same
// certificate keeps its activation state; a different one starts inactive
// unless autoActivate is set.
func (c *certStore) put(ctx context.Context, cr *contentRep, authID string, cert *x509.Certificate, autoActivate bool) (*certRecord, error) {
	_ = c.reload(ctx, time.Minute)
	sum := sha256.Sum256(cert.Raw)
	fp := hex.EncodeToString(sum[:])
	now := time.Now().UTC()
	rec := &certRecord{
		ContRep:     cr.id,
		AuthID:      authID,
		CertDER:     cert.Raw,
		Subject:     cert.Subject.String(),
		Issuer:      cert.Issuer.String(),
		Algorithm:   cert.PublicKeyAlgorithm.String(),
		NotAfter:    cert.NotAfter,
		Fingerprint: fp,
		ReceivedAt:  now,
		cert:        cert,
	}
	if prev := c.lookup(cr.id, authID); prev != nil && prev.Fingerprint == fp && prev.Active {
		rec.Active, rec.ActivatedAt = true, prev.ActivatedAt
	} else if autoActivate {
		rec.Active, rec.ActivatedAt = true, &now
	}
	return rec, c.write(ctx, cr, rec)
}

func (c *certStore) setActive(ctx context.Context, contRep, authID string, active bool) (*certRecord, error) {
	cr, ok := c.reps[contRep]
	if !ok {
		return nil, apperr.New(apperr.CSContRepUnknown, "content repository %q is not configured", contRep).WithStatus(400)
	}
	if err := c.reload(ctx, 0); err != nil {
		return nil, err
	}
	prev := c.lookup(contRep, authID)
	if prev == nil {
		return nil, apperr.New(apperr.CertNotReceived, "no certificate was received for contRep %q and authId %q; certificates known for %q: %s", contRep, authID, contRep, c.knownFor(contRep))
	}
	rec := *prev
	rec.Active = active
	if active {
		now := time.Now().UTC()
		rec.ActivatedAt = &now
	} else {
		rec.ActivatedAt = nil
	}
	return &rec, c.write(ctx, cr, &rec)
}

// knownFor lists the authIds for which certificates exist in contRep.
func (c *certStore) knownFor(contRep string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []string
	for _, r := range c.recs {
		if r.ContRep == contRep {
			state := "inactive"
			if r.Active {
				state = "active"
			}
			out = append(out, fmt.Sprintf("%q (%s)", r.AuthID, state))
		}
	}
	if len(out) == 0 {
		return "none; send the certificate from OAC0"
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
