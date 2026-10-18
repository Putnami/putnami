package agentctx

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

// The build skill publishes with `putnami change finalize`, which the
// installed @putnami/contributor serves through an interactive launch, where
// it used to run the core's `putnami proposals finalize`. Each case runs the
// core command in one workspace and the extension command in a twin built
// the same way: the local provider bound, a local bare repository as origin,
// and the same task in the same store. The two runs exit with the same code,
// print the same stdout and the same message, leave the same branch head,
// push the same refs, and write the same proposal and task records. A run that
// pushes nothing writes no record. Only the message prefix, putnami's warning
// about an undeclared flag and the store's timestamps differ.
func TestChangeFinalizeMatchesProposalsFinalize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness launcher requires bash")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("finalize requires git: %v", err)
	}
	spectest.Proves(t, "cli/contributor-workflows", "change-finalize-parity", "change-finalize-matches-the-core-command")
	harness := collaborationHarness(t)
	publish := []string{"--base", "main", "--branch", "topic", "--title", "feat(lib): read the second line",
		"--body-file", "{body}", "--task-source", "{source}", "--task-id", "T-1"}
	for _, tc := range []struct {
		name string
		// change is the file the branch commit edits; without one, the branch
		// has no commit ahead of main.
		change string
		args   []string
		exit   int
		// message is the core's message without its prefix. With prefix, the
		// message starts with it.
		message string
		prefix  bool
		pushed  bool
		// undeclared is a flag the manifest does not declare: putnami warns
		// about it before it launches the executable, and the core does not.
		undeclared string
	}{
		{name: "a draft seed", args: append(publish[:len(publish):len(publish)], "--draft"), pushed: true},
		{name: "a draft seed with JSON output", args: append(publish[:len(publish):len(publish)], "--draft", "--output=json"), pushed: true},
		{name: "a hosted partial delivery of a featureless project", change: "lib/source.txt",
			args: append(publish[:len(publish):len(publish)], "--gate", "ci", "--partial-delivery"),
			exit: 1, message: "hosted checks unsupported; proposal returned to draft", pushed: true},
		{name: "a hosted partial delivery of a feature-authoring project", change: "app/source.txt",
			args: append(publish[:len(publish):len(publish)], "--gate", "ci", "--partial-delivery"),
			exit: 1, message: "--gate ci with a parent task requires --workflow-file; projects without featureAuthority.none or with authored features: /app"},
		{name: "a usage error", change: "lib/source.txt", args: append(publish[:len(publish):len(publish)], "--gate", "local"),
			exit: 2, message: "--gate accepts only ci; local proof uses --verification-file"},
		{name: "an undeclared flag", change: "lib/source.txt", args: append(publish[:len(publish):len(publish)], "--squash"),
			exit: 2, message: `unexpected finalize argument "--squash"`, undeclared: "--squash"},
		// The dossier is JSON but no dossier: the verifier of each command
		// refuses it with the same not-verified document, which both quote.
		{name: "a dossier that does not verify", change: "lib/source.txt",
			args: append(publish[:len(publish):len(publish)], "--verification-file", finalizeTwinDossier),
			exit: 1, message: `native dossier verification: {"verdict":"not-verified","reason":`, prefix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := newFinalizeTwin(t, harness, tc.change)
			source := core.createTask(t)
			native := newFinalizeTwin(t, harness, tc.change)
			native.adoptStore(t, core)
			seeded := core.store(t)

			coreRun := core.putnami(t, append([]string{"proposals", "finalize"}, core.expand(tc.args, source)...)...)
			nativeRun := native.putnami(t, append([]string{"change", "finalize"}, native.expand(tc.args, source)...)...)
			coreMessage := core.message(t, coreRun.stderr, "putnami: proposals finalize: ")
			nativeStderr := nativeRun.stderr
			if tc.undeclared != "" {
				warning := "putnami: flag " + tc.undeclared + " is not declared by `change finalize`; "
				line, rest, _ := strings.Cut(nativeStderr, "\n")
				if !strings.HasPrefix(line, warning) {
					t.Fatalf("change finalize wrote %q to stderr, want a first line starting with %q", nativeStderr, warning)
				}
				nativeStderr = rest
			}
			nativeMessage := native.message(t, nativeStderr, "change finalize: ")
			if coreRun.code != tc.exit || !tc.prefix && coreMessage != tc.message || tc.prefix && !strings.HasPrefix(coreMessage, tc.message) {
				t.Fatalf("proposals finalize exited %d with stderr %q; the case expects %d and %q",
					coreRun.code, coreRun.stderr, tc.exit, tc.message)
			}
			if nativeRun.code != coreRun.code || nativeRun.stdout != coreRun.stdout || nativeMessage != coreMessage {
				t.Errorf("change finalize exited %d with stdout %q and stderr %q;\nproposals finalize exited %d with stdout %q and stderr %q",
					nativeRun.code, nativeRun.stdout, nativeRun.stderr, coreRun.code, coreRun.stdout, coreRun.stderr)
			}
			if tc.exit == 0 && !strings.HasPrefix(coreRun.stdout, "PROPOSAL_REF=") && !strings.HasPrefix(coreRun.stdout, "{") {
				t.Errorf("proposals finalize printed %q, want the proposal reference", coreRun.stdout)
			}
			coreRefs, nativeRefs := core.pushed(t), native.pushed(t)
			if nativeRefs != coreRefs {
				t.Errorf("change finalize pushed\n%s\nproposals finalize pushed\n%s", nativeRefs, coreRefs)
			}
			if got := strings.Contains(coreRefs, "refs/heads/topic "); got != tc.pushed {
				t.Errorf("proposals finalize pushed topic: %t, the case expects %t:\n%s", got, tc.pushed, coreRefs)
			}
			if coreHead, nativeHead := core.git(t, "rev-parse", "HEAD"), native.git(t, "rev-parse", "HEAD"); nativeHead != coreHead {
				t.Errorf("change finalize left HEAD at %s, proposals finalize at %s", nativeHead, coreHead)
			}
			coreStore, nativeStore := core.store(t), native.store(t)
			if nativeStore != coreStore {
				t.Errorf("change finalize stored\n%s\nproposals finalize stored\n%s", nativeStore, coreStore)
			}
			if !tc.pushed && coreStore != seeded {
				t.Errorf("proposals finalize pushed nothing and still wrote the store:\n%s", coreStore)
			}
		})
	}
}

