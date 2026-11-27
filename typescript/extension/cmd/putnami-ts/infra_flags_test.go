package main

import (
	"slices"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// `putnami test` declares --infra-down, a boolean that defaults to false on CI
// and elsewhere, and --infra, one of auto, require or skip with no default.
// The CI default of --infra-down keeps it out of the key of every task that
// does not read it; the tests that consume the test environment fold the
// inputs of test-env-up, which reads it, into their own keys.
func TestTestDeclaresTheInfraFlags(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "test-infra-flags", "test-declares-the-infra-flags")
	command, ok := loadExtensionManifest(t).Commands["test"]
	if !ok {
		t.Fatal("the manifest declares no test command")
	}
	down := command.Flags["infra-down"]
	if down.Type != "boolean" || down.Default != false || down.CIDefault != false {
		t.Errorf("--infra-down = %+v, want a boolean whose default is false on CI and elsewhere", down)
	}
	infra := command.Flags["infra"]
	if infra.Type != "string" || !slices.Equal(infra.Choices, []string{"auto", "require", "skip"}) || infra.Default != nil || infra.CIDefault != nil {
		t.Errorf("--infra = %+v, want a string of auto, require or skip with no default, so test-env-up resolves it", infra)
	}
}

// --infra-down and --infra reach test-env-up through the command params, and
// the teardown that finalizes test-env over its consumers is pruned only while
// --infra-down is off: under it, test-env-up runs and sweeps (dbtestenv.Up)
// and test-env-down finalizes the sweeping lease it wrote.
func TestInfraDownReachesTestEnvUpAndItsTeardown(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "test-infra-flags", "infra-down-reaches-test-env-up-and-its-teardown")
	manifest := loadExtensionManifest(t)
	for _, param := range []string{"infra", "infra-down"} {
		if input := manifest.Tasks["test-env-up"].Inputs[param]; input.From != "params" {
			t.Errorf("test-env-up reads %s from %q, want the command params", param, input.From)
		}
	}
	var teardown *proto.PipelineStep
	for i, step := range manifest.Commands["test"].Run {
		if step.Finalizes != nil && step.Finalizes.Producer == "test-env" {
			teardown = &manifest.Commands["test"].Run[i]
		}
	}
	if teardown == nil {
		t.Fatal("test plans no teardown of test-env")
	}
	if teardown.Task != "test-env-down" || teardown.Finalizes.PruneIf != "!params.infra-down" {
		t.Errorf("the teardown runs %q pruned if %q, want test-env-down pruned only while infra-down is off",
			teardown.Task, teardown.Finalizes.PruneIf)
	}
}
