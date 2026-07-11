package cli

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// resolveTarget resolves an invocation's engine/connection settings through the
// configuration precedence of specification section 8: the global
// --socket/--host/--token flags override the IRIS_* environment, which overrides
// an optional .iris/iris.toml under the workspace, which overrides the built-in
// defaults (the local socket + managed Postgres of the zero-config path). Full
// consumption -- actually dialing the resolved target -- arrives with the daemon
// client in E02/E03; today the exit-3 dial path resolves through it so the
// precedence is real rather than a library sitting unused, and a misconfigured
// iris.toml or IRIS_* value surfaces as a warning instead of silently.
func (a *app) resolveTarget(cmd *cobra.Command) config.Settings {
	workspace, err := os.Getwd()
	if err != nil {
		workspace = "" // fall back to .iris-relative defaults
	}
	env, err := config.FromEnv(os.Getenv)
	if err != nil {
		a.logger.Warn("ignoring malformed IRIS_* configuration", "err", err)
		env = config.Layer{}
	}
	file, err := config.LoadTOMLFile(filepath.Join(workspace, config.DirName, config.FileName))
	if err != nil {
		a.logger.Warn("ignoring unreadable iris.toml", "err", err)
		file = config.TOML{}
	}
	for _, key := range file.Ignored {
		a.logger.Warn("ignoring non-engine key in iris.toml (never a project manifest)", "key", key)
	}
	settings := config.Resolve(config.Defaults(workspace), file.Layer, env, flagLayer(cmd))
	if a.forceLocalTarget {
		// The quickstart tour's child apps only ever target the local workspace
		// engine: any ambient host (IRIS_HOST or iris.toml) is dropped here, after
		// the tour has announced it once. The socket keeps its full precedence.
		settings.Host = ""
	}
	return settings
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
	// the daemon starts (specification section 8).
	if v, ok := changedString(cmd, "pg-dsn"); ok {
		l.PgDSN = &v
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
