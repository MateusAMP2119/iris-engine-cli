package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// These tests pin the stats surface of specification section 11: the read-only
// rollup payload GET /stats serves (and `iris engine stats` prints identically),
// its clock-free purity rules, and the deliberate absence of a /metrics endpoint
// in core.

// clockishNames are field-name fragments that would smuggle a clock-derived
// metric into the stats payload. The payload's doctrine is counts and last-values
// only (specification section 11: "the pass counter is clock-free (a count, not a
// duration): no time-series, no clock-derived metric"), so no stats field may be
// named like a timestamp, duration, rate, or liveness readout.
var clockishNames = []string{
	"time", "duration", "seconds", "millis", "uptime", "age", "latency",
	"heartbeat", "seen", "rate", "elapsed", "since", "when", "at",
}

// TestStatsClockFree proves every stats value is a current count or a last-value:
// the payload's type tree carries no time.Time, no time.Duration, no float (a rate
// or an average would be one), and no field named like a clock readout; its leaves
// are integers (counts), strings (last-values: ids, states, digests), and
// explicit-absence pointers/maps/slices over those. The per-lane pass counter is
// an integer count, never a duration.
func TestStatsClockFree(t *testing.T) {
	t.Run("S11/stats-clock-free", func(t *testing.T) {
		var walked []string
		walkStatsType(t, reflect.TypeOf(api.StatsPayload{}), "StatsPayload", &walked)
		if len(walked) == 0 {
			t.Fatal("walked no stats fields; the payload type is empty")
		}

		// The pass counter is a count, not a duration: an integer field, and not
		// time.Duration (walkStatsType already rejects Duration's int64 by type name).
		passes, ok := reflect.TypeOf(api.LaneStats{}).FieldByName("Passes")
		if !ok {
			t.Fatal("LaneStats has no Passes field (loop passes completed since daemon start)")
		}
		switch passes.Type.Kind() {
		case reflect.Int, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint32, reflect.Uint64:
			// a count
		default:
			t.Fatalf("LaneStats.Passes kind = %s, want an integer count", passes.Type.Kind())
		}
		if passes.Type == reflect.TypeOf(time.Duration(0)) {
			t.Fatal("LaneStats.Passes is a time.Duration; the pass counter is a count, not a duration")
		}
	})
}

// walkStatsType recursively asserts every field reachable from t is clock-free:
// no time.Time or time.Duration anywhere, no float leaves, and no clock-ish field
// name. It appends each visited field path to walked so the caller can prove the
// walk saw the payload.
func walkStatsType(t *testing.T, typ reflect.Type, path string, walked *[]string) {
	t.Helper()
	switch typ {
	case reflect.TypeOf(time.Time{}):
		t.Errorf("%s is a time.Time; stats are clock-free", path)
		return
	case reflect.TypeOf(time.Duration(0)):
		t.Errorf("%s is a time.Duration; stats are counts and last-values only", path)
		return
	}
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		walkStatsType(t, typ.Elem(), path, walked)
	case reflect.Map:
		walkStatsType(t, typ.Key(), path+"[key]", walked)
		walkStatsType(t, typ.Elem(), path+"[value]", walked)
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			fieldPath := path + "." + f.Name
			*walked = append(*walked, fieldPath)
			lower := strings.ToLower(f.Name)
			for _, bad := range clockishNames {
				// Match whole camel-case words, not substrings: "state" contains
				// "at" but is not a clock readout, so compare against the field's
				// lower-cased name split points via a simple contains on the exact
				// fragment bounded check below.
				if lower == bad || strings.HasPrefix(lower, bad+"_") || strings.HasSuffix(lower, "_"+bad) ||
					strings.HasSuffix(lower, bad) && len(bad) > 2 {
					t.Errorf("%s: field name suggests a clock-derived metric (%q)", fieldPath, bad)
				}
			}
			walkStatsType(t, f.Type, fieldPath, walked)
		}
	case reflect.Float32, reflect.Float64:
		t.Errorf("%s is a float; stats leaves are integer counts and string last-values", path)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.String, reflect.Bool:
		// A count or a last-value: fine.
	default:
		t.Errorf("%s has unexpected kind %s in a stats payload", path, typ.Kind())
	}
}

// fixedStats is a StatsHandler serving one fixed payload, standing in for the
// daemon-side rollup so the route's shape is proven against the real mux.
type fixedStats struct{ payload api.StatsPayload }

// compile-time proof the fake satisfies the handler seam the mux routes to.
var _ api.StatsHandler = fixedStats{}

func (f fixedStats) Stats(context.Context) (api.StatsPayload, error) { return f.payload, nil }

