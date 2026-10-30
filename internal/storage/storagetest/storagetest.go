// Package storagetest lets the REST and SAP test suites run against the
// local backend as well as the SharePoint emulator: set ZAKS_TEST_BACKEND=local.
// It is used by tests only.
package storagetest

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"

	"github.com/Lamiv/zaks-drive/internal/storage"
)

// Local reports whether the suites should use the local backend.
func Local() bool { return os.Getenv("ZAKS_TEST_BACKEND") == "local" }

// Seeder puts files straight into local repositories, the way graphmock's
// Seeder puts them into the emulated drive. Prefixes are the drive paths of
// the SharePoint layout ("SAP/DMS"), each mapped to a repository folder.
type Seeder struct {
	Svc   *storage.Service
	Repos map[string]Repo
}

type Repo struct {
	ID  string
	Dir string
}

// AddFile writes data at drive path p and returns the item ID. A path that
// belongs to no repository stays unreachable, as an item outside the root
// folder is on SharePoint; its ID is one that resolves to nothing.
func (s Seeder) AddFile(_, p string, data []byte) string {
	p = strings.Trim(p, "/")
	for prefix, r := range s.Repos {
		rel, ok := strings.CutPrefix(p, prefix+"/")
		if !ok {
			continue
		}
		full := filepath.Join(r.Dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			panic(err)
		}
		if err := os.WriteFile(full, data, 0o600); err != nil {
			panic(err)
		}
		repo, err := s.Svc.Repository(r.ID)
		if err != nil {
			panic(err)
		}
		it, err := s.Svc.GetByPath(context.Background(), repo.Unrestricted(), rel)
		if err != nil {
			panic(err)
		}
		return it.ID
	}
	return base64.RawURLEncoding.EncodeToString([]byte("/outside/" + p))
}
