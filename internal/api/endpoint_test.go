package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// This file proves the serving half of the endpoint apply lifecycle
// (specification section 7) over a running HTTP server and fakes -- no live
// Postgres: an applied endpoint takes effect on commit and serves /q requests
// without a daemon restart, and a re-apply swaps the shape at a request
// boundary while in-flight requests finish with their starting shape. The
// endpoint read executor is a fake that echoes the shape it was handed, so a
// response proves exactly which compiled shape served it.

// qTables is the declared source the /q test endpoints compile against.
func qTables() map[string]*declare.Table {
	return map[string]*declare.Table{"analytics.orders": {
		Schema: "analytics",
		Table:  "orders",
		Columns: []declare.Column{
			{Name: "id", Type: "uuid", PrimaryKey: true},
			{Name: "customer_id", Type: "uuid"},
			{Name: "amount", Type: "numeric"},
		},
	}}
}

// compileQEndpoint compiles one endpoint YAML document against qTables.
func compileQEndpoint(t *testing.T, doc string) *declare.CompiledEndpoint {
	t.Helper()
	ep, err := declare.ParseEndpoint([]byte(doc))
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	ce, err := declare.CompileEndpoint(ep, qTables())
	if err != nil {
		t.Fatalf("compile endpoint: %v", err)
	}
	return ce
}

// okVerifier is a PrepareVerifier that accepts every derived statement.
type okVerifier struct{}

func (okVerifier) PrepareVerify(context.Context, string, string) error { return nil }

// shapeEchoReader is a fake api.EndpointReader: it answers one row naming the
// fields of the compiled shape it was handed, so a response proves which shape
// served it. The first holdN reads signal started and then block until release
// closes, modeling an in-flight request held open across a re-apply.
type shapeEchoReader struct {
	mu      sync.Mutex
	holdN   int
	started chan string
	release chan struct{}
}

func (f *shapeEchoReader) ReadEndpoint(_ context.Context, shape *declare.CompiledEndpoint, _ *api.QueryPlan) ([]map[string]any, error) {
	fields := strings.Join(shape.Fields, ",")
	f.mu.Lock()
	hold := f.holdN > 0
	if hold {
		f.holdN--
	}
	f.mu.Unlock()
	if f.started != nil {
		f.started <- fields
	}
	if hold {
		<-f.release
	}
	return []map[string]any{{"served_fields": fields}}, nil
}

// qHarness is the wired serving surface: one mux (built once, never rebuilt)
// over the live endpoint registry, plus the lifecycle applier that publishes
// into it on commit.
type qHarness struct {
	applier *dispatch.EndpointApplier
	live    *dispatch.EndpointRegistry
	reader  *shapeEchoReader
	mux     http.Handler
}

func newQHarness(t *testing.T, reader *shapeEchoReader) qHarness {
	t.Helper()
	rec := storetest.NewWriteRecorder()
	d := dispatch.New(rec)
	d.Start(context.Background())
	t.Cleanup(d.Stop)
	live := dispatch.NewEndpointRegistry()
	return qHarness{
		applier: dispatch.NewEndpointApplier(okVerifier{}, d, live),
		live:    live,
		reader:  reader,
		mux:     api.NewMux(api.WithEndpoints(live), api.WithEndpointReader(reader)),
	}
}

