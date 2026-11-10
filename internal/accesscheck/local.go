package accesscheck

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/storage"
	"github.com/Lamiv/zaks-drive/internal/transfer"
)

// lowSpace is the free space below which a local repository gets a warning.
const lowSpace = 1 << 30

// RunLocal checks a repository stored in a local folder: the folder can be
// opened, a file can be written, read back, replaced (creating a version)
// and deleted, and the disk has room. The test file is removed again.
func RunLocal(ctx context.Context, w io.Writer, repo config.RepositoryConfig, eng *transfer.Engine) int {
	rep := &reporter{w: w}
	p := "[" + repo.ID + "] "
	fmt.Fprintf(w, "Access check for local repository %s (a test file is created and removed again)\n", repo.ID)

	svc, err := storage.NewService(ctx, nil, eng, []config.RepositoryConfig{repo}, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		rep.add(p+"Open the repository folder", "a folder the server's user can create and write", "open "+repo.LocalRoot, fail, err.Error(),
			"Check that the state volume is mounted and writable by the server (the compose file mounts it at /var/lib/zaks-drive)")
		for _, s := range []string{"Write", "Read", "Replace and keep a version", "Delete"} {
			rep.skip(p+s, "see above", "-", "folder")
		}
		return rep.summary()
	}
	defer svc.Close()
	r, _ := svc.Repository(repo.ID)
	rep.add(p+"Open the repository folder", "a folder the server's user can create and write", "open "+repo.LocalRoot, pass, "", "")

	name := testPrefix + strings.ToLower(repo.ID) + ".txt"
	payload := []byte("zaks-drive access test")
	it, err := svc.Upload(ctx, r, storage.UploadRequest{Name: name, Conflict: storage.ConflictReplace, Body: bytes.NewReader(payload), Size: int64(len(payload))})
	if err != nil {
		rep.add(p+"Write: upload a small file", "write permission in the folder", "upload "+name, fail, err.Error(), "Check the ownership and free space of the folder")
		for _, s := range []string{"Read", "Replace and keep a version", "Delete"} {
			rep.skip(p+s, "see above", "-", "write")
		}
		return rep.summary()
	}
	rep.add(p+"Write: upload a small file", "write permission in the folder", "upload "+name, pass, "", "")
	defer func() { _ = svc.Delete(context.WithoutCancel(ctx), r, it.ID, "") }()

	resp, err := svc.Open(ctx, r, it, "")
	var got []byte
	if err == nil {
		got, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	switch {
	case err != nil:
		rep.add(p+"Read: download and compare", "read permission", "download "+name, fail, err.Error(), "Check the permissions of the folder")
	case !bytes.Equal(got, payload):
		rep.add(p+"Read: download and compare", "content read back equals content written", "compare bytes", fail, "the content differs from what was written", "Check the disk and file system")
	default:
		rep.add(p+"Read: download and compare", "content read back equals content written", "compare bytes", pass, "", "")
	}

	keep := repo.Local.KeepVersions()
	if _, err := svc.Replace(ctx, r, it.ID, "", strings.NewReader("second"), 6); err != nil {
		rep.add(p+"Replace and keep a version", "replace a file", "replace "+name, fail, err.Error(), "Check the permissions of the folder")
	} else if vs, err := svc.Versions(ctx, r, it.ID); err != nil {
		rep.add(p+"Replace and keep a version", "replace a file", "list versions", fail, err.Error(), "")
	} else if want := min(keep, 1) + 1; len(vs) != want {
		rep.add(p+"Replace and keep a version", fmt.Sprintf("%d previous version(s) kept (local.versions %d)", min(keep, 1), keep), "list versions", fail,
			fmt.Sprintf("found %d entries, expected %d", len(vs), want), "")
	} else {
		rep.add(p+"Replace and keep a version", fmt.Sprintf("%d previous version(s) kept (local.versions %d)", min(keep, 1), keep), "list versions", pass, "", "")
	}

	if free, ok := freeBytes(repo.LocalRoot); ok {
		res, detail, fix := pass, fmt.Sprintf("%d MiB free", free>>20), ""
		if free < lowSpace {
			res, fix = fail, "Free disk space or move the state volume to a larger disk"
			detail += " (less than 1 GiB)"
		}
		rep.add(p+"Free disk space", "room for new documents", "free space of the folder's file system", res, detail, fix)
	}

	if err := svc.Delete(ctx, r, it.ID, ""); err != nil {
		rep.add(p+"Delete the test file", "delete permission", "delete "+name, fail, err.Error(), "Check the permissions of the folder")
	} else {
		rep.add(p+"Delete the test file", "delete permission", "delete "+name, pass, "", "")
	}
	return rep.summary()
}
