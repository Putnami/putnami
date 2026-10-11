package cli

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// TestRefuseRemovedCoreRootRefusesOnlyWhatNoExtensionDeclares holds the
// refusal to the roots that left the core, and to the workspaces where no
// extension declares that exact root as a command group or as a job.
func TestRefuseRemovedCoreRootRefusesOnlyWhatNoExtensionDeclares(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "command-catalog", "a-root-that-left-the-core-is-refused-unless-an-extension-declares-it")
	t.Parallel()
	jobs := func(name string) []*extension.ExtensionDescription {
		return []*extension.ExtensionDescription{{
			Name: "@example/delivery",
			Jobs: map[string]*extension.JobDefinition{name: {Name: name, ExtensionName: "@example/delivery"}},
		}}
	}
	for _, root := range removedCoreRoots {
		t.Run(root, func(t *testing.T) {
			t.Parallel()
			err := refuseRemovedCoreRoot(root, nil, nil)
			if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
				t.Fatalf("error = %v, want a usage error", err)
			}
			message := err.Error()
			if !strings.Contains(message, "`putnami "+root+"` is no longer a core command") ||
				!strings.Contains(message, commandmeta.CommandsThatLeftTheCoreURL) {
				t.Fatalf("message = %q, want the root and the reference that lists where it moved", message)
			}
			if strings.Contains(message, "@putnami/") || strings.Contains(message, "cloud") {
				t.Fatalf("message = %q names an extension", message)
			}
			// The refusal also fires outside a workspace, where no extension
			// is installed and `putnami extensions list` fails.
			if strings.Contains(message, "extensions list") || strings.Contains(message, " here") {
				t.Fatalf("message = %q assumes a workspace", message)
			}
			if err := refuseRemovedCoreRoot(root, map[string]bool{root: true}, nil); err != nil {
				t.Fatalf("an extension command group named %s is refused: %v", root, err)
			}
			if err := refuseRemovedCoreRoot(root, nil, jobs(root)); err != nil {
				t.Fatalf("an extension job named %s is refused: %v", root, err)
			}
			if err := refuseRemovedCoreRoot(root, map[string]bool{"other": true}, jobs("other")); err == nil {
				t.Fatalf("%s is accepted although no extension declares it", root)
			}
		})
	}
	for _, command := range []string{"lint", "build", "cloud", "typo", ""} {
		if err := refuseRemovedCoreRoot(command, nil, nil); err != nil {
			t.Errorf("%q is refused as a removed root: %v", command, err)
		}
	}
}

// TestRemovedCoreRootIsReadAfterAliases keeps a workspace alias named like a
// removed root working: the refusal reads the command the alias resolves to.
func TestRemovedCoreRootIsReadAfterAliases(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"lint", "lint,test,build"} {
		parsed := ParseArgs([]string{"ci", "--all"}, map[string]string{"ci": target}, nil)
		if parsed.Err != nil || len(parsed.Commands) == 0 || parsed.Commands[0] == "ci" {
			t.Fatalf("alias ci=%s: parsed commands = %v, %v; want the alias expanded", target, parsed.Commands, parsed.Err)
		}
		if err := refuseRemovedCoreRoot(parsed.Commands[0], nil, nil); err != nil {
			t.Fatalf("alias ci=%s is refused: %v", target, err)
		}
	}
	parsed := ParseArgs([]string{"ci", "validate"}, nil, nil)
	if len(parsed.Commands) == 0 || parsed.Commands[0] != "ci" {
		t.Fatalf("parsed commands = %v, want ci as the root", parsed.Commands)
	}
	if err := refuseRemovedCoreRoot(parsed.Commands[0], nil, nil); err == nil {
		t.Fatal("`putnami ci validate` is not refused")
	}
}

// TestHelpChecksOnlyTheRemovedRoots keeps `putnami help <command>` free of
// extension discovery for every command that is not a removed root. The
// refusal of `putnami help ci` and `putnami help channel` runs a whole CLI
// session, so e2e/removedroots holds it.
func TestHelpChecksOnlyTheRemovedRoots(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"lint", "cloud", "help", ""} {
		if err := refuseHelpOfRemovedCoreRoot(&CommandEnv{}, command); err != nil {
			t.Errorf("help %q is refused: %v", command, err)
		}
	}
}
