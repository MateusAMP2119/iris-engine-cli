package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// TestMetaEngineKeyReaderDerivesPublicFromStoredPrivate proves the production
// engine-key reader turns the raw private half stored in the engine_key meta table
// into an EngineKey exposing only the public half, and that a read failure or an
// empty table maps to ErrEngineNotInstalled so `iris engine info` reports a clear
// operation failure. The meta byte-load is faked (integration tier, no live
// Postgres); the real byte read over meta is proven at conformance.
//
// spec: S04/engine-key-public-via-info
func TestMetaEngineKeyReaderDerivesPublicFromStoredPrivate(t *testing.T) {
	// The private half exactly as the engine_key table stores it (raw ed25519
	// bytes): the meta store returns these bytes to the reader.
	minted, err := MintEngineKey()
	if err != nil {
		t.Fatalf("MintEngineKey: %v", err)
	}
	priv := minted.privateBytes()

	t.Run("stored private yields the public half, never the private", func(t *testing.T) {
		// spec: S04/engine-key-public-via-info
		r := metaEngineKeyReader{
			load: func(context.Context, config.Settings) ([]byte, error) { return priv, nil },
		}
		key, err := r.ReadEngineKey(context.Background())
		if err != nil {
			t.Fatalf("ReadEngineKey: %v", err)
		}
		if key.PublicBase64() != minted.PublicBase64() {
			t.Errorf("public half = %q, want %q (derived from the stored private half)",
				key.PublicBase64(), minted.PublicBase64())
		}
		// The private material never renders through the returned key.
		privHex := fmt.Sprintf("%x", priv)
		privB64 := base64.StdEncoding.EncodeToString(priv)
		for _, rendered := range []string{
			fmt.Sprintf("%v", key), fmt.Sprintf("%s", key), fmt.Sprintf("%#v", key),
			key.String(), key.GoString(),
		} {
			if strings.Contains(rendered, privHex) || strings.Contains(rendered, privB64) {
				t.Errorf("reader-derived engine key leaked its private half: %q", rendered)
			}
		}
	})

	t.Run("empty table maps to ErrEngineNotInstalled", func(t *testing.T) {
		// spec: S04/engine-key-public-via-info
		r := metaEngineKeyReader{
			load: func(context.Context, config.Settings) ([]byte, error) { return nil, nil },
		}
		if _, err := r.ReadEngineKey(context.Background()); !errors.Is(err, ErrEngineNotInstalled) {
			t.Errorf("empty engine_key table: err = %v, want ErrEngineNotInstalled", err)
		}
	})

	t.Run("unreachable meta maps to ErrEngineNotInstalled", func(t *testing.T) {
		// spec: S04/engine-key-public-via-info
		r := metaEngineKeyReader{
			load: func(context.Context, config.Settings) ([]byte, error) {
				return nil, errors.New("dial meta: connection refused")
			},
		}
		if _, err := r.ReadEngineKey(context.Background()); !errors.Is(err, ErrEngineNotInstalled) {
			t.Errorf("unreachable meta: err = %v, want a wrapped ErrEngineNotInstalled", err)
		}
	})

	t.Run("malformed stored bytes are a distinct error, not not-installed", func(t *testing.T) {
		// spec: S04/engine-key-public-via-info
		r := metaEngineKeyReader{
			load: func(context.Context, config.Settings) ([]byte, error) {
				return []byte("too short to be an ed25519 private key"), nil
			},
		}
		_, err := r.ReadEngineKey(context.Background())
		if err == nil {
			t.Fatal("malformed stored key: err = nil, want a corruption error")
		}
		if errors.Is(err, ErrEngineNotInstalled) {
			t.Errorf("malformed stored key masked as ErrEngineNotInstalled: %v", err)
		}
	})
}

// TestMetaSourceForInfoResolvesPerMode proves the production reader reaches meta the
// right way for each Postgres mode without any side effect: external mode derives
// the source from the configured admin DSN; managed mode reconstructs the running
// instance's localhost DSN from its engine-owned runtime files (superuser
// credential + postmaster.pid port) and never starts a postmaster. A managed
// instance with no runtime files is "not installed / not running", surfaced as an
// error.
//
// spec: S04/engine-key-public-via-info
func TestMetaSourceForInfoResolvesPerMode(t *testing.T) {
	t.Run("external mode uses the configured admin DSN", func(t *testing.T) {
		// spec: S04/engine-key-public-via-info
		s := config.Settings{PgDSN: "postgres://admin@db.example:5432/postgres"}
		src, err := metaSourceForInfo(s)
		if err != nil {
			t.Fatalf("metaSourceForInfo(external): %v", err)
		}
		if src.ConnString() != s.PgDSN {
			t.Errorf("external source = %q, want the admin DSN %q verbatim", src.ConnString(), s.PgDSN)
		}
	})

	t.Run("managed mode reconstructs the running instance DSN from runtime files", func(t *testing.T) {
		// spec: S04/engine-key-public-via-info
		ws := t.TempDir()
		s := config.Settings{Socket: filepath.Join(ws, config.DirName, config.SocketName)}
		if !s.Managed() {
			t.Fatal("test setup: empty PgDSN must select managed mode")
		}
		pgDir := ManagedPGDir(s)
		dataDir := managedDataDir(pgDir)
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatalf("mkdir managed data dir: %v", err)
		}
		// The persisted superuser credential and a postmaster.pid whose 4th line is
		// the live port, exactly as a running managed Postgres records them.
		if err := os.WriteFile(filepath.Join(pgDir, superuserPasswordFile), []byte("s3cr3t-pw\n"), 0o600); err != nil {
			t.Fatalf("write superuser credential: %v", err)
		}
		pid := "12345\n" + dataDir + "\n1700000000\n54329\n/tmp/.s.PGSQL.54329\n"
		if err := os.WriteFile(filepath.Join(dataDir, "postmaster.pid"), []byte(pid), 0o600); err != nil {
			t.Fatalf("write postmaster.pid: %v", err)
		}

		src, err := metaSourceForInfo(s)
		if err != nil {
			t.Fatalf("metaSourceForInfo(managed): %v", err)
		}
		got := src.ConnString()
		for _, want := range []string{ManagedSuperuser, "s3cr3t-pw", "localhost:54329"} {
			if !strings.Contains(got, want) {
				t.Errorf("managed source %q missing %q", got, want)
			}
		}
	})

	t.Run("managed mode with no running instance is an error", func(t *testing.T) {
		// spec: S04/engine-key-public-via-info
		ws := t.TempDir()
		s := config.Settings{Socket: filepath.Join(ws, config.DirName, config.SocketName)}
		if _, err := metaSourceForInfo(s); err == nil {
			t.Error("managed mode with no runtime files: err = nil, want an error the reader maps to not-installed")
		}
	})
}
