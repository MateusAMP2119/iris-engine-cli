package cli

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// ceremonyCmd exposes shared install/uninstall ceremony helpers so install.sh
// can drive Bubble Tea progress bars after the binary is on disk (download still
// has to stay in shell — there is no binary yet).
func (a *app) ceremonyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    "ceremony",
		Short:  "Install/uninstall ceremony helpers",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	progress := &cobra.Command{
		Use:   "progress [label]",
		Short: "Run one aligned Bubble Tea progress bar",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			label := "• Working"
			if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
				label = args[0]
			}
			if a.out == nil {
				a.out = os.Stdout
			}
			runProgressBar(a.out, label)
			return nil
		},
	}
	c.AddCommand(daemonless(progress))
	return daemonless(c)
}
