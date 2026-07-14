package declare_test

import (
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// TestAccessEntryTableFieldsRequired proves each reads/writes entry must name a
// dotted schema.table plus an explicit, non-empty fields list: an entry
// omitting fields (or naming a table that is not exactly schema.table) is
// rejected, with no implicit all-columns fallback.
func TestAccessEntryTableFieldsRequired(t *testing.T) {
	t.Run("access-entry-table-fields-required", func(t *testing.T) {
		cases := []struct {
			name  string
			p     *declare.Pipeline
			want  []string // substrings the rejection must contain
			valid bool
		}{
			{
				name:  "valid read and write entries accepted",
				p:     newAccessPipeline([]declare.Access{{Table: "raw.orders_staging", Fields: []string{"id", "amount"}}}, []declare.Access{{Table: "analytics.orders", Fields: []string{"id"}}}),
				valid: true,
			},
			{
				name: "omitted fields on a read entry rejected, no implicit all-columns",
				p:    newAccessPipeline([]declare.Access{{Table: "raw.orders_staging", Fields: nil}}, nil),
				want: []string{"reads[0]", "raw.orders_staging", "no implicit all-columns"},
			},
			{
				name: "empty fields list on a write entry rejected",
				p:    newAccessPipeline(nil, []declare.Access{{Table: "analytics.orders", Fields: []string{}}}),
				want: []string{"writes[0]", "analytics.orders"},
			},
			{
				name: "table with no dot rejected",
				p:    newAccessPipeline([]declare.Access{{Table: "orders", Fields: []string{"id"}}}, nil),
				want: []string{"reads[0]", "orders", "schema.table"},
			},
			{
				name: "table with two dots rejected",
				p:    newAccessPipeline([]declare.Access{{Table: "a.b.c", Fields: []string{"id"}}}, nil),
				want: []string{"reads[0]", "a.b.c", "schema.table"},
			},
			{
				name: "table with empty schema part rejected",
				p:    newAccessPipeline([]declare.Access{{Table: ".orders", Fields: []string{"id"}}}, nil),
				want: []string{"reads[0]", "schema.table"},
			},
			{
				name: "table with empty table part rejected",
				p:    newAccessPipeline([]declare.Access{{Table: "raw.", Fields: []string{"id"}}}, nil),
				want: []string{"reads[0]", "schema.table"},
			},
			{
				name: "malformed table and missing fields both named",
				p:    newAccessPipeline(nil, []declare.Access{{Table: "orders", Fields: nil}}),
				want: []string{"writes[0]", "schema.table", "no implicit all-columns"},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := declare.ValidateAccess(tc.p)
				if tc.valid {
					if err != nil {
						t.Fatalf("ValidateAccess() = %v, want nil (accepted)", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("ValidateAccess() = nil, want a rejection naming %v", tc.want)
				}
				for _, want := range tc.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("rejection %q does not contain %q", err, want)
					}
				}
			})
		}
	})
}

// TestPublicReadsWritesRejected proves apply rejects any reads/writes entry
// targeting public.* (public is engine-reserved), in both reads and writes,
// while a non-public entry with the same shape is accepted.
func TestPublicReadsWritesRejected(t *testing.T) {
	t.Run("public-reads-writes-rejected", func(t *testing.T) {
		cases := []struct {
			name  string
			p     *declare.Pipeline
			want  []string
			valid bool
		}{
			{
				name: "public read entry rejected",
				p:    newAccessPipeline([]declare.Access{{Table: "public.orders", Fields: []string{"id"}}}, nil),
				want: []string{"reads[0]", "public", "engine-reserved"},
			},
			{
				name: "public write entry rejected",
				p:    newAccessPipeline(nil, []declare.Access{{Table: "public.customers", Fields: []string{"id"}}}),
				want: []string{"writes[0]", "public", "engine-reserved"},
			},
			{
				name:  "non-public entry with the same shape accepted",
				p:     newAccessPipeline([]declare.Access{{Table: "raw.orders", Fields: []string{"id"}}}, nil),
				valid: true,
			},
			{
				name:  "schema merely containing public as a substring is not the reserved schema",
				p:     newAccessPipeline([]declare.Access{{Table: "public_archive.orders", Fields: []string{"id"}}}, nil),
				valid: true,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := declare.ValidateAccess(tc.p)
				if tc.valid {
					if err != nil {
						t.Fatalf("ValidateAccess() = %v, want nil (accepted)", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("ValidateAccess() = nil, want a rejection naming %v", tc.want)
				}
				for _, want := range tc.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("rejection %q does not contain %q", err, want)
					}
				}
			})
		}
	})
}