// finalizeTwin is a workspace on the branch topic, with the local provider
// and the contributor installed from the harness and a local bare
// repository as origin. Its main branch holds a featureless project, lib,
// and a project that holds requirements, app.
type finalizeTwin struct {
	harness, root, origin, body string
	env                         []string
}

// finalizeTwinStore is the local provider's store below the workspace root.
const finalizeTwinStore = ".putnami/collaboration/local/state.json"

// finalizeTwinDossier is a JSON document that is not a dossier, in an
// ignored directory of the workspace, so the tree stays clean.
const finalizeTwinDossier = ".context/dossier.json"

func newFinalizeTwin(t *testing.T, harness, change string) finalizeTwin {
	t.Helper()
	dir := t.TempDir()
	twin := finalizeTwin{harness: harness, root: filepath.Join(dir, "ws"), origin: filepath.Join(dir, "origin.git"),
		body: filepath.Join(dir, "body.md")}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// Every commit, the draft seed included, has the same identity and date in
	// both twins, so both push the same objects. The empty home keeps the
	// user's Git configuration, such as commit signing, out of the run.
	twin.env = append(os.Environ(), "PATH="+harness+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Finalize Test", "GIT_AUTHOR_EMAIL=finalize-test@example.com",
		"GIT_COMMITTER_NAME=Finalize Test", "GIT_COMMITTER_EMAIL=finalize-test@example.com",
		"GIT_AUTHOR_DATE=2026-01-02T03:04:05Z", "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z")
	binding := collab.Binding{Provider: "@putnami/local-collaboration", Version: 1,
		Settings: json.RawMessage(`{"repository":"acme/app"}`)}
	config, err := json.Marshal(map[string]any{
		"name": "finalize-fixture",
		"extensions": []string{filepath.ToSlash(filepath.Join(harness, "local-collaboration")),
			filepath.ToSlash(filepath.Join(harness, "contributor"))},
		"includes": []string{"lib", "app"},
		"options": map[string]any{
			"collaboration": collab.Bindings{collab.ContractTasks: binding, collab.ContractProposals: binding},
			"@putnami/contributor": map[string]any{"version": 1,
				"verification": map[string]any{"ciGate": map[string]any{"checks": []string{"Putnami CI"}, "pollSeconds": 1}},
				"publication":  map[string]any{"draft": false}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writePathFile(t, twin.root, "putnami.workspace.json", string(config)+"\n")
	writePathFile(t, twin.root, ".gitignore", ".putnami/\n.context/\n")
	writePathFile(t, twin.root, finalizeTwinDossier, `{"version":2}`+"\n")
	writePathFile(t, twin.root, "lib/putnami.json", `{"name":"lib","featureAuthority":{"none":"Fixture plumbing with no user-facing feature."}}`+"\n")
	writePathFile(t, twin.root, "lib/source.txt", "first\n")
	writePathFile(t, twin.root, "app/putnami.json", `{"name":"app"}`+"\n")
	writePathFile(t, twin.root, "app/source.txt", "first\n")
	if err := os.WriteFile(twin.body, []byte("Read the second line of the fixture.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	twin.run(t, dir, "git", "init", "-q", "--bare", twin.origin)
	twin.git(t, "init", "-q", "-b", "main")
	twin.git(t, "config", "commit.gpgSign", "false")
	twin.git(t, "remote", "add", "origin", twin.origin)
	twin.git(t, "add", "-A")
	twin.git(t, "commit", "-q", "-m", "chore: add the fixture")
	twin.git(t, "push", "-q", "origin", "main")
	twin.git(t, "checkout", "-q", "-b", "topic")
	if change != "" {
		writePathFile(t, twin.root, change, "second\n")
		twin.git(t, "commit", "-q", "-am", "feat: read the second line")
	}
	return twin
}

// run runs a command in dir and fails the test when it fails.
func (w finalizeTwin) run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	command.Env = w.env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (w finalizeTwin) git(t *testing.T, args ...string) string {
	t.Helper()
	return w.run(t, w.root, "git", args...)
}

// createTask creates the task T-1 in the store and returns the store's
// reference source.
func (w finalizeTwin) createTask(t *testing.T) string {
	t.Helper()
	output := w.run(t, w.root, filepath.Join(w.harness, "putnami"), "tasks", "create",
		"--input", `{"title":"Read the second line","idempotencyKey":"finalize-fixture"}`, "--output=json")
	var envelope struct {
		Outcome collab.Outcome          `json:"outcome"`
		Result  collab.TaskCreateResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil || envelope.Outcome != collab.OutcomeOK ||
		envelope.Result.Task.Ref.ID != "T-1" {
		t.Fatalf("tasks create answered %s: %v", output, err)
	}
	return envelope.Result.Task.Ref.Source
}

// adoptStore copies the store of other, so both stores issue the same
// references.
func (w finalizeTwin) adoptStore(t *testing.T, other finalizeTwin) {
	t.Helper()
	state, err := os.ReadFile(filepath.Join(other.root, filepath.FromSlash(finalizeTwinStore)))
	if err != nil {
		t.Fatal(err)
	}
	writePathFile(t, w.root, finalizeTwinStore, string(state))
}

// expand replaces the {body} and {source} placeholders of args.
func (w finalizeTwin) expand(args []string, source string) []string {
	expanded := make([]string, len(args))
	for i, arg := range args {
		expanded[i] = strings.NewReplacer("{body}", w.body, "{source}", source).Replace(arg)
	}
	return expanded
}

type finalizeRun struct {
	stdout, stderr string
	code           int
}

func (w finalizeTwin) putnami(t *testing.T, args ...string) finalizeRun {
	t.Helper()
	command := exec.Command(filepath.Join(w.harness, "putnami"), args...)
	command.Dir = w.root
	command.Env = w.env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	run := finalizeRun{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		run.code = exit.ExitCode()
	case err != nil:
		t.Fatalf("putnami %v did not run: %v", args, err)
	}
	return run
}

// message is stderr without the command's prefix, with the twin's own paths
// replaced. It is empty when stderr is.
func (w finalizeTwin) message(t *testing.T, stderr, prefix string) string {
	t.Helper()
	if stderr == "" {
		return ""
	}
	text, found := strings.CutPrefix(strings.TrimSuffix(stderr, "\n"), prefix)
	if !found || strings.Contains(text, "\n") {
		t.Fatalf("stderr %q is not one line starting with %q", stderr, prefix)
	}
	return strings.NewReplacer(w.root, "<root>", w.origin, "<origin>", w.body, "<body>").Replace(text)
}

// pushed lists the origin's refs and the objects they name.
func (w finalizeTwin) pushed(t *testing.T) string {
	t.Helper()
	return w.run(t, w.root, "git", "--git-dir", w.origin, "for-each-ref", "--format=%(refname) %(objectname)")
}

// store is the provider's state with every timestamp replaced, in a
// canonical encoding.
func (w finalizeTwin) store(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(w.root, filepath.FromSlash(finalizeTwinStore)))
	if err != nil {
		t.Fatal(err)
	}
	var state any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("the store is not JSON: %v\n%s", err, data)
	}
	var redact func(value any)
	redact = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "createdAt" || key == "updatedAt" {
					typed[key] = "<timestamp>"
					continue
				}
				redact(child)
			}
		case []any:
			for _, child := range typed {
				redact(child)
			}
		}
	}
	redact(state)
	canonical, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(canonical)
}
