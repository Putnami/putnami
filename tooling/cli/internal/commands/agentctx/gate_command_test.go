package agentctx

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// writeCIDocument writes a minimal valid version 3 document declaring the given
// commands, so a fixture workspace only has to state the part under test. It
// builds the protocol's declared types rather than untyped maps: the CI
// document is a typed contract, and marshaling ciproto.Document is what proves
// the fixture is the shape production reads.
func writeCIDocument(t *testing.T, dir string, commands ...string) {
	t.Helper()
	document := ciproto.Document{Schema: ciproto.SchemaURL, Version: ciproto.Version, Commands: commandEntries(commands...)}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal CI document: %v", err)
	}
	if _, parseErr := ciproto.Parse(data); parseErr != nil {
		t.Fatalf("fixture CI document is invalid, the test would prove nothing: %v", parseErr)
	}
	if err := os.WriteFile(filepath.Join(dir, ciproto.Filename), data, 0o644); err != nil {
		t.Fatalf("write CI document: %v", err)
	}
}

// commandEntries builds the blocking form of every named command.
func commandEntries(names ...string) []ciproto.CommandEntry {
	entries := make([]ciproto.CommandEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, ciproto.CommandEntry{Name: name})
	}
	return entries
}

// defaultGate is the line the protocol's default CI document yields: its
// blocking commands in its order. The test derives it rather than pinning a
// literal, so the core names no command here either.
func defaultGate() string {
	return strings.Join(gateCommandNames(ciproto.DefaultDocument()), ",")
}

// A workspace without a usable document receives the blocking commands of the
// protocol's default CI document, in that document's order.
func TestGateTasksFallsBackWithoutCIDocument(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "commands-not-gate", "an-unusable-document-falls-back-to-the-default-documents-commands")
	want := []string{"lint", "test", "build"}
	if got := gateCommandNames(ciproto.DefaultDocument()); !reflect.DeepEqual(got, want) {
		t.Fatalf("default document commands = %v, want %v; update the documents that quote the fallback", got, want)
	}
	if got := gateTasksForWorkspace(t.TempDir()); got != defaultGate() {
		t.Fatalf("gate without a CI document = %q, want %q", got, defaultGate())
	}
}

// extensionManifest returns a loadable putnami.extension.json body for an
// extension that declares one command per job name, each running one task. It
// marshals the protocol's manifest type, the shape discovery reads.
func extensionManifest(t *testing.T, name string, jobs ...string) []byte {
	t.Helper()
	manifest := extproto.Manifest{
		Name:     name,
		Commands: make(map[string]extproto.CommandDefinition, len(jobs)),
		Tasks:    make(map[string]extproto.TaskDefinition, len(jobs)),
	}
	for _, job := range jobs {
		manifest.Commands[job] = extproto.CommandDefinition{Run: []extproto.PipelineStep{{ID: job, Task: job + "-exec"}}}
		manifest.Tasks[job+"-exec"] = extproto.TaskDefinition{Kind: "command", Command: "echo", Args: []string{job}}
	}
	manifest.CLIContract = extproto.RequiredCLIContract(&manifest)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal extension manifest: %v", err)
	}
	return data
}

// writeExtension writes an extension manifest declaring jobs at the
// workspace-relative path, the shorthand a workspace config uses.
func writeExtension(t *testing.T, dir, path string, manifest []byte) {
	t.Helper()
	manifestDir := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(path, "/")))
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", manifestDir, err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "putnami.extension.json"), manifest, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// writeWorkspaceExtensions writes a minimal putnami.workspace.json declaring
// the given extensions, so a fixture states exactly what discovery reads.
func writeWorkspaceExtensions(t *testing.T, dir string, extensions ...string) {
	t.Helper()
	document := struct {
		Name       string   `json:"name"`
		Extensions []string `json:"extensions"`
	}{Name: "fixture", Extensions: extensions}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal workspace config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), data, 0o644); err != nil {
		t.Fatalf("write workspace config: %v", err)
	}
}

