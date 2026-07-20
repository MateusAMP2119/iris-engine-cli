package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// ceremonyCmd exposes shared install/uninstall ceremony helpers so install.sh
// can drive Bubble Tea progress bars and matching done/quote lines after the
// binary is on disk (download still has to stay in shell — there is no binary
// yet). Also hosts `ceremony review` for the post-run scrollback pager.
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
	done := &cobra.Command{
		Use:   "done [label]",
		Short: "Print one ceremony step-done line with right-aligned [✓]",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			label := "Done"
			if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
				label = strings.TrimSpace(args[0])
			}
			// Strip a leading bullet if the caller passed "• Foo" the way progress does.
			label = strings.TrimSpace(strings.TrimPrefix(label, "•"))
			if a.out == nil {
				a.out = os.Stdout
			}
			jsonMode, _ := cmd.Flags().GetBool("json")
			p := a.newPainter(jsonMode)
			mark := ceremonyCheckMark(p.green("✓"))
			line := formatCeremonyLine(label, mark)
			fmt.Fprintln(a.out, line)
			appendCeremonyLogFile(line)
			return nil
		},
	}
	quote := &cobra.Command{
		Use:   "quote",
		Short: "Print one farewell quote aligned to the ceremony edge",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.out == nil {
				a.out = os.Stdout
			}
			jsonMode, _ := cmd.Flags().GetBool("json")
			a.farewellQuote(a.newPainter(jsonMode))
			return nil
		},
	}
	review := &cobra.Command{
		Use:   "review [file]",
		Short: "Scrollable pager over a ceremony transcript (file, $IRIS_CEREMONY_LOG, or stdin)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.out == nil {
				a.out = os.Stdout
			}
			force, _ := cmd.Flags().GetBool("force")
			content, err := readCeremonyReviewContent(args)
			if err != nil {
				return err
			}
			if strings.TrimSpace(content) == "" {
				return nil
			}
			// Parent install sets IRIS_CEREMONY_LOG; clear the "parent owns review"
			// guard so this explicit subcommand can open the pager.
			if force {
				runCeremonyReview(a.out, content)
				return nil
			}
			// Height-gated, but ignore parent-log disable (this *is* the review).
			if strings.TrimSpace(os.Getenv(ceremonyNoReviewEnv)) != "" {
				return nil
			}
			rows := termRows(a.out)
			nLines := strings.Count(strings.TrimRight(content, "\n"), "\n") + 1
			if nLines <= rows-1 {
				return nil
			}
			runCeremonyReview(a.out, content)
			return nil
		},
	}
	review.Flags().Bool("force", false, "open the pager even when the transcript fits on screen")
	c.AddCommand(daemonless(progress), daemonless(done), daemonless(quote), daemonless(review))
	return daemonless(c)
}

// readCeremonyReviewContent loads a transcript from args[0], $IRIS_CEREMONY_LOG, or piped stdin.
func readCeremonyReviewContent(args []string) (string, error) {
	if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
		b, err := os.ReadFile(args[0])
		if err != nil {
			return "", &fault{code: exitOpFailed, codeStr: "ceremony_review_failed", message: fmt.Sprintf("iris ceremony review: read %s: %v", args[0], err)}
		}
		return string(b), nil
	}
	if path := strings.TrimSpace(os.Getenv(ceremonyLogPathEnv)); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return string(b), nil
		}
	}
	if !stdinLooksLikeTTY() {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", &fault{code: exitOpFailed, codeStr: "ceremony_review_failed", message: fmt.Sprintf("iris ceremony review: read stdin: %v", err)}
		}
		return string(b), nil
	}
	return "", nil
}
