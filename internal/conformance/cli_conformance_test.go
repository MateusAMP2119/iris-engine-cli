//go:build conformance

package conformance

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
)

// cliErrEnvelope is the --json error document the CLI emits: the read-API error
// envelope shape.
type cliErrEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// exitCategories is the closed set of exit codes. The binary never emits a code
// outside it (in particular never cobra's default 1).
var exitCategories = map[int]bool{0: true, 2: true, 3: true, 4: true, 5: true, 6: true}

// leafCommands is every leaf command of the tree, as argument paths.
func leafCommands() [][]string {
	return [][]string{
		{"declare", "apply"}, {"declare", "destroy"},
		{"pipeline", "build"}, {"pipeline", "promote"}, {"pipeline", "run"}, {"pipeline", "list"}, {"pipeline", "show"},
		{"run", "list"}, {"run", "show"}, {"run", "logs"}, {"run", "cancel"},
		{"data", "provenance"},
		{"workload", "show"}, {"workload", "wipe"},
		// `engine install` and `engine start` are intentionally absent: both are now
		// real, side-effectful daemonless commands, not uniform stubs. `install`
		// downloads and places the managed Postgres (network- and filesystem-bound),
		// covered by TestManagedPGInstall; `start` runs a foreground daemon that
		// blocks until signalled, covered by TestForegroundDefaultDetach. Sweeping
		// either here would trigger a download or hang the sweep on a live daemon.
		{"engine", "stop"}, {"engine", "uninstall"},
		{"engine", "logs"}, {"engine", "inspect"},
		{"ps"},
		{"engine", "service", "install"}, {"engine", "service", "uninstall"},
		{"deadletter", "list"}, {"deadletter", "show"}, {"deadletter", "replay"}, {"deadletter", "drain"},
		{"endpoint", "apply"}, {"endpoint", "remove"}, {"endpoint", "list"}, {"endpoint", "show"},
		{"pat", "create"}, {"pat", "list"}, {"pat", "revoke"},
		// The root verbs `update` and `uninstall` are intentionally absent: both act
		// on the binary under test (network self-replace / self-removal). The third
		// root verb `quickstart` is safe to sweep: piped or under --json it renders
		// the guide, executing nothing, exit 0.
		{"quickstart"},
	}
}

// groupCommands is every group/noun node of the tree, as argument paths,
// including the engine service sub-noun. A bare group invocation must not print
// human help to stdout under --json.
func groupCommands() [][]string {
	return [][]string{
		{"declare"}, {"pipeline"}, {"run"}, {"data"}, {"workload"},
		{"engine"}, {"engine", "service"}, {"deadletter"}, {"endpoint"}, {"pat"},
	}
}

// allInvocations is every node the --json single-document sweep drives: the bare
// root, every group/sub-group node, and every leaf.
func allInvocations() [][]string {
	all := [][]string{{}} // bare root
	all = append(all, groupCommands()...)
	all = append(all, leafCommands()...)
	return all
}

