package sdd

import (
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"sort"

	archproto "go.putnami.dev/protocol/architecture"
	capabilityproto "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// Framework evidence: what a build recorded about the contracts a project's
// components actually enforce.
//
// A `domainAccess` row in a committed capability manifest says a running
// component was configured with one declared import. `architecture validate`
// joins those rows to the declarations so it can report a declared active
// contract nothing implements, and an implemented one nobody declared.
//
// # Why this reads committed files and nothing else
//
// The verdict has to stay cacheable, and a cache key may only name what the task
// reads. Capability manifests are committed at an exact path under each project
// root, so the `architecture-validate` task declares them as one more file
// pattern beside the ARC declarations — no git, no network, no build. That is
// the same rule the existing inputs follow, and it is why this cannot instead
// ask a build to run: a verdict that depended on unkeyed state would be restored
// from cache while claiming to have checked.
//
// # What this is NOT
//
// It is not runtime observation. Nothing here watched a request, a query, or a
// delivered event; it read what a producer recorded a component was CONFIGURED
// with. The snapshot says exactly that — `framework-evidence`, never
// `observed` — because a coverage report that overstated itself would be worse
// than one that detects nothing.

// CapabilityManifestRelativePath is where a project commits its capability
// manifest. The path is exact and project-relative, which is what lets the
// cached task name it as a file pattern.
var CapabilityManifestRelativePath = path.Join("schema", capabilityproto.ManifestFilename)

// DetectFrameworkEvidence reads each mapped project's committed capability
// manifest and projects its domain-access rows onto the protocol's evidence
// records. The manifest is only a container: an aggregated dependency row is
// attributed to the workspace project its contribution identity owns.
//
// An identity owner outside every declared domain stays outside coverage,
// exactly as an unmapped project's dependencies do: a row that could not be
// attributed to a domain cannot be compared with a declaration, and guessing
// the domain from its container is the inference this system exists to avoid.
//
// A manifest that does not parse is reported as a diagnostic rather than
// silently skipped. Treating an unreadable producer artifact as "no evidence"
// would turn a broken build into a clean architecture verdict.
func DetectFrameworkEvidence(ws *workspace.Workspace, sources []archproto.ManifestSource) ([]archproto.EvidenceRecord, []diag.Diagnostic) {
	if ws == nil {
		return nil, nil
	}
	domainByProject := make(map[string]string)
	for _, source := range sources {
		if source.Manifest == nil {
			continue
		}
		for _, project := range source.Manifest.Projects {
			domainByProject[project] = source.Manifest.Domain
		}
	}

	var (
		records     []archproto.EvidenceRecord
		diagnostics []diag.Diagnostic
	)
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		if _, mapped := domainByProject[project.ID]; !mapped {
			continue
		}
		relative := path.Join(filepath.ToSlash(project.Path), CapabilityManifestRelativePath)
		data, err := readOptionalBoundedRegularFile(filepath.Join(ws.Root, filepath.FromSlash(relative)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeReadFailure, relative,
				"read capability manifest: %v", err))
			continue
		}
		document, findings := capabilityproto.ParseAndValidateManifestDocument(data)
		if diag.HasErrors(findings) || document == nil {
			appendPathDiagnostics(&diagnostics, relative, diag.Errors(findings))
			continue
		}
		// Only v2 carries domain-access rows. A v1 manifest is a producer that
		// predates the evidence channel, not a project that implements nothing,
		// so it contributes no record and no diagnostic.
		if document.V2 == nil {
			continue
		}
		for _, row := range document.V2.DomainAccess {
			owner := resolveProjectSelector(ws, row.Identity.OwnerProject)
			if owner == nil {
				continue
			}
			domain, mapped := domainByProject[owner.ID]
			if !mapped {
				continue
			}
			records = append(records, evidenceRecord(domain, owner.ID, row))
		}
	}
	sortDiagnostics(diagnostics)
	sort.Slice(records, func(i, j int) bool {
		left, right := records[i], records[j]
		if left.ConsumerDomain != right.ConsumerDomain {
			return left.ConsumerDomain < right.ConsumerDomain
		}
		if left.Import != right.Import {
			return left.Import < right.Import
		}
		if left.Mode != right.Mode {
			return left.Mode < right.Mode
		}
		return left.ConsumerProject < right.ConsumerProject
	})
	return records, diagnostics
}

// evidenceRecord carries a capability row onto the architecture protocol's
// vocabulary without interpreting it.
//
// The mode, the transport kinds, and the availabilities are strings on the
// capability wire on purpose — that protocol owns none of their meaning — and
// they are typed here without being checked. A value the architecture protocol
// does not recognize simply matches no declaration, which is reported as
// evidence-without-declaration: the disagreement between the two documents IS
// the finding, and a second copy of the vocabulary here would hide it.
func evidenceRecord(domain, project string, row capabilityproto.DomainAccessV2) archproto.EvidenceRecord {
	record := archproto.EvidenceRecord{
		Kind:            archproto.EvidenceFrameworkPrimitive,
		ConsumerDomain:  domain,
		ConsumerProject: project,
		Import:          row.Import,
		Mode:            archproto.AccessMode(row.Mode),
	}
	for _, transport := range row.Transports {
		record.Transports = append(record.Transports, archproto.EvidenceTransport{
			Role:         transport.Role,
			Kind:         archproto.TransportKind(transport.Kind),
			Availability: archproto.LifecycleStatus(transport.Availability),
		})
	}
	return record
}
