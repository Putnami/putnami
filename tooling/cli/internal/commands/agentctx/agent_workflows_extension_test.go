package agentctx

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func writePathFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeLocalContentExtension authors a content-only extension at rel under
// root: one skill under its closed source root, the project config that
// declares its content policy, and the manifest that contributes the content.
func writeLocalContentExtension(t *testing.T, root, rel, name, skill, body string, supersedes ...string) string {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	contribution := `{"path":"content","source":"src"`
	if len(supersedes) > 0 {
		quoted := make([]string, 0, len(supersedes))
		for _, superseded := range supersedes {
			quoted = append(quoted, fmt.Sprintf("%q", superseded))
		}
		contribution += `,"supersedes":[` + strings.Join(quoted, ",") + `]`
	}
	contribution += `}`
	writePathFile(t, dir, "putnami.extension.json", fmt.Sprintf(`{"name":%q,"cliContract":%d,"agentContent":%s}`,
		name, protocolcli.AgentContentContract, contribution))
	writePathFile(t, dir, "putnami.json", fmt.Sprintf(`{"name":%q,"options":{"agent-artifact":{"forbiddenContent":[],"requiredSkills":[%q]}}}`, name, skill))
	writePathFile(t, dir, "src/skills/"+skill+"/SKILL.md", "---\nname: "+skill+"\ndescription: A local skill\n---\n\n"+body)
	return dir
}

// localContentConfig declares the local extension at rel and opts into the
// content of the extension name, plus any further entries.
func localContentConfig(rel, name string, entries ...string) *wsproto.Config {
	return &wsproto.Config{
		Extensions:     wsproto.ExtensionsConfig{List: map[string]string{rel: ""}},
		AgentArtifacts: append([]string{extensionAgentContentPrefix + name}, entries...),
	}
}

// Every pass refuses a legacy entry with nothing written and gives the steps
// that replace it in this workspace, naming the extension that supersedes it
// when one is known: a declared one whose content says so, else the
// first-party successor. An entry is deleted when that extension is already
// opted into, and the extension is declared only when it is not yet. There is
// no command that moves it any more, and no step edits the lock.
func TestLegacyEntriesFailClearly(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "legacy-declarations-refused", "every-pass-refuses-a-legacy-entry")

	t.Run("beside an opt-in", func(t *testing.T) {
		root := t.TempDir()
		writePathFile(t, filepath.Join(root, "tools", "workflows"), "putnami.json", `{"name":"@local/workflows"}`)
		writeLocalContentExtension(t, root, "/tools/contributor", "@local/contributor", "local-plan", "Body.\n", "@local/workflows")
		cfg := localContentConfig("/tools/contributor", "@local/contributor", "/tools/workflows")

		_, phaseErr := AgentWorkflowPhaseApplies(root, cfg)
		_, generateErr := MaterializeLocalAgentContent(root, cfg, nil)
		passes := map[string]error{
			"install":          InstallAgentWorkflows(context.Background(), root, cfg, nil),
			"upgrade":          AdoptAgentWorkflows(context.Background(), root, cfg, nil),
			"phase":            phaseErr,
			"ensure":           EnsureAgentWorkflows(context.Background(), root, cfg),
			"context generate": generateErr,
		}
		for name, err := range passes {
			if !errors.Is(err, protocolcli.ErrInvalidConfig) || !strings.Contains(err.Error(), `"/tools/workflows" in a form this CLI no longer installs`) ||
				!strings.HasSuffix(err.Error(), "entry: delete each entry, since extension:@local/contributor already opts into the content that supersedes it, "+
					"then run `putnami install`") {
				t.Fatalf("%s error = %v, want the legacy entry refused with only the steps that apply", name, err)
			}
			if next := protocolcli.SuggestedNext(err); next != "" {
				t.Fatalf("%s next step = %q, want none", name, next)
			}
		}
		for _, host := range []string{".agents", ".claude", ".codex", ".putnami"} {
			if _, err := os.Lstat(filepath.Join(root, host)); !os.IsNotExist(err) {
				t.Fatalf("a refused pass wrote %s: %v", host, err)
			}
		}
	})

	// Entries with different successors get their own steps: an opt-in that
	// supersedes one entry never tells the user to delete another.
	t.Run("with several successors", func(t *testing.T) {
		root := t.TempDir()
		writePathFile(t, filepath.Join(root, "tools", "workflows"), "putnami.json", `{"name":"@local/workflows"}`)
		writeLocalContentExtension(t, root, "/tools/contributor", "@local/contributor", "local-plan", "Body.\n", "@local/workflows")
		cfg := localContentConfig("/tools/contributor", "@local/contributor", "/tools/workflows", "@acme/flows", "@putnami/agent-workflows")

		_, err := DeclaredAgentArtifacts(root, cfg)
		want := `entry: for "/tools/workflows": delete each entry, since extension:@local/contributor already opts into the content that supersedes it; ` +
			`for "@acme/flows": replace each entry with extension:<name> for the extension whose agent content supersedes it, declare that extension in extensions; ` +
			`for "@putnami/agent-workflows": replace each entry with extension:@putnami/contributor, declare @putnami/contributor in extensions, ` +
			"then run `putnami install`"
		if !errors.Is(err, protocolcli.ErrInvalidConfig) || !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("error = %v, want the steps grouped by successor: %q", err, want)
		}
	})

	const (
		contributorSteps = "replace each entry with extension:@putnami/contributor, declare @putnami/contributor in extensions, then run `putnami install`"
		unknownSteps     = "replace each entry with extension:<name> for the extension whose agent content supersedes it, " +
			"declare that extension in extensions, then run `putnami install`"
	)
	for entry, steps := range map[string]string{
		"@putnami/agent-workflows:stable": contributorSteps,
		"@putnami/maintainer-workflows":   contributorSteps,
		"/tooling/agent-workflows":        contributorSteps,
		"@acme/workflows:1.2.x":           unknownSteps,
		"/tools/acme-workflows":           unknownSteps,
	} {
		t.Run(entry, func(t *testing.T) {
			cfg := &wsproto.Config{AgentArtifacts: []string{entry}}
			_, err := DeclaredAgentArtifacts(t.TempDir(), cfg)
			if !errors.Is(err, protocolcli.ErrInvalidConfig) || !strings.Contains(err.Error(), fmt.Sprintf("%q", entry)) ||
				!strings.HasSuffix(err.Error(), "entry: "+steps) {
				t.Fatalf("error = %v, want %q refused with the steps %q", err, entry, steps)
			}
		})
	}

	// An in-tree entry is named by its project's putnami.json, so a directory
	// named like a first-party artifact does not make it one; the directory
	// name answers only when the project is gone.
	for name, steps := range map[string]string{
		"@acme/agent-workflows":    unknownSteps,
		"@putnami/agent-workflows": contributorSteps,
	} {
		t.Run("/tools/agent-workflows holding "+name, func(t *testing.T) {
			root := t.TempDir()
			writePathFile(t, filepath.Join(root, "tools", "agent-workflows"), "putnami.json", fmt.Sprintf(`{"name":%q}`, name))
			_, err := DeclaredAgentArtifacts(root, &wsproto.Config{AgentArtifacts: []string{"/tools/agent-workflows"}})
			if !errors.Is(err, protocolcli.ErrInvalidConfig) || !strings.HasSuffix(err.Error(), "entry: "+steps) {
				t.Fatalf("error = %v, want the steps %q", err, steps)
			}
		})
	}
}

