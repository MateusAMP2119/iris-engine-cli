// Package config resolves the Iris engine/connection configuration under strict
// precedence: command flags override IRIS_* environment variables, which
// override an optional thin iris.toml, which override built-in defaults. Env
// resolution is implemented with spf13/viper (see viper.go); iris.toml stays a
// deliberately strict flat parser (toml.go). It is pure resolution logic layered
// over the flag surface of the cobra tree: a caller assembles one Layer per
// source and folds them with Resolve.
//
// The scope is deliberately narrow. iris.toml carries engine/connection settings
// only and is never a project manifest (the workload graph lives in
// iris-declare.yaml); project-level keys in an iris.toml are not honored (see
// ParseTOML). With nothing configured the resolution yields the local socket
// under the per-user engine home (~/.iris) and an empty admin DSN, which selects
// the engine's managed Postgres.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The documented IRIS_* environment variable names. These nine are the complete
// recognized set; no other IRIS_* variable feeds configuration.
const (
	// EnvHome relocates the engine home wholesale (tests, packaging). It is not a
	// Layer field: it moves where the default socket, iris.toml, and object store
	// live rather than setting any one of them.
	EnvHome                 = "IRIS_HOME"
	EnvSocket               = "IRIS_SOCKET"
	EnvHost                 = "IRIS_HOST"
	EnvToken                = "IRIS_TOKEN"
	EnvPgDSN                = "IRIS_PG_DSN"
	EnvRetain               = "IRIS_RETAIN"
	EnvJournalPartitionRows = "IRIS_JOURNAL_PARTITION_ROWS"
	EnvObjectsPath          = "IRIS_OBJECTS_PATH"
	EnvWorkspace            = "IRIS_WORKSPACE"
	// EnvCatalogs is a comma-separated list of catalog index URLs.
	EnvCatalogs = "IRIS_CATALOGS"
)

// The built-in default numeric settings: run-history retention (keep the newest
// 1000 runs per pipeline) and the journal partition size that seals a partition.
const (
	DefaultRetain               int64 = 1000
	DefaultJournalPartitionRows int64 = 10_000_000
)

// The engine home layout: Iris runs ONE engine per machine, and its state --
// the default socket file, the object-store directory, and the optional
// iris.toml -- lives under a fixed per-user engine home, ~/.iris (relocated
// wholesale by IRIS_HOME). Nothing about the engine target is derived from the
// invoking directory, so every iris command finds the engine from any cwd.
const (
	// DirName is the engine home's directory name under the user's home
	// directory (~/.iris). It is also the legacy per-workspace state directory
	// pre-engine-home releases used, which `iris engine start` detects and
	// refuses with migration guidance.
	DirName = ".iris"
	// SocketName is the default Unix control socket filename under the engine home.
	SocketName = "iris.sock"
	// ObjectsDir is the default object-store directory name under the engine home.
	ObjectsDir = "objects"
	// WorkspaceDir is the default workspace directory name under the engine home; the daemon derives it from settings, never from its cwd (#203).
	WorkspaceDir = "workspace"
	// FileName is the optional configuration file's name under the engine home.
	FileName = "iris.toml"
)

// Home resolves the per-user engine home: IRIS_HOME when set (the wholesale
// relocation for tests and packaging), otherwise ~/.iris. getenv is injected
// (os.Getenv in production) so resolution stays testable; the home-directory
// lookup itself is the one OS fact this package reads.
func Home(getenv func(string) string) (string, error) {
	if v := getenv(EnvHome); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve the engine home: %w", err)
	}
	return filepath.Join(home, DirName), nil
}

// Settings is the fully resolved engine/connection configuration: one typed
// field per documented setting. It is the output of Resolve and the input every
// later consumer (daemon dial target, admin DSN chain, object store) reads.
type Settings struct {
	// Socket is the path to the engine's Unix control socket.
	Socket string
	// Host is the address of a remote engine reached over TCP.
	Host string
	// Token is the PAT presented to a remote engine over TCP.
	Token string
	// PgDSN is the daemon-owned admin DSN. Empty selects the managed Postgres
	// (default engine-managed; any DSN is external mode).
	PgDSN string
	// Retain is the run-history retention count.
	Retain int64
	// JournalPartitionRows is the number of rows per journal partition before
	// sealing.
	JournalPartitionRows int64
	// ObjectsPath is the filesystem path of the content-addressed object store.
	ObjectsPath string
	// TCP is the address the read API and control plane are exposed over TCP,
	// empty when the engine is socket-only.
	TCP string
	// TLSCert is the TLS certificate for the TCP listener, empty for plain TCP.
	TLSCert string
	// TLSKey is the TLS key for the TCP listener, empty for plain TCP.
	TLSKey string
	// Workspace is the tree the daemon dispatches from (pipelines/, schemas/, env files); defaults to <engine home>/workspace.
	Workspace string
	// Catalogs is the ordered list of catalog index URLs pipeline packs install from (#220); empty by default.
	Catalogs []string
}

