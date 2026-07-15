package declare_test

import (
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// boolPtr returns a pointer to b, for setting an explicit nullable modifier.
func boolPtr(b bool) *bool { return &b }

// TestTypeMappingClosedSet proves each YAML column type in the closed set maps to
// exactly its listed Postgres type, including the
// parametrized varchar(n) / numeric(p,s) forms and the bare numeric.
func TestTypeMappingClosedSet(t *testing.T) {
	t.Run("type-mapping-closed-set", func(t *testing.T) {
		// The full closed type set, plus the two parametrized forms and the
		// bare numeric, each mapping to exactly its Postgres type.
		cases := []struct {
			yaml string
			pg   string
		}{
			{"uuid", "uuid"},
			{"text", "text"},
			{"varchar(255)", "varchar(255)"},
			{"varchar(1)", "varchar(1)"},
			{"int", "integer"},
			{"bigint", "bigint"},
			{"smallint", "smallint"},
			{"numeric(10,2)", "numeric(10,2)"},
			{"numeric", "numeric"},
			{"double", "double precision"},
			{"bool", "boolean"},
			{"timestamptz", "timestamptz"},
			{"timestamp", "timestamp"},
			{"date", "date"},
			{"time", "time"},
			{"json", "json"},
			{"jsonb", "jsonb"},
			{"bytea", "bytea"},
		}
		for _, tc := range cases {
			got, err := declare.ResolveColumnType(declare.Column{Name: "c", Type: tc.yaml})
			if err != nil {
				t.Errorf("ResolveColumnType(%q) errored: %v", tc.yaml, err)
				continue
			}
			if got != tc.pg {
				t.Errorf("ResolveColumnType(%q) = %q, want %q", tc.yaml, got, tc.pg)
			}
		}
	})
}

// TestUnknownTypeFailsApply proves a column declaring a YAML type outside the
// closed set fails apply validation, and the error names the offending table,
// column, and type.
func TestUnknownTypeFailsApply(t *testing.T) {
	t.Run("unknown-type-fails-apply", func(t *testing.T) {
		// A valid table sweeps clean.
		ok := &declare.Table{
			Schema: "analytics",
			Table:  "orders",
			Columns: []declare.Column{
				{Name: "id", Type: "uuid", PrimaryKey: true},
				{Name: "amount", Type: "numeric"},
			},
		}
		if err := declare.ValidateTableTypes(ok); err != nil {
			t.Fatalf("valid table rejected: %v", err)
		}

		// An out-of-set type fails apply and names table, column, and bad type.
		bad := &declare.Table{
			Schema: "analytics",
			Table:  "orders",
			Columns: []declare.Column{
				{Name: "id", Type: "uuid", PrimaryKey: true},
				{Name: "blobby", Type: "blob"},
			},
		}
		err := declare.ValidateTableTypes(bad)
		if err == nil {
			t.Fatalf("out-of-set type accepted; want apply failure")
		}
		for _, want := range []string{"analytics", "orders", "blobby", "blob"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}

		// Near-set tokens that are still outside the closed set fail too: a bare
		// varchar without a length, an empty parameter list, and unlisted aliases.
		for _, typ := range []string{"varchar", "numeric()", "varchar()", "money", "serial", "int4", "", "TEXT"} {
			if _, err := declare.ResolveColumnType(declare.Column{Name: "c", Type: typ}); err == nil {
				t.Errorf("out-of-set type %q accepted; want rejection", typ)
			}
		}
	})
}

// TestNullableDefaultsTrue proves a column parsed without an explicit nullable
// modifier is nullable by default.
func TestNullableDefaultsTrue(t *testing.T) {
	t.Run("nullable-defaults-true", func(t *testing.T) {
		// No explicit nullable, no primary key: nullable.
		c := declare.Column{Name: "amount", Type: "numeric"}
		if !c.IsNullable() {
			t.Errorf("column without explicit nullable = not nullable, want nullable by default")
		}
		// An explicit nullable: false still overrides the default to not-null.
		c = declare.Column{Name: "customer_id", Type: "uuid", Nullable: boolPtr(false)}
		if c.IsNullable() {
			t.Errorf("column with nullable: false reported nullable, want not-null")
		}
	})
}

// TestPrimaryKeyImpliesNotNull proves a column marked primary_key is treated as
// not nullable even when nullable is unspecified.
func TestPrimaryKeyImpliesNotNull(t *testing.T) {
	t.Run("primary-key-implies-not-null", func(t *testing.T) {
		// primary_key with nullable unspecified: not nullable.
		c := declare.Column{Name: "id", Type: "uuid", PrimaryKey: true}
		if c.IsNullable() {
			t.Errorf("primary_key column with unspecified nullable reported nullable, want not-null")
		}
		// primary_key with an explicit nullable: false: still not nullable.
		c = declare.Column{Name: "id", Type: "uuid", PrimaryKey: true, Nullable: boolPtr(false)}
		if c.IsNullable() {
			t.Errorf("primary_key column reported nullable, want not-null")
		}
	})
}
