package manifest

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
)

// commandGroupsFixture is the protocol's own valid command-groups manifest. It
// is read from protocols/extension rather than copied into this package's
// testdata on purpose: a copy would keep passing after the protocol changed the
// shape, which is the one failure this test exists to catch.
const commandGroupsFixture = "../../../protocols/extension/fixtures/valid/command-groups.json"

// TestCommandGroup_RoundTripsTheProtocolFixture builds the protocol's fixture
// through the builder and asserts the result IS that document. The fixture is
// the contract: it is what the CLI's own parser is tested against, so a builder
// that produces anything else produces a manifest a consumer reads differently
// from the one the author meant.
//
// Both sides are compared in the protocol's OWN canonical form — the shape
// proto.ParseAndValidateManifest leaves a manifest in, which is the shape the
// CLI holds a loaded one in — so the comparison is over the document and never
// over an encoder's field order or a defaulted-map spelling. That normalization
// is the protocol's, not this test's: a hand-rolled one would be a second
// opinion about canonical form, which is the drift the fixture exists to catch.
//
// One key is removed, deliberately and only after being asserted: cliContract.
// Build stamps it — that stamp is what makes an SDK-authored manifest loadable
// at all since contract 3 — and the fixture predates stamping. Everything else
// must match exactly.
func TestCommandGroup_RoundTripsTheProtocolFixture(t *testing.T) {
	built, err := commandGroupsBuilder().Build()
	if err != nil {
		t.Fatalf("the fixture manifest was rejected at authoring time: %v", err)
	}
	if built.CLIContract != protocolcli.CurrentContract {
		t.Errorf("built cliContract = %d, want the earned stamp %d", built.CLIContract, protocolcli.CurrentContract)
	}

	fixture, err := os.ReadFile(commandGroupsFixture)
	if err != nil {
		t.Fatalf("read the protocol fixture: %v", err)
	}
	encoded, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}

	gotDoc := canonicalDocument(t, encoded)
	wantDoc := canonicalDocument(t, fixture)
	if gotDoc["$schema"] != wantDoc["$schema"] {
		t.Errorf("$schema = %v, want %v", gotDoc["$schema"], wantDoc["$schema"])
	}
	delete(gotDoc, "cliContract")

	if got, want := canonical(t, gotDoc), canonical(t, wantDoc); got != want {
		t.Errorf("built manifest is not the fixture document.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// commandGroupsBuilder authors protocols/extension/fixtures/valid/command-groups.json
// through the builder. Every member of that fixture appears here, so a
// constructor that silently stopped applying breaks the round trip above.
func commandGroupsBuilder() *Builder {
	return New("sample/command-groups", "").
		CommandGroup("cloud", "Interact with Putnami Cloud",
			GroupFlag("env", proto.FlagDefinition{
				Type:        "string",
				Default:     "prod",
				Description: "Target deployment environment.",
			}),
			Subcommand("status", "Show the deployment status of the current workspace.",
				Runs("cloud-status"),
				SubcommandFlag("detailed", proto.FlagDefinition{
					Type:        "boolean",
					Short:       "r",
					Description: "Include per-revision detail.",
				}),
				Example("putnami cloud status", "Human-readable status table."),
				Example("putnami cloud status --output=jsonl",
					"Machine-readable snapshot for an agent or CI."),
			),
		).
		Command("cloud-status", proto.CommandDefinition{
			Description: "Show deployment status.",
			Run:         []proto.PipelineStep{{ID: "status", Task: "status-exec"}},
		}).
		Task("status-exec", proto.TaskDefinition{
			Kind:    "command",
			Command: "echo",
			Args:    []string{"status"},
		})
}

// TestCommandGroup_RoundTripsTheWorkspaceFixture builds the protocol's
// workspace-requirement fixture through the builder and asserts the result is
// that document, with the same canonicalization as the command-groups round
// trip above.
func TestCommandGroup_RoundTripsTheWorkspaceFixture(t *testing.T) {
	built, err := New("sample/command-groups-without-workspace", "").
		CommandGroup("audit", "Audit a repository, with or without a workspace",
			DefaultSubcommand("run"),
			Subcommand("run", "Audit the current directory.",
				Runs("audit-run"),
				Interactive(),
				WorkspaceRequirement(proto.SubcommandWorkspaceOptional),
			),
			Subcommand("report", "Summarize the last audit of this workspace.",
				Runs("audit-report"),
				WorkspaceRequirement(proto.SubcommandWorkspaceRequired),
			),
		).
		Command("audit-run", proto.CommandDefinition{
			Description: "Audit the current directory.",
			Run:         []proto.PipelineStep{{ID: "run", Task: "audit-exec"}},
		}).
		Command("audit-report", proto.CommandDefinition{
			Description: "Summarize the last audit.",
			Run:         []proto.PipelineStep{{ID: "report", Task: "audit-exec"}},
		}).
		Task("audit-exec", proto.TaskDefinition{Kind: "command", Command: "echo", Args: []string{"audit"}}).
		Build()
	if err != nil {
		t.Fatalf("the fixture manifest was rejected at authoring time: %v", err)
	}
	if built.CLIContract != protocolcli.CurrentContract {
		t.Errorf("built cliContract = %d, want %d: the workspace members need no contract increment",
			built.CLIContract, protocolcli.CurrentContract)
	}
	fixture, err := os.ReadFile(workspaceFixture)
	if err != nil {
		t.Fatalf("read the protocol fixture: %v", err)
	}
	encoded, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	gotDoc := canonicalDocument(t, encoded)
	wantDoc := canonicalDocument(t, fixture)
	delete(gotDoc, "cliContract")
	if got, want := canonical(t, gotDoc), canonical(t, wantDoc); got != want {
		t.Errorf("built manifest is not the fixture document.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// workspaceFixture is the protocol's valid manifest for a group default and a
// subcommand that runs without a workspace.
const workspaceFixture = "../../../protocols/extension/fixtures/valid/command-groups-without-workspace.json"

// TestCommandGroup_AuthoringFeatures pins what the constructors produce beyond
// the fixture's own surface: the interactive flag D6 makes every SDD subcommand
// carry, and positionals, whose ORDER is the argument order and therefore the
// one thing a map-based member could not express.
func TestCommandGroup_AuthoringFeatures(t *testing.T) {
	built, err := New("sample/groups", "1.0.0").
		CommandGroup("features", "Inspect the feature registry",
			Subcommand("diff", "Compare two revisions",
				Runs("features-diff"),
				Interactive(),
				RequiredPositional("base"),
				RequiredPositional("head"),
			),
			Subcommand("list", "",
				Runs("features-diff"),
				Positional("query"),
			),
		).
		Command("features-diff", proto.CommandDefinition{
			Run: []proto.PipelineStep{{ID: "diff", Task: "diff-exec"}},
		}).
		Task("diff-exec", proto.TaskDefinition{Kind: "command", Command: "echo"}).
		Build()
	if err != nil {
		t.Fatalf("valid command group rejected: %v", err)
	}

	group := built.CommandGroups["features"]
	if group.Description != "Inspect the feature registry" {
		t.Errorf("group description = %q", group.Description)
	}
	diffSub := group.Subcommands["diff"]
	if !diffSub.Interactive {
		t.Error("Interactive() did not reach the subcommand")
	}
	want := []proto.PositionalDefinition{{Name: "base", Required: true}, {Name: "head", Required: true}}
	if len(diffSub.Positionals) != len(want) {
		t.Fatalf("positionals = %+v, want %+v", diffSub.Positionals, want)
	}
	for i, positional := range diffSub.Positionals {
		if positional != want[i] {
			t.Errorf("positional %d = %+v, want %+v", i, positional, want[i])
		}
	}
	listSub := group.Subcommands["list"]
	if listSub.Interactive {
		t.Error("Interactive leaked to a subcommand that did not ask for it")
	}
	if len(listSub.Positionals) != 1 || listSub.Positionals[0].Required {
		t.Errorf("optional positional = %+v", listSub.Positionals)
	}
	if listSub.Description != "" {
		t.Errorf("empty description became %q instead of falling back to the command's", listSub.Description)
	}
}

// TestCommandGroup_Replaces pins last-write-wins for a group name, the rule
// Command and Task already follow, so a generator never has to track what it
// emitted.
func TestCommandGroup_Replaces(t *testing.T) {
	built, err := commandGroupsBuilder().
		CommandGroup("cloud", "Replaced", Subcommand("status", "", Runs("cloud-status"))).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	group := built.CommandGroups["cloud"]
	if group.Description != "Replaced" || len(group.Flags) != 0 {
		t.Errorf("group was merged instead of replaced: %+v", group)
	}
}

// TestCommandGroup_RejectsProtocolViolations pins that the new block reaches the
// SAME verdict every other member does. The SDK adds no validator of its own —
// these codes come from protocols/extension — so what is asserted is that an
// authored group is validated at all, and with the codes a consumer keys on.
func TestCommandGroup_RejectsProtocolViolations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Builder)
		code   string
	}{
		{
			name: "a group with no subcommand is not a group",
			mutate: func(b *Builder) {
				b.CommandGroup("cloud", "Interact with Putnami Cloud")
			},
			code: "required-field",
		},
		{
			name: "a subcommand must route to a command of this manifest",
			mutate: func(b *Builder) {
				b.CommandGroup("cloud", "", Subcommand("status", "", Runs("cloud-stats")))
			},
			code: "unresolved-command",
		},
		{
			name: "a subcommand with no command target",
			mutate: func(b *Builder) {
				b.CommandGroup("cloud", "", Subcommand("status", ""))
			},
			code: "required-field",
		},
		{
			// D6 depends on this: an extension subcommand can never declare
			// `output`, which is why it reads the forwarded params["output"].
			name: "a group flag may not shadow a reserved global flag",
			mutate: func(b *Builder) {
				b.CommandGroup("cloud", "",
					GroupFlag("output", proto.FlagDefinition{Type: "string"}),
					Subcommand("status", "", Runs("cloud-status")))
			},
			code: "reserved-global-flag",
		},
		{
			// The short form is checked too: -v is --verbose everywhere.
			name: "a subcommand flag may not shadow a reserved short flag",
			mutate: func(b *Builder) {
				b.CommandGroup("cloud", "", Subcommand("status", "",
					Runs("cloud-status"),
					SubcommandFlag("detailed", proto.FlagDefinition{Type: "boolean", Short: "v"})))
			},
			code: "reserved-global-flag",
		},
		{
			name: "an example without a command line",
			mutate: func(b *Builder) {
				b.CommandGroup("cloud", "", Subcommand("status", "",
					Runs("cloud-status"),
					Example("", "no command")))
			},
			code: "required-field",
		},
		{
			name: "an optional workspace on a subcommand that is not interactive",
			mutate: func(b *Builder) {
				b.CommandGroup("cloud", "", Subcommand("status", "",
					Runs("cloud-status"),
					WorkspaceRequirement(proto.SubcommandWorkspaceOptional)))
			},
			code: "invalid-workspace-requirement",
		},
		{
			name: "a group default that names no subcommand",
			mutate: func(b *Builder) {
				b.CommandGroup("cloud", "",
					DefaultSubcommand("stats"),
					Subcommand("status", "", Runs("cloud-status")))
			},
			code: "unresolved-default-subcommand",
		},
		{
			name: "a positional without a name",
			mutate: func(b *Builder) {
				b.CommandGroup("cloud", "", Subcommand("status", "",
					Runs("cloud-status"),
					Positional("")))
			},
			code: "required-field",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builder := commandGroupsBuilder()
			tc.mutate(builder)

			built, err := builder.Build()
			if err == nil {
				t.Fatalf("authoring accepted an invalid command group: %+v", built.CommandGroups)
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error is %T, want *ValidationError: %v", err, err)
			}
			if !hasCode(verr.Codes(), tc.code) {
				t.Fatalf("codes = %v, want %q (%v)", verr.Codes(), tc.code, err)
			}
		})
	}
}

// canonicalDocument parses manifest bytes through the protocol's strict parser
// — which validates and then normalizes — and returns the result as a generic
// document. Going through the parser is what makes the comparison total: a
// manifest a consumer would reject never reaches the diff, and the defaulting
// the protocol applies to a loaded manifest (an absent dependency block becomes
// an empty one, task declarations are canonicalized) is applied to both sides
// by the same code.
func canonicalDocument(t *testing.T, data []byte) map[string]any {
	t.Helper()
	parsed, diags := proto.ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("manifest failed the protocol's strict parse: %v\n%s", diags, data)
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("encode normalized manifest: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return document
}

// canonical re-encodes a decoded document with every key sorted, so a diff shows
// a real difference and never a field-order difference.
func canonical(t *testing.T, document map[string]any) string {
	t.Helper()
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return string(data)
}
