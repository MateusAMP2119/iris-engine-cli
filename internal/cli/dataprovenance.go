package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// This file is the CLI side of `iris data provenance <schema.table> <pk>`: the
// row-level lineage walk. It GETs /provenance/{schema}/{table}/{pk} and renders
// the same payload the route
// serves. Under --json the data envelope is identical (render parity); the
// human form prints the layer stack with per-stamp disposition.
//
// The route (and thus CLI) requires only the read scope; it carries lineage
// only, never row images.

// dataProvenance is the handler for `iris data provenance <schema.table> <pk>`.
func (a *app) dataProvenance() runE {
	return func(cmd *cobra.Command, args []string) error {
		src := args[0]
		pk := args[1]
		parts := strings.SplitN(src, ".", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return a.usage("data provenance: <schema.table> must be two dot-separated identifiers")
		}
		schema, table := parts[0], parts[1]

		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		path := fmt.Sprintf("%s/provenance/%s/%s/%s", base, url.PathEscape(schema), url.PathEscape(table), url.PathEscape(pk))
		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, path, nil)
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("data provenance: build request: %v", err)}
		}
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "data provenance", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			return a.controlFailure(resp, "data provenance")
		}
		var env struct {
			Data api.ProvenanceResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("data provenance: decode daemon response: %v", err)}
		}
		return a.emitDataProvenance(cmd, env.Data)
	}
}

// emitDataProvenance renders the provenance report: --json the envelope carrying
// exactly the route payload; otherwise a stable human layer list.
func (a *app) emitDataProvenance(cmd *cobra.Command, d api.ProvenanceResult) error {
	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: d})
	}
	fmt.Fprintf(a.out, "provenance %s.%s %s\n", d.Schema, d.Table, d.PK)
	if len(d.Stamps) == 0 {
		fmt.Fprintln(a.out, "  (no stamps)")
		return nil
	}
	fmt.Fprintln(a.out, "  stamps:")
	for _, s := range d.Stamps {
		cur := ""
		if d.Author != nil && s.EntryID == d.Author.EntryID {
			cur = " (current)"
		}
		fmt.Fprintf(a.out, "    %d run=%d op=%s undo=%s%s\n", s.EntryID, s.RunID, s.Op, s.Undo, cur)
	}
	if d.Authored && d.Pipeline != "" {
		fmt.Fprintf(a.out, "  author: run %d pipeline %s state %s\n", d.Author.RunID, d.Pipeline, d.State)
		if d.DeclarationChecksum != "" {
			fmt.Fprintf(a.out, "  declaration: %s\n", d.DeclarationChecksum)
		}
		if d.ArtifactHash != nil {
			fmt.Fprintf(a.out, "  artifact: %s\n", *d.ArtifactHash)
		}
	}
	if len(d.Ancestry) > 0 {
		fmt.Fprintln(a.out, "  ancestry:")
		for _, e := range d.Ancestry {
			fmt.Fprintf(a.out, "    %d <- %d (depth %d)\n", e.RunID, e.UpstreamRunID, e.Depth)
		}
	}
	return nil
}
