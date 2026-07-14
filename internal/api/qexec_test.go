package api_test

import (
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file proves the /q execution contracts at the mux tier with a recording
// executor fake (integration, no live Postgres): the route authorizes with the
// data scope and executes the compiled endpoint statement as the calling PAT's
// role -- never a role of the endpoint's own, because endpoints own no roles
// and mint no credentials -- and a Postgres grant refusal surfaces as 403
// forbidden naming the endpoint, never the missing fields.

// mapEndpointSource is a fake api.EndpointSource over a fixed compiled set.
type mapEndpointSource map[string]*declare.CompiledEndpoint

func (m mapEndpointSource) Endpoint(name string) (*declare.CompiledEndpoint, bool) {
	ce, ok := m[name]
	return ce, ok
}

// qExecMux wires a mux serving one compiled endpoint through the production
// pool reader over the recording executor.
func qExecMux(ce *declare.CompiledEndpoint, exec *fakeExecutor) http.Handler {
	return api.NewMux(
		api.WithEndpoints(mapEndpointSource{ce.Name: ce}),
		api.WithEndpointReader(api.NewPoolReader(exec)),
	)
}

// TestQCallerRoleExecution proves /q/{endpoint} authorizes with the data scope
// and executes as the caller PAT's role, never an endpoint-owned role.
func TestQCallerRoleExecution(t *testing.T) {
	t.Run("q-caller-role-execution", func(t *testing.T) {
		const doc = `endpoint: orders_by_customer
source: analytics.orders
fields: [id, customer_id, amount]
filters:
  customer_id: eq
sort: id
`
		const cid = "3b241101-e2bb-4255-8caf-4136c566a962"

		t.Run("executes the compiled statement as the calling PAT's role", func(t *testing.T) {
			ce := compileQEndpoint(t, doc)
			exec := &fakeExecutor{rows: []map[string]any{{"id": cid, "customer_id": cid, "amount": "10"}}}
			mux := qExecMux(ce, exec)

			code, env := doGet(t, mux, http.MethodGet, "/q/orders_by_customer?customer_id="+cid,
				dataPAT("a1", "iris_pat_r_alice"))
			if code != http.StatusOK || env.Error != nil {
				t.Fatalf("GET /q = %d %+v, want 200", code, env.Error)
			}
			call := exec.last(t)
			if call.role != "iris_pat_r_alice" || call.self {
				t.Errorf("executed as (role=%q, self=%v), want the calling PAT's role", call.role, call.self)
			}
			if call.sql != ce.SQL {
				t.Errorf("executed statement is not the compiled endpoint text:\n%s", call.sql)
			}
			if !reflect.DeepEqual(call.columns, ce.Fields) {
				t.Errorf("served columns = %v, want the compiled projection %v", call.columns, ce.Fields)
			}
			if !containsArg(call.args, cid) {
				t.Errorf("args %v missing the bound filter value", call.args)
			}
			if len(env.Data) != 1 {
				t.Errorf("data = %v, want the executor's row", env.Data)
			}
		})

		t.Run("two callers, one endpoint, two roles: the role is the caller's, never the endpoint's", func(t *testing.T) {
			ce := compileQEndpoint(t, doc)
			exec := &fakeExecutor{}
			mux := qExecMux(ce, exec)

			for _, caller := range []struct{ id, role string }{
				{"a1", "iris_pat_r_alice"},
				{"b1", "iris_pat_r_bob"},
			} {
				if code, _ := doGet(t, mux, http.MethodGet, "/q/orders_by_customer", dataPAT(caller.id, caller.role)); code != http.StatusOK {
					t.Fatalf("GET as %s = %d, want 200", caller.id, code)
				}
			}
			if len(exec.calls) != 2 || exec.calls[0].role != "iris_pat_r_alice" || exec.calls[1].role != "iris_pat_r_bob" {
				t.Fatalf("recorded roles = %+v, want each caller's own role", exec.calls)
			}
		})

		t.Run("the data scope is required: a read-only PAT never reaches execution", func(t *testing.T) {
			ce := compileQEndpoint(t, doc)
			exec := &fakeExecutor{}
			readOnly := api.Authority{PATID: "r1", Scopes: []pat.Scope{pat.ScopeRead}}
			code, env := doGet(t, qExecMux(ce, exec), http.MethodGet, "/q/orders_by_customer", readOnly)
			if code != http.StatusForbidden || env.Error == nil || env.Error.Code != "forbidden" {
				t.Fatalf("read-only GET /q = %d %+v, want 403 forbidden", code, env.Error)
			}
			if exec.count() != 0 {
				t.Error("a scope-refused request reached the executor")
			}
		})

		t.Run("an ambient request executes as the engine itself, assuming no role", func(t *testing.T) {
			ce := compileQEndpoint(t, doc)
			exec := &fakeExecutor{}
			code, _ := doGet(t, qExecMux(ce, exec), http.MethodGet, "/q/orders_by_customer", api.Authority{Ambient: true})
			if code != http.StatusOK {
				t.Fatalf("ambient GET /q = %d, want 200", code)
			}
			call := exec.last(t)
			if !call.self || call.role != "" {
				t.Errorf("ambient executed as (role=%q, self=%v), want the engine's own self read", call.role, call.self)
			}
		})

		t.Run("a data authority without a role is an internal fault, never a fallback role", func(t *testing.T) {
			ce := compileQEndpoint(t, doc)
			exec := &fakeExecutor{}
			noRole := api.Authority{PATID: "x1", Scopes: []pat.Scope{pat.ScopeData}}
			code, env := doGet(t, qExecMux(ce, exec), http.MethodGet, "/q/orders_by_customer", noRole)
			if code != http.StatusInternalServerError || env.Error == nil || env.Error.Code != "internal" {
				t.Fatalf("roleless data GET /q = %d %+v, want 500 internal", code, env.Error)
			}
			if exec.count() != 0 {
				t.Error("a roleless request reached the executor")
			}
		})
	})
}

// TestQForbiddenNamesEndpointMapping proves the integration half of the 403
// contract: when the executor reports a Postgres grant refusal, /q answers 403
// forbidden naming the endpoint and never the missing fields or the Postgres
// error text. (The conformance tier proves the refusal originates in a real
// Postgres.)
func TestQForbiddenNamesEndpointMapping(t *testing.T) {
	t.Run("q-caller-role-execution", func(t *testing.T) {
		t.Run("a grant refusal is 403 naming the endpoint, never the fields", func(t *testing.T) {
			ce := compileQEndpoint(t, `endpoint: orders_by_customer
source: analytics.orders
fields: [id, customer_id, amount]
filters:
  customer_id: eq
sort: id
`)
			exec := &fakeExecutor{err: fmt.Errorf(
				"execute q: %w: ERROR: permission denied for table orders (column customer_id)", store.ErrReadForbidden)}
			code, env := doGet(t, qExecMux(ce, exec), http.MethodGet, "/q/orders_by_customer",
				dataPAT("a1", "iris_pat_r_alice"))
			if code != http.StatusForbidden || env.Error == nil || env.Error.Code != "forbidden" {
				t.Fatalf("refused GET /q = %d %+v, want 403 forbidden", code, env.Error)
			}
			if !strings.Contains(env.Error.Message, "orders_by_customer") {
				t.Errorf("message %q does not name the endpoint", env.Error.Message)
			}
			for _, leak := range []string{"customer_id", "amount", "permission denied"} {
				if strings.Contains(env.Error.Message, leak) {
					t.Errorf("message %q leaks %q; a 403 names the endpoint, never the missing fields", env.Error.Message, leak)
				}
			}
		})
	})
}

// TestEndpointAuthorityCallingPAT proves endpoints own no roles or
// credentials -- structurally, the compiled shape carries none, and
// behaviorally, a read request executes under the calling PAT's role.
func TestEndpointAuthorityCallingPAT(t *testing.T) {
	t.Run("endpoint-authority-calling-pat", func(t *testing.T) {
		t.Run("a compiled endpoint carries no role or credential", func(t *testing.T) {
			credential := regexp.MustCompile(`(?i)role|credential|password|secret|token`)
			ty := reflect.TypeOf(declare.CompiledEndpoint{})
			for i := 0; i < ty.NumField(); i++ {
				if name := ty.Field(i).Name; credential.MatchString(name) {
					t.Errorf("declare.CompiledEndpoint carries field %q; endpoints own no roles or credentials", name)
				}
			}
		})

		t.Run("a read request executes under the calling PAT's role", func(t *testing.T) {
			ce := compileQEndpoint(t, `endpoint: orders_by_customer
source: analytics.orders
fields: [id, amount]
filters:
  customer_id: eq
sort: id
`)
			exec := &fakeExecutor{}
			code, _ := doGet(t, qExecMux(ce, exec), http.MethodGet, "/q/orders_by_customer",
				dataPAT("c1", "iris_pat_r_carol"))
			if code != http.StatusOK {
				t.Fatalf("GET /q = %d, want 200", code)
			}
			if call := exec.last(t); call.role != "iris_pat_r_carol" || call.self {
				t.Errorf("executed as (role=%q, self=%v), want exactly the calling PAT's role", call.role, call.self)
			}
		})
	})
}
