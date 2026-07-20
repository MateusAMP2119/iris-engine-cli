package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// TestPipelineListLanePassthrough proves the CLI's `pipeline list --json`
// envelope carries the listing rows' lanes verbatim (the typed decode and
// re-encode never drops the field), while the human line stays name and state
// only -- the live ps view is the human lane surface.
func TestPipelineListLanePassthrough(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	t.Run("pipeline-list-lane-passthrough", func(t *testing.T) {
		sock := shortSocket(t)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatalf("listen unix %s: %v", sock, err)
		}
		srv := &http.Server{
			Handler: api.NewMux(api.WithPipelines(&pipelinesListFunc{items: []api.PipelineListItem{
				{Name: "extract", Active: true, Lane: "ingest"},
				{Name: "solo", Active: false},
			}})),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

		var out, errb bytes.Buffer
		if code := newApp(&out, &errb).run([]string{"--socket", sock, "--json", "pipeline", "list", "--all"}); code != exitOK {
			t.Fatalf("pipeline list --json exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		var env struct {
			Data api.PipelineListResult `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("decode envelope: %v\nstdout: %s", err, out.String())
		}
		if len(env.Data.Pipelines) != 2 || env.Data.Pipelines[0].Lane != "ingest" || env.Data.Pipelines[1].Lane != "" {
			t.Errorf("lanes did not ride the CLI envelope: %+v", env.Data.Pipelines)
		}

		out.Reset()
		if code := newApp(&out, &errb).run([]string{"--socket", sock, "pipeline", "list", "--all"}); code != exitOK {
			t.Fatalf("pipeline list exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if s := out.String(); !strings.Contains(s, "extract\tactive") || strings.Contains(s, "ingest") {
			t.Errorf("human listing must stay name and state only:\n%s", s)
		}
	})
}


// pipelinesListFunc serves a canned pipeline listing for CLI tests.
type pipelinesListFunc struct{ items []api.PipelineListItem }

func (p *pipelinesListFunc) RunPipeline(context.Context, api.PipelineRunRequest) (api.PipelineRunResult, error) {
	return api.PipelineRunResult{}, nil
}

func (p *pipelinesListFunc) ListPipelines(context.Context, bool) (api.PipelineListResult, error) {
	return api.PipelineListResult{Pipelines: p.items}, nil
}
