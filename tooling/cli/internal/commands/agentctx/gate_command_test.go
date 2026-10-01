package agentctx

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
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

// A workspace with no CI document must receive exactly the line it received
// before the gate became derived. This is the only place the generic value is
// pinned as a literal, and it is the byte guard against a derivation that
// quietly rewrites every consumer workspace's guidance.
func TestGateTasksFallsBackWithoutCIDocument(t *testing.T) {
	if got := gateTasksForWorkspace(t.TempDir()); got != "lint,test,build" {
		t.Fatalf("gate without a CI document = %q, want %q", got, "lint,test,build")
	}
	if defaultGateTasks != "lint,test,build" {
		t.Fatalf("defaultGateTasks = %q, want the historical generic gate", defaultGateTasks)
	}
}

// writeWorkspaceExtensions writes a minimal putnami.workspace.json declaring
// the given extensions, and a manifest for every local path among them, so a
// fixture can state exactly the declaration the default gate reads.
func writeWorkspaceExtensions(t *testing.T, dir string, extensions []string, manifestNames map[string]string) {
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
	for path, name := range manifestNames {
		manifestDir := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(manifestDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", manifestDir, err)
		}
		manifest := []byte(`{"name":` + strconv.Quote(name) + `,"version":"0.0.0"}`)
		if err := os.WriteFile(filepath.Join(manifestDir, "putnami.extension.json"), manifest, 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
}

// A workspace without a CI document is told the gate Putnami Cloud's native
// runner actually executes: the generic trio, plus `validate` exactly when the
// workspace declares the SDD extension that owns it — by name or as a local
// path whose manifest carries that name.
func TestGateTasksWithoutCIDocumentFollowsTheSDDDeclaration(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-agent-guidance", "the-default-gate-follows-the-sdd-declaration")
	cases := []struct {
		name       string
		extensions []string
		manifests  map[string]string
		want       string
	}{
		{"no workspace file", nil, nil, defaultGateTasks},
		{"no sdd", []string{"@putnami/typescript", "/go/extension"}, map[string]string{"/go/extension": "@putnami/go"}, defaultGateTasks},
		{"sdd by name", []string{"@putnami/typescript", "@putnami/sdd"}, nil, sddDefaultGateTasks},
		{"sdd by local path", []string{"/go/extension", "/tooling/sdd-extension"}, map[string]string{"/go/extension": "@putnami/go", "/tooling/sdd-extension": "@putnami/sdd"}, sddDefaultGateTasks},
		{"local path without manifest", []string{"/tooling/sdd-extension"}, nil, defaultGateTasks},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.extensions != nil {
				writeWorkspaceExtensions(t, dir, tc.extensions, tc.manifests)
			}
			if got := gateTasksForWorkspace(dir); got != tc.want {
				t.Fatalf("gate = %q, want %q", got, tc.want)
			}
			if got := GateTasks(dir); got != tc.want {
				t.Fatalf("GateTasks = %q, want the same derivation %q", got, tc.want)
			}
		})
	}

	// A usable document stays authoritative over the declaration: a document
	// that names only the generic trio runs only the generic trio.
	t.Run("document wins over the declaration", func(t *testing.T) {
		dir := t.TempDir()
		writeWorkspaceExtensions(t, dir, []string{"@putnami/sdd"}, nil)
		writeCIDocument(t, dir, "build", "lint", "test")
		if got := gateTasksForWorkspace(dir); got != defaultGateTasks {
			t.Fatalf("gate = %q, want the document's %q", got, defaultGateTasks)
		}
	})

	// An unusable document falls back to the declaration-derived default, not
	// to the generic trio.
	t.Run("unusable document falls back to the declaration", func(t *testing.T) {
		dir := t.TempDir()
		writeWorkspaceExtensions(t, dir, []string{"@putnami/sdd"}, nil)
		if err := os.WriteFile(filepath.Join(dir, ciproto.Filename), []byte(`{"version":3,"commands":[]}`), 0o644); err != nil {
			t.Fatalf("write document: %v", err)
		}
		if got := gateTasksForWorkspace(dir); got != sddDefaultGateTasks {
			t.Fatalf("gate = %q, want %q", got, sddDefaultGateTasks)
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
			if got := gateTasksForWorkspace(dir); got != defaultGateTasks {
				t.Fatalf("gate = %q, want the fallback %q", got, defaultGateTasks)
			}
		})
	}

	t.Run("empty commands", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ciproto.Filename), []byte(`{"version":3,"commands":[]}`), 0o644); err != nil {
			t.Fatalf("write document: %v", err)
		}
		if got := gateTasksForWorkspace(dir); got != defaultGateTasks {
			t.Fatalf("gate = %q, want the fallback %q for an empty command list", got, defaultGateTasks)
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
		if got := gateTasksForWorkspace(dir); got != defaultGateTasks {
			t.Fatalf("gate = %q, want the fallback %q", got, defaultGateTasks)
		}
	})

	t.Run("no command at all", func(t *testing.T) {
		if got := orderGateTasks(nil); got != "" {
			t.Fatalf("gate = %q, want empty so the caller falls back", got)
		}
	})
}

