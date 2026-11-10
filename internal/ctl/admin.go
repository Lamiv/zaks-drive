package ctl

import (
	"context"
	"flag"
	"fmt"
)

// cmdAdmin implements `zaks-drive admin reset-password`.
//
// The admin UI login is the user "admin" and a password the administrator
// chose at the first login. A forgotten password is reset by creating an empty
// file NO_ADMIN in the server's state volume and restarting the server: the
// server then accepts admin/admin again, but only to choose a new password, and
// deletes the file once that is done. This command creates the file the safe
// way (a one-off container of the same image and volume, as for `repo`) and
// restarts the server through its drain.
func (a *App) cmdAdmin(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] != "reset-password" {
		fmt.Fprintln(a.Err, "usage: zaks-drive admin reset-password [--yes] [--no-restart] [--dir DIR]")
		return ExitUsage
	}
	fset := flag.NewFlagSet("admin reset-password", flag.ContinueOnError)
	fset.SetOutput(a.Err)
	dir := fset.String("dir", "", "deployment directory")
	yes := fset.Bool("yes", false, "do not ask for confirmation")
	noRestart := fset.Bool("no-restart", false, "create the reset request but do not restart now")
	if err := fset.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	d, c, code := a.prepareRepo(ctx, *dir)
	if code != ExitOK {
		return code
	}
	a.printf("This resets the login of the admin web page:\n")
	a.printf("  - the stored password is discarded; the login becomes admin / admin again, but only to choose a new password at once\n")
	a.printf("  - everyone who is logged in to the page is logged out (the server restarts)\n")
	a.printf("  - repositories, documents and the ADMIN_TOKEN used by this command are not touched\n")
	a.printf("  - the server deletes the NO_ADMIN file once the new password is set\n\n%s\n", changeNote(c, *noRestart))
	if !a.confirm(ctx, "Reset the admin password?", *yes) {
		a.printf("Nothing was changed.\n")
		return ExitDeclined
	}
	if _, err := c.call(ctx, repoOp{name: "admin-reset"}); err != nil {
		return a.failRemote(d, err)
	}
	a.printf("Reset requested (NO_ADMIN created in the state volume).\n")
	if rc := a.applyChanges(ctx, d, c, *noRestart); rc != ExitOK {
		return rc
	}
	a.printf("Open %s, log in as admin with the password admin, and choose a new password.\n", a.readyURL(d, "/admin/ui/"))
	a.printf("If the page is not reachable from your machine, use an SSH tunnel to the host's port; do not publish the admin port.\n")
	return ExitOK
}
