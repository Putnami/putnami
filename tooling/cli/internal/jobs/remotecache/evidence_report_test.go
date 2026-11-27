package remotecache

import (
	"testing"

	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// TestTheEvidenceFileIsAPostRunReport pins that a task declaring the evidence
// fragment file never takes a status-only hit: the evidence verdict reads the
// file from the command output directory after the scheduler finishes, so a
// hit that placed no bytes would read as a refused file (CLI ADR 0134).
func TestTheEvidenceFileIsAPostRunReport(t *testing.T) {
	job := declaredJob("validate~features", "features-validate", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"evidence": fileDeclaration(extension.OutputRootCommandOutput, cachekey.EvidenceFragmentFilename, true),
		},
	})
	if !declaresPostRunReport(job) {
		t.Fatal("a task declaring putnami-evidence.jsonl does not declare a post-run report")
	}
	project := declaredJob("validate~features", "features-validate", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"evidence": fileDeclaration(extension.OutputRootProject, cachekey.EvidenceFragmentFilename, true),
		},
	})
	if declaresPostRunReport(project) {
		t.Fatal("the fragment file name in the project tree is not the reserved command output")
	}
}
