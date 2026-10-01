package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// ProjectIDFromPath derives a project's canonical logical ID from its physical
// workspace-relative path. Complete, non-empty parenthesized segments are
// transparent grouping folders: they remain in Project.Path but are omitted
// from Project.ID.
func ProjectIDFromPath(projectPath string) string {
	physicalPath := CleanWorkspacePath(projectPath)
	if physicalPath == "" {
		return "/"
	}

	segments := strings.Split(physicalPath, "/")
	logicalSegments := make([]string, 0, len(segments))
	for _, segment := range segments {
		if isTransparentGroupSegment(segment) {
			continue
		}
		logicalSegments = append(logicalSegments, segment)
	}
	return "/" + strings.Join(logicalSegments, "/")
}

func isTransparentGroupSegment(segment string) bool {
	return len(segment) > 2 && segment[0] == '(' && segment[len(segment)-1] == ')'
}

// identityDigestFormat versions the CLI-side identity fold. It is hashed with
// the payload, so bumping it moves every identity and every cache key that
// consumes one at once — the reviewed lever for changing what identity covers.
const identityDigestFormat = "wsid1"

// ProbeViewOf projects a discovered project onto the probe protocol's project
// shape. It is the view a core-owned provider would
// return for this project, which is what makes the identity digest below a
// function of the SAME normalized shape that out-of-tree providers will
// contribute to once the probe pipeline lands — rather than a private,
// second encoding of the same facts that would drift away from the contract.
//
// Two exclusions are deliberate:
//
//   - Version is left empty. The authored version already keys the cache
//     through CacheKey.EmbeddedVersion, and only for tasks that actually stamp
//     it into their output. Folding it in here would make every task's key move
//     on every version bump, which is over-invalidation, not completeness.
//
//   - Nothing derived from an absolute path, a timestamp, or a file size enters
//     the view. Project.Path is workspace-relative by construction; keeping it
//     that way is what lets two checkouts of the same commit agree on a key.
//
// resolveDependency maps a declared dependency name to the depended-on
// project's workspace-relative path; it returns ("", false) for a name no
// workspace project answers to.
func ProbeViewOf(p *Project, resolveDependency func(string) (string, bool)) wsproto.ProbeProject {
	if p == nil {
		return wsproto.ProbeProject{}
	}
	view := wsproto.ProbeProject{
		Path:       ProbePathOf(p.Path),
		SourceName: p.SourceName,
		Type:       p.Type,
		Tags:       append([]string(nil), p.Tags...),
		Publish:    append([]string(nil), p.Publish...),
		RunsWith:   append([]string(nil), p.RunsWith...),
		Extensions: append([]string(nil), p.Extensions...),
	}
	if resolveDependency != nil {
		for _, dep := range p.Dependencies {
			depPath, ok := resolveDependency(dep)
			if !ok {
				continue
			}
			probePath := ProbePathOf(depPath)
			view.Dependencies = append(view.Dependencies, probePath)
			// Provenance travels with the edge it attributes, so the view a
			// consumer re-adopts describes the same graph. The metadata digest
			// drops it again: no task reads provenance (see
			// Workspace.MetadataDigest).
			if source, attributed := p.DependencySources[dep]; attributed && source.ReportableByProvider() {
				if view.DependencySources == nil {
					view.DependencySources = make(map[string]wsproto.DependencySource, len(p.DependencySources))
				}
				view.DependencySources[probePath] = source
			}
		}
	}
	// NormalizeProbeProject sorts and dedupes every list, so the view is
	// canonical regardless of discovery order.
	wsproto.NormalizeProbeProject(&view)
	return view
}

// ProbePathOf translates the CLI's workspace-relative path spelling (where the
// workspace root is "") into the probe protocol's ("." for the root).
func ProbePathOf(projectPath string) string {
	cleaned := CleanWorkspacePath(projectPath)
	if cleaned == "" {
		return wsproto.ProbeRootPath
	}
	return cleaned
}

// ProjectMetadataDigest is the per-project identity that enters every cache key
// affected by project metadata or dependency edges.
//
// It folds three things, and needs all three:
//
//   - The normalized probe-shaped view's digest, which covers source identity,
//     type, tags, publish channels, runs-with services, requested extensions,
//     and the RESOLVED dependency paths.
//   - The raw declared dependency NAMES, sorted. A name is a build address: a
//     dependency that keeps its path but changes its name is a different edge
//     for everything that resolves it, and a dependency the workspace cannot
//     resolve at all contributes no path and would otherwise vanish from the
//     key entirely.
//   - The PROVIDER-OWNED metadata blocks, in extension order, each canonicalized.
//     This is the half that moved to provider ownership: those blocks
//     are delivered to every task as `project.metadata[<extension>]`, replacing
//     the npm-shaped members job context v2 used to carry, so a task's answer can
//     depend on them and a key that ignored them would serve a stale hit after a
//     `bin`/`exports` edit. Canonicalization is what keeps a provider's own
//     re-ordering of its JSON object keys from moving the key.
//   - For a generated client target, its CONTRACT BINDING: the service it was
//     generated from and the digest of the contract bytes it was generated at.
//     That digest is the target's whole provider-side identity — the contract
//     edge is an ordering relation and carries no upstream key — so without it
//     a moved contract would leave every key of the client where it was. A
//     project with no committed client manifest folds nothing here and keeps
//     the digest it had.
//
// Deterministic by construction: every input is sorted or canonically encoded,
// and none carries a timestamp, an absolute path, or a stat value.
func ProjectMetadataDigest(
	view wsproto.ProbeProject, declaredDependencies []string, metadata map[string]json.RawMessage,
	contract *GeneratedClientBinding,
) string {
	names := append([]string(nil), declaredDependencies...)
	sort.Strings(names)

	h := sha256.New()
	writeIdentityField(h, identityDigestFormat)
	writeIdentityField(h, wsproto.ProbeProjectDigest(view))
	writeIdentityField(h, "dependencyNames")
	previous := ""
	for i, name := range names {
		if i > 0 && name == previous {
			continue
		}
		writeIdentityField(h, name)
		previous = name
	}

	writeIdentityField(h, "providerMetadata")
	extensions := make([]string, 0, len(metadata))
	for extension := range metadata {
		extensions = append(extensions, extension)
	}
	sort.Strings(extensions)
	for _, extension := range extensions {
		canonical, ok := wsproto.CanonicalMetadata(metadata[extension])
		if !ok {
			// A block that is not a JSON object cannot travel on the wire and is
			// rejected by the probe contract before it reaches here. Hashing the
			// raw bytes anyway keeps the key honest about the fact that SOMETHING
			// was reported, rather than silently agreeing with the absent case.
			canonical = metadata[extension]
		}
		writeIdentityField(h, extension)
		writeIdentityField(h, string(canonical))
	}

	if contract != nil {
		writeIdentityField(h, "contractBinding")
		writeIdentityField(h, contract.ServiceID)
		writeIdentityField(h, contract.ContractSHA256)
		writeIdentityField(h, contract.Language)
	}
	return identityDigestFormat + ":" + hex.EncodeToString(h.Sum(nil))
}

func writeIdentityField(h interface{ Write([]byte) (int, error) }, s string) {
	h.Write([]byte(s)) //nolint:errcheck // hash.Hash.Write never errors
	h.Write([]byte{0}) //nolint:errcheck // hash.Hash.Write never errors
}
