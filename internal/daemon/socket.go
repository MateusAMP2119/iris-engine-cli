package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// This file holds the control-socket setup leg of `iris engine install`: create
// the workspace .iris directory the Unix control socket lives in, and clear any
// stale socket file left by a prior run so a fresh daemon can bind cleanly
// (specification section 4, "set up the socket"). It is real local filesystem I/O
// -- no database -- so it is proven directly against a temp workspace.

// socketDirPerm is the mode of the workspace .iris directory the control socket
// lives in. The control plane is local-only, guarded by filesystem permissions
// (specification section 2), so the directory is owner-only.
const socketDirPerm os.FileMode = 0o700

// PrepareSocketDir sets up the engine's control socket location: it creates the
// directory the socket path sits in (the workspace .iris tree) and removes a stale
// socket file at the socket path if one is present, so a freshly started daemon can
// bind without colliding with a previous run's socket. It is idempotent and never
// touches any database.
func PrepareSocketDir(s config.Settings) error {
	dir := filepath.Dir(s.Socket)
	if err := os.MkdirAll(dir, socketDirPerm); err != nil {
		return fmt.Errorf("daemon: create control socket directory %s: %w", dir, err)
	}
	if err := os.Remove(s.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon: remove stale control socket %s: %w", s.Socket, err)
	}
	return nil
}

// FileSocketPreparer is the production SocketPreparer: it runs PrepareSocketDir
// against the engine settings, so BootstrapEngine's socket step is the real
// filesystem setup. The recording preparer used in the install-sequence test
// substitutes for it there.
type FileSocketPreparer struct {
	// Settings carries the resolved socket path whose directory is prepared.
	Settings config.Settings
}

// PrepareSocket sets up the control socket directory for the configured settings.
func (p FileSocketPreparer) PrepareSocket(context.Context) error {
	return PrepareSocketDir(p.Settings)
}
