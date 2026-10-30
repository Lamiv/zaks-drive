// Package accesscheck verifies, against the real Microsoft Graph, every
// access the adapter relies on, and reports for each check the access
// required, what was tested, and PASS / FAIL (with cause and fix) / SKIP.
//
// It is run with `zaks-drive check` (e.g. inside the container:
// `docker compose exec adapter /zaks-drive check`). Test files are created in
// each repository's root folder and removed again; the secret is never
// printed.
package accesscheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Lamiv/zaks-drive/internal/auth"
	"github.com/Lamiv/zaks-drive/internal/config"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/safelog"
)

const testPrefix = "adapter-access-test-"

type result string

const (
	pass result = "PASS"
	fail result = "FAIL"
	skip result = "SKIP"
)

type row struct {
	name   string
	result result
}

type reporter struct {
	w    io.Writer
	rows []row
}

func (r *reporter) add(name, required, tested string, res result, detail, fix string) {
	r.rows = append(r.rows, row{name, res})
	fmt.Fprintf(r.w, "\n[%s] %s\n    required: %s\n    tested  : %s\n", res, name, required, tested)
	if detail != "" {
		fmt.Fprintf(r.w, "    detail  : %s\n", detail)
	}
	if res == fail && fix != "" {
		fmt.Fprintf(r.w, "    fix     : %s\n", fix)
	}
}

func (r *reporter) skip(name, required, tested, because string) {
	r.add(name, required, tested, skip, "depends on: "+because, "")
}

// summary prints the totals and returns the process exit code.
func (r *reporter) summary() int {
	fmt.Fprintf(r.w, "\n%s\nSUMMARY\n%s\n", strings.Repeat("=", 78), strings.Repeat("=", 78))
	var passed, failed, skipped int
	firstFail := ""
	for _, x := range r.rows {
		fmt.Fprintf(r.w, "  %-4s  %s\n", x.result, x.name)
		switch x.result {
		case pass:
			passed++
		case fail:
			failed++
			if firstFail == "" {
				firstFail = x.name
			}
		default:
			skipped++
		}
	}
	fmt.Fprintf(r.w, "%s\n  passed %d, failed %d, skipped %d\n", strings.Repeat("-", 78), passed, failed, skipped)
	if failed > 0 {
		fmt.Fprintf(r.w, "\n  First failure to fix: %s (see its 'fix' line above)\n", firstFail)
		return 1
	}
	fmt.Fprintln(r.w, "\n  All required access is in place.")
	return 0
}

// hint turns a Graph error into the most likely fix.
func hint(err error) string {
	// Errors that already carry a code (token, library, site, Graph) bring their own fix.
	if ae := graph.Classify(err); ae != nil {
		return ae.Fix()
	}
	switch graph.StatusOf(err) {
	case http.StatusUnauthorized:
		return "Graph rejected the token: the app has no usable APPLICATION permission or admin consent is missing (see the permission check)"
	case http.StatusForbidden:
		return "the app has no access to this site/library: grant it write access (Sites.Selected needs POST /sites/{site-id}/permissions with role 'write'), or use Sites.ReadWrite.All"
	case http.StatusNotFound:
		return "not found: check siteUrl / userId / driveName / rootPath in config.yaml"
	case 0:
		return "network problem reaching graph.microsoft.com (firewall, proxy or DNS)"
	}
	return "see the error above"
}