// qEnvelope is the decoded /q success document: the data rows plus the
// pagination half of the wire contract.
type qEnvelope struct {
	Data []map[string]any `json:"data"`
	Page *struct {
		NextAfter any `json:"next_after"`
		Limit     int `json:"limit"`
	} `json:"page"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// getQ performs a GET against a running test server and decodes the envelope.
func getQ(t *testing.T, baseURL, path string) (int, qEnvelope) {
	t.Helper()
	res, err := http.Get(baseURL + path) //nolint:gosec,noctx // G107: a test-server URL.
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close() //nolint:errcheck // a test read
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", path, err)
	}
	var env qEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %s envelope %q: %v", path, body, err)
	}
	return res.StatusCode, env
}

// TestEndpointApplyLiveOnCommit proves an applied endpoint takes effect on
// commit and serves requests without a daemon restart (specification section
// 7): against one running HTTP server, /q/{name} is a 404 not_found before the
// apply, and the moment Apply commits, the very same server -- same mux, same
// listener, nothing rebuilt or restarted -- serves the endpoint's rows in the
// data+page envelope.
//
// spec: S07/endpoint-apply-live-on-commit
func TestEndpointApplyLiveOnCommit(t *testing.T) {
	ctx := context.Background()
	h := newQHarness(t, &shapeEchoReader{})
	srv := httptest.NewServer(h.mux)
	defer srv.Close()

	const doc = `endpoint: orders_by_customer
source: analytics.orders
fields: [id, customer_id, amount]
filters:
  customer_id: eq
sort: id
`

	// Before the apply the endpoint does not exist: 404 on the mounted surface.
	code, env := getQ(t, srv.URL, "/q/orders_by_customer")
	if code != http.StatusNotFound || env.Error == nil || env.Error.Code != "not_found" {
		t.Fatalf("pre-apply GET /q/orders_by_customer = %d %+v, want 404 not_found", code, env.Error)
	}

	if err := h.applier.Apply(ctx, []*declare.CompiledEndpoint{compileQEndpoint(t, doc)}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The same running server serves the endpoint immediately: no restart, no
	// rebuild, effective on commit.
	code, env = getQ(t, srv.URL, "/q/orders_by_customer")
	if code != http.StatusOK {
		t.Fatalf("post-apply GET /q/orders_by_customer = %d (%+v), want 200 on the same running server", code, env.Error)
	}
	if len(env.Data) != 1 || env.Data[0]["served_fields"] != "id,customer_id,amount" {
		t.Errorf("post-apply data = %v, want the applied shape's rows", env.Data)
	}
	if env.Page == nil || env.Page.Limit != api.DefaultLimit || env.Page.NextAfter != nil {
		t.Errorf("post-apply page = %+v, want limit %d and next_after null (a short page)", env.Page, api.DefaultLimit)
	}
}

// TestEndpointReapplyBoundarySwap proves re-applying an endpoint swaps its
// shape at a request boundary and in-flight requests finish with their
// starting shape (specification section 7): a request checked out on the old
// shape is held mid-flight while the re-apply commits; a new request on the
// same running server immediately serves the new shape, and the held request
// then completes with the shape it started with.
//
// spec: S07/endpoint-reapply-boundary-swap
func TestEndpointReapplyBoundarySwap(t *testing.T) {
	ctx := context.Background()
	reader := &shapeEchoReader{
		holdN:   1,
		started: make(chan string, 4),
		release: make(chan struct{}),
	}
	h := newQHarness(t, reader)
	srv := httptest.NewServer(h.mux)
	defer srv.Close()

	const oldDoc = `endpoint: orders_by_customer
source: analytics.orders
fields: [id, customer_id]
filters:
  customer_id: eq
sort: id
`
	const newDoc = `endpoint: orders_by_customer
source: analytics.orders
fields: [id, customer_id, amount]
filters:
  customer_id: eq
sort: id
`

	if err := h.applier.Apply(ctx, []*declare.CompiledEndpoint{compileQEndpoint(t, oldDoc)}); err != nil {
		t.Fatalf("Apply old shape: %v", err)
	}

	// Request 1 checks out the old shape at its boundary and is held in flight.
	type result struct {
		code int
		env  qEnvelope
	}
	res1 := make(chan result, 1)
	go func() {
		code, env := getQ(t, srv.URL, "/q/orders_by_customer")
		res1 <- result{code, env}
	}()
	if started := <-reader.started; started != "id,customer_id" {
		t.Fatalf("in-flight request checked out shape %q, want the old shape id,customer_id", started)
	}

	// Re-apply commits while request 1 is still in flight: the swap happens at
	// the request boundary, so the registry serves the new shape from now on.
	if err := h.applier.Apply(ctx, []*declare.CompiledEndpoint{compileQEndpoint(t, newDoc)}); err != nil {
		t.Fatalf("re-apply new shape: %v", err)
	}
	if shape, ok := h.live.Endpoint("orders_by_customer"); !ok || strings.Join(shape.Fields, ",") != "id,customer_id,amount" {
		t.Fatalf("post-re-apply registry shape = %+v, want the new projection live on commit", shape)
	}

	// Request 2, started after the swap, serves the new shape immediately --
	// while request 1 is still in flight on the old one.
	code, env := getQ(t, srv.URL, "/q/orders_by_customer")
	if code != http.StatusOK || len(env.Data) != 1 || env.Data[0]["served_fields"] != "id,customer_id,amount" {
		t.Fatalf("post-swap GET = %d %v, want 200 with the new shape's rows", code, env.Data)
	}
	<-reader.started // drain request 2's checkout signal

	// Release request 1: it finishes with the shape it started with, untouched
	// by the swap that committed mid-flight.
	close(reader.release)
	r1 := <-res1
	if r1.code != http.StatusOK || len(r1.env.Data) != 1 || r1.env.Data[0]["served_fields"] != "id,customer_id" {
		t.Errorf("in-flight request finished = %d %v, want 200 with its starting shape id,customer_id", r1.code, r1.env.Data)
	}
}
