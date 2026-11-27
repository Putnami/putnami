package cachekey

import (
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// TestACacheHitRestoresTheEvidenceArtifact pins that a cache hit gives a task
// its putnami-evidence artifact back when the entry holds the reserved
// fragment file, as a run's report lists it (CLI ADR 0134). Any task may state
// evidence, so no command kind gates it.
func TestACacheHitRestoresTheEvidenceArtifact(t *testing.T) {
	job := &jobmodel.ScheduledJob{}
	entry := &store.TaskEntry{Outputs: []store.TaskEntryOutput{{
		ID: "evidence", Kind: extensionproto.OutputKindFile, Root: extensionproto.OutputRootCommandOutput,
		Path: EvidenceFragmentFilename, State: store.TaskOutputPresent,
	}}}
	result := &jobmodel.JobResult{Status: "success"}
	RestoreReservedDeclaredArtifactEvents(job, entry, result)
	artifacts := jobmodel.TaskArtifactsOf(result)
	if len(artifacts) != 1 || artifacts[0].ID != EvidenceFragmentArtifactID || artifacts[0].Path != EvidenceFragmentFilename {
		t.Fatalf("artifacts = %+v, want the restored putnami-evidence artifact", artifacts)
	}
	// Restoring twice adds nothing.
	RestoreReservedDeclaredArtifactEvents(job, entry, result)
	if got := len(jobmodel.TaskArtifactsOf(result)); got != 1 {
		t.Fatalf("artifacts after a second restore = %d, want 1", got)
	}
	// An empty output restores nothing.
	entry.Outputs[0].State = store.TaskOutputEmpty
	empty := &jobmodel.JobResult{Status: "success"}
	RestoreReservedDeclaredArtifactEvents(job, entry, empty)
	if got := len(jobmodel.TaskArtifactsOf(empty)); got != 0 {
		t.Fatalf("artifacts from an empty output = %d, want 0", got)
	}
}
