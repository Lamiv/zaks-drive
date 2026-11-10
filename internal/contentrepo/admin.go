package contentrepo

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/graph"
)

// RegisterAdmin adds certificate administration (the equivalent of the
// CSADMIN "Certificates" tab) to the admin listener:
//
//	GET  /admin/contentserver/certificates
//	POST /admin/contentserver/certificates/activate    {"contRep":"Z1","authId":"CN=S4H"}
//	POST /admin/contentserver/certificates/deactivate  {"contRep":"Z1","authId":"CN=S4H"}
//
// When token is non-empty, requests need "Authorization: Bearer <token>".
func (s *Server) RegisterAdmin(mux *http.ServeMux, token string) {
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if token != "" {
				got := []byte(r.Header.Get("Authorization"))
				if subtle.ConstantTimeCompare(got, []byte("Bearer "+token)) != 1 {
					adminFail(w, apperr.New(apperr.AuthAdminUnauthorized, "the Authorization header is missing or does not carry the ADMIN_TOKEN"))
					return
				}
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /admin/contentserver/certificates", guard(func(w http.ResponseWriter, r *http.Request) {
		if err := s.certs.reload(r.Context(), 0); err != nil {
			adminFail(w, err)
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"value": s.certs.list()})
	}))
	set := func(active bool) http.HandlerFunc {
		return guard(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				ContRep string `json:"contRep"`
				AuthID  string `json:"authId"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.ContRep == "" || body.AuthID == "" {
				adminFail(w, apperr.New(apperr.CertRequestInvalid, "the body must be JSON like {\"contRep\":\"Z1\",\"authId\":\"CN=...\"} and both fields must be non-empty"))
				return
			}
			rec, err := s.certs.setActive(r.Context(), body.ContRep, body.AuthID, active)
			if err != nil {
				adminFail(w, err)
				return
			}
			s.log.Info("content server certificate state changed", "contRep", rec.ContRep, "authId", rec.AuthID, "active", active, "fingerprint", rec.Fingerprint)
			out := *rec
			out.CertDER = nil
			writeAdminJSON(w, http.StatusOK, out)
		})
	}
	mux.HandleFunc("POST /admin/contentserver/certificates/activate", set(true))
	mux.HandleFunc("POST /admin/contentserver/certificates/deactivate", set(false))
}

func writeAdminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// adminFail answers with the code, what happened and the fix, as plain text
// that the `adapter certs` command prints unchanged.
func adminFail(w http.ResponseWriter, err error) {
	e, ok := apperr.As(err)
	if !ok {
		if c := graph.Classify(err); c != nil {
			e = c
		} else {
			e = apperr.New(apperr.InternalError, "%v", err)
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(e.Status())
	fmt.Fprintf(w, "%s: %s\n  fix: %s\n  more: %s\n", e.Code, firstNonEmpty(e.Detail, e.Title()), e.Fix(), e.URL())
}

// CertificateInfo is a received SAP certificate as the admin interfaces show
// it (without the certificate bytes).
type CertificateInfo = certRecord

// CertificateList returns the certificates SAP has sent, refreshed from storage.
func (s *Server) CertificateList(ctx context.Context) ([]CertificateInfo, error) {
	if err := s.certs.reload(ctx, 0); err != nil {
		return nil, err
	}
	list := s.certs.list()
	for i := range list {
		list[i].CertDER = nil
	}
	return list, nil
}

// SetCertificateActive activates or deactivates a received certificate.
func (s *Server) SetCertificateActive(ctx context.Context, contRep, authID string, active bool) (*CertificateInfo, error) {
	rec, err := s.certs.setActive(ctx, contRep, authID, active)
	if err != nil {
		return nil, err
	}
	s.log.Info("content server certificate state changed", "contRep", rec.ContRep, "authId", rec.AuthID, "active", active, "fingerprint", rec.Fingerprint)
	out := *rec
	out.CertDER = nil
	return &out, nil
}