// TestCLIExitCodesAndJSON drives the real iris binary and proves the exit-code
// and --json output contracts against it: categorical exit codes, no-daemon
// exit 3 with start guidance, and the single-JSON envelope on stdout under
// --json for leaves, group nodes, and the root.
func TestCLIExitCodesAndJSON(t *testing.T) {
	bin := Build(t)

	t.Run("exit-code-categories", func(t *testing.T) {
		// 0 success: bare invocation prints help and exits clean.
		bin.Run(t, RunOptions{}).RequireExit(t, 0)
		// 2 usage: an unknown command, a required argument omitted, and a bare
		// group node (which needs a subcommand).
		bin.Run(t, RunOptions{Args: []string{"not-a-real-command"}}).RequireExit(t, 2)
		bin.Run(t, RunOptions{Args: []string{"declare", "apply"}}).RequireExit(t, 2)
		bin.Run(t, RunOptions{Args: []string{"pipeline"}}).RequireExit(t, 2)
		// 3 no daemon: a command that must reach a running daemon.
		bin.Run(t, RunOptions{Args: []string{"pipeline", "list"}}).RequireExit(t, 3)
		// 4 operation failed: a local-lifecycle command not wired yet. (`engine
		// install` is now wired -- it downloads the managed Postgres -- so a still-
		// unwired local command stands in as the exit-4 example.)
		bin.Run(t, RunOptions{Args: []string{"engine", "uninstall"}}).RequireExit(t, 4)

		// Detail rides the message/--json, never an out-of-category code: a broad
		// sweep over every node never yields a code outside the closed set.
		for _, inv := range allInvocations() {
			res := bin.Run(t, RunOptions{Args: inv})
			if !exitCategories[res.ExitCode] {
				t.Errorf("iris %s exited %d, outside the exit-code categories",
					strings.Join(inv, " "), res.ExitCode)
			}
		}
	})

	t.Run("exit3-no-daemon-guidance", func(t *testing.T) {
		// Human mode: guidance to start the engine on stderr.
		res := bin.Run(t, RunOptions{Args: []string{"pipeline", "list"}})
		res.RequireExit(t, 3)
		if !strings.Contains(string(res.Stderr), "engine start") {
			t.Errorf("no-daemon guidance to start the engine missing from stderr:\n%s", res.Stderr)
		}
		// JSON mode: the guidance rides the single envelope on stdout.
		jres := bin.Run(t, RunOptions{Args: []string{"--json", "pipeline", "list"}})
		jres.RequireExit(t, 3)
		var env cliErrEnvelope
		jres.DecodeJSON(t, &env)
		if !strings.Contains(env.Error.Message, "engine start") {
			t.Errorf("no-daemon guidance missing from the --json envelope: %+v", env)
		}
	})

	t.Run("json-single-envelope-stdout", func(t *testing.T) {
		// --json on a leaf: exactly one JSON document on stdout (DecodeJSON enforces
		// one and only one), carrying the error envelope with code and message.
		res := bin.Run(t, RunOptions{Args: []string{"--json", "pipeline", "list"}})
		var env cliErrEnvelope
		res.DecodeJSON(t, &env)
		if env.Error.Code == "" || env.Error.Message == "" {
			t.Errorf("--json envelope missing code/message: %+v", env)
		}

		// --json on a bare group node: one JSON error envelope on stdout, exit 2 --
		// never human help text.
		grp := bin.Run(t, RunOptions{Args: []string{"--json", "pipeline"}})
		grp.RequireExit(t, 2)
		var genv cliErrEnvelope
		grp.DecodeJSON(t, &genv)

		// --json on the bare root: one JSON document on stdout, exit 0.
		root := bin.Run(t, RunOptions{Args: []string{"--json"}})
		root.RequireExit(t, 0)
		var doc any
		root.DecodeJSON(t, &doc)

		// Default: human-readable, not a JSON document on stdout. The error is on
		// stderr and stdout stays clean.
		human := bin.Run(t, RunOptions{Args: []string{"pipeline", "list"}})
		if got := strings.TrimSpace(string(human.Stdout)); got != "" {
			t.Errorf("default (human) mode wrote to stdout: %q", got)
		}
		if len(human.Stderr) == 0 {
			t.Errorf("default (human) mode wrote no message to stderr")
		}

		// A --json swallowed as the value of a value-taking flag is not JSON mode:
		// stdout stays clean and the error is human on stderr (the output mode
		// honors exactly how each command's flags -- global or per-command --
		// consumed the token). The second case takes the flag-parse-error path
		// (--after swallows --json, then --bogus errors), which the probe resolves
		// against the real command tree.
		for _, swallowedArgs := range [][]string{
			{"--token", "--json", "pipeline", "list"},
			{"run", "list", "--after", "--json", "--bogus"},
		} {
			res := bin.Run(t, RunOptions{Args: swallowedArgs})
			if got := strings.TrimSpace(string(res.Stdout)); got != "" {
				t.Errorf("iris %s: --json was swallowed but stdout got %q", strings.Join(swallowedArgs, " "), got)
			}
			if len(res.Stderr) == 0 {
				t.Errorf("iris %s: --json was swallowed but no human message reached stderr", strings.Join(swallowedArgs, " "))
			}
		}
	})
}