// Run executes all checks and writes the report to w. It returns 0 when
// everything passed, 1 otherwise.
func Run(ctx context.Context, cfg config.Config, gc *graph.Client, tokens auth.TokenSource, w io.Writer) int {
	rep := &reporter{w: w}
	// Local repositories need no Microsoft access; only SharePoint ones are
	// checked here (`zaks-drive repo check` tests a local folder).
	var sharePoint []config.RepositoryConfig
	for _, r := range cfg.Repositories {
		if r.Backend == config.BackendLocal {
			rep.add("["+r.ID+"] local repository", "a writable folder on the server", "not tested by this command", pass,
				"stored in a local folder; no Microsoft access is needed", "")
			continue
		}
		sharePoint = append(sharePoint, r)
	}
	cfg.Repositories = sharePoint
	if len(sharePoint) == 0 {
		return rep.summary()
	}
	fmt.Fprintf(w, "Access check for %d repositor%s (test files are created and removed again)\n",
		len(cfg.Repositories), map[bool]string{true: "y", false: "ies"}[len(cfg.Repositories) == 1])

	// 1. credentials
	rep.add("Credentials configured",
		"graph.tenantId, graph.clientId, graph.clientSecret (the secret VALUE, not its ID)",
		fmt.Sprintf("config values (secret length %d, never printed)", len(cfg.Graph.ClientSecret)), pass, "", "")

	// 2. token
	const tokReq = "valid tenant ID, client ID and unexpired secret"
	const tokTest = "client-credentials token request to Microsoft Entra ID"
	token, err := tokens.Token(ctx)
	if err != nil {
		rep.add("Access token from Microsoft Entra ID", tokReq, tokTest, fail, detail(err), hint(err))
		rep.skip("Application permission in the token", "a Graph APPLICATION permission with admin consent", "token 'roles' claim", "token")
		for _, r := range cfg.Repositories {
			rep.skip("["+r.ID+"] all access checks", "see below", "-", "token")
		}
		return rep.summary()
	}
	rep.add("Access token from Microsoft Entra ID", tokReq, tokTest, pass, "", "")

	// 3. roles
	roles := auth.TokenRoles(token)
	hasRole := false
	for _, r := range roles {
		switch r {
		case "Sites.Selected", "Sites.ReadWrite.All", "Sites.FullControl.All", "Files.ReadWrite.All":
			hasRole = true
		}
	}
	d := "roles in token: " + fmt.Sprint(roles)
	if len(roles) == 0 {
		d = "roles in token: NONE"
	}
	if len(roles) > 0 && !hasRole {
		d += " (none of Sites.Selected, Sites.ReadWrite.All, Sites.FullControl.All, Files.ReadWrite.All)"
	}
	if contains(roles, "Sites.Selected") && !contains(roles, "Sites.ReadWrite.All") {
		d += " | Sites.Selected grants nothing until the site itself is shared with the app"
	}
	res := pass
	if !hasRole {
		res = fail
	}
	rep.add("Application permission in the token",
		"Graph APPLICATION permission (type 'Application', admin consent granted): Sites.Selected + site grant, or Sites.ReadWrite.All, or Files.ReadWrite.All for a OneDrive userId",
		"token 'roles' claim", res, d,
		"Entra admin center > App registrations > your app > API permissions: add it as type APPLICATION and click 'Grant admin consent'; wait about a minute")

	for _, repo := range cfg.Repositories {
		checkRepo(ctx, rep, gc, repo)
	}
	return rep.summary()
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func checkRepo(ctx context.Context, rep *reporter, gc *graph.Client, repo config.RepositoryConfig) {
	p := "[" + repo.ID + "] "
	loc := repo.SiteURL
	switch {
	case repo.DriveID != "":
		loc = "drive " + repo.DriveID
	case repo.UserID != "":
		loc = "OneDrive of " + repo.UserID
	case repo.SiteID != "":
		loc = "site " + repo.SiteID
	}
	if repo.DriveName != "" {
		loc += " / library " + repo.DriveName
	}

	// resolve
	const resReq = "read access to the site and its libraries: Sites.ReadWrite.All, or Sites.Selected AND a per-site grant for this app (OneDrive userId: Files.ReadWrite.All)"
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	drive := repo.DriveID
	var err error
	if drive == "" {
		drive, err = gc.ResolveDrive(rctx, repo.UserID, repo.SiteID, repo.SiteURL, repo.DriveName)
	}
	cancel()
	if err != nil || drive == "" {
		if err == nil {
			err = errors.New("no drive returned")
		}
		rep.add(p+"Find the library", resReq, "resolve "+loc, fail, detail(err), hint(err))
		for _, n := range []string{"Write: upload a small file", "Read: download and compare", "Resumable upload session (files over 4 MiB)", "Version history", "Delete the test file", "Clean up earlier test files"} {
			rep.skip(p+n, "access to the library", "-", "find the library")
		}
		return
	}
	where := ""
	if _, web, err := gc.DriveInfo(ctx, drive); err == nil && web != "" {
		where = " | library URL: " + web + " | root folder: " + repo.RootPath
	}
	rep.add(p+"Find the library", resReq, "resolve "+loc, pass, "drive "+short(drive)+where, "")

	if repo.ReadOnly {
		for _, n := range []string{"Write: upload a small file", "Read: download and compare", "Resumable upload session (files over 4 MiB)", "Version history", "Delete the test file", "Clean up earlier test files"} {
			rep.skip(p+n, "write access", "-", "repository is configured readOnly")
		}
		return
	}

	const wr = "WRITE access to the library: Sites.Selected grant with role 'write' (not 'read'), or Sites.ReadWrite.All / Files.ReadWrite.All"
	name := fmt.Sprintf("%s%d.txt", testPrefix, time.Now().UnixNano())
	full := path.Join(repo.RootPath, name)
	payload := []byte("adapter access test " + name)
	tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// write
	item, err := gc.UploadSmallByPath(tctx, drive, full, graph.ConflictReplace, payload, "")
	if err != nil {
		rep.add(p+"Write: upload a small file", wr, "PUT "+full+" (creates the folder if missing)", fail, detail(err),
			hint(err)+"; if the grant is 'read', change it to 'write'")
		for _, n := range []string{"Read: download and compare", "Resumable upload session (files over 4 MiB)", "Version history", "Delete the test file"} {
			rep.skip(p+n, "access to the library", "-", "write")
		}
		cleanup(tctx, rep, p, gc, drive, repo.RootPath)
		return
	}
	rep.add(p+"Write: upload a small file", wr, "PUT "+full+" (creates the folder if missing)", pass, "item "+short(item.ID), "")

	// read
	const rd = "READ access to the library"
	rd1 := func() (string, error) {
		it, err := gc.GetItem(tctx, drive, item.ID)
		if err != nil {
			return "", err
		}
		via := "download URL"
		var resp *http.Response
		if it.DownloadURL != "" {
			resp, err = gc.OpenDownload(tctx, it.DownloadURL, "")
		} else {
			via = "/content redirect (no download URL on the item)"
			resp, err = gc.OpenContent(tctx, drive, item.ID, "")
		}
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(b, payload) {
			return "", fmt.Errorf("downloaded content differs (%d bytes)", len(b))
		}
		return fmt.Sprintf("%d bytes, identical, via %s", len(b), via), nil
	}
	if d, err := rd1(); err != nil {
		rep.add(p+"Read: download and compare", rd, "GET item, GET its download URL, compare bytes", fail, detail(err), hint(err))
	} else {
		rep.add(p+"Read: download and compare", rd, "GET item, GET its download URL, compare bytes", pass, d, "")
	}

	// upload session
	sess, err := gc.CreateUploadSessionForItem(tctx, drive, item.ID, "")
	if err != nil {
		rep.add(p+"Resumable upload session (files over 4 MiB)", wr, "createUploadSession, then cancel", fail, detail(err),
			hint(err)+"; documents over 4 MiB depend on this")
	} else {
		gc.CancelUploadSession(tctx, sess)
		rep.add(p+"Resumable upload session (files over 4 MiB)", wr, "createUploadSession, then cancel", pass, "session created and cancelled", "")
	}

	// versions
	if v, err := gc.Versions(tctx, drive, item.ID); err != nil {
		rep.add(p+"Version history", rd, "GET versions", fail, detail(err), hint(err))
	} else {
		rep.add(p+"Version history", rd, "GET versions", pass, fmt.Sprintf("%d version(s)", len(v)), "")
	}

	// delete
	if err := gc.Delete(tctx, drive, item.ID, ""); err != nil {
		rep.add(p+"Delete the test file", "DELETE access (part of 'write')", "DELETE item", fail, detail(err),
			hint(err)+"; remove "+full+" by hand; SAP deletes need this")
	} else {
		rep.add(p+"Delete the test file", "DELETE access (part of 'write')", "DELETE item", pass, "", "")
	}
	cleanup(tctx, rep, p, gc, drive, repo.RootPath)
}

// cleanup removes test files left behind by interrupted earlier runs.
func cleanup(ctx context.Context, rep *reporter, p string, gc *graph.Client, drive, root string) {
	const nm = "Clean up earlier test files"
	pg, err := gc.ListChildren(ctx, drive, root, 200, "")
	if err != nil {
		if graph.IsNotFound(err) {
			rep.add(p+nm, "DELETE access", "list the root folder", pass, "folder does not exist yet; nothing to clean", "")
			return
		}
		rep.add(p+nm, "DELETE access", "list the root folder", fail, detail(err), hint(err))
		return
	}
	var removed, failed int
	for _, it := range pg.Items {
		if strings.HasPrefix(it.Name, testPrefix) {
			if gc.Delete(ctx, drive, it.ID, "") == nil {
				removed++
			} else {
				failed++
			}
		}
	}
	if failed > 0 {
		rep.add(p+nm, "DELETE access", "list the root folder, delete "+testPrefix+"*", fail, fmt.Sprintf("%d removed, %d could not be removed", removed, failed), "delete them by hand")
		return
	}
	rep.add(p+nm, "DELETE access", "list the root folder, delete "+testPrefix+"*", pass, fmt.Sprintf("removed %d leftover file(s)", removed), "")
}

func short(id string) string {
	if len(id) > 14 {
		return id[:14] + "..."
	}
	return id
}

// detail is what the checklist prints about a failure. Operators paste this output into
// bug reports, so it carries Microsoft's explanation (scrubbed of URL query strings and
// credential-shaped values) and never a raw URL or token.
func detail(err error) string {
	if ae := graph.Classify(err); ae != nil {
		return safelog.OneLine(safelog.Redact(ae.Detail))
	}
	return safelog.OneLine(safelog.Redact(safelog.Summary(err)))
}
