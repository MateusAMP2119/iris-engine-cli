package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// fakeKeyReader is a daemon.EngineKeyReader that returns a scripted key or error,
// standing in for the meta-connection read of the engine_key table so `iris engine
// info` can be driven with no live meta.
type fakeKeyReader struct {
	key daemon.EngineKey
	err error
}

func (f fakeKeyReader) ReadEngineKey(context.Context) (daemon.EngineKey, error) {
	return f.key, f.err
}

// TestEngineInfoExposesPublicKeyHalf proves `iris engine info` exposes the engine
// key's public half and never its private half (specification sections 4 and 11):
// the human and --json renderings both carry the base64 public key read back from
// meta, and neither stream ever carries the private material. When the key cannot
// be read (the engine is not installed / meta unreachable) info fails with the
// operation-failed category and a clear message.
//
// spec: S04/engine-key-public-via-info
func TestEngineInfoExposesPublicKeyHalf(t *testing.T) {
	t.Chdir(t.TempDir())

	key, err := daemon.MintEngineKey()
	if err != nil {
		t.Fatalf("MintEngineKey: %v", err)
	}
	// The private half, as it would live in meta -- extracted from the engine_key
	// insert DDL (a bytea hex literal) so the test can prove it never reaches an
	// output stream.
	insDDL := daemon.InsertEngineKeyDDL(key)
	privB64 := insDDL[strings.Index(insDDL, "'")+1 : strings.LastIndex(insDDL, "'")]

	newInstalledApp := func(out, errOut *bytes.Buffer) *app {
		a := newApp(out, errOut)
		a.newKeyReader = func(config.Settings) daemon.EngineKeyReader { return fakeKeyReader{key: key} }
		return a
	}

	t.Run("human output shows the public half, not the private", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := newInstalledApp(&out, &errb).run([]string{"engine", "info"})
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if !strings.Contains(out.String(), key.PublicBase64()) {
			t.Errorf("engine info did not show the public key half %q:\n%s", key.PublicBase64(), out.String())
		}
		if strings.Contains(out.String()+errb.String(), privB64) {
			t.Errorf("engine info leaked the private key half onto an output stream")
		}
	})

	t.Run("json envelope carries the public half only", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := newInstalledApp(&out, &errb).run([]string{"--json", "engine", "info"})
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		var doc struct {
			Data struct {
				EngineKeyPublic string `json:"engine_key_public"`
			} `json:"data"`
		}
		decodeSingleJSON(t, out.Bytes(), &doc)
		if doc.Data.EngineKeyPublic != key.PublicBase64() {
			t.Errorf("json engine_key_public = %q, want %q", doc.Data.EngineKeyPublic, key.PublicBase64())
		}
		if strings.Contains(out.String(), privB64) {
			t.Errorf("json engine info leaked the private key half: %s", out.String())
		}
	})

	t.Run("uninstalled engine fails with a clear message", func(t *testing.T) {
		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.newKeyReader = func(config.Settings) daemon.EngineKeyReader {
			return fakeKeyReader{err: daemon.ErrEngineNotInstalled}
		}
		code := a.run([]string{"engine", "info"})
		if code != exitOpFailed {
			t.Fatalf("exit = %d, want %d (operation failed)\nstdout: %s\nstderr: %s", code, exitOpFailed, out.String(), errb.String())
		}
		if !strings.Contains(strings.ToLower(errb.String()), "not installed") {
			t.Errorf("uninstalled engine info message is not clear: %q", errb.String())
		}
	})
}
