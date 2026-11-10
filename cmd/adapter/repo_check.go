package main

import (
	"bytes"
	"context"
	"os"

	"github.com/Lamiv/zaks-drive/internal/accesscheck"
	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/auth"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

// checkRepository tests the access of one repository, served or not yet
// applied, and returns the report. A SharePoint repository contacts
// Microsoft (with its own application when it has one); a local one tests
// its folder.
func checkRepository(ctx context.Context, cfg config.Config, id string, gc *graph.Client, tokens auth.TokenSource, apps *appClients) (string, bool, error) {
	var repo *config.RepositoryConfig
	for i := range cfg.Repositories {
		if cfg.Repositories[i].ID == id {
			repo = &cfg.Repositories[i]
		}
	}
	if repo == nil {
		return "", false, apperr.New(apperr.AdminRepoNotFound, "there is no repository %q", id)
	}
	var out bytes.Buffer
	var code int
	if repo.Backend == config.BackendLocal {
		eng := transfer.New(nil, transfer.Options{
			SimpleUploadMax: 1 << 20, ChunkSize: 320 << 10, MemoryBudget: 8 << 20, MaxUpload: cfg.Transfer.MaxUpload, SpoolDir: os.TempDir(),
		})
		code = accesscheck.RunLocal(ctx, &out, *repo, eng)
	} else {
		cfg.Repositories = []config.RepositoryConfig{*repo}
		code = accesscheck.Run(ctx, cfg, gc, tokens, &out, apps.checkResolver)
	}
	return out.String(), code == 0, nil
}
