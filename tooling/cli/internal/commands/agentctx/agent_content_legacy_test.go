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

// Every pass refuses a legacy entry with nothing written, names the migration,
// and names the extension that supersedes it: a declared one whose content
// says so, else the first-party successor, else a placeholder.
func TestLegacyEntriesFailClearlyAndNameTheMigration(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "legacy-declarations-refused", "every-pass-refuses-a-legacy-entry-and-names-the-migration")

	t.Run("a declared local extension that supersedes it", func(t *testing.T) {
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
				!strings.Contains(err.Error(), "Move the declarations, pins and ownership records to extension @local/contributor") {
				t.Fatalf("%s error = %v, want the legacy entry refused", name, err)
			}
			if next := protocolcli.SuggestedNext(err); next != "putnami migrate agent-content @local/contributor" {
				t.Fatalf("%s next step = %q", name, next)
			}
		}
		for _, host := range []string{".agents", ".claude", ".codex", ".putnami"} {
			if _, err := os.Lstat(filepath.Join(root, host)); !os.IsNotExist(err) {
				t.Fatalf("a refused pass wrote %s: %v", host, err)
			}
		}
	})

	for name, tc := range map[string]struct {
		extensions []string
		entry      string
		step, next string
	}{
		"a first-party artifact before its successor is declared": {
			entry: "@putnami/agent-workflows:stable",
			step:  "Declare extension @putnami/contributor in extensions, run `putnami install`",
			next:  "putnami migrate agent-content @putnami/contributor",
		},
		"a first-party artifact whose successor is declared but not installed": {
			extensions: []string{"@putnami/contributor"},
			entry:      "@putnami/maintainer-workflows",
			step:       "Run `putnami install` to install extension @putnami/contributor",
			next:       "putnami migrate agent-content @putnami/contributor",
		},
		"a first-party in-tree artifact whose project is gone": {
			entry: "/tooling/agent-workflows",
			step:  "Declare extension @putnami/contributor in extensions, run `putnami install`",
			next:  "putnami migrate agent-content @putnami/contributor",
		},
		"an artifact no known extension supersedes": {
			entry: "@acme/workflows:1.2.x",
			step:  "Declare the extension whose agent content supersedes them",
			next:  "putnami migrate agent-content <extension>",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			list := map[string]string{}
			for _, ext := range tc.extensions {
				list[ext] = ""
			}
			cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: list}, AgentArtifacts: []string{tc.entry}}
			_, err := DeclaredAgentArtifacts(root, cfg)
			if !errors.Is(err, protocolcli.ErrInvalidConfig) || !strings.Contains(err.Error(), fmt.Sprintf("%q", tc.entry)) || !strings.Contains(err.Error(), tc.step) {
				t.Fatalf("error = %v, want %q refused with %q", err, tc.entry, tc.step)
			}
			if next := protocolcli.SuggestedNext(err); next != tc.next {
				t.Fatalf("next step = %q, want %q", next, tc.next)
			}
		})
	}
}

func TestParseLegacyAgentArtifactNamesItsArtifact(t *testing.T) {
	root := t.TempDir()
	writePathFile(t, filepath.Join(root, "tools", "workflows"), "putnami.json", `{"name":"@local/workflows"}`)
	writePathFile(t, filepath.Join(root, "tools", "nameless"), "putnami.json", `{}`)

	for _, tc := range []struct{ declared, want string }{
		{" /tools/workflows/ ", "@local/workflows"},
		{"@putnami/agent-workflows:stable", "@putnami/agent-workflows"},
		{"@putnami/agent-workflows", "@putnami/agent-workflows"},
	} {
		got, err := parseLegacyAgentArtifact(root, tc.declared)
		if err != nil || got.Name != tc.want || got.Declared != strings.TrimSpace(tc.declared) {
			t.Errorf("parseLegacyAgentArtifact(%q) = %+v, %v; want %s", tc.declared, got, err, tc.want)
		}
	}
	for declared, want := range map[string]string{
		"/../outside":      "leaves the workspace",
		"/tools/../escape": "leaves the workspace",
		"/":                "names the workspace root",
		"/tools/missing":   "has no readable project config",
		"/tools/nameless":  "declares no project name",
		`/tools\workflows`: "must use / separators",
		":stable":          "names no artifact",
	} {
		if _, err := parseLegacyAgentArtifact(root, declared); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseLegacyAgentArtifact(%q) error = %v, want %q", declared, err, want)
		}
	}
}

// An in-tree entry whose project is gone is named by the one candidate whose
// unscoped name is the path's last segment, and by nothing else.
func TestNameMissingLegacyProjectMatchesOneCandidateBySegment(t *testing.T) {
	root := t.TempDir()
	_, err := parseLegacyAgentArtifact(root, "/tooling/agent-workflows")
	var missing *missingLegacyProjectError
	if !errors.As(err, &missing) {
		t.Fatalf("error = %v, want a missing project", err)
	}
	for _, tc := range []struct {
		candidates []string
		want       string
	}{
		{[]string{"@putnami/maintainer-workflows", "@putnami/agent-workflows"}, "@putnami/agent-workflows"},
		{[]string{"agent-workflows"}, "agent-workflows"},
		{[]string{"@putnami/agent-workflows", "@putnami/agent-workflows"}, "@putnami/agent-workflows"},
		{[]string{"@putnami/agent-workflows", "@acme/agent-workflows"}, ""},
		{[]string{"@putnami/workflows"}, ""},
		{nil, ""},
	} {
		if got := nameMissingLegacyProject(missing, tc.candidates); got != tc.want {
			t.Errorf("nameMissingLegacyProject(%v) = %q, want %q", tc.candidates, got, tc.want)
		}
	}
}

func TestAgentArtifactDeclarationNameNamesBothForms(t *testing.T) {
	root := t.TempDir()
	writePathFile(t, filepath.Join(root, "tools", "workflows"), "putnami.json", `{"name":"@local/workflows"}`)
	for _, tc := range []struct{ declared, want string }{
		{"extension:@putnami/contributor", "@putnami/contributor"},
		{"@putnami/agent-workflows:stable", "@putnami/agent-workflows"},
		{"/tools/workflows", "@local/workflows"},
		{"/tools/missing", ""},
		{"extension:Not A Name", ""},
		{"   ", ""},
	} {
		if got := AgentArtifactDeclarationName(root, tc.declared); got != tc.want {
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
