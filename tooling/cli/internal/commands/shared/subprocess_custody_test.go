package shared

import (
	"context"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// Both auxiliary spawn helpers start repository code (`go` in a workspace
// module, a nested CLI): on a hosted run each records it before the spawn, so
// no process started after receives the run credential. The record names the
// command.
func TestRunGroupHelpersRecordRepositoryCode(t *testing.T) {
	name, env := child("true")
	for helper, run := range map[string]func() error{
		"RunGroupCombined": func() error {
			_, err := RunGroupCombined(context.Background(), "", env, name, "mod", "tidy")
			return err
		},
		"RunGroupStreaming": func() error {
			return RunGroupStreaming(context.Background(), "", env, name, "mod", "tidy")
		},
	} {
		restore := runcredential.SetForTest("run-bearer")
		if err := run(); err != nil {
			t.Errorf("%s: %v", helper, err)
		}
		err := runcredential.RequireCustody("the cache provider of @acme/cache")
		restore()
		if err == nil || !strings.Contains(err.Error(), "mod tidy") {
			t.Errorf("%s: custody after the spawn = %v, want a refusal naming the command", helper, err)
		}
	}
}
