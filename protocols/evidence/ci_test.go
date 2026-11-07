package evidence

import (
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func envOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestDetectCI_EachMarkerNamesItsCISystem(t *testing.T) {
	spectest.Proves(t, feature, "the-ci-system-is-recognized-by-its-marker", "each-marker-names-its-ci-system")
	for name, tc := range map[string]struct {
		env    map[string]string
		system string
	}{
		"github actions":      {map[string]string{"GITHUB_ACTIONS": "true"}, CISystemGitHubActions},
		"gitlab ci":           {map[string]string{"GITLAB_CI": "true"}, CISystemGitLabCI},
		"jenkins":             {map[string]string{"JENKINS_URL": "https://ci.example.com/"}, CISystemJenkins},
		"circleci":            {map[string]string{"CIRCLECI": "true"}, CISystemCircleCI},
		"buildkite":           {map[string]string{"BUILDKITE": "true"}, CISystemBuildkite},
		"azure pipelines":     {map[string]string{"TF_BUILD": "True"}, CISystemAzurePipelines},
		"bitbucket pipelines": {map[string]string{"BITBUCKET_BUILD_NUMBER": "42"}, CISystemBitbucketPipelines},
		"teamcity":            {map[string]string{"TEAMCITY_VERSION": "2025.07"}, CISystemTeamCity},
		"the first one wins":  {map[string]string{"JENKINS_URL": "https://ci.example.com/", "GITLAB_CI": "true", "CI": "true"}, CISystemGitLabCI},
	} {
		t.Run(name, func(t *testing.T) {
			system, inCI := DetectCI(envOf(tc.env))
			if system != tc.system || !inCI {
				t.Fatalf("DetectCI = (%q, %v), want (%q, true)", system, inCI, tc.system)
			}
		})
	}
	var named []string
	for _, marker := range CIMarkers {
		named = append(named, marker.System)
		if CISystemName(marker.System) != marker.Name || marker.Name == "" {
			t.Errorf("CISystemName(%q) = %q, want %q", marker.System, CISystemName(marker.System), marker.Name)
		}
	}
	if want := slices.DeleteFunc(slices.Clone(CISystems), func(s string) bool { return s == CISystemGeneric }); !slices.Equal(named, want) {
		t.Fatalf("markers name %v, want every CI system but generic: %v", named, want)
	}
}

func TestDetectCI_CITrueAloneIsAGenericCI(t *testing.T) {
	spectest.Proves(t, feature, "the-ci-system-is-recognized-by-its-marker", "ci-true-alone-is-a-generic-ci")
	for _, value := range []string{"true", "TRUE", "1"} {
		system, inCI := DetectCI(envOf(map[string]string{"CI": value}))
		if system != CISystemGeneric || !inCI {
			t.Fatalf("CI=%s gives (%q, %v), want (generic, true)", value, system, inCI)
		}
	}
	// A marker with another value names no system.
	system, inCI := DetectCI(envOf(map[string]string{"GITHUB_ACTIONS": "false", "CI": "true"}))
	if system != CISystemGeneric || !inCI {
		t.Fatalf("GITHUB_ACTIONS=false gives (%q, %v), want (generic, true)", system, inCI)
	}
	if CISystemName(CISystemGeneric) != "generic CI" {
		t.Fatalf("CISystemName(generic) = %q", CISystemName(CISystemGeneric))
	}
}

func TestDetectCI_NoVariableIsNoCI(t *testing.T) {
	spectest.Proves(t, feature, "the-ci-system-is-recognized-by-its-marker", "no-variable-is-no-ci")
	for name, env := range map[string]map[string]string{
		"nothing":       nil,
		"CI=false":      {"CI": "false"},
		"blank markers": {"JENKINS_URL": " ", "CI": ""},
	} {
		system, inCI := DetectCI(envOf(env))
		if system != CISystemGeneric || inCI {
			t.Fatalf("%s gives (%q, %v), want (generic, false)", name, system, inCI)
		}
	}
}