// TestPublicSchemaFolderRejected proves apply rejects a public schema folder
// under schemas/ (public is engine-reserved), while a schemas/ tree without one
// is unaffected.
func TestPublicSchemaFolderRejected(t *testing.T) {
	t.Run("public-schema-folder-rejected", func(t *testing.T) {
		t.Run("public schema folder rejected", func(t *testing.T) {
			dir := writeTree(t, t.TempDir(), map[string]string{
				"public/orders/table.yaml": "schema: public\ntable: orders\n",
			})
			err := declare.ValidateSchemaTreeReserved(dir)
			if err == nil {
				t.Fatalf("ValidateSchemaTreeReserved() = nil, want rejection of the public schema folder")
			}
			for _, want := range []string{"public", "engine-reserved"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("rejection %q does not contain %q", err, want)
				}
			}
		})

		t.Run("no public folder accepted", func(t *testing.T) {
			dir := writeTree(t, t.TempDir(), map[string]string{
				"analytics/orders/table.yaml": "schema: analytics\ntable: orders\n",
			})
			if err := declare.ValidateSchemaTreeReserved(dir); err != nil {
				t.Fatalf("ValidateSchemaTreeReserved() = %v, want nil (no public folder present)", err)
			}
		})

		t.Run("a folder merely named public_archive is not the reserved schema", func(t *testing.T) {
			dir := writeTree(t, t.TempDir(), map[string]string{
				"public_archive/orders/table.yaml": "schema: public_archive\ntable: orders\n",
			})
			if err := declare.ValidateSchemaTreeReserved(dir); err != nil {
				t.Fatalf("ValidateSchemaTreeReserved() = %v, want nil (not the reserved public schema)", err)
			}
		})
	})
}

// TestReadsWritesNoOrdering proves reads/writes are access-only and never
// exclusive: two pipelines with overlapping writes on the same table both pass
// ValidateAccess, and registering them (upstream-first, as apply does) with no
// depends_on of their own leaves the dependency graph edge-free for both --
// ordering derives only from lanes and depends_on, never from access overlap.
func TestReadsWritesNoOrdering(t *testing.T) {
	t.Run("reads-writes-no-ordering", func(t *testing.T) {
		overlap := declare.Access{Table: "analytics.orders", Fields: []string{"id", "amount"}}
		a := &declare.Pipeline{Name: "a", Writes: []declare.Access{overlap}}
		b := &declare.Pipeline{Name: "b", Writes: []declare.Access{overlap}}

		// Both pipelines pass access validation despite writing the same table:
		// reads/writes are not exclusive.
		if err := declare.ValidateAccess(a); err != nil {
			t.Fatalf("ValidateAccess(a) = %v, want nil (overlapping writes are accepted)", err)
		}
		if err := declare.ValidateAccess(b); err != nil {
			t.Fatalf("ValidateAccess(b) = %v, want nil (overlapping writes are accepted)", err)
		}

		// Register both, upstream-first, as apply does. Neither declares
		// depends_on, so the dependency check reads only Name/DependsOn -- the
		// overlapping access plays no part.
		reg := declare.NewRegistry()
		if err := declare.ValidateDependencies(reg, a); err != nil {
			t.Fatalf("ValidateDependencies(a) = %v, want nil", err)
		}
		reg.Add(a.Name, a.DependsOn...)
		if err := declare.ValidateDependencies(reg, b); err != nil {
			t.Fatalf("ValidateDependencies(b) = %v, want nil", err)
		}
		reg.Add(b.Name, b.DependsOn...)

		if got := reg.DependsOn("a"); len(got) != 0 {
			t.Errorf("pipeline a has depends_on edges %v; overlapping writes must create no dependency edge", got)
		}
		if got := reg.DependsOn("b"); len(got) != 0 {
			t.Errorf("pipeline b has depends_on edges %v; overlapping writes must create no dependency edge", got)
		}
	})
}

// newAccessPipeline builds a minimal pipeline declaration carrying only the
// reads/writes access entries ValidateAccess reads.
func newAccessPipeline(reads, writes []declare.Access) *declare.Pipeline {
	return &declare.Pipeline{Name: "p", Reads: reads, Writes: writes}
}
