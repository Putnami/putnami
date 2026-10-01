package workspace

import (
	"slices"

	diag "go.putnami.dev/protocol/diagnostic"
)

// DistributionConfig is what a project declares to the release-set provider
// about the artifacts it publishes, or what a scope declares for every project
// below it.
//
// It lives in putnami.json so that making one package public is a change to
// that package, not to the repository's single putnami.ci.json: a central
// member list is a file every team edits.
//
// It is unrelated to ProjectConfig.Visibility. That field is an import boundary
// inside the workspace; this one decides who may pull a published artifact.
type DistributionConfig struct {
	// Visibility is the member level of the distribution inheritance chain for
	// every release-set member the project produces: internal, private, or
	// public. A project's own declaration wins over its scope's, and the
	// deepest scope wins over a shallower one.
	Visibility string `json:"visibility,omitempty"`
}

// DistributionVisibilities is the closed, ordered vocabulary of a distribution
// level. go.putnami.dev/protocol/distribution owns it; it is restated here, as
// protocol/ci restates it, so a project file can be validated without that
// module.
var DistributionVisibilities = []string{"internal", "private", "public"}

// ValidDistributionVisibility reports whether v is one of the three levels.
// The empty value is not valid: a declared block must declare a level.
func ValidDistributionVisibility(v string) bool {
	return slices.Contains(DistributionVisibilities, v)
}

// validateDistribution refuses a block that states no level or an unknown one.
// An unknown level is an error rather than a fallback, because the value
// decides who may pull an artifact: a typo must not publish anything.
func validateDistribution(d *DistributionConfig) []diag.Diagnostic {
	if d == nil {
		return nil
	}
	if d.Visibility == "" {
		return []diag.Diagnostic{diag.Errorf("required-field", "distribution.visibility",
			"distribution must declare a visibility: internal, private, or public")}
	}
	if !ValidDistributionVisibility(d.Visibility) {
		return []diag.Diagnostic{diag.Errorf("invalid-distribution-visibility", "distribution.visibility",
			"unknown distribution visibility %q (want internal, private, or public)", d.Visibility)}
	}
	return nil
}