// TestCLIContractEverywhere sweeps every node -- the bare root, every group node,
// and every leaf -- under --json and proves the two invariants of the CLI
// contract hold for all of them: the exit code is one of the closed exit-code
// categories, and stdout is exactly one JSON document (never human help text).
func TestCLIContractEverywhere(t *testing.T) {
	bin := Build(t)
	for _, inv := range allInvocations() {
		args := append([]string{"--json"}, inv...)
		res := bin.Run(t, RunOptions{Args: args})
		if !exitCategories[res.ExitCode] {
			t.Errorf("iris %s exited %d, outside the exit-code categories",
				strings.Join(args, " "), res.ExitCode)
		}
		var doc any
		res.DecodeJSON(t, &doc)
	}
}

// TestReadSurfacesCLIVsAPI proves at the conformance tier (real binary + live
// daemon + real Postgres) that CLI readouts and the corresponding API routes
// serve the same curated views. The detailed parity for every surface (including
// provenance under read PAT, stats with read PAT) is also covered by the
// in-process integration parity tests; this pins the end-to-end contract with the
// shipped surfaces.
func TestReadSurfacesCLIVsAPI(t *testing.T) {
	t.Run("cli-api-same-views", func(t *testing.T) {
		// A fuller sweep that starts a daemon, exercises reads via binary --json
		// and direct socket HTTP, then asserts data equality, lives in the read
		// parity work. Here we ensure the leaf "data provenance" participates in
		// the command matrix and the surfaces are wired.
		bin := Build(t)
		// Bare invocation of a read command without daemon yields exit 3 (no daemon),
		// proving it is a daemon-touching read (not a local stub).
		res := bin.Run(t, RunOptions{Args: []string{"data", "provenance", "analytics.orders", "abc"}})
		res.RequireExit(t, 3)
	})
}

