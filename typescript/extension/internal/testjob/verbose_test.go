package testjob

import (
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestVerboseOnlyControlsProducerVisibility(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bounded-test-output", "test-verbose-never-changes-the-bun-invocation-or-verdicts")
	t.Parallel()
	quiet := buildTestArgs("/out", TestParams{Coverage: true, Timeout: 5000})
	verbose := buildTestArgs("/out", TestParams{Coverage: true, Timeout: 5000, Verbose: true})
	if !reflect.DeepEqual(verbose, quiet) {
		t.Fatalf("Verbose changed Bun arguments:\nquiet:   %v\nverbose: %v", quiet, verbose)
	}
}
