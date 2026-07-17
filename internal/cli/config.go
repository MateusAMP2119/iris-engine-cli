package cli

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// resolveTarget resolves an invocation's engine/connection settings through the
// configuration precedence: the global --socket/--host/--token flags override
// the IRIS_* environment, which overrides an optional iris.toml under the
// engine home, which overrides the built-in defaults (the local socket + managed
// Postgres of the zero-config path). The engine home is fixed per user (~/.iris,
// relocated wholesale by IRIS_HOME) -- Iris runs one engine per machine, so the
// invoking directory plays no part in where the engine is found and every iris
// command works from any cwd. A misconfigured iris.toml or IRIS_* value surfaces
// as a warning instead of being silently ignored.
func (a *app) resolveTarget(cmd *cobra.Command) config.Settings {
	home, err := config.Home(os.Getenv)
	if err != nil {
		a.logger.Warn("no home directory; falling back to .iris under the invoking directory", "err", err)
		home = config.DirName
	}
	env, err := config.FromEnv(os.Getenv)
	if err != nil {
		a.logger.Warn("ignoring malformed IRIS_* configuration", "err", err)
		env = config.Layer{}
	}
	file, err := config.LoadTOMLFile(filepath.Join(home, config.FileName))
	if err != nil {
		a.logger.Warn("ignoring unreadable iris.toml", "err", err)
		file = config.TOML{}
	}
	for _, key := range file.Ignored {
		a.logger.Warn("ignoring non-engine key in iris.toml (never a project manifest)", "key", key)
	}
	return config.Resolve(config.Defaults(home), file.Layer, env, flagLayer(cmd))
}

// flagLayer builds the highest-precedence configuration layer from the global
// flags an invocation explicitly set. Only a flag the user changed contributes:
// an untouched flag leaves its field unset, so it defers to the IRIS_* env,
// iris.toml, and default layers below rather than overriding them with an empty
// value.
func flagLayer(cmd *cobra.Command) config.Layer {
	var l config.Layer
	if cmd == nil {
		return l
	}
	if v, ok := changedString(cmd, "socket"); ok {
		l.Socket = &v
	}
	if v, ok := changedString(cmd, "host"); ok {
		l.Host = &v
	}
	if v, ok := changedString(cmd, "token"); ok {
		l.Token = &v
	}
	// The daemon-scoped flags live only on `engine start`; on any other command
	// the Lookup misses and contributes nothing. They configure the running engine
	// the daemon starts.
	if v, ok := changedString(cmd, "pg-dsn"); ok {
		l.PgDSN = &v
	}
	if v, ok := changedInt64(cmd, "retain"); ok {
		l.Retain = &v
	}
	if v, ok := changedInt64(cmd, "journal-partition-rows"); ok {
		l.JournalPartitionRows = &v
	}
	if v, ok := changedString(cmd, "objects-path"); ok {
		l.ObjectsPath = &v
	}
	if v, ok := changedString(cmd, "tcp"); ok {
		l.TCP = &v
	}
	if v, ok := changedString(cmd, "tls-cert"); ok {
		l.TLSCert = &v
	}
	if v, ok := changedString(cmd, "tls-key"); ok {
		l.TLSKey = &v
	}
	return l
}

// changedString returns the value of a string flag and whether the invocation
// explicitly set it (cobra's Changed bit), so an unset flag never contributes to
// the configuration layer.
func changedString(cmd *cobra.Command, name string) (string, bool) {
	f := cmd.Flags().Lookup(name)
	if f == nil || !f.Changed {
		return "", false
	}
	return f.Value.String(), true
}

// changedInt64 returns the value of an int64 flag and whether the invocation
// explicitly set it. Cobra already rejected a non-integer value at parse time,
// so the re-parse of the flag's canonical string form is total.
func changedInt64(cmd *cobra.Command, name string) (int64, bool) {
	f := cmd.Flags().Lookup(name)
	if f == nil || !f.Changed {
		return 0, false
	}
	v, err := strconv.ParseInt(f.Value.String(), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
