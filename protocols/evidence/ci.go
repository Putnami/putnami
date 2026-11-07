package evidence

import "strings"

// CIMarker is the environment variable a CI system sets in every job.
type CIMarker struct {
	// System is the CI system the variable names.
	System string
	// Name is the CI system as a person reads it.
	Name string
	// Variable is the environment variable.
	Variable string
	// Value is the value the variable holds, compared without case; empty
	// matches any non-empty value.
	Value string
}

// CIMarkers lists the marker of every named CI system. The first marker set
// names the system.
var CIMarkers = []CIMarker{
	{System: CISystemGitHubActions, Name: "GitHub Actions", Variable: "GITHUB_ACTIONS", Value: "true"},
	{System: CISystemGitLabCI, Name: "GitLab CI", Variable: "GITLAB_CI", Value: "true"},
	{System: CISystemJenkins, Name: "Jenkins", Variable: "JENKINS_URL"},
	{System: CISystemCircleCI, Name: "CircleCI", Variable: "CIRCLECI", Value: "true"},
	{System: CISystemBuildkite, Name: "Buildkite", Variable: "BUILDKITE", Value: "true"},
	{System: CISystemAzurePipelines, Name: "Azure Pipelines", Variable: "TF_BUILD", Value: "true"},
	{System: CISystemBitbucketPipelines, Name: "Bitbucket Pipelines", Variable: "BITBUCKET_BUILD_NUMBER"},
	{System: CISystemTeamCity, Name: "TeamCity", Variable: "TEAMCITY_VERSION"},
}

// DetectCI reads the CI system from the environment getenv reads: the system
// of the first marker set, else generic. inCI reports a marker, or CI set to
// true or 1, the variable most CI systems set.
func DetectCI(getenv func(string) string) (system string, inCI bool) {
	for _, marker := range CIMarkers {
		value := strings.TrimSpace(getenv(marker.Variable))
		if value == "" {
			continue
		}
		if marker.Value == "" || strings.EqualFold(value, marker.Value) {
			return marker.System, true
		}
	}
	value := strings.TrimSpace(getenv("CI"))
	return CISystemGeneric, strings.EqualFold(value, "true") || value == "1"
}

// CISystemName returns the CI system as a person reads it, "generic CI" for
// generic.
func CISystemName(system string) string {
	for _, marker := range CIMarkers {
		if marker.System == system {
			return marker.Name
		}
	}
	return "generic CI"
}