// The derived gate names every task the document declares, in the canonical
// order, whatever order the document was authored in.
func TestGateTasksDerivesFromDocumentGate(t *testing.T) {
	dir := t.TempDir()
	writeCIDocument(t, dir, "validate-workspace", "build", "validate", "test", "lint")

	const want = "lint,test,build,validate,validate-workspace"
	if got := gateTasksForWorkspace(dir); got != want {
		t.Fatalf("derived gate = %q, want %q", got, want)
	}
}

// A gate that declares only the conventional trio must render the same string
// as the fallback, so adopting `putnami ci init` does not churn the generated
// guidance.
func TestGateTasksForDefaultDocumentMatchesFallback(t *testing.T) {
	if got := orderGateTasks(blockingCommandNames(ciproto.DefaultDocument())); got != defaultGateTasks {
		t.Fatalf("gate for the scaffolded document = %q, want %q", got, defaultGateTasks)
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
	if got := blockingCommandNames(document); !reflect.DeepEqual(got, []string{"lint", "test"}) {
		t.Fatalf("blocking commands = %v, want [lint test]", got)
	}
	if got := orderGateTasks(blockingCommandNames(document)); got != "lint,test" {
		t.Fatalf("gate = %q, want %q", got, "lint,test")
	}
}

// orderGateTasks owns the ordering contract, so pin it directly.
func TestOrderGateTasks(t *testing.T) {
	cases := []struct {
		name  string
		tasks []string
		want  string
	}{
		{"canonical prefix is reordered", []string{"build", "lint", "test"}, "lint,test,build"},
		{"tail is sorted", []string{"validate-workspace", "test", "audit", "lint", "build", "validate"}, "lint,test,build,audit,validate,validate-workspace"},
		{"missing prefix members are skipped", []string{"validate", "build"}, "build,validate"},
		{"duplicates collapse", []string{"lint", "lint", "build"}, "lint,build"},
		{"blank entries are dropped", []string{"lint", " ", ""}, "lint"},
		{"no task yields no command", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orderGateTasks(tc.tasks); got != tc.want {
				t.Fatalf("orderGateTasks(%v) = %q, want %q", tc.tasks, got, tc.want)
			}
		})
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

	const gate = "lint,test,build,validate,validate-workspace"
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
		if strings.Contains(body, "putnami "+defaultGateTasks+" -") || strings.Contains(body, "putnami "+defaultGateTasks+"`") {
			t.Errorf("%s still names the hardcoded gate %q", path, defaultGateTasks)
		}
	}
}

// Regeneration must be a pure function of the workspace, or CI's install stage
// fails with "worktree mutated" on a clean checkout.
func TestWriteAgentEntrypointsIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeCIDocument(t, dir, "build", "lint", "test", "validate")

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
// (the blocking commands of its CI document when one exists, otherwise its SDD
// extension declaration) and the committed guidance stop agreeing in either
// direction.
func TestGeneratedGuidanceMatchesRepositoryCIDocument(t *testing.T) {
	root := repositoryRootForAIContext(t)
	gate := gateTasksForWorkspace(root)
	if gate == defaultGateTasks {
		t.Fatalf("this workspace derives only %q, so the check is vacuous; it declares @putnami/sdd and should carry the validate task", gate)
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
