package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// newSettingsViper builds a Viper instance scoped to Iris engine settings:
// IRIS_* environment variables map onto the flat iris.toml keys (socket, host,
// token, …). Callers bind only the documented keys so stray IRIS_* noise never
// becomes configuration.
func newSettingsViper() *viper.Viper {
	v := viper.New()
	v.SetEnvPrefix("IRIS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// Bind the documented keys so AutomaticEnv exposes them even when no
	// config file is loaded (Viper only surfaces env for known keys).
	for _, key := range []string{
		"socket", "host", "token", "pg_dsn", "retain",
		"journal_partition_rows", "objects_path", "workspace",
		"tcp", "tls_cert", "tls_key", "catalogs",
	} {
		_ = v.BindEnv(key)
	}
	// IRIS_PG_DSN / IRIS_OBJECTS_PATH use names that differ from the dotted form.
	_ = v.BindEnv("pg_dsn", EnvPgDSN)
	_ = v.BindEnv("objects_path", EnvObjectsPath)
	_ = v.BindEnv("journal_partition_rows", EnvJournalPartitionRows)
	_ = v.BindEnv("tls_cert", "IRIS_TLS_CERT")
	_ = v.BindEnv("tls_key", "IRIS_TLS_KEY")
	_ = v.BindEnv("tcp", "IRIS_TCP")
	return v
}

// FromEnv reads the documented IRIS_* environment variables through getenv and
// returns a layer that sets exactly the variables that are present. An unset or
// empty variable contributes nothing (a nil field), so it defers to the layers
// below rather than overriding them with an empty value.
//
// Implementation uses spf13/viper so env binding stays one place with iris.toml
// key names. getenv is still honored: when it is not os.Getenv (tests), values
// are pushed into a fresh Viper via Set so resolution stays pure and testable.
func FromEnv(getenv func(string) string) (Layer, error) {
	v := newSettingsViper()
	// When getenv is injected (tests), seed Viper from it rather than the process
	// environment so resolution stays pure. Production passes os.Getenv; Viper's
	// AutomaticEnv already reads the process env for the bound keys, but we still
	// seed from getenv so a single path covers both.
	seed := map[string]string{
		"socket":                 getenv(EnvSocket),
		"host":                   getenv(EnvHost),
		"token":                  getenv(EnvToken),
		"pg_dsn":                 getenv(EnvPgDSN),
		"objects_path":           getenv(EnvObjectsPath),
		"workspace":              getenv(EnvWorkspace),
		"catalogs":               getenv(EnvCatalogs),
		"retain":                 getenv(EnvRetain),
		"journal_partition_rows": getenv(EnvJournalPartitionRows),
	}
	for k, val := range seed {
		if val != "" {
			v.Set(k, val)
		}
	}

	var l Layer
	if s := strings.TrimSpace(v.GetString("socket")); s != "" {
		l.Socket = &s
	}
	if s := strings.TrimSpace(v.GetString("host")); s != "" {
		l.Host = &s
	}
	if s := strings.TrimSpace(v.GetString("token")); s != "" {
		l.Token = &s
	}
	if s := strings.TrimSpace(v.GetString("pg_dsn")); s != "" {
		l.PgDSN = &s
	}
	if s := strings.TrimSpace(v.GetString("objects_path")); s != "" {
		l.ObjectsPath = &s
	}
	if s := strings.TrimSpace(v.GetString("workspace")); s != "" {
		l.Workspace = &s
	}
	if raw := strings.TrimSpace(v.GetString("catalogs")); raw != "" {
		list := splitList(raw)
		l.Catalogs = &list
	}
	if raw := strings.TrimSpace(v.GetString("retain")); raw != "" {
		n, err := parseInt(raw)
		if err != nil {
			return Layer{}, fmt.Errorf("config: %s: %w", EnvRetain, err)
		}
		l.Retain = &n
	}
	if raw := strings.TrimSpace(v.GetString("journal_partition_rows")); raw != "" {
		n, err := parseInt(raw)
		if err != nil {
			return Layer{}, fmt.Errorf("config: %s: %w", EnvJournalPartitionRows, err)
		}
		l.JournalPartitionRows = &n
	}
	return l, nil
}
