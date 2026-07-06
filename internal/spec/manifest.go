// Package spec loads and validates spec/contracts.yaml, the traceability
// manifest that bridges the Iris specification inventory (the source of truth)
// and the test suite. Each manifest row is one spec contract carrying a stable
// id (spec section plus slug), a doc anchor into the specification, the cheapest
// tier that proves it, and its lifecycle status. Non-behavioral contracts
// (naming, rationale, philosophy) are marked exempt and need no claiming test.
package spec

import (
	"fmt"
	"os"
	"regexp"

	"github.com/goccy/go-yaml"
)

// Tier names the cheapest execution tier that proves a contract. The three
// behavioral tiers describe execution only, not importance; TierExempt marks a
// contract that no test can or should claim.
type Tier string

// The behavioral tiers plus the exempt sentinel.
const (
	TierUnit        Tier = "unit"
	TierIntegration Tier = "integration"
	TierConformance Tier = "conformance"
	TierExempt      Tier = "exempt"
)

// Status is a contract's lifecycle state in the manifest.
type Status string

// The statuses a seeded contract may hold. Unclaimed is the starting state of
// every behavioral contract (awaiting a claiming test); Exempt is the terminal
// state of every non-behavioral one.
const (
	StatusUnclaimed Status = "unclaimed"
	StatusExempt    Status = "exempt"
)

// Contract is one manifest row: a single spec contract and its bookkeeping.
type Contract struct {
	ID     string `yaml:"id"`
	Anchor string `yaml:"anchor"`
	Tier   Tier   `yaml:"tier"`
	Status Status `yaml:"status"`
}

// IsExempt reports whether the contract is non-behavioral and therefore exempt
// from the traceability gate's claiming-test requirement.
func (c Contract) IsExempt() bool { return c.Status == StatusExempt }

// NeedsClaimingTest reports whether the traceability gate requires a test to
// claim this contract. Exempt contracts need none; every other contract must be
// claimed by at least one test.
func (c Contract) NeedsClaimingTest() bool { return !c.IsExempt() }

// Manifest is the parsed spec/contracts.yaml.
type Manifest struct {
	Contracts []Contract `yaml:"contracts"`
}

// idPattern matches a stable contract id: a spec section (Sxx or Sxx.y) plus a
// lowercase slug, e.g. S16/manifest-row-schema or S06.2/gate-awaits-latest-success.
var idPattern = regexp.MustCompile(`^S\d\d(\.\d+)?/[a-z0-9-]+$`)

// Load reads and parses the manifest at path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the repo-controlled spec/contracts.yaml location supplied by the build tooling, never user or network input.
	if err != nil {
		return nil, fmt.Errorf("spec: read manifest %s: %w", path, err)
	}
	return Parse(data)
}

// Parse parses a manifest from YAML bytes.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("spec: parse manifest: %w", err)
	}
	return &m, nil
}

// Validate checks the manifest's structural invariants and returns the first
// violation found, or nil when every row is well-formed. It enforces that the
// manifest is non-empty; that every row carries a well-formed id, a doc anchor,
// a known tier, and a known status; that ids are unique (one row per contract);
// and that tier and status agree on exemption. It never requires a claiming
// test, so an exempt row is valid on its own.
func (m *Manifest) Validate() error {
	if len(m.Contracts) == 0 {
		return fmt.Errorf("spec: manifest carries no contracts")
	}
	seen := make(map[string]bool, len(m.Contracts))
	for i, c := range m.Contracts {
		if !idPattern.MatchString(c.ID) {
			return fmt.Errorf("spec: contract #%d: malformed id %q (want Sxx[.y]/slug)", i, c.ID)
		}
		if seen[c.ID] {
			return fmt.Errorf("spec: duplicate contract id %q (one row per contract)", c.ID)
		}
		seen[c.ID] = true
		if c.Anchor == "" {
			return fmt.Errorf("spec: contract %s: missing doc anchor", c.ID)
		}
		switch c.Tier {
		case TierUnit, TierIntegration, TierConformance, TierExempt:
		default:
			return fmt.Errorf("spec: contract %s: unknown tier %q", c.ID, c.Tier)
		}
		switch c.Status {
		case StatusUnclaimed, StatusExempt:
		default:
			return fmt.Errorf("spec: contract %s: unknown status %q", c.ID, c.Status)
		}
		if (c.Tier == TierExempt) != (c.Status == StatusExempt) {
			return fmt.Errorf("spec: contract %s: tier %q and status %q disagree on exemption", c.ID, c.Tier, c.Status)
		}
	}
	return nil
}

// Find returns the contract with the given id and whether it was present.
func (m *Manifest) Find(id string) (Contract, bool) {
	for _, c := range m.Contracts {
		if c.ID == id {
			return c, true
		}
	}
	return Contract{}, false
}

// Testable returns the contracts that require a claiming test, i.e. every
// non-exempt contract, in manifest order.
func (m *Manifest) Testable() []Contract {
	var out []Contract
	for _, c := range m.Contracts {
		if c.NeedsClaimingTest() {
			out = append(out, c)
		}
	}
	return out
}

// Exempt returns the contracts exempt from the claiming-test requirement, in
// manifest order.
func (m *Manifest) Exempt() []Contract {
	var out []Contract
	for _, c := range m.Contracts {
		if c.IsExempt() {
			out = append(out, c)
		}
	}
	return out
}