// Managed reports whether the engine runs its own managed Postgres. That is the
// default whenever no admin DSN is configured; supplying any DSN (via --pg-dsn,
// IRIS_PG_DSN, or iris.toml pg_dsn) selects external mode instead: two modes,
// one code path.
func (s Settings) Managed() bool { return s.PgDSN == "" }

// Layer is one configuration source's contribution to the resolution. Each field
// is a pointer that is non-nil exactly when the source explicitly set that
// setting; a nil field is unset and defers to the next lower-precedence layer.
// The pointer model distinguishes "set to the zero value" from "unset", so a
// higher layer that explicitly sets a field to its zero value still overrides the
// layer below, making precedence strict and per-field.
type Layer struct {
	Socket               *string
	Host                 *string
	Token                *string
	PgDSN                *string
	Retain               *int64
	JournalPartitionRows *int64
	ObjectsPath          *string
	TCP                  *string
	TLSCert              *string
	TLSKey               *string
	Workspace            *string
	// Catalogs is set-whole-or-unset: a later layer replaces the entire list, never merges.
	Catalogs *[]string
}

// Resolve folds the four configuration sources into resolved Settings under
// strict precedence. The layers are given lowest-precedence first -- defaults,
// then iris.toml, then IRIS_* env, then command flags -- and each layer's
// explicitly-set fields override the value accumulated so far, so the highest
// layer that set a field wins. A layer that leaves a field unset contributes
// nothing to it.
func Resolve(defaults, file, env, flags Layer) Settings {
	var s Settings
	for _, l := range []Layer{defaults, file, env, flags} {
		if l.Socket != nil {
			s.Socket = *l.Socket
		}
		if l.Host != nil {
			s.Host = *l.Host
		}
		if l.Token != nil {
			s.Token = *l.Token
		}
		if l.PgDSN != nil {
			s.PgDSN = *l.PgDSN
		}
		if l.Retain != nil {
			s.Retain = *l.Retain
		}
		if l.JournalPartitionRows != nil {
			s.JournalPartitionRows = *l.JournalPartitionRows
		}
		if l.ObjectsPath != nil {
			s.ObjectsPath = *l.ObjectsPath
		}
		if l.TCP != nil {
			s.TCP = *l.TCP
		}
		if l.TLSCert != nil {
			s.TLSCert = *l.TLSCert
		}
		if l.TLSKey != nil {
			s.TLSKey = *l.TLSKey
		}
		if l.Workspace != nil {
			s.Workspace = *l.Workspace
		}
		if l.Catalogs != nil {
			s.Catalogs = *l.Catalogs // whole-list replacement: the highest layer that set it wins
		}
	}
	return s
}

// Defaults returns the built-in default layer, the lowest-precedence source. The
// socket defaults to <home>/iris.sock and the object store to <home>/objects,
// where home is the engine home (Home: IRIS_HOME, or ~/.iris); retention and
// journal partition size take their documented defaults; and the admin DSN is
// left unset, which selects the managed Postgres. An empty home yields paths
// relative to the invoking directory, the caller's last-resort fallback when no
// home directory resolves. The catalog list is left unset: no catalog indexes by
// default.
func Defaults(home string) Layer {
	socket := filepath.Join(home, SocketName)
	objects := filepath.Join(home, ObjectsDir)
	workspace := filepath.Join(home, WorkspaceDir)
	retain := DefaultRetain
	journal := DefaultJournalPartitionRows
	empty := ""
	return Layer{
		Socket:               &socket,
		Host:                 &empty,
		Token:                &empty,
		PgDSN:                &empty, // empty admin DSN -> managed Postgres
		Retain:               &retain,
		JournalPartitionRows: &journal,
		ObjectsPath:          &objects,
		TCP:                  &empty,
		TLSCert:              &empty,
		TLSKey:               &empty,
		Workspace:            &workspace,
	}
}

// splitList parses a comma-separated value (IRIS_CATALOGS), trimming spaces and dropping empties.
func splitList(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseInt parses a base-10 signed 64-bit integer, the form both the integer
// environment variables and the integer iris.toml keys take.
func parseInt(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not an integer", s)
	}
	return n, nil
}
