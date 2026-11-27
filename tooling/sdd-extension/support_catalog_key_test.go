package sdd

import (
	"reflect"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	supportproto "go.putnami.dev/protocol/support"
)

// TestSpecsValidateKeysTheSupportCatalogItReads pins the one file outside the
// identity tier that specs-validate reads: the completeness assessment reads
// the workspace support catalog (internal/sdd readSupportCatalog), so an edit
// to the catalog must move the key. Without the port, a cached verdict that
// reported a publishable project as unsupported was replayed after the
// catalog gained its entry.
func TestSpecsValidateKeysTheSupportCatalogItReads(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "declared-inputs-cover-the-read-set", "the-cache-policy-matches-the-declared-read-set")
	m := committedManifest(t)

	task, found := m.Tasks["specs-validate"]
	if !found {
		t.Fatal("the manifest declares no specs-validate task")
	}
	want := proto.TaskInputPort{From: proto.TaskInputFromWorkspace, Files: []string{supportproto.CatalogFilename}}
	if got := task.Inputs["support-catalog"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("specs-validate support-catalog port = %+v, want %+v", got, want)
	}
}
