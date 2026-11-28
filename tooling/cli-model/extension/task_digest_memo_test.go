package extension

import (
	"testing"

	proto "go.putnami.dev/protocol/extension"
)

// A resolved description digests each task's contract once, and the digest
// is the protocol's; a description built without Resolve digests each time.
func TestAResolvedDescriptionDigestsEachTaskOnce(t *testing.T) {
	task := proto.TaskDefinition{Command: "go", Args: []string{"build"}}
	other := proto.TaskDefinition{Command: "go", Args: []string{"vet"}}
	desc := Resolve(&Manifest{Name: "@putnami/test", Tasks: map[string]TaskDefinition{"build": task, "vet": other}}, t.TempDir())
	want := proto.TaskContractDigest(task)
	for range 2 {
		if got := desc.taskContractDigest("build", task); got != want {
			t.Fatalf("digest %q, want %q", got, want)
		}
	}
	copied := *desc
	changed := proto.TaskDefinition{Command: "go", Args: []string{"test"}}
	copied.Tasks = map[string]TaskDefinition{"build": changed}
	if got := copied.taskContractDigest("build", changed); got != proto.TaskContractDigest(changed) {
		t.Fatalf("a copy with other tasks kept the first digest %q", got)
	}
	if got := desc.taskContractDigest("vet", other); got != proto.TaskContractDigest(other) {
		t.Fatalf("the second task's digest %q", got)
	}
	unresolved := &ExtensionDescription{Tasks: map[string]TaskDefinition{"build": task}}
	if got := unresolved.taskContractDigest("build", task); got != want {
		t.Fatalf("an unresolved description's digest %q, want %q", got, want)
	}
}
