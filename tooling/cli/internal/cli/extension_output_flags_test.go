package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// An interactive extension subcommand renders its own output, and the host
// consumes --verbose, --debug, --quiet, --color and --no-color as globals, so
// the job context is where the subcommand reads them: `verbose`, `quiet` and
// `color`, each absent when not typed. A planned alias runs through the engine,
// which renders its output itself, and its params stay as they were, so no
// planned task's cache key moves.
func TestAppRun_ExtensionSubcommand_ReceivesTheTypedOutputGlobals(t *testing.T) {
	requireShell(t)
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-launch-names-the-directory-it-was-typed-in",
		"the-context-carries-the-typed-output-globals")
	// want is the output params the context carries, as JSON.
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"nothing typed", []string{"ops", "login"}, `{}`},
		{"verbose", []string{"ops", "login", "--verbose"}, `{"verbose":true}`},
		{"debug is verbose", []string{"ops", "login", "--debug"}, `{"verbose":true}`},
		{"quiet", []string{"ops", "login", "--quiet"}, `{"quiet":true}`},
		{"no color", []string{"ops", "login", "--no-color"}, `{"color":false}`},
		{"color", []string{"ops", "login", "--color"}, `{"color":true}`},
		{"a planned alias", []string{"ops", "sweep", "app", "--verbose", "--quiet", "--no-color"}, `{}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wsRoot := t.TempDir()
			writeNegationFixture(t, wsRoot)
			seen := filepath.Join(wsRoot, "seen-context.json")
			writeNegationProbe(t, wsRoot, `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    cp "$2" '`+seen+`'
  fi
  shift
done
exit 0
`)
			t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
			runAlias(t, wsRoot, c.argv, ExitSuccess)

			data, err := os.ReadFile(seen)
			if err != nil {
				t.Fatalf("the probe received no job context: %v", err)
			}
			// A nil member is a param the context does not carry; a param of
			// another type than a boolean fails the decode.
			var document struct {
				Params struct {
					Verbose *bool `json:"verbose,omitempty"`
					Quiet   *bool `json:"quiet,omitempty"`
					Color   *bool `json:"color,omitempty"`
				} `json:"params"`
			}
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatalf("job context: %v", err)
			}
			got, err := json.Marshal(document.Params)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("output params %s, want %s", got, c.want)
			}
		})
	}
}
