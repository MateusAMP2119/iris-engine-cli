package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file is the /q/{endpoint} serving surface of the endpoint apply
// lifecycle (specification section 7): the request-boundary shape checkout and
// the seams the daemon wires it through. A request resolves its compiled shape
// exactly once, at its boundary, from the live EndpointSource (dispatch's
// endpoint registry, swapped on apply commit) and holds that shape to the end,
// so an applied endpoint serves the very next request with no daemon restart
// and a re-apply never disturbs a request already in flight. Execution is
// delegated to the EndpointReader seam: the production reader (the shared
// data-database read pool, SET ROLE to the caller PAT's role, the compiled
// statement with bound params, per-column serialization) lands with
// E09.7/E09.8; until both seams are wired, /q answers the internal-fault
// envelope exactly as E09.5 mounted it.

// EndpointSource is the live compiled-shape lookup the /q route checks a
// request's endpoint out of. The daemon supplies dispatch's endpoint registry;
// shapes are immutable once returned, so a checkout stays valid for the whole
// request regardless of concurrent re-applies.
type EndpointSource interface {
	// Endpoint returns the live compiled shape for name, or false when no such
	// endpoint is applied (the route's 404).
	Endpoint(name string) (*declare.CompiledEndpoint, bool)
}

// EndpointReader executes one compiled endpoint read: the checked-out shape's
// statement with the request's validated plan, returning the served rows as
// column-to-value maps in served order. The production implementation rides
// the shared read pool on the data database (E09.7/E09.8); a fake stands in
// for integration tests.
type EndpointReader interface {
	// ReadEndpoint runs the shape's compiled statement under the request's plan.
	ReadEndpoint(ctx context.Context, shape *declare.CompiledEndpoint, plan *QueryPlan) ([]map[string]any, error)
}

// WithEndpoints wires the live endpoint-shape source the /q route resolves
// requests against. A nil source is ignored, keeping the unwired default (/q
// answers the internal-fault envelope, never a fabricated 404).
func WithEndpoints(src EndpointSource) MuxOption {
	return func(m *mux) {
		if src != nil {
			m.endpoints = src
		}
	}
}

// WithEndpointReader wires the endpoint read executor the /q route serves
// rows through. A nil reader is ignored, keeping the unwired default.
func WithEndpointReader(rd EndpointReader) MuxOption {
	return func(m *mux) {
		if rd != nil {
			m.qreader = rd
		}
	}
}

// Page is the pagination half of a collection envelope (specification section
// 7): {"page": {"next_after": <key|null>, "limit": <n>}}. NextAfter is the
// continuation cursor (null when the page did not fill), Limit the resolved
// page size.
type Page struct {
	// NextAfter is the continuation key of the last served row, or nil.
	NextAfter any `json:"next_after"`
	// Limit is the resolved page size the request was served with.
	Limit int `json:"limit"`
}

// WriteDataPage writes a success envelope wrapping rows plus the pagination
// contract at the given status: {"data": [...], "page": {...}}. It is the one
// place a paged data response is serialized, so every collection route emits
// the identical shape.
func WriteDataPage(w http.ResponseWriter, status int, v any, page Page) {
	writeJSON(w, status, Envelope{Data: v, Page: &page})
}

// serveEndpoint handles GET /q/{endpoint}: the declared read contract
// (specification section 7). With either seam unwired it answers the E09.5
// internal-fault envelope (mounted, scope-checked, never a silent payload).
// Wired, it checks the request's shape out of the live source exactly once --
// the request boundary the re-apply swap honors -- resolves the wire grammar
// against that shape (400 naming a refused param), executes through the
// reader, and serves the data+page envelope.
func (m *mux) serveEndpoint(w http.ResponseWriter, r *http.Request, name string) {
	if m.endpoints == nil || m.qreader == nil {
		serveUnwiredRead(w, r, "endpoint")
		return
	}
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, string(CodeMethodNotAllowed), "GET "+r.URL.Path+" only")
		return
	}

	// The request-boundary checkout: this request serves this shape to its end,
	// even if a re-apply swaps the registry mid-flight.
	shape, ok := m.endpoints.Endpoint(name)
	if !ok {
		WriteError(w, http.StatusNotFound, string(CodeNotFound), "no such endpoint: "+name)
		return
	}

	plan, err := PlanEndpointQuery(shape, r.URL.Query())
	if err != nil {
		var pe *ParamError
		if errors.As(err, &pe) {
			WriteError(w, http.StatusBadRequest, string(CodeBadParam), pe.Error())
			return
		}
		WriteError(w, http.StatusInternalServerError, string(CodeInternal), "api: endpoint "+name+": "+err.Error())
		return
	}

	rows, err := m.qreader.ReadEndpoint(r.Context(), shape, plan)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, string(CodeInternal), "api: endpoint "+name+": "+err.Error())
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	WriteDataPage(w, http.StatusOK, rows, Page{NextAfter: plan.Cursor.NextAfter(rows), Limit: plan.Cursor.Limit})
}
