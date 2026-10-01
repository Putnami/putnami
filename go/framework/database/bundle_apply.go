package database

import (
	"context"
	"io/fs"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/migration"
	diag "go.putnami.dev/protocol/diagnostic"
	protocolmigration "go.putnami.dev/protocol/migration"
)

// LoadBundleSources reconstructs SQL migration sources from a published bundle
// directory rooted at fsys (the directory containing bundle.json). It is the
// inverse of bundle emission: LoadBundle validates the manifest and payloads,
// then each `sql` operation becomes a Definition (its up/down payload bytes
// loaded from the bundle) grouped into one SQLSource per (datasource,
// namespace).
//
// The reconstructed sources apply byte-for-byte identically to the originals —
// same names, SQL, datasources, and therefore same hashes and canonical ids —
// so a bundle can be executed without the application graph that produced it.
// Non-SQL operations are ignored; their runners arrive in a later slice.
func LoadBundleSources(fsys fs.FS) ([]SQLSource, error) {
	bundle, payloads, diags := protocolmigration.LoadBundle(fsys)
	if diag.HasErrors(diags) {
		return nil, perrors.Newf(CodeMigrationInvalidDef, "invalid migration bundle: %s", joinDiagnostics(diags))
	}

	// Group definitions by (datasource, namespace), preserving first-seen order
	// for deterministic source construction.
	type key struct{ datasource, namespace string }
	groups := make(map[key][]Definition)
	schemas := make(map[key]string)
	var order []key

	for i := range bundle.Operations {
		op := bundle.Operations[i]
		if op.Kind != protocolmigration.KindSQL {
			continue
		}
		def := Definition{
			Name:       op.Name,
			SQL:        string(payloads[op.Up.Path]),
			Namespace:  op.Namespace,
			Datasource: op.Target,
		}
		if op.Down != nil {
			def.Down = string(payloads[op.Down.Path])
		}
		k := key{datasource: op.Target, namespace: op.Namespace}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], def)
		// All operations in a group come from one source, so they carry the same
		// schema; record it so the reconstructed source sets the apply
		// search_path. Cross-source consistency for a datasource is enforced by
		// the SQLRunner at materialization.
		if op.Schema != "" {
			schemas[k] = op.Schema
		}
	}

	sources := make([]SQLSource, 0, len(order))
	for _, k := range order {
		// The schema round-trips through the bundle so the applied source sets it
		// as the search_path — that is what lets one published bundle target many
		// schemas. The datasource (Target) still scopes canonical ids and payload
		// paths byte-for-byte.
		sources = append(sources, NewSQLSource(k.namespace, Datasource{Name: k.datasource, Schema: schemas[k]}, nil, groups[k]...))
	}
	return sources, nil
}

// ApplyBundle applies a published migration bundle against pool. It is the
// generic SQL bundle runner: it reconstructs sources with LoadBundleSources and
// drives them through ApplyToPool, with no service image or application graph
// required. The pool's lifecycle stays with the caller.
//
// This is the local counterpart of remote bundle execution: the same bundle
// the publish channel uploads can be applied directly from disk against an
// environment-bound pool.
func ApplyBundle(ctx context.Context, pool *Pool, fsys fs.FS) ([]migration.Record, error) {
	sources, err := LoadBundleSources(fsys)
	if err != nil {
		return nil, err
	}
	return ApplyToPool(ctx, pool, sources...)
}
