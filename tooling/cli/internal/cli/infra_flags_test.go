package cli

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/repotest"
)

// The test-environment extensions of this repository, by manifest directory.
var testEnvironmentExtensions = []struct{ name, dir string }{
	{"@putnami/go", "go/extension"},
	{"@putnami/typescript", "typescript/extension"},
}

// `putnami test` accepts --infra-down, --no-infra-down and --infra on the Go
// and TypeScript extensions: the flag validator passes them, their values
// reach the command params, test-env-up declares both params as inputs, and
// the teardown's pruneIf, evaluated over those params, is false under
// --infra-down and true under --no-infra-down.
func TestTestAcceptsTheInfraFlagsAndInfraDownReachesTheTeardown(t *testing.T) {
	repotest.Require(t)
	t.Parallel()
	root := workspaceRoot(t)
	for _, shipped := range testEnvironmentExtensions {
		t.Run(shipped.name, func(t *testing.T) {
			dir := filepath.Join(root, filepath.FromSlash(shipped.dir))
			ext := extension.LoadExtensionFromDir(dir, shipped.name)
			if ext == nil {
				t.Fatalf("load %s", shipped.dir)
			}
			manifest, err := extension.LoadManifest(filepath.Join(dir, extension.ManifestFilename))
			if err != nil {
				t.Fatal(err)
			}
			command, ok := manifest.Commands["test"]
			if !ok {
				t.Fatalf("%s declares no test command", shipped.dir)
			}
			if flag := command.Flags["infra-down"]; flag.Type != "boolean" || flag.Default != false {
				t.Errorf("test declares --infra-down as %+v, want a boolean that defaults to false", flag)
			}
			if flag := command.Flags["infra"]; flag.Type != "string" || strings.Join(flag.Choices, ",") != "auto,require,skip" || flag.Default != nil {
				t.Errorf("test declares --infra as %+v, want a string of auto, require or skip, with no default", flag)
			}

			var teardown *extension.PipelineStep
			for i, step := range command.Run {
				if step.Finalizes != nil && step.Finalizes.Producer == "test-env" {
					teardown = &command.Run[i]
				}
			}
			if teardown == nil {
				t.Fatalf("%s plans no teardown of test-env under test", shipped.dir)
			}
			for _, param := range []string{"infra", "infra-down"} {
				if input := manifest.Tasks["test-env-up"].Inputs[param]; input.From != "params" {
					t.Errorf("test-env-up reads %s from %q, want the command params", param, input.From)
				}
			}

			discovered := &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{ext}}
			for _, c := range []struct {
				args      []string
				infraDown any // nil: the run names no --infra-down
				pruneable bool
			}{
				{args: []string{"test", "--infra-down"}, infraDown: true},
				{args: []string{"test", "--no-infra-down"}, infraDown: false, pruneable: true},
				{args: []string{"test", "--infra", "require"}},
			} {
				parsed := ParseArgs(c.args, nil, nil)
				if parsed.Err != nil {
					t.Fatalf("%v: %v", c.args, parsed.Err)
				}
				if err := commandFlagValidator(parsed, io.Discard)(discovered, parsed.Global); err != nil {
					t.Fatalf("%v is refused: %v", c.args, err)
				}
				params := buildCommandParams(parsed.RawJobArgs)
				if c.infraDown == nil {
					if params["infra"] != "require" {
						t.Errorf("%v hands infra = %v to the tasks, want require", c.args, params["infra"])
					}
					continue
				}
				if params["infra-down"] != c.infraDown {
					t.Errorf("%v hands infra-down = %v to the tasks, want %v", c.args, params["infra-down"], c.infraDown)
				}
				prune, valid := extension.EvaluateExpressionChecked(teardown.Finalizes.PruneIf, &extension.WhenContext{Params: params})
				if !valid || prune != c.pruneable {
					t.Errorf("%v: the teardown's pruneIf %q answers %v (valid %v), want %v",
						c.args, teardown.Finalizes.PruneIf, prune, valid, c.pruneable)
				}
			}
		})
	}
}
