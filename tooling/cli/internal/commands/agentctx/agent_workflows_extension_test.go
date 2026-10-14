package agentctx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
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

// Every pass refuses a legacy entry with nothing written and gives every step
// that replaces it, naming the extension that supersedes it when one is known:
// a declared one whose content says so, else the first-party successor. There
// is no command that moves it any more.
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
			"reconcile":        ReconcileAgentWorkflows(context.Background(), root, cfg),
			"context generate": generateErr,
		}
		for name, err := range passes {
			if !errors.Is(err, protocolcli.ErrInvalidConfig) || !strings.Contains(err.Error(), `"/tools/workflows" in a form this CLI no longer installs`) ||
				!strings.Contains(err.Error(), "replace each entry with extension:@local/contributor, declare that extension in extensions, "+
					"delete the entries' pins under agentArtifacts in putnami.lock.json, then run `putnami install`") {
				t.Fatalf("%s error = %v, want the legacy entry refused", name, err)
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

	for entry, replacement := range map[string]string{
		"@putnami/agent-workflows:stable": "extension:@putnami/contributor",
		"@putnami/maintainer-workflows":   "extension:@putnami/contributor",
		"/tooling/agent-workflows":        "extension:@putnami/contributor",
		"@acme/workflows:1.2.x":           "extension:<name> for the extension whose agent content supersedes it",
		"/tools/acme-workflows":           "extension:<name> for the extension whose agent content supersedes it",
	} {
		t.Run(entry, func(t *testing.T) {
			cfg := &wsproto.Config{AgentArtifacts: []string{entry}}
			_, err := DeclaredAgentArtifacts(t.TempDir(), cfg)
			if !errors.Is(err, protocolcli.ErrInvalidConfig) || !strings.Contains(err.Error(), fmt.Sprintf("%q", entry)) ||
				!strings.Contains(err.Error(), "replace each entry with "+replacement+", declare") {
				t.Fatalf("error = %v, want %q refused and replaced with %s", err, entry, replacement)
			}
		})
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
