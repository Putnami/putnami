package telemetry

import (
	"reflect"
	"testing"

	"go.putnami.dev/protocol/telemetry/cliusage"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

func TestCommandVocabularyMatchesPublicJobCatalog(t *testing.T) {
	catalog := commandmeta.PublicJobCommands()
	got := make([]string, len(catalog))
	for i, command := range catalog {
		got[i] = command.Name
	}
	if !reflect.DeepEqual(got, cliusage.Commands) {
		t.Fatalf("public job catalog = %v, telemetry command vocabulary = %v", got, cliusage.Commands)
	}
}
