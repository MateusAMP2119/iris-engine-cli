package store_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// TestEndpointsTableShape proves the endpoints and endpoint_filters store shape:
// endpoints keys name (PK) to a dotted schema.table source, a JSON fields
// projection, and a unique-column keyset sort; endpoint_filters is keyed
// (endpoint, param) with a foreign key to endpoints.name and an op drawn from the
// closed set (eq, range). The DDL is E02.1's; this locks the shape the endpoint
// compiler's results persist into.
func TestEndpointsTableShape(t *testing.T) {
	s := store.MetaSchema()

	ep := tableByName(t, s, "endpoints")

	// name is the whole primary key: one row per declared endpoint.
	if !reflect.DeepEqual(ep.PrimaryKey, []string{"name"}) {
		t.Errorf("endpoints primary key = %v, want [name]", ep.PrimaryKey)
	}

	// source is the dotted schema.table, fields is the JSON projection, sort is the
	// keyset key (a unique source column). All are required (non-nullable).
	source := columnByName(t, ep, "source")
	if source.Type != "text" || source.Nullable {
		t.Errorf("endpoints.source = {%q, nullable=%v}, want a non-null text schema.table", source.Type, source.Nullable)
	}
	fields := columnByName(t, ep, "fields")
	if fields.Type != "json" || fields.Nullable {
		t.Errorf("endpoints.fields = {%q, nullable=%v}, want a non-null json projection", fields.Type, fields.Nullable)
	}
	sort := columnByName(t, ep, "sort")
	if sort.Type != "text" || sort.Nullable {
		t.Errorf("endpoints.sort = {%q, nullable=%v}, want a non-null text keyset key", sort.Type, sort.Nullable)
	}

	ef := tableByName(t, s, "endpoint_filters")

	// (endpoint, param) is the composite primary key: one row per filter param.
	if !reflect.DeepEqual(ef.PrimaryKey, []string{"endpoint", "param"}) {
		t.Errorf("endpoint_filters primary key = %v, want [endpoint param]", ef.PrimaryKey)
	}

	// endpoint -> endpoints.name is the foreign key: filters hang off their endpoint.
	var epFK *store.ForeignKey
	for i := range ef.ForeignKeys {
		if ef.ForeignKeys[i].Column == "endpoint" {
			epFK = &ef.ForeignKeys[i]
		}
	}
	if epFK == nil || epFK.RefTable != "endpoints" || epFK.RefColumn != "name" {
		t.Errorf("endpoint_filters.endpoint FK = %+v, want -> endpoints.name", epFK)
	}

	// op is the closed enum the CHECK pins: eq or range, nothing else.
	var opCheck *store.Check
	for i := range ef.Checks {
		if ef.Checks[i].Column == "op" {
			opCheck = &ef.Checks[i]
		}
	}
	if opCheck == nil {
		t.Fatal("endpoint_filters has no CHECK on op")
	}
	if !reflect.DeepEqual(opCheck.Values, []string{"eq", "range"}) {
		t.Errorf("endpoint_filters.op values = %v, want [eq range]", opCheck.Values)
	}

	// The rendered DDL meets Postgres exactly as asserted above.
	ddl := ef.CreateTableDDL()
	for _, want := range []string{
		"CHECK (op IN ('eq', 'range'))",
		"FOREIGN KEY (endpoint) REFERENCES endpoints (name)",
		"PRIMARY KEY (endpoint, param)",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("endpoint_filters DDL is missing %q:\n%s", want, ddl)
		}
	}
}
