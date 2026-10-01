package infra

import (
	diag "go.putnami.dev/protocol/diagnostic"
	pstorage "go.putnami.dev/protocol/storage"
)

// StoragesFromManifest derives the thin, deployer-facing infra storage
// requirements from the canonical storage Manifest. It is the single bridge
// that makes protocol/storage the source of truth for infra's bucket entries,
// mirroring how DatabasesFromManifest bridges the database protocol: infra no
// longer models bucket needs independently, it projects them.
//
// The manifest is flattened with m.Project() — the secret-free
// {name, access, public, retention} projection, already sorted by resource name
// — and each entry is mapped onto infra.StorageBucket after translating the
// storage-protocol access level into the infra enum.
//
// Binding coordinates cannot leak through this path. A Manifest has no backend,
// bucket, prefix, or identity field at all — those live only in a storage
// Binding — and Project() additionally drops the isolation scope and the
// signed-URL signer. There is no field on either the input or the output that
// could carry the resolved physical location or a credential.
//
// A nil or empty manifest yields no buckets and no diagnostics. An access level
// the infra protocol does not recognize yields an ErrorCodeInvalidAccess
// diagnostic for that entry and drops it, so a caller can surface a build error
// instead of emitting an unprocessable requirement. An unset access maps to an
// unset infra access — a valid "no grant declared" state, not an error.
func StoragesFromManifest(m *pstorage.Manifest) ([]StorageBucket, []diag.Diagnostic) {
	projected := m.Project()
	if len(projected) == 0 {
		return nil, nil
	}
	out := make([]StorageBucket, 0, len(projected))
	var diags []diag.Diagnostic
	for _, p := range projected {
		access, ok := storageAccessFromProtocol(p.Access)
		if !ok {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidAccess, "storage."+p.Name+".access",
				"storage protocol access %q has no infra mapping", string(p.Access)))
			continue
		}
		out = append(out, StorageBucket{
			Name:      p.Name,
			Access:    access,
			Public:    p.Public,
			Retention: p.Retention,
		})
	}
	if len(out) == 0 {
		out = nil
	}
	return out, diags
}

// storageAccessFromProtocol maps a storage-protocol access level onto the infra
// enum. The two protocols share the same closed set (read / write / readwrite),
// so the mapping is one-to-one; an empty access is carried through as unset (the
// storage protocol leaves access optional), and an unrecognized value returns
// ok=false so StoragesFromManifest can report it rather than silently coerce it.
func storageAccessFromProtocol(a pstorage.Access) (StorageAccess, bool) {
	switch a {
	case "":
		return "", true
	case pstorage.AccessRead:
		return StorageAccessRead, true
	case pstorage.AccessWrite:
		return StorageAccessWrite, true
	case pstorage.AccessReadWrite:
		return StorageAccessReadWrite, true
	default:
		return "", false
	}
}
