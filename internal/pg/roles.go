package pg

// This file is the live pipeline-role surface of specification sections 4 and 7: the
// least-privilege Postgres login role the engine ensures for each pipeline, the grants
// it applies onto the data database, and the meta-database denial that keeps the
// control plane unreachable to a pipeline. pg owns the data cluster, so the CREATE
// ROLE / GRANT / REVOKE DDL is issued here (store owns the meta access ledger's truth;
// the two never cross). Roles are cluster-global, so this DDL runs on the same
// data-database admin connection every other provisioning statement rides.

// pipelineRolePrefix is the fixed prefix of every engine-managed pipeline login role
// name, so a role in the cluster is recognizably engine-owned and never collides with
// a hand-created role.
const pipelineRolePrefix = "iris_pipeline_"

// PipelineRoleName derives the cluster-unique Postgres login-role name for a
// pipeline's least-privilege role (roles.pg_role): the fixed engine prefix followed by
// the pipeline name. It is the single derivation both the live role provisioning (pg)
// and the meta access ledger (store, via the caller) use, so the ledger's pg_role and
// the created role are always the same name.
func PipelineRoleName(pipeline string) string {
	return pipelineRolePrefix + pipeline
}
