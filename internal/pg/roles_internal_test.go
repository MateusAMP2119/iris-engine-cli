package pg

import (
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// TestRenderProvisionPipelineRoleGrantsCapture proves the pipeline-role provisioning
// path grants capture reachability out of the box (specification section 4: capture is
// always on, every role). A freshly provisioned least-privilege role must be able to
// reach the always-on iris.capture() function -- its write fires the per-table capture
// trigger, which calls the function -- so provisioning issues USAGE on the iris schema
// and EXECUTE on iris.capture() for every role, independent of any declared field
// grant. These grants are what E06.2's capture-emission proof relied on as explicit
// test setup before; now they ride provisioning.
//
// spec: S04/pipeline-role-reaches-capture
func TestRenderProvisionPipelineRoleGrantsCapture(t *testing.T) {
	role := PipelineRoleName("ingest")

	// A role with NO declared field grants still gets capture reachability: the grants
	// are pipeline-independent, not derived from reads/writes.
	stmts, err := renderProvisionPipelineRole(RoleProvision{
		Role:          role,
		CredentialDDL: "ALTER ROLE " + quoteIdentifier(role) + " PASSWORD 'x';",
		MetaDatabase:  "meta",
		DataDatabase:  "data",
	})
	if err != nil {
		t.Fatalf("renderProvisionPipelineRole: %v", err)
	}

	wantUsage := "GRANT USAGE ON SCHEMA iris TO " + quoteIdentifier(role) + ";"
	wantExec := "GRANT EXECUTE ON FUNCTION iris.capture() TO " + quoteIdentifier(role) + ";"
	if !containsExactly(stmts, wantUsage, 1) {
		t.Errorf("provisioning must grant iris-schema USAGE exactly once; want %q in\n%s", wantUsage, strings.Join(stmts, "\n"))
	}
	if !containsExactly(stmts, wantExec, 1) {
		t.Errorf("provisioning must grant capture EXECUTE exactly once; want %q in\n%s", wantExec, strings.Join(stmts, "\n"))
	}

	// Capture reachability follows CONNECT: the role can connect before it may execute.
	connectIdx := indexOfContaining(stmts, "GRANT CONNECT ON DATABASE")
	usageIdx := indexOf(stmts, wantUsage)
	execIdx := indexOf(stmts, wantExec)
	if connectIdx < 0 || usageIdx < connectIdx || execIdx < connectIdx {
		t.Errorf("capture grants must come after GRANT CONNECT; connect@%d usage@%d exec@%d", connectIdx, usageIdx, execIdx)
	}

	// A role WITH declared field grants gets capture reachability too, and exactly once.
	withGrants, err := renderProvisionPipelineRole(RoleProvision{
		Role:          role,
		CredentialDDL: "ALTER ROLE " + quoteIdentifier(role) + " PASSWORD 'x';",
		MetaDatabase:  "meta",
		DataDatabase:  "data",
		Grants: []declare.FieldGrant{
			{Schema: "analytics", Table: "orders", Field: "amount", Access: declare.AccessRead},
		},
	})
	if err != nil {
		t.Fatalf("renderProvisionPipelineRole with grants: %v", err)
	}
	if !containsExactly(withGrants, wantUsage, 1) || !containsExactly(withGrants, wantExec, 1) {
		t.Errorf("capture reachability must appear exactly once regardless of field grants:\n%s", strings.Join(withGrants, "\n"))
	}
}

// TestRenderProvisionPipelineRoleEnsuresCaptureSurface proves the pipeline-role
// provisioning stream is self-healing and order-independent: it ensures the engine
// capture surface (the iris schema and the iris.capture() function) BEFORE it grants
// the role reachability on that function, so a role provisioned on a cluster where the
// capture function is not yet installed still provisions successfully and the capture
// EXECUTE grant resolves. Without this ensure, GRANT EXECUTE ON FUNCTION iris.capture()
// fails with `schema "iris" does not exist` when role provisioning runs before capture
// install (specification section 4: capture is always on, every role -- the role must
// be able to reach it out of the box).
//
// spec: S04/pipeline-role-reaches-capture
func TestRenderProvisionPipelineRoleEnsuresCaptureSurface(t *testing.T) {
	role := PipelineRoleName("ingest")
	stmts, err := renderProvisionPipelineRole(RoleProvision{
		Role:          role,
		CredentialDDL: "ALTER ROLE " + quoteIdentifier(role) + " PASSWORD 'x';",
		MetaDatabase:  "meta",
		DataDatabase:  "data",
	})
	if err != nil {
		t.Fatalf("renderProvisionPipelineRole: %v", err)
	}

	schemaIdx := indexOfContaining(stmts, "CREATE SCHEMA IF NOT EXISTS iris")
	funcIdx := indexOfContaining(stmts, "CREATE OR REPLACE FUNCTION iris.capture()")
	usageIdx := indexOf(stmts, "GRANT USAGE ON SCHEMA iris TO "+quoteIdentifier(role)+";")
	execIdx := indexOf(stmts, "GRANT EXECUTE ON FUNCTION iris.capture() TO "+quoteIdentifier(role)+";")
	if schemaIdx < 0 || funcIdx < 0 {
		t.Fatalf("provisioning must ensure the iris schema and iris.capture() function; schema@%d func@%d in\n%s",
			schemaIdx, funcIdx, strings.Join(stmts, "\n"))
	}
	if schemaIdx >= funcIdx || funcIdx >= usageIdx || funcIdx >= execIdx {
		t.Errorf("capture surface must be ensured before the capture grants; schema@%d func@%d usage@%d exec@%d",
			schemaIdx, funcIdx, usageIdx, execIdx)
	}
}

// TestRenderProvisionPipelineRoleAttributesAtCreate proves the pipeline login role's
// least-privilege attributes ride the CREATE ROLE and are never re-asserted by a later
// ALTER ROLE. Re-asserting a role's SUPERUSER attribute (even NOSUPERUSER) requires the
// SUPERUSER attribute itself on PG16+, which the engine's non-superuser CREATEROLE admin
// lacks, so a re-provision that issued `ALTER ROLE ... NOSUPERUSER` would hard-fail on
// every repeat. The attributes are set at creation (allowed for a CREATEROLE admin, which
// never grants an attribute it lacks) and the DO-block existence guard keeps a
// re-provision idempotent.
//
// spec: S05/provision-idempotent
func TestRenderProvisionPipelineRoleAttributesAtCreate(t *testing.T) {
	role := PipelineRoleName("ingest")
	stmts, err := renderProvisionPipelineRole(RoleProvision{
		Role:          role,
		CredentialDDL: "ALTER ROLE " + quoteIdentifier(role) + " PASSWORD 'x';",
		MetaDatabase:  "meta",
		DataDatabase:  "data",
	})
	if err != nil {
		t.Fatalf("renderProvisionPipelineRole: %v", err)
	}
	joined := strings.Join(stmts, "\n")

	wantCreate := "CREATE ROLE " + quoteIdentifier(role) + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;"
	if !strings.Contains(joined, wantCreate) {
		t.Errorf("role must be created with its least-privilege attributes baked in; want %q in\n%s", wantCreate, joined)
	}
	// No attribute-asserting ALTER ROLE: the only ALTER ROLE issued is the meta-rendered
	// credential (PASSWORD) statement, which a CREATEROLE admin may issue on repeat.
	for _, s := range stmts {
		if strings.HasPrefix(s, "ALTER ROLE") && strings.Contains(s, "NOSUPERUSER") {
			t.Errorf("provisioning must not re-assert attributes with an ALTER ROLE (PG16+ needs SUPERUSER for that): %q", s)
		}
	}
}

// TestRenderCaptureReachabilityGrants proves the shared helper renders exactly the two
// idempotent capture-reachability grants, with the role as a quoted identifier and the
// engine-owned iris schema and iris.capture() function named.
//
// spec: S04/pipeline-role-reaches-capture
func TestRenderCaptureReachabilityGrants(t *testing.T) {
	got := RenderCaptureReachabilityGrants(`weird"role`)
	want := []string{
		`GRANT USAGE ON SCHEMA iris TO "weird""role";`,
		`GRANT EXECUTE ON FUNCTION iris.capture() TO "weird""role";`,
	}
	if len(got) != len(want) {
		t.Fatalf("RenderCaptureReachabilityGrants returned %d statements, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("grant[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func containsExactly(stmts []string, target string, want int) bool {
	n := 0
	for _, s := range stmts {
		if s == target {
			n++
		}
	}
	return n == want
}

func indexOf(stmts []string, target string) int {
	for i, s := range stmts {
		if s == target {
			return i
		}
	}
	return -1
}

func indexOfContaining(stmts []string, sub string) int {
	for i, s := range stmts {
		if strings.Contains(s, sub) {
			return i
		}
	}
	return -1
}
