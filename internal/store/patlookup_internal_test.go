package store

import (
	"context"
	"errors"
	"testing"
)

// scriptedRow is a poolRows fake yielding at most one scripted row whose columns
// are scanned into the destination pointers in order.
type scriptedRow struct {
	present bool
	hash    string
	revoked bool
	scopes  []string
	role    string
	done    bool
	closed  bool
}

func (r *scriptedRow) Next() bool {
	if !r.present || r.done {
		return false
	}
	r.done = true
	return true
}

func (r *scriptedRow) Scan(dest ...any) error {
	*dest[0].(*string) = r.hash
	*dest[1].(*bool) = r.revoked
	*dest[2].(*[]string) = r.scopes
	*dest[3].(*string) = r.role
	return nil
}

func (r *scriptedRow) Err() error { return nil }
func (r *scriptedRow) Close()     { r.closed = true }

// scriptedPool returns a fixed scriptedRow for every query.
type scriptedPool struct{ row *scriptedRow }

func (p *scriptedPool) query(context.Context, string, ...any) (poolRows, error) {
	return p.row, nil
}

// TestLookupPATResolvesRecord proves the PAT lookup resolves a token prefix to its
// hash, revoked flag, scope union, and owned read role -- the record a bearer-token
// verifier authenticates against.
func TestLookupPATResolvesRecord(t *testing.T) {
	row := &scriptedRow{
		present: true,
		hash:    "$argon2id$hash",
		revoked: false,
		scopes:  []string{"data", "read"},
		role:    "iris_pat_abc",
	}
	r := &pgxPATReader{pool: &scriptedPool{row: row}}
	got, err := r.LookupPAT(context.Background(), "abc")
	if err != nil {
		t.Fatalf("LookupPAT: %v", err)
	}
	if got.ID != "abc" || got.Hash != "$argon2id$hash" || got.Revoked {
		t.Errorf("record = %+v, want id=abc hash set revoked=false", got)
	}
	if got.DataRole != "iris_pat_abc" {
		t.Errorf("data role = %q, want iris_pat_abc", got.DataRole)
	}
	if len(got.Scopes) != 2 {
		t.Errorf("scopes = %v, want the two rows", got.Scopes)
	}
	if !row.closed {
		t.Errorf("the row cursor was not closed")
	}
}

// TestLookupPATNotFound proves an absent prefix is ErrPATNotFound (mapped to 401),
// and an empty prefix short-circuits to the same sentinel.
func TestLookupPATNotFound(t *testing.T) {
	r := &pgxPATReader{pool: &scriptedPool{row: &scriptedRow{present: false}}}
	if _, err := r.LookupPAT(context.Background(), "nope"); !errors.Is(err, ErrPATNotFound) {
		t.Fatalf("absent PAT err = %v, want ErrPATNotFound", err)
	}
	if _, err := r.LookupPAT(context.Background(), ""); !errors.Is(err, ErrPATNotFound) {
		t.Fatalf("empty prefix err = %v, want ErrPATNotFound", err)
	}
}