// A workspace without a usable CI document is told the default document's
// blocking commands, whatever its extensions declare: the gate reads no extension, so it does not
// change with what a machine installed or with the host a regeneration runs
// on. A workspace whose gate runs more says so in its CI document.
func TestGateTasksWithoutCIDocumentIsTheGenericGate(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "commands-not-gate", "an-unusable-document-falls-back-to-the-default-documents-commands")
	t.Parallel()
	validate := extensionManifest(t, "@example/checks", "validate")
	cases := []struct {
		name       string
		extensions []string
		manifests  map[string][]byte
	}{
		{"no workspace file", nil, nil},
		{"no extension", []string{}, nil},
		{"a declared local extension declares validate", []string{"/lang", "/checks"}, map[string][]byte{
			"/lang":   extensionManifest(t, "@example/lang", "lint", "test", "build"),
			"/checks": validate,
		}},
		{"an extension declared by name", []string{"@putnami/sdd"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.extensions != nil {
				writeWorkspaceExtensions(t, dir, tc.extensions...)
			}
			for path, manifest := range tc.manifests {
				writeExtension(t, dir, path, manifest)
			}
			if got := gateTasksForWorkspace(dir); got != defaultGate() {
				t.Fatalf("gate = %q, want %q", got, defaultGate())
			}
			if got := GateTasks(dir); got != defaultGate() {
				t.Fatalf("GateTasks = %q, want the same derivation %q", got, defaultGate())
			}
		})
	}

	// A workspace project that holds an extension manifest declaring validate
	// leaves the gate generic too.
	t.Run("a project manifest declares validate", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{"name":"fixture","includes":["checks"]}`), 0o644); err != nil {
			t.Fatalf("write workspace config: %v", err)
		}
		writeExtension(t, dir, "/checks", validate)
		if err := os.WriteFile(filepath.Join(dir, "checks", "putnami.json"), []byte(`{"name":"checks"}`), 0o644); err != nil {
			t.Fatalf("write project config: %v", err)
		}
		if got := gateTasksForWorkspace(dir); got != defaultGate() {
			t.Fatalf("gate = %q, want %q", got, defaultGate())
		}
	})

	// A usable document is the only way to widen the gate, and it stays
	// authoritative in both directions.
	t.Run("the document decides", func(t *testing.T) {
		t.Parallel()
		withValidate := t.TempDir()
		writeCIDocument(t, withValidate, "build", "lint", "test", "validate")
		if got := gateTasksForWorkspace(withValidate); got != "build,lint,test,validate" {
			t.Fatalf("gate = %q, want the document's build,lint,test,validate", got)
		}
		trio := t.TempDir()
		writeWorkspaceExtensions(t, trio, "/checks")
		writeExtension(t, trio, "/checks", validate)
		writeCIDocument(t, trio, "build", "lint", "test")
		if got := gateTasksForWorkspace(trio); got != "build,lint,test" {
			t.Fatalf("gate = %q, want the document's build,lint,test", got)
		}
	})

	// An unusable document falls back to the generic gate, whatever the
	// extensions declare.
	t.Run("an unusable document falls back to the generic gate", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeWorkspaceExtensions(t, dir, "/checks")
		writeExtension(t, dir, "/checks", validate)
		if err := os.WriteFile(filepath.Join(dir, ciproto.Filename), []byte(`{"version":3,"commands":[]}`), 0o644); err != nil {
			t.Fatalf("write document: %v", err)
		}
		if got := gateTasksForWorkspace(dir); got != defaultGate() {
			t.Fatalf("gate = %q, want %q", got, defaultGate())
		}
	})
}

// Every way a document can fail to name a runnable required job falls back
// rather than emitting a broken or empty command.
func TestGateTasksFallsBackForUnusableDocuments(t *testing.T) {
	cases := map[string]string{
		"invalid json":   `{`,
		"wrong version":  `{"version":2,"commands":["lint"]}`,
		"unknown field":  `{"version":3,"commands":["lint"],"pipelines":{}}`,
		"leftover gate":  `{"version":3,"gate":["lint","test","build","validate"]}`,
		"empty document": ``,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ciproto.Filename), []byte(body), 0o644); err != nil {
				t.Fatalf("write document: %v", err)
			}
			if got := gateTasksForWorkspace(dir); got != defaultGate() {
				t.Fatalf("gate = %q, want the fallback %q", got, defaultGate())
			}
		})
	}

	t.Run("empty commands", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ciproto.Filename), []byte(`{"version":3,"commands":[]}`), 0o644); err != nil {
			t.Fatalf("write document: %v", err)
		}
		if got := gateTasksForWorkspace(dir); got != defaultGate() {
			t.Fatalf("gate = %q, want the fallback %q for an empty command list", got, defaultGate())
		}
	})

	// A document whose every command is advisory declares no gate at all, so
	// the guidance falls back rather than telling a contributor that a command
	// which never fails a run is what gates their merge.
	t.Run("only advisory commands", func(t *testing.T) {
		dir := t.TempDir()
		body := `{"version":3,"commands":[{"name":"audit","failOnError":false}]}`
		if err := os.WriteFile(filepath.Join(dir, ciproto.Filename), []byte(body), 0o644); err != nil {
			t.Fatalf("write document: %v", err)
		}
		if got := gateTasksForWorkspace(dir); got != defaultGate() {
			t.Fatalf("gate = %q, want the fallback %q", got, defaultGate())
		}
	})

	t.Run("only blank command names", func(t *testing.T) {
		if got := gateCommandNames(ciproto.Document{Commands: commandEntries(" ", "")}); len(got) != 0 {
			t.Fatalf("gate commands = %v, want none so the caller falls back", got)
		}
	})
}

// The derived gate names every blocking command the document declares, in the
// document's order.
func TestGateTasksKeepsTheDocumentsOrder(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "commands-not-gate", "the-gate-keeps-the-documents-order")
	cases := []struct {
		name     string
		commands []string
		want     string
	}{
		{"test lint build stays test lint build", []string{"test", "lint", "build"}, "test,lint,build"},
		{"build first", []string{"validate-workspace", "build", "validate", "test", "lint"}, "validate-workspace,build,validate,test,lint"},
		{"the conventional order", []string{"lint", "test", "build", "validate"}, "lint,test,build,validate"},
		{"one command", []string{"check"}, "check"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCIDocument(t, dir, tc.commands...)
			if got := gateTasksForWorkspace(dir); got != tc.want {
				t.Fatalf("derived gate = %q, want %q", got, tc.want)
			}
			if got := GateTasks(dir); got != tc.want {
				t.Fatalf("GateTasks = %q, want %q", got, tc.want)
			}
		})
	}
}

// Blank names are skipped, and a name that repeats counts once, so a
// hand-edited document cannot emit an empty or repeated command.
func TestGateCommandNamesSkipsBlankAndRepeatedNames(t *testing.T) {
	document := ciproto.Document{Commands: commandEntries("lint", " ", "", "lint", " test ", "build")}
	if got, want := gateCommandNames(document), []string{"lint", "test", "build"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("gate commands = %v, want %v", got, want)
	}
	if got := gateCommandNames(ciproto.Document{}); len(got) != 0 {
		t.Fatalf("gate commands of an empty document = %v, want none", got)
	}
}

// A document that declares the default document's commands renders the same
// string as the fallback, so adopting the protocol's default CI document does
// not churn the generated guidance.
func TestGateTasksForDefaultDocumentMatchesFallback(t *testing.T) {
	dir := t.TempDir()
	data, err := json.Marshal(ciproto.DefaultDocument())
	if err != nil {
		t.Fatalf("marshal default document: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ciproto.Filename), data, 0o644); err != nil {
		t.Fatalf("write default document: %v", err)
	}
	if got, want := gateTasksForWorkspace(dir), gateTasksForWorkspace(t.TempDir()); got != want {
		t.Fatalf("gate for the default document = %q, want the fallback %q", got, want)
	}
}

// Only the blocking commands reach the guidance line: an advisory command runs
// but never fails the run, so naming it as part of the gate would be false.
func TestGateTasksKeepsBlockingCommandsOnly(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "commands-not-gate", "guidance-follows-blocking-commands")
	advisory := false
	document := ciproto.Document{Version: ciproto.Version, Commands: []ciproto.CommandEntry{
		{Name: "lint"},
		{Name: "audit", FailOnError: &advisory},
		{Name: "test"},
	}}
	if got := gateCommandNames(document); !reflect.DeepEqual(got, []string{"lint", "test"}) {
		t.Fatalf("blocking commands = %v, want [lint test]", got)
	}
}

// The derived gate has to reach every generated file. A site left hardcoded
// would tell an agent to run a weaker command than CI requires.
func TestGeneratedGuidanceCarriesDerivedGateAtEverySite(t *testing.T) {
	dir := t.TempDir()
	writeCIDocument(t, dir, "build", "lint", "test", "validate", "validate-workspace")

	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("WriteAgentEntrypoints: %v", err)
	}

	const gate = "build,lint,test,validate,validate-workspace"
	wantPerFile := map[string][]string{
		// CLAUDE.md imports AGENTS.md, so the block — and its gate — is
		// stated once, in AGENTS.md.
		ClaudeEntrypointPath: {"@AGENTS.md"},
		agentEntrypointPath:  {"`putnami " + gate + " --impacted --enforce-coverage`"},
	}
	for path, wants := range wantPerFile {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(data)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing derived gate site %q", path, want)
			}
		}
		if strings.Contains(body, gatePlaceholder) {
			t.Errorf("%s still contains the unsubstituted %s placeholder", path, gatePlaceholder)
		}
		// A hardcoded remnant reads as the fallback followed by a flag, which
		// the derived line never produces because it continues with `,validate`.
		if strings.Contains(body, "putnami "+defaultGate()+" -") || strings.Contains(body, "putnami "+defaultGate()+"`") {
			t.Errorf("%s still names the hardcoded gate %q", path, defaultGate())
		}
	}
}

