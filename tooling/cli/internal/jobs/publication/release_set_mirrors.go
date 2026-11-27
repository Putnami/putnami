package publication

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	ciproto "go.putnami.dev/protocol/ci"
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
)

// mirrorNotices receives the line publish prints when it withholds a mirror
// destination. Tests replace it; production writes to standard error.
var mirrorNotices io.Writer = os.Stderr

// BuildMirrorTargets copies the declared registry destinations into the one
// release request. The CLI does not select public members or resolve backend
// credentials; it retains this policy snapshot from planning to finalization.
func BuildMirrorTargets(policy *ciproto.Distribution) (map[string]distribution.MirrorTarget, error) {
	if policy == nil {
		return nil, nil
	}
	var targets map[string]distribution.MirrorTarget
	for ecosystem, registry := range policy.Registries {
		if registry.Mirror == nil {
			continue
		}
		if targets == nil {
			targets = make(map[string]distribution.MirrorTarget)
		}
		targets[ecosystem] = distribution.MirrorTarget{To: strings.TrimSpace(registry.Mirror.To)}
	}
	if diagnostics := distribution.ValidateMirrorTargets(targets); diag.HasErrors(diagnostics) {
		return nil, fmt.Errorf("distribution mirror policy: %s", firstReleaseSetDiagnostic(diagnostics))
	}
	return targets, nil
}

// ReleasedMirrorTargets keeps the destination of each ecosystem whose every
// member in the released set carries a release version, and withholds the
// others (ADR 0114). The provider copies every public member of the set to a
// destination the request names, and a copy on an external registry cannot be
// withdrawn: a canary or pull-request version must never reach it. The map is
// per ecosystem, so one prerelease member withholds its whole ecosystem; an
// omitted entry requests no copy and revokes none already accepted
// (protocols/distribution ADR 0004, rule 6). It returns the kept targets and,
// per withheld ecosystem, the first prerelease member in coordinate order.
func ReleasedMirrorTargets(targets map[string]distribution.MirrorTarget, members []distribution.ReleaseSetMember) (map[string]distribution.MirrorTarget, map[string]distribution.ReleaseSetMember) {
	if len(targets) == 0 {
		return targets, nil
	}
	ordered := append([]distribution.ReleaseSetMember(nil), members...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Coordinate < ordered[j].Coordinate })
	var withheld map[string]distribution.ReleaseSetMember
	for _, member := range ordered {
		ecosystem := string(member.Ecosystem)
		if _, declared := targets[ecosystem]; !declared || isReleaseVersion(member.Version) {
			continue
		}
		if _, seen := withheld[ecosystem]; seen {
			continue
		}
		if withheld == nil {
			withheld = make(map[string]distribution.ReleaseSetMember)
		}
		withheld[ecosystem] = member
	}
	if len(withheld) == 0 {
		return targets, nil
	}
	kept := make(map[string]distribution.MirrorTarget, len(targets))
	for ecosystem, target := range targets {
		if _, skip := withheld[ecosystem]; !skip {
			kept[ecosystem] = target
		}
	}
	return kept, withheld
}

// releaseVersionPattern is the only shape a release takes: an optional "v",
// MAJOR.MINOR.PATCH without leading zeros, then optional build metadata.
var releaseVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\+[0-9A-Za-z.-]+)?$`)

// isReleaseVersion reports a version without a prerelease part, the shape the
// visibility chain calls stable: 0.5.0 and v0.5.0 are releases,
// 0.5.1-20261126120000-abcdef1 is not. It fails closed: anything that is not
// MAJOR.MINOR.PATCH, such as an empty version or "latest", is not a release.
// The placeholder 0.0.0 a run stamps without version information is not one
// either.
func isReleaseVersion(version string) bool {
	return releaseVersionPattern.MatchString(version) && strings.TrimPrefix(strings.SplitN(version, "+", 2)[0], "v") != "0.0.0"
}

// reportWithheldMirrors prints one line per withheld ecosystem, in ecosystem
// order, naming the member that withheld it.
func reportWithheldMirrors(targets map[string]distribution.MirrorTarget, withheld map[string]distribution.ReleaseSetMember) {
	ecosystems := make([]string, 0, len(withheld))
	for ecosystem := range withheld {
		ecosystems = append(ecosystems, ecosystem)
	}
	sort.Strings(ecosystems)
	for _, ecosystem := range ecosystems {
		member := withheld[ecosystem]
		_, _ = fmt.Fprintf(mirrorNotices, "putnami: %s mirror to %s withheld: %s %s is a prerelease version; only a set whose %s members all carry release versions is copied\n",
			ecosystem, targets[ecosystem].To, member.Coordinate, member.Version, ecosystem)
	}
}
