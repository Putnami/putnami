package sdd

import (
	"fmt"
	"path"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
)

// specInitFromMaxBytes bounds --from: a format name, a colon and a reference
// path, which the protocol bounds at 4096 bytes.
const specInitFromMaxBytes = 4096 + 32

// parseSpecInitFrom reads `--from <format>:<path>`. The format must be one of
// the closed vocabulary; the path is compared with the manifest reference,
// which is canonical, so a trailing slash or a "./" prefix does not matter.
func parseSpecInitFrom(raw string) (featureproto.SpecReference, error) {
	if len(raw) > specInitFromMaxBytes {
		return featureproto.SpecReference{}, protocolcli.Usagef(
			"--from is longer than %d bytes; it takes <format>:<path>", specInitFromMaxBytes)
	}
	format, location, found := strings.Cut(strings.TrimSpace(raw), ":")
	if !found || format == "" || location == "" {
		return featureproto.SpecReference{}, protocolcli.Usagef(
			"--from takes <format>:<path>, for example openspec:openspec/specs/invoice-export")
	}
	known := false
	names := make([]string, 0, len(featureproto.SpecFormats()))
	for _, candidate := range featureproto.SpecFormats() {
		names = append(names, string(candidate))
		if string(candidate) == format {
			known = true
		}
	}
	if !known {
		return featureproto.SpecReference{}, protocolcli.Usagef(
			"--from names an unknown format; the formats are %s", strings.Join(names, ", "))
	}
	return featureproto.SpecReference{Format: featureproto.SpecFormat(format), Path: path.Clean(location)}, nil
}

// nativeSpecOf returns the native spec that details feature. An external spec
// of the same feature is the source of --from, not a native spec.
func (repository *specRepository) nativeSpecOf(feature string) (featureengine.DiscoveredSpec, bool) {
	for _, discovered := range repository.discovery.Specs {
		if discovered.Format == "" && discovered.Spec != nil && discovered.Spec.Feature == feature {
			return discovered, true
		}
	}
	return featureengine.DiscoveredSpec{}, false
}

// specDraft is the native spec --from drafts and the manifest whose entry names
// its source, the root the native file lands in.
type specDraft struct {
	spec     *featureproto.Spec
	manifest string
}

// draftFromExternalSpec copies the projection the gate's own discovery made of
// the feature's external spec. It reads no file itself, so the draft and the
// gate cannot disagree about what the source says. It refuses a --from that is
// not the feature's reference, a source the discovery could not project, and a
// projection that enforce would refuse: an unanchored positional requirement
// would give the native spec an ID that depends on order (D-042, D-056).
func (repository *specRepository) draftFromExternalSpec(feature string, from featureproto.SpecReference) (specDraft, []diag.Diagnostic, error) {
	reference, field, manifest, found := repository.specReferenceOf(feature)
	if !found {
		return specDraft{}, []diag.Diagnostic{diag.Errorf(SpecCodeInitFromMismatch, "feature",
				"feature %q names no external spec in %s; --from reads only the spec a manifest entry names",
				feature, featureproto.ManifestFilename)},
			protocolcli.Usagef("feature %q names no external spec", feature)
	}
	if reference != from {
		return specDraft{}, []diag.Diagnostic{diag.Errorf(SpecCodeInitFromMismatch, field,
				"--from is not the spec feature %q names, %s:%s", feature, reference.Format, reference.Path)},
			protocolcli.Usagef("--from must name %s:%s, the spec of feature %q", reference.Format, reference.Path, feature)
	}

	findings := repository.externalSpecFindings(reference, field)
	projected, ok := repository.externalSpecOf(feature, reference)
	if !ok {
		if !diag.HasErrors(findings) {
			findings = append(findings, diag.Errorf(featureproto.ErrorCodeSpecRead, reference.Path,
				"the %s spec at %s produced no spec", reference.Format, reference.Path))
		}
		return specDraft{}, findings, protocolcli.InvalidConfigf(
			"the %s spec at %s cannot become a native spec; fix it at the positions listed", reference.Format, reference.Path)
	}
	enforced := projected.Projection.EnforceOnly(featureproto.VerificationModeEnforce)
	findings = replaceFindings(findings, enforced)
	if diag.HasErrors(enforced) {
		return specDraft{}, findings, protocolcli.InvalidConfigf(
			"the %s spec at %s has findings enforce refuses; fix them at the positions listed", reference.Format, reference.Path)
	}

	spec := *projected.Spec
	spec.Schema = featureproto.SpecSchemaURL
	spec.Outcomes = append([]string(nil), spec.Outcomes...)
	spec.NonGoals = append([]string(nil), spec.NonGoals...)
	spec.Requirements = append([]featureproto.SpecRequirement(nil), spec.Requirements...)
	spec.Decisions = append([]string(nil), spec.Decisions...)
	if spec.Outcomes == nil {
		spec.Outcomes = []string{}
	}
	if spec.Requirements == nil {
		spec.Requirements = []featureproto.SpecRequirement{}
	}
	return specDraft{spec: &spec, manifest: manifest}, findings, nil
}

// specReferenceOf finds the manifest entry that names feature's external spec.
func (repository *specRepository) specReferenceOf(feature string) (featureproto.SpecReference, string, string, bool) {
	for _, source := range repository.manifests {
		if source.Manifest == nil {
			continue
		}
		for index, declared := range source.Manifest.Features {
			if declared.ID == feature && declared.Spec != nil {
				return *declared.Spec, fmt.Sprintf("%s#features[%d].spec", source.Path, index), source.Path, true
			}
		}
	}
	return featureproto.SpecReference{}, "", "", false
}

// externalSpecOf returns the discovery's projection of feature's external spec.
func (repository *specRepository) externalSpecOf(feature string, reference featureproto.SpecReference) (featureengine.DiscoveredSpec, bool) {
	for _, discovered := range repository.discovery.Specs {
		if discovered.Format == reference.Format && discovered.Path == reference.Path &&
			discovered.Spec != nil && discovered.Spec.Feature == feature {
			return discovered, true
		}
	}
	return featureengine.DiscoveredSpec{}, false
}

// externalSpecFindings keeps the repository findings on the source: those on
// the manifest entry and its members, and those inside the reference path. The
// member test keeps features[1].spec from owning features[10].spec.
func (repository *specRepository) externalSpecFindings(reference featureproto.SpecReference, field string) []diag.Diagnostic {
	var findings []diag.Diagnostic
	for _, finding := range repository.diagnostics {
		artifact, _, _ := strings.Cut(finding.Field, "#")
		if finding.Field == field || strings.HasPrefix(finding.Field, field+".") || artifact == reference.Path ||
			strings.HasPrefix(artifact, reference.Path+"/") {
			findings = append(findings, finding)
		}
	}
	return findings
}

// replaceFindings drops from findings each finding enforced restates, whatever
// its severity, and appends enforced: a project in report holds the warning
// form of a finding --from must report as an error.
func replaceFindings(findings, enforced []diag.Diagnostic) []diag.Diagnostic {
	restated := make(map[string]bool, len(enforced))
	key := func(finding diag.Diagnostic) string {
		return finding.Code + "\x00" + finding.Field + "\x00" + finding.Message
	}
	for _, finding := range enforced {
		restated[key(finding)] = true
	}
	kept := make([]diag.Diagnostic, 0, len(findings)+len(enforced))
	for _, finding := range findings {
		if !restated[key(finding)] {
			kept = append(kept, finding)
		}
	}
	return append(kept, enforced...)
}
