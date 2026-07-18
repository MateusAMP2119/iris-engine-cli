package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/plugin"
)

// pluginEntry is one installed plugin in the --json data envelope.
type pluginEntry struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Kind    string   `json:"kind,omitempty"`
	Verbs   []string `json:"verbs,omitempty"`
	Digest  string   `json:"digest,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// pluginCmd builds `iris plugin`: the daemonless install/list/remove/verify
// lifecycle of digest-pinned plugin binaries under ~/.iris/plugins (#215).
func (a *app) pluginCmd() *cobra.Command {
	install := &cobra.Command{
		Use: "install <manifest>", Short: "Install a plugin from a manifest (local path or URL), verifying its pinned sha256",
		Args: cobra.ExactArgs(1), RunE: a.pluginInstall(),
	}
	list := &cobra.Command{
		Use: "list", Short: "List installed plugins with their verified digests",
		Args: cobra.NoArgs, RunE: a.pluginList(false),
	}
	remove := &cobra.Command{
		Use: "remove <name[@version]>", Short: "Remove an installed plugin (one version, or all of them)",
		Args: cobra.ExactArgs(1), RunE: a.pluginRemove(),
	}
	verify := &cobra.Command{
		Use: "verify", Short: "Re-verify every installed plugin binary against its manifest pin",
		Args: cobra.NoArgs, RunE: a.pluginList(true),
	}
	return a.group("plugin", "Manage installed plugins (declared external capabilities)",
		daemonless(install), daemonless(list), daemonless(remove), daemonless(verify))
}

// pluginsRoot resolves the installed-plugins root under the engine home.
func pluginsRoot() (string, error) {
	home, err := config.Home(os.Getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, plugin.DirName), nil
}

// pluginFault maps a plugin operation failure to operation-failed (exit 4).
func pluginFault(op string, err error) error {
	return &fault{code: exitOpFailed, codeStr: "plugin_" + op + "_failed", message: fmt.Sprintf("iris plugin %s: %v", op, err)}
}

// pluginInstall handles `iris plugin install <manifest>`.
func (a *app) pluginInstall() runE {
	return func(cmd *cobra.Command, args []string) error {
		root, err := pluginsRoot()
		if err != nil {
			return pluginFault("install", err)
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		res, err := plugin.Install(ctx, root, args[0], nil)
		if err != nil {
			return pluginFault("install", err)
		}
		verbs := make([]string, 0, len(res.Manifest.Verbs))
		for v := range res.Manifest.Verbs {
			verbs = append(verbs, v)
		}
		entry := pluginEntry{
			Name: res.Manifest.Name, Version: res.Manifest.Version,
			Kind: string(res.Manifest.Kind), Verbs: verbs, Digest: res.Digest,
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: entry})
		}
		fmt.Fprintf(a.out, "installed %s@%s (sha256 %s)\n", entry.Name, entry.Version, shortDigest(entry.Digest))
		return nil
	}
}

// pluginList handles `iris plugin list` and, with verifying, `iris plugin
// verify` (same walk; verify fails the invocation when any entry is broken).
func (a *app) pluginList(verifying bool) runE {
	return func(cmd *cobra.Command, _ []string) error {
		op := "list"
		if verifying {
			op = "verify"
		}
		root, err := pluginsRoot()
		if err != nil {
			return pluginFault(op, err)
		}
		entries, err := plugin.List(root)
		if err != nil {
			return pluginFault(op, err)
		}
		payload := make([]pluginEntry, 0, len(entries))
		broken := 0
		for _, e := range entries {
			pe := pluginEntry{Name: e.Name, Version: e.Version, Kind: string(e.Kind), Verbs: e.Verbs, Digest: e.Digest}
			if e.Err != nil {
				pe.Error = e.Err.Error()
				broken++
			}
			payload = append(payload, pe)
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			if err := json.NewEncoder(a.out).Encode(dataEnvelope{Data: payload}); err != nil {
				return err
			}
		} else {
			if len(payload) == 0 {
				fmt.Fprintln(a.out, "no plugins installed")
			}
			for _, pe := range payload {
				if pe.Error != "" {
					fmt.Fprintf(a.out, "%s@%s  BROKEN  %s\n", pe.Name, pe.Version, pe.Error)
					continue
				}
				fmt.Fprintf(a.out, "%s@%s  %s  sha256 %s  verbs %s\n",
					pe.Name, pe.Version, pe.Kind, shortDigest(pe.Digest), strings.Join(pe.Verbs, ","))
			}
		}
		if verifying && broken > 0 {
			return &fault{code: exitOpFailed, codeStr: "plugin_verify_failed",
				message: fmt.Sprintf("iris plugin verify: %d of %d installed plugins failed verification", broken, len(payload))}
		}
		return nil
	}
}

// pluginRemove handles `iris plugin remove <name[@version]>`.
func (a *app) pluginRemove() runE {
	return func(cmd *cobra.Command, args []string) error {
		root, err := pluginsRoot()
		if err != nil {
			return pluginFault("remove", err)
		}
		name, version := args[0], ""
		if strings.Contains(args[0], "@") {
			ref, err := plugin.ParseRef(args[0])
			if err != nil {
				return a.usage(fmt.Sprintf("iris plugin remove: %v", err))
			}
			name, version = ref.Name, ref.Version
		}
		if err := plugin.Remove(root, name, version); err != nil {
			return pluginFault("remove", err)
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: map[string]string{"removed": args[0]}})
		}
		fmt.Fprintf(a.out, "removed %s\n", args[0])
		return nil
	}
}

// shortDigest abbreviates a sha256 for the human line.
func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
