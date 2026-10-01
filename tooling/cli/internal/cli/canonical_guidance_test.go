package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

func TestCanonicalWorkspaceGuideCoversCatalogAndPolicies(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/canonical-workspace-guidance", "onboarding-path", "the-canonical-guide-covers-the-catalog")
	spectest.Proves(t, "cli/canonical-workspace-guidance", "support-maturity-separation", "stability-and-maturity-are-stated-separately")
	spectest.Proves(t, "cli/canonical-workspace-guidance", "internal-model-boundary", "the-guide-declares-cli-model-internal")
	spectest.Proves(t, "cli/canonical-workspace-guidance", "experimental-python", "python-is-documented-experimental-and-opt-in")
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), "doc", "19-core-workspace-experience.md"))
	if err != nil {
		t.Fatalf("read canonical workspace guide: %v", err)
	}
	guide := string(data)

	for _, command := range commandmeta.Commands() {
		if !strings.Contains(guide, "`"+command.Path+"`") {
			t.Errorf("canonical workspace guide omits catalog path %q", command.Path)
		}
	}
	// The four SDD tools (list_features, feature_context, list_specs,
	// spec_context) left this list: they are served by
	// @putnami/sdd as sdd.<name> now, so they are no longer core MCP tools and
	// this guide is the CORE workspace experience.
	for _, tool := range []string{
		"workspace_map", "list_projects", "describe_project", "agent_context",
		"deps", "find_owner", "why_impacted", "topo_sort", "impacted",
		"run_jobs", "get_diagnostics",
	} {
		if !strings.Contains(guide, "`"+tool+"`") {
			t.Errorf("canonical workspace guide omits core MCP tool %q", tool)
		}
	}
	normalizedGuide := strings.Join(strings.Fields(guide), " ")
	for _, policy := range []string{
		"./putnamiw",
		"Python integration is experimental, explicit opt-in, non-default",
		"does not claim Go or TypeScript parity",
		"FSL-1.1-MIT",
		"unpublished",
		"absent from the support catalog",
		"not a public API",
		"stable under the repository's pre-1.0 migration policy",
		"Support status and feature maturity are separate",
		"Evidence is never hand-authored",
	} {
		if !strings.Contains(normalizedGuide, policy) {
			t.Errorf("canonical workspace guide omits policy %q", policy)
		}
	}
}
