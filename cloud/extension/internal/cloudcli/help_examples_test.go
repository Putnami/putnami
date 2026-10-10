package cloudcli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// TestEveryCloudSubcommandHasExample enforces that every cloud
// subcommand in putnami.extension.json must ship at least one runnable example
// so that `putnami cloud <cmd> --help` always renders an Examples: section.
// The framework renders examples only from the subcommand definition under
// commandGroups.cloud.subcommands, so this guards that exact shape.
//
// Beyond mere presence it validates every example's long --flags are actually
// DECLARED for that command (the union of the subcommand's own flags and its
// flat command's flags), catching an example that references a typo'd or removed
// flag. NOTE: this is a structural guard, not an execution one — it cannot catch
// a declared-but-misused flag (a bad value, a deprecated flag, or one the code
// ignores); a clean-machine walkthrough is the backstop for those.
func TestEveryCloudSubcommandHasExample(t *testing.T) {
	data, err := os.ReadFile("../../putnami.extension.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		CommandGroups struct {
			Cloud struct {
				Subcommands map[string]struct {
					Command  string                     `json:"command"`
					Flags    map[string]json.RawMessage `json:"flags"`
					Examples []struct {
						Command     string `json:"command"`
						Description string `json:"description"`
					} `json:"examples"`
				} `json:"subcommands"`
			} `json:"cloud"`
		} `json:"commandGroups"`
		Commands map[string]struct {
			Flags map[string]json.RawMessage `json:"flags"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	subs := manifest.CommandGroups.Cloud.Subcommands
	if len(subs) == 0 {
		t.Fatal("no cloud subcommands found — the manifest path or shape is wrong")
	}

	// Reserved global contract flags (--output, --json, --verbose, …) are owned
	// by the launcher and valid on every command, so examples may use them even
	// though the manifest must not declare them.
	reserved := map[string]bool{}
	for _, rf := range protocolcli.ReservedGlobalFlags() {
		reserved[rf.Name] = true
		if rf.Negatable {
			reserved["no-"+rf.Name] = true
		}
	}

	for name, sub := range subs {
		if len(sub.Examples) == 0 {
			t.Errorf("cloud subcommand %q has no examples; add a runnable example under commandGroups.cloud.subcommands.%s.examples", name, name)
			continue
		}
		// Declared flags = the subcommand's own flags ∪ its flat command's flags.
		declared := map[string]bool{}
		for f := range sub.Flags {
			declared[f] = true
		}
		for f := range manifest.Commands[sub.Command].Flags {
			declared[f] = true
		}
		for i, ex := range sub.Examples {
			if strings.TrimSpace(ex.Command) == "" {
				t.Errorf("cloud subcommand %q example[%d] has an empty command", name, i)
				continue
			}
			if !strings.HasPrefix(ex.Command, "putnami cloud ") {
				t.Errorf("cloud subcommand %q example[%d] command %q must start with %q", name, i, ex.Command, "putnami cloud ")
			}
			for _, flag := range exampleFlags(ex.Command) {
				if !declared[flag] && !reserved[flag] {
					t.Errorf("cloud subcommand %q example[%d] uses --%s, not a declared flag of the command: %q", name, i, flag, ex.Command)
				}
			}
		}
	}
}

// exampleFlags extracts the long-flag names (--foo, --foo=bar) an example
// command line references, so they can be checked against the command's declared
// flags. Short flags and positionals are ignored.
func exampleFlags(command string) []string {
	var flags []string
	for tok := range strings.FieldsSeq(command) {
		if !strings.HasPrefix(tok, "--") {
			continue
		}
		name := strings.TrimPrefix(tok, "--")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if name != "" {
			flags = append(flags, name)
		}
	}
	return flags
}

func TestCloudSourceDoesNotShadowReservedFlags(t *testing.T) {
	data, err := os.ReadFile("../../putnami.extension.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Commands map[string]struct {
			Flags map[string]json.RawMessage `json:"flags"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	reserved := map[string]bool{"plan": true, "dry-run": true}
	for _, flag := range protocolcli.ReservedGlobalFlags() {
		reserved[flag.Name] = true
	}
	for name := range manifest.Commands["cloud-source"].Flags {
		if reserved[name] {
			t.Errorf("cloud-source must not redeclare reserved flag --%s", name)
		}
	}
}
