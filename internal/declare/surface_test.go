package declare

import (
	"strings"
	"testing"
)

func acc(table string, fields ...string) Access {
	return Access{Table: table, Fields: fields}
}

func surfaceComposer() *Composer {
	return &Composer{
		Lane:  "ingest",
		Order: []string{"a", "b"},
		Reads: []Access{acc("raw.orders", "id", "amount"), acc("raw.customers", "id")},
		Writes: []Access{
			acc("raw.orders", "id", "amount"),
			acc("raw.totals", "id", "total"),
		},
	}
}

func TestHasSurface(t *testing.T) {
	if (&Composer{Lane: "l", Order: []string{"a"}}).HasSurface() {
		t.Fatal("composer without reads/writes must not have a surface")
	}
	if !surfaceComposer().HasSurface() {
		t.Fatal("composer with reads/writes must have a surface")
	}
	var nilC *Composer
	if nilC.HasSurface() {
		t.Fatal("nil composer must not have a surface")
	}
}

func TestValidateMemberSurface(t *testing.T) {
	tests := []struct {
		name     string
		member   *Pipeline
		siblings []*Pipeline
		wantErr  string
	}{
		{
			name:   "declared subset passes",
			member: &Pipeline{Name: "a", Reads: []Access{acc("raw.orders", "id")}, Writes: []Access{acc("raw.totals", "total")}},
		},
		{
			name:   "undeclared member passes",
			member: &Pipeline{Name: "a"},
		},
		{
			name:    "read table outside surface refused",
			member:  &Pipeline{Name: "a", Reads: []Access{acc("raw.other", "id")}},
			wantErr: `reads table "raw.other" is outside the folder surface`,
		},
		{
			name:    "write table outside surface refused",
			member:  &Pipeline{Name: "a", Writes: []Access{acc("raw.customers", "id")}},
			wantErr: `writes table "raw.customers" is outside the folder surface`,
		},
		{
			name:    "field outside surface entry refused",
			member:  &Pipeline{Name: "a", Reads: []Access{acc("raw.orders", "id", "created_at")}},
			wantErr: `field "created_at" is outside the folder surface's fields`,
		},
		{
			name:     "sibling write claim collides",
			member:   &Pipeline{Name: "a", Writes: []Access{acc("raw.totals", "total")}},
			siblings: []*Pipeline{{Name: "b", Writes: []Access{acc("raw.totals", "id")}}},
			wantErr:  `already claimed by sibling "b"`,
		},
		{
			name:     "sibling reads never collide",
			member:   &Pipeline{Name: "a", Reads: []Access{acc("raw.orders", "id")}},
			siblings: []*Pipeline{{Name: "b", Reads: []Access{acc("raw.orders", "id", "amount")}}},
		},
		{
			name:     "distinct sibling write tables pass",
			member:   &Pipeline{Name: "a", Writes: []Access{acc("raw.orders", "id")}},
			siblings: []*Pipeline{{Name: "b", Writes: []Access{acc("raw.totals", "total")}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMemberSurface(surfaceComposer(), tt.member, tt.siblings)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateMemberSurfaceNoSurface(t *testing.T) {
	c := &Composer{Lane: "ingest", Order: []string{"a", "b"}}
	member := &Pipeline{Name: "a", Writes: []Access{acc("raw.anything", "id")}}
	sibling := &Pipeline{Name: "b", Writes: []Access{acc("raw.anything", "id")}}
	if err := ValidateMemberSurface(c, member, []*Pipeline{sibling}); err != nil {
		t.Fatalf("no surface must validate nothing, got %v", err)
	}
}

func TestValidateComposerSurface(t *testing.T) {
	members := map[string]*Pipeline{
		"a": {Name: "a", Writes: []Access{acc("raw.totals", "total")}},
		"b": {Name: "b", Writes: []Access{acc("raw.totals", "id")}},
	}
	err := ValidateComposerSurface(surfaceComposer(), members)
	if err == nil || !strings.Contains(err.Error(), "already claimed by sibling") {
		t.Fatalf("want duplicate write-claim refusal, got %v", err)
	}

	ok := map[string]*Pipeline{
		"a": {Name: "a", Writes: []Access{acc("raw.totals", "total")}},
		"b": {Name: "b", Reads: []Access{acc("raw.orders", "id")}},
	}
	if err := ValidateComposerSurface(surfaceComposer(), ok); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bad := &Composer{Lane: "ingest", Order: []string{"a"}, Reads: []Access{acc("public.x", "id")}}
	if err := ValidateComposerSurface(bad, nil); err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("want public-schema refusal on the surface itself, got %v", err)
	}
}

func TestEffectiveAccess(t *testing.T) {
	c := surfaceComposer()

	t.Run("declared sides stand as declared", func(t *testing.T) {
		m := &Pipeline{Name: "a", Reads: []Access{acc("raw.orders", "id")}, Writes: []Access{acc("raw.totals", "total")}}
		reads, writes := EffectiveAccess(c, m, nil)
		if len(reads) != 1 || reads[0].Table != "raw.orders" || len(reads[0].Fields) != 1 {
			t.Fatalf("declared reads must stand, got %v", reads)
		}
		if len(writes) != 1 || writes[0].Table != "raw.totals" {
			t.Fatalf("declared writes must stand, got %v", writes)
		}
	})

	t.Run("undeclared inherits surface minus sibling write claims", func(t *testing.T) {
		m := &Pipeline{Name: "a"}
		sib := &Pipeline{Name: "b", Writes: []Access{acc("raw.totals", "total")}}
		reads, writes := EffectiveAccess(c, m, []*Pipeline{sib})
		if len(reads) != 2 {
			t.Fatalf("undeclared reads must inherit the whole surface reads, got %v", reads)
		}
		if len(writes) != 1 || writes[0].Table != "raw.orders" {
			t.Fatalf("inherited writes must drop the sibling-claimed table, got %v", writes)
		}
	})

	t.Run("sibling reads never narrow inheritance", func(t *testing.T) {
		m := &Pipeline{Name: "a"}
		sib := &Pipeline{Name: "b", Reads: []Access{acc("raw.orders", "id")}}
		reads, writes := EffectiveAccess(c, m, []*Pipeline{sib})
		if len(reads) != 2 || len(writes) != 2 {
			t.Fatalf("sibling reads must not narrow anything, got reads %v writes %v", reads, writes)
		}
	})

	t.Run("no surface passes declarations through", func(t *testing.T) {
		plain := &Composer{Lane: "ingest", Order: []string{"a"}}
		m := &Pipeline{Name: "a", Reads: []Access{acc("raw.x", "id")}}
		reads, writes := EffectiveAccess(plain, m, nil)
		if len(reads) != 1 || len(writes) != 0 {
			t.Fatalf("no surface must pass through, got reads %v writes %v", reads, writes)
		}
	})
}

func TestParseComposerSurface(t *testing.T) {
	doc := []byte(`lane: ingest
order: [a, b]
reads:
  - table: raw.orders
    fields: [id]
writes:
  - table: raw.totals
    fields: [id, total]
`)
	d, err := ParseDeclaration(doc)
	if err != nil {
		t.Fatalf("parse composer with surface: %v", err)
	}
	if d.Kind != KindComposer {
		t.Fatalf("want composer, got %v", d.Kind)
	}
	if len(d.Composer.Reads) != 1 || len(d.Composer.Writes) != 1 {
		t.Fatalf("surface not decoded: %+v", d.Composer)
	}
	if !d.Composer.HasSurface() {
		t.Fatal("decoded composer must report a surface")
	}
}