// TestStatsRoute proves GET /stats serves the wired handler's payload in the
// section-7 data envelope on the real mux (the same handler the CLI's `iris
// engine stats` reads, so both surfaces are one payload), rejects non-GET
// methods, and faults internally when no handler is wired.
//
// spec: S11/stats-cli-http-parity
func TestStatsRoute(t *testing.T) {
	head := &api.ChainHead{Seq: 3, Digest: "ab12", Location: "resident"}
	payload := api.StatsPayload{
		Engine: api.EngineStats{
			DeadLetterDepth:     2,
			DeadLettersByReason: map[string]int64{"failed": 1, "stopped": 1},
			RunningRuns:         1,
			CapturedWrites:      120,
			WipeEligibleRows:    40,
			JournalRows:         200,
			HotRows:             150,
			SealedPartitions:    3,
			ArchivedPartitions:  1,
			CheckpointChainHead: head,
		},
		Lanes:     []api.LaneStats{{Lane: "ingest", Pipelines: 2, Queued: 1, Running: 1, Passes: 12}},
		Pipelines: []api.PipelineStats{{Pipeline: "extract", LatestRunState: "succeeded", RunsByState: map[string]int64{"succeeded": 3}, LastRunID: "run-3"}},
	}
	mux := api.NewMux(api.WithStats(fixedStats{payload: payload}))

	t.Run("GET /stats serves the data envelope", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /stats status = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
		}
		var env struct {
			Data api.StatsPayload `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode /stats envelope: %v", err)
		}
		if !reflect.DeepEqual(env.Data, payload) {
			t.Errorf("GET /stats payload = %+v, want %+v", env.Data, payload)
		}
	})

	t.Run("chain head is an explicit field, null when absent", func(t *testing.T) {
		empty := payload
		empty.Engine.CheckpointChainHead = nil
		mux := api.NewMux(api.WithStats(fixedStats{payload: empty}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode /stats document: %v", err)
		}
		engine, ok := doc["data"].(map[string]any)["engine"].(map[string]any)
		if !ok {
			t.Fatalf("no engine object in /stats document: %s", rec.Body.String())
		}
		headVal, present := engine["checkpoint_chain_head"]
		if !present {
			t.Fatal("checkpoint_chain_head field absent; the payload keeps the field present and null when no chain exists")
		}
		if headVal != nil {
			t.Fatalf("checkpoint_chain_head = %v, want explicit null with no checkpoints", headVal)
		}
	})

	t.Run("non-GET is method_not_allowed", func(t *testing.T) {
		// On a leader (past the mux's standby mutation gate, which turns any
		// non-safe method on a non-leader into not_leader) a POST to the
		// read-only stats route is a plain 405.
		rec := httptest.NewRecorder()
		leaderMux(api.WithStats(fixedStats{payload: payload})).ServeHTTP(rec,
			httptest.NewRequest(http.MethodPost, "/stats", strings.NewReader("{}")))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST /stats status = %d, want 405", rec.Code)
		}
	})

	t.Run("unwired stats handler is an internal fault", func(t *testing.T) {
		rec := httptest.NewRecorder()
		api.NewMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("unwired GET /stats status = %d, want 500 (fault, never a silent empty payload)", rec.Code)
		}
	})
}

// TestNoMetricsEndpoint proves core exposes no /metrics: the daemon mux answers a
// /metrics request with the closed not_found error envelope, never a metrics
// document -- with and without the stats handler wired (specification section 11:
// a monitor consumes GET /stats; /metrics is deliberately left out).
func TestNoMetricsEndpoint(t *testing.T) {
	t.Run("S11/no-metrics-endpoint", func(t *testing.T) {
		muxes := map[string]http.Handler{
			"bare mux":         api.NewMux(),
			"stats-wired mux":  api.NewMux(api.WithStats(fixedStats{})),
			"role-leader mux":  leaderMux(),
			"role-leader wire": leaderMux(api.WithStats(fixedStats{})),
		}
		for name, mux := range muxes {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: GET /metrics status = %d, want 404 not-found", name, rec.Code)
			}
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Errorf("%s: /metrics body is not the JSON error envelope: %v\nbody: %s", name, err, rec.Body.String())
				continue
			}
			if env.Error.Code != "not_found" {
				t.Errorf("%s: /metrics error code = %q, want not_found", name, env.Error.Code)
			}
			if strings.Contains(rec.Body.String(), "# HELP") || strings.Contains(rec.Body.String(), "# TYPE") {
				t.Errorf("%s: /metrics answered a Prometheus exposition document; core has no metrics surface", name)
			}
		}
	})
}

// leaderMux builds a mux reporting the leader role, so the /metrics refusal is
// proven on a leader too (not an artifact of the standby mutation gate).
func leaderMux(opts ...api.MuxOption) http.Handler {
	role := api.NewRoleState()
	role.SetLeader()
	return api.NewMux(append([]api.MuxOption{api.WithRole(role)}, opts...)...)
}