// Regeneration must be a pure function of the workspace, or CI's install stage
// fails with "worktree mutated" on a clean checkout. That holds for a gate read
// from the CI document and for the generic gate of a workspace without one.
func TestWriteAgentEntrypointsIsIdempotent(t *testing.T) {
	fixtures := map[string]struct {
		write func(t *testing.T, dir string)
		gate  string
	}{
		"CI document": {func(t *testing.T, dir string) {
			writeCIDocument(t, dir, "build", "lint", "test", "validate")
		}, "build,lint,test,validate"},
		"extensions without a CI document": {func(t *testing.T, dir string) {
			writeWorkspaceExtensions(t, dir, "/checks")
			writeExtension(t, dir, "/checks", extensionManifest(t, "@example/checks", "validate"))
		}, defaultGate()},
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fixture.write(t, dir)

			first := make(map[string]string)
			for run := range 3 {
				if err := WriteAgentEntrypoints(dir); err != nil {
					t.Fatalf("run %d: WriteAgentEntrypoints: %v", run, err)
				}
				for _, path := range []string{ClaudeEntrypointPath, agentEntrypointPath} {
					data, err := os.ReadFile(filepath.Join(dir, path))
					if err != nil {
						t.Fatalf("run %d: read %s: %v", run, path, err)
					}
					if run == 0 {
						first[path] = string(data)
						continue
					}
					if string(data) != first[path] {
						t.Errorf("run %d rewrote %s", run, path)
					}
				}
			}
			want := "`putnami " + fixture.gate + " --impacted --enforce-coverage`"
			if !strings.Contains(first[agentEntrypointPath], want) {
				t.Errorf("%s does not carry the derived gate %s:\n%s", agentEntrypointPath, want, first[agentEntrypointPath])
			}
		})
	}
}