// The first-party successors the legacy refusal names are exactly what the
// contributor extension's manifest supersedes, so the two cannot drift apart.
func TestFirstPartySuccessorsMatchTheContributorManifest(t *testing.T) {
	manifest, err := extproto.LoadManifest(filepath.Join(driftRepositoryRoot(t), "tooling", "contributor", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.AgentContent == nil {
		t.Fatalf("%s ships no agent content", manifest.Name)
	}
	want := make(map[string]string, len(manifest.AgentContent.Supersedes))
	for _, superseded := range manifest.AgentContent.Supersedes {
		want[superseded] = manifest.Name
	}
	if !maps.Equal(firstPartyAgentContentSuccessors, want) {
		t.Fatalf("first-party successors = %v, want %v from the contributor manifest", firstPartyAgentContentSuccessors, want)
	}
}

func TestAgentArtifactDeclarationNameNamesOptIns(t *testing.T) {
	for _, tc := range []struct{ declared, want string }{
		{"extension:@putnami/contributor", "@putnami/contributor"},
		{" extension:@putnami/contributor ", "@putnami/contributor"},
		{"@putnami/agent-workflows:stable", ""},
		{"/tools/workflows", ""},
		{"extension:Not A Name", ""},
		{"   ", ""},
	} {
		if got := AgentArtifactDeclarationName(tc.declared); got != tc.want {
			t.Errorf("AgentArtifactDeclarationName(%q) = %q, want %q", tc.declared, got, tc.want)
		}
	}
}

// A local extension's content version follows its content: an unchanged
// source rebuilds into the same verified tree, a tampered staged tree is
// replaced, and any content change moves the version.
func TestLocalContentVersionFollowsContent(t *testing.T) {
	root := t.TempDir()
	dir := writeLocalContentExtension(t, root, "/tools/contributor", "@local/contributor", "local-plan", "First body.\n")
	cfg := localContentConfig("/tools/contributor", "@local/contributor")
	build := func() AgentArtifactResolution {
		t.Helper()
		source, err := extension.LocateAgentContent(root, cfg, "@local/contributor")
		if err != nil {
			t.Fatal(err)
		}
		resolution, err := buildLocalExtensionAgentContent(root, source)
		if err != nil {
			t.Fatal(err)
		}
		return resolution
	}

	first := build()
	if !strings.HasPrefix(first.Entry.Version, localContentProbeVersion+"-") {
		t.Fatalf("version = %q, want a content-derived local version", first.Entry.Version)
	}
	if _, err := agentartifacts.LoadArtifact(first.Dir, "@local/contributor", first.Entry); err != nil {
		t.Fatalf("staged tree does not verify: %v", err)
	}
	if again := build(); again != first {
		t.Fatalf("an unchanged source resolved differently: %+v vs %+v", again, first)
	}

	tampered := filepath.Join(first.Dir, ".agents", "skills", "local-plan", "SKILL.md")
	if err := os.WriteFile(tampered, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build()
	if _, err := agentartifacts.LoadArtifact(first.Dir, "@local/contributor", first.Entry); err != nil {
		t.Fatalf("a tampered staged tree was not replaced: %v", err)
	}

	writePathFile(t, dir, "src/skills/local-plan/SKILL.md", "---\nname: local-plan\ndescription: A local skill\n---\n\nSecond body.\n")
	if changed := build(); changed.Entry.Version == first.Entry.Version || changed.Dir == first.Dir {
		t.Fatalf("a content change kept version %q", first.Entry.Version)
	}
}
