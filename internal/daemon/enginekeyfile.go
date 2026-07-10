package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// This file persists the engine key as an engine-owned workspace file
// (specification section 14: the ed25519 key minted at install whose signature
// seals the checkpoint chain). The spec states the private half lives "in meta", and
// enginekey.go documents the original choice to store it as an ALTER DATABASE meta
// SET per-database GUC. That storage requires SUPERUSER, which the external admin
// role does not hold (specification section 2 requires only CREATEDB of it), so it
// cannot run in external mode -- exactly where the CI conformance suite runs. The key
// was therefore left unstored, and the seal step could not sign.
//
// This file closes that gap with a non-superuser-safe realization: the private key
// is written once to <.iris>/engine.key (0600) under the workspace tree, beside the
// socket and object store. Like the object store, the .iris workspace tree is a
// per-host prerequisite the spec already treats as pointable at shared storage for
// HA (section 14, object store); a shared workspace therefore shares the key so a
// failover leader signs the same chain. The file is created once with O_EXCL, so two
// racing leaders converge on one key (the loser reads the winner's). It remains a
// documented departure from "private half in meta", flagged here and in
// enginekey.go for spec review.
//
// The choice deliberately keeps the key OUT of any output stream: the file holds the
// base64 private half at 0600, and only the seal (and an offline auditor reading the
// public half) consumes it.

// EngineKeyFileName is the engine key file's name under the workspace .iris tree.
const EngineKeyFileName = "engine.key"

// EngineKeyFilePath returns the engine key file path for the given settings: the
// engine.key file beside the control socket in the workspace .iris tree. It is
// derived from the socket path so it lands under the same per-host workspace tree the
// socket and object store already occupy.
func EngineKeyFilePath(s config.Settings) string {
	return filepath.Join(filepath.Dir(s.Socket), EngineKeyFileName)
}

// LoadOrMintEngineKey loads the engine key from path, minting and persisting a fresh
// one when the file is absent (create-once with O_EXCL, so racing minters converge on
// the winner's key). A present-but-corrupt file is a hard error rather than a silent
// re-mint, so a damaged key surfaces instead of forking the chain under a new key.
// The parent directory is created 0700 if missing.
func LoadOrMintEngineKey(path string) (EngineKey, error) {
	//nolint:gosec // G304: path is the engine-owned workspace key file, not user input.
	raw, err := os.ReadFile(path)
	if err == nil {
		key, derr := DecodeEngineKey(strings.TrimSpace(string(raw)))
		if derr != nil {
			return EngineKey{}, fmt.Errorf("daemon: load engine key %s: %w", path, derr)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return EngineKey{}, fmt.Errorf("daemon: read engine key %s: %w", path, err)
	}

	key, err := MintEngineKey()
	if err != nil {
		return EngineKey{}, err
	}
	if err := writeEngineKeyFile(path, key); err != nil {
		// Lost the create-once race: read the winner's key rather than fail.
		if errors.Is(err, os.ErrExist) {
			//nolint:gosec // G304: engine-owned workspace key file.
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return EngineKey{}, fmt.Errorf("daemon: read engine key after race %s: %w", path, rerr)
			}
			return DecodeEngineKey(strings.TrimSpace(string(raw)))
		}
		return EngineKey{}, err
	}
	return key, nil
}

// writeEngineKeyFile writes the key's base64 private half to path exactly once: it
// creates the parent 0700, opens the file with O_CREATE|O_EXCL|O_WRONLY at 0600 (so a
// concurrent minter that got there first fails with os.ErrExist), writes the material,
// and closes it before returning any error. The private half never renders through fmt
// -- privateBase64 is the one accessor, used only here.
func writeEngineKeyFile(path string, key EngineKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("daemon: create engine key dir: %w", err)
	}
	//nolint:gosec // G304: engine-owned workspace key file.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.WriteString(key.privateBase64()); werr != nil {
		_ = f.Close()
		return fmt.Errorf("daemon: write engine key: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("daemon: close engine key file: %w", cerr)
	}
	return nil
}