// repositoryGuidanceBytes reads the bytes whose parity the repository test is
// meant to guard. Locally those are the working-tree bytes, so changing the
// generator, regenerating the guidance, and running the gate before committing
// remains a valid workflow. In CI they are the committed bytes: the Cloud
// preparer runs an installed CLI before the branch CLI is built, and that
// lifecycle step may regenerate guidance with an older template.
//
// A source archive has no Git metadata, so its working tree is the only source
// of truth available. A real repository with an unreadable HEAD or an untracked
// guidance path is different: silently accepting its mutable file would make
// the committed-byte ratchet vacuous, so those failures remain errors.
func repositoryGuidanceBytes(root, path string) ([]byte, error) {
	readWorktree := func() ([]byte, error) {
		return os.ReadFile(filepath.Join(root, path))
	}
	if strings.TrimSpace(os.Getenv("CI")) == "" {
		return readWorktree()
	}

	if _, err := os.Stat(filepath.Join(root, ".git")); os.IsNotExist(err) {
		return readWorktree()
	} else if err != nil {
		return nil, fmt.Errorf("inspect repository metadata: %w", err)
	}

	committed, err := exec.Command("git", "-C", root, "show", "HEAD:"+filepath.ToSlash(path)).Output()
	if err == nil {
		return committed, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return readWorktree()
	}
	return nil, fmt.Errorf("read committed %s: %w", path, err)
}