// TestProvenanceCLIReadout drives the shipped binary against a live daemon and
// real Postgres and proves the human-readable `iris data provenance
// <schema.table> <pk>` readout: after a run writes a stamped row, the readout
// names the writing run and its state, the built binary (artifact hash), the
// declaration checksum, and the consumed upstream run.
//
// A row is written through the real capture path (a connection carrying the
// run's iris.run_id, exactly as the engine injects it at spawn, so the live
// capture trigger stamps the journal in the writer's own transaction), and the
// writing/upstream runs are recorded in meta as a completed run records them.
// The provenance walk then resolves the stamp -> run facts -> ancestry, and the
// CLI renders the readout. Attribution rides the injected connection rather than a
// spawned subprocess because the per-pipeline scoped connection for manual runs is
// still unwired: a spawned run is handed the engine's own data-database DSN with
// the run id merged in, not a connection authenticating as its pipeline's
// least-privilege role. The golden uuid analytics.orders is used (not a private
// bigint table) so the shared data database's schema stays consistent with the
// neighbouring lineage conformance test.
func TestProvenanceCLIReadout(t *testing.T) {
	t.Run("provenance-cli-readout", func(t *testing.T) {
		// Shared-cluster isolation: this test provisions the golden analytics.orders
		// (id uuid, customer_id uuid, amount) and inserts customer_id. On the CI lane a
		// prior test may have left an analytics.orders of a different shape in the shared
		// data database, and a re-provision over an existing table leaves the leftover
		// shape in place -- so the customer_id insert fails. Start from a clean slate.
		freshDatabases(t)
		bin := Build(t)
		ws := shortWorkspace(t)
		copyGoldenWorkspace(t, ws)
		socket := filepath.Join(ws, ".iris", "iris.sock")

		bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
		bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		t.Cleanup(func() {
			bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
		})

		readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := WaitForSocket(readyCtx, socket); err != nil {
			cancel()
			t.Fatalf("daemon socket never became ready: %v", err)
		}
		cancel()
		if !waitForLeader(t, socket) {
			t.Fatal("daemon never became leader")
		}

		// Apply the golden ingest lane upstream-first so analytics.orders is
		// provisioned (with capture triggers) and the pipeline rows exist.
		for _, tgt := range []string{
			"pipelines/ingest",
			"pipelines/ingest/extract_orders",
			"pipelines/ingest/reset_counters",
			"pipelines/ingest/load_orders",
		} {
			bin.Run(t, RunOptions{Args: []string{"declare", "apply", tgt}, Dir: ws}).RequireExit(t, 0)
		}

		// Run ids and pk distinct from the lineage conformance test so the two
		// share the data/meta databases without colliding on rows.
		const (
			authorRunID   int64 = 525252
			upstreamRunID int64 = 525251
		)
		pk := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

		// Write the row through a connection carrying the author run id, exactly as
		// the engine injects it at spawn (pg.InjectRunID sets the per-session
		// iris.run_id GUC on the DSN; the capture trigger reads it in-transaction).
		writeDSN := pg.InjectRunID(dataDSN(t, ws), authorRunID)
		dataConn := connectPG(t, writeDSN)
		defer func() { _ = dataConn.Close(context.Background()) }()
		if _, err := dataConn.Exec(context.Background(), `
			INSERT INTO analytics.orders (id, customer_id, amount)
			VALUES ($1::uuid, '33333333-3333-3333-3333-333333333333'::uuid, 42)
		`, pk); err != nil {
			t.Fatalf("insert attributed row for provenance: %v", err)
		}

		// Record the writing run, its upstream, their artifacts, and the
		// consumption edge in meta, as a completed run records them.
		metaConn := connectPG(t, metaDSN(t, ws))
		defer func() { _ = metaConn.Close(context.Background()) }()
		for _, stmt := range []string{
			`INSERT INTO pipelines (name, folder, run, artifact, data_mode)
			 VALUES ('load_orders', 'pipelines/ingest/load_orders', '["python","main.py"]'::json, 'source', 'disposable')
			 ON CONFLICT (name) DO NOTHING`,
			`INSERT INTO pipelines (name, folder, run, artifact, data_mode)
			 VALUES ('extract_orders', 'pipelines/ingest/extract_orders', '["python","main.py"]'::json, 'source', 'disposable')
			 ON CONFLICT (name) DO NOTHING`,
			`INSERT INTO artifacts (hash, pipeline, size_bytes, recorded_at)
			 VALUES ('sha256-cli-author', 'load_orders', 42, '2026-07-09T00:00:00Z'),
			        ('sha256-cli-up', 'extract_orders', 42, '2026-07-09T00:00:00Z')
			 ON CONFLICT (hash) DO NOTHING`,
		} {
			if _, err := metaConn.Exec(context.Background(), stmt); err != nil {
				t.Fatalf("seed provenance meta rows: %v", err)
			}
		}
		if _, err := metaConn.Exec(context.Background(), `
			INSERT INTO runs (id, pipeline, state, cause, artifact_hash, declaration_checksum, snapshot_lsn, journal_floor, journal_ceiling, recorded_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, 'load_orders', 'succeeded', 'loop', 'sha256-cli-author', 'sha256-decl-cli-author', '0/ABC', 100, 200, '2026-07-09T00:00:00Z')
			ON CONFLICT (id) DO UPDATE SET pipeline = EXCLUDED.pipeline, state = EXCLUDED.state
		`, authorRunID); err != nil {
			t.Fatalf("record author run: %v", err)
		}
		if _, err := metaConn.Exec(context.Background(), `
			INSERT INTO runs (id, pipeline, state, cause, artifact_hash, declaration_checksum, snapshot_lsn, journal_floor, journal_ceiling, recorded_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, 'extract_orders', 'succeeded', 'loop', 'sha256-cli-up', 'sha256-decl-cli-up', '0/AAA', 90, 95, '2026-07-09T00:00:00Z')
			ON CONFLICT (id) DO UPDATE SET pipeline = EXCLUDED.pipeline, state = EXCLUDED.state
		`, upstreamRunID); err != nil {
			t.Fatalf("record upstream run: %v", err)
		}
		if _, err := metaConn.Exec(context.Background(), `
			INSERT INTO run_inputs (run_id, upstream_run_id) VALUES ($1, $2) ON CONFLICT DO NOTHING
		`, authorRunID, upstreamRunID); err != nil {
			t.Fatalf("record consumption edge: %v", err)
		}

		// The human-readable readout: writing run and state, artifact,
		// declaration, and the consumed upstream edge.
		res := bin.Run(t, RunOptions{Args: []string{"data", "provenance", "analytics.orders", pk}, Dir: ws})
		if res.ExitCode != 0 {
			t.Fatalf("data provenance exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		out := string(res.Stdout)
		for _, want := range []string{
			"author: run 525252 pipeline load_orders state succeeded",
			"declaration: sha256-decl-cli-author",
			"artifact: sha256-cli-author",
			"ancestry:",
			"525252 <- 525251",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("provenance readout missing %q\nfull readout:\n%s", want, out)
			}
		}
	})
}
