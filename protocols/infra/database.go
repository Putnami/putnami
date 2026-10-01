package infra

import (
	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
)

// DatabasesFromManifest derives the thin, deployer-facing infra database
// requirements from the canonical database RequirementManifest. It is the
// single bridge that makes protocol/database the source of truth for infra's
// database entries: infra no longer models database needs independently, it
// projects them from the database protocol.
//
// The manifest is flattened with m.Project() — the secret-free
// {name, engine, schemas} triplet, already sorted by datasource name — and each
// entry is mapped onto infra.Database after translating the database-protocol
// engine into the infra engine enum.
//
// Secrets cannot leak through this path. A RequirementManifest has no
// connection field at all (strict parsing rejects one; see
// database.ParseAndValidateRequirementManifest), and Project() carries only a
// logical name, an engine, and the owning schemas. There is no field on either
// the input or the output that could hold a DSN, host, user, password, ssl
// flag, driver param, or any test policy.
//
// A nil or empty manifest yields no databases and no diagnostics. An engine the
// infra protocol does not recognize yields an ErrorCodeInvalidEngine diagnostic
// for that entry and drops it, so a caller can surface a build error instead of
// emitting an unprocessable requirement.
func DatabasesFromManifest(m *pdb.RequirementManifest) ([]Database, []diag.Diagnostic) {
	projected := m.Project()
	if len(projected) == 0 {
		return nil, nil
	}
	out := make([]Database, 0, len(projected))
	var diags []diag.Diagnostic
	for _, p := range projected {
		engine, ok := engineFromProtocol(p.Engine)
		if !ok {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidEngine, "databases."+p.Name+".engine",
				"database protocol engine %q has no infra mapping", string(p.Engine)))
			continue
		}
		out = append(out, Database{
			Name:    p.Name,
			Engine:  engine,
			Schemas: append([]string(nil), p.Schemas...),
		})
	}
	if len(out) == 0 {
		out = nil
	}
	return out, diags
}

// engineFromProtocol maps a database-protocol engine onto the infra engine
// enum. The database protocol is Postgres-only in v1, so postgres is the only
// mapping; an unrecognized engine returns ok=false so DatabasesFromManifest can
// report it rather than silently coerce it.
func engineFromProtocol(e pdb.Engine) (Engine, bool) {
	switch e {
	case pdb.EnginePostgres:
		return EnginePostgres, true
	default:
		return "", false
	}
}