func TestRepositoryGuidanceBytesUsesTheRightSourceOfTruth(t *testing.T) {
	repo := t.TempDir()
	path := ClaudeEntrypointPath
	write := func(root, relative, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, relative), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", relative, err)
		}
	}
	runGit := func(args ...string) {
		t.Helper()
		commandArgs := append([]string{"-C", repo}, args...)
		if output, err := exec.Command("git", commandArgs...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}

	runGit("init", "--quiet")
	write(repo, path, "committed guidance\n")
	runGit("add", path)
	runGit(
		"-c", "user.name=Putnami Tests",
		"-c", "user.email=tests@putnami.dev",
		"-c", "commit.gpgsign=false",
		"commit", "--quiet", "-m", "fixture",
	)
	write(repo, path, "regenerated working-tree guidance\n")

	t.Run("CI reads committed guidance after preparer mutation", func(t *testing.T) {
		t.Setenv("CI", "true")
		got, err := repositoryGuidanceBytes(repo, path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "committed guidance\n" {
			t.Fatalf("guidance = %q, want committed bytes", got)
		}
	})

	t.Run("local gate reads freshly regenerated guidance", func(t *testing.T) {
		t.Setenv("CI", "")
		got, err := repositoryGuidanceBytes(repo, path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "regenerated working-tree guidance\n" {
			t.Fatalf("guidance = %q, want working-tree bytes", got)
		}
	})

	t.Run("source archive falls back to its working tree", func(t *testing.T) {
		t.Setenv("CI", "true")
		archive := t.TempDir()
		write(archive, path, "archive guidance\n")
		got, err := repositoryGuidanceBytes(archive, path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "archive guidance\n" {
			t.Fatalf("guidance = %q, want archive bytes", got)
		}
	})

	t.Run("missing Git executable falls back to the working tree", func(t *testing.T) {
		t.Setenv("CI", "true")
		t.Setenv("PATH", "")
		got, err := repositoryGuidanceBytes(repo, path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "regenerated working-tree guidance\n" {
			t.Fatalf("guidance = %q, want working-tree bytes without Git", got)
		}
	})

	t.Run("Git repository failures are not hidden", func(t *testing.T) {
		t.Setenv("CI", "true")
		write(repo, agentEntrypointPath, "untracked guidance\n")
		if _, err := repositoryGuidanceBytes(repo, agentEntrypointPath); err == nil {
			t.Fatal("repositoryGuidanceBytes succeeded for an untracked guidance file")
		}
	})
}

// The ratchet: CLAUDE.md and AGENTS.md are generated, so the
// committed bytes must equal what the generator produces for this workspace.
// Without this test, a hand edit to either file fails only when CI's install
// stage compares the worktree against a fresh
// `putnami context generate`. This test moves that detection into
// `putnami test`, and it fails the moment the workspace's gate declaration
// (the blocking commands of its CI document when one is usable, otherwise
// lint, test and build) and the committed guidance stop agreeing in either
// direction.
func TestGeneratedGuidanceMatchesRepositoryCIDocument(t *testing.T) {
	root := repositoryRootForAIContext(t)
	gate := gateTasksForWorkspace(root)
	if gate == defaultGate() {
		t.Fatalf("this workspace derives only the default %q, so the check is vacuous; its CI document declares the validate task", gate)
	}
	if exported := GateTasks(root); exported != gate {
		t.Fatalf("GateTasks(%s) = %q, want %q", root, exported, gate)
	}

	// Putnami owns one block inside each entrypoint, so that block is what is
	// compared: the repository may keep its own text around it. CLAUDE.md may
	// instead import AGENTS.md, which then carries the block for both.
	want := guidanceBlock(gate, decisionsGuidanceForWorkspace(root))
	for _, path := range []string{agentEntrypointPath, ClaudeEntrypointPath} {
		got, readErr := repositoryGuidanceBytes(root, path)
		if readErr != nil {
			t.Fatalf("read repository %s: %v", path, readErr)
		}
		content := string(got)
		if path == ClaudeEntrypointPath && importsAgentsEntrypoint(content) {
			continue
		}
		start, end, intact, found := findGuidanceBlock(content)
		if !found || !intact || end < 0 || content[start:end] != want {
			t.Errorf("committed %s does not carry the block `putnami context generate` produces; run it and commit the result.\n--- committed ---\n%s\n--- generated block ---\n%s", path, got, want)
		}
	}
}
