package config

// Permission scopes define the canonical authorization scopes for
// config server operations. These are used in JWT claims and middleware
// to control access to config, schema, and secret endpoints.
const (
	ScopeSchemaRead  = "config.schema.read"
	ScopeSchemaWrite = "config.schema.write"
	ScopeValueRead   = "config.value.read"
	ScopeValueWrite  = "config.value.write"
	ScopeSecretRead  = "config.secret.read"  // #nosec G101 -- permission scope, not a credential
	ScopeSecretWrite = "config.secret.write" // #nosec G101 -- permission scope, not a credential
)

// AllScopes returns all canonical permission scopes.
func AllScopes() []string {
	return []string{
		ScopeSchemaRead,
		ScopeSchemaWrite,
		ScopeValueRead,
		ScopeValueWrite,
		ScopeSecretRead,
		ScopeSecretWrite,
	}
}
