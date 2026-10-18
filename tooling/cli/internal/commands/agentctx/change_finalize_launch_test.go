package agentctx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

// recordFinalizeEnv names the putnami binary a recording run takes the
// goldens from. The current core refuses `proposals finalize`, so the binary
// is a putnami built from a commit before the core deleted it. When it is set,
// TestChangeFinalizeMatchesProposalsFinalize runs `<binary> proposals
// finalize` for every case and writes testdata/change-finalize/<case>.golden
// instead of comparing. A binary that refuses the command fails the run.
const recordFinalizeEnv = "PUTNAMI_CLI_RECORD_PROPOSALS_FINALIZE"

// The build skill publishes with `putnami change finalize`, which the
// installed @putnami/contributor serves through an interactive launch. Each
// case runs it in a workspace with the local provider bound, a local bare
// repository as origin and the task T-1 in the store, and compares what the
// run leaves with testdata/change-finalize/<case>.golden: the exit code,
// stdout, the message, the branch head, the refs the origin holds and the
// proposal and task records. The goldens hold what `putnami proposals
// finalize`, the core command change finalize replaces, left in the same
// workspace. Only the message prefix, putnami's warning about an undeclared
// flag, the store's timestamps and random identity, and the run's own paths
// in the message are not compared. A run that pushes nothing writes no record.
func TestChangeFinalizeMatchesProposalsFinalize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness launcher requires bash")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("finalize requires git: %v", err)
	}
	spectest.Proves(t, "cli/contributor-workflows", "change-finalize-parity", "change-finalize-matches-the-core-command")
	harness := collaborationHarness(t)
	recorder := ""
	if binary := os.Getenv(recordFinalizeEnv); binary != "" {
		recorder = finalizeRecorder(t, harness, binary)
	}
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
		// about it before it launches the executable, and the core did not.
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
		// The dossier is JSON but no dossier: the verifier refuses it with a
		// not-verified document, which the message quotes.
		{name: "a dossier that does not verify", change: "lib/source.txt",
			args: append(publish[:len(publish):len(publish)], "--verification-file", finalizeTwinDossier),
			exit: 1, message: `native dossier verification: {"verdict":"not-verified","reason":`, prefix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			twin := newFinalizeTwin(t, harness, tc.change)
			source := twin.createTask(t)
			args := twin.expand(tc.args, source)
			seeded := twin.store(t)
			golden := filepath.Join("testdata", "change-finalize", strings.ReplaceAll(tc.name, " ", "-")+".golden")

			var got finalizeOutcome
			if recorder != "" {
				run := twin.launch(t, recorder, append([]string{"proposals", "finalize"}, args...)...)
				if strings.Contains(run.stderr, "`putnami proposals finalize` is no longer a core command") {
					t.Fatalf("%s refuses proposals finalize; record with a putnami built before the core deleted it:\n%s",
						os.Getenv(recordFinalizeEnv), run.stderr)
				}
				got = twin.outcome(t, run, run.stderr, "putnami: proposals finalize: ")
			} else {
				run := twin.launch(t, filepath.Join(harness, "putnami"), append([]string{"change", "finalize"}, args...)...)
				stderr := run.stderr
				if tc.undeclared != "" {
					warning := "putnami: flag " + tc.undeclared + " is not declared by `change finalize`; "
					line, rest, _ := strings.Cut(stderr, "\n")
					if !strings.HasPrefix(line, warning) {
						t.Fatalf("change finalize wrote %q to stderr, want a first line starting with %q", stderr, warning)
					}
					stderr = rest
				}
				got = twin.outcome(t, run, stderr, "change finalize: ")
			}

			if got.exit != tc.exit || !tc.prefix && got.message != tc.message || tc.prefix && !strings.HasPrefix(got.message, tc.message) {
				t.Fatalf("the run exited %d with message %q; the case expects %d and %q", got.exit, got.message, tc.exit, tc.message)
			}
			if tc.exit == 0 && !strings.HasPrefix(got.stdout, "PROPOSAL_REF=") && !strings.HasPrefix(got.stdout, "{") {
				t.Errorf("the run printed %q, want the proposal reference", got.stdout)
			}
			if pushed := strings.Contains(got.pushed, "refs/heads/topic "); pushed != tc.pushed {
				t.Errorf("the run pushed topic: %t, the case expects %t:\n%s", pushed, tc.pushed, got.pushed)
			}
			if !tc.pushed && got.store != seeded {
				t.Errorf("the run pushed nothing and still wrote the store:\n%s", got.store)
			}
			if recorder != "" {
				writePathFile(t, ".", golden, got.render(source))
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read the golden of proposals finalize: %v", err)
			}
			if rendered := got.render(source); rendered != string(want) {
				t.Errorf("change finalize differs from the golden of proposals finalize\n--- got\n%s\n--- want\n%s", rendered, want)
			}
		})
	}
}

// finalizeRecorder writes the harness launcher around binary, so a recording
// run starts it as the harness starts its own CLI.
func finalizeRecorder(t *testing.T, harness, binary string) string {
	t.Helper()
	launcher := strings.ReplaceAll(collaborationLauncher, "@HOME@", shellQuote(filepath.Join(harness, "home")))
	launcher = strings.ReplaceAll(launcher, "@BINARY@", shellQuote(binary))
	path := filepath.Join(t.TempDir(), "putnami")
	if err := os.WriteFile(path, []byte(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// finalizeTwin is a workspace on the branch topic, with the local provider
// and the contributor installed from the harness and a local bare repository
// as origin. Its main branch holds a featureless project, lib, and a project
// that holds requirements, app. Every committed file has the same bytes on
// every machine, so every run pushes the same objects.
type finalizeTwin struct {
	harness, root, origin, body string
	env                         []string
}

// finalizeTwinStore is the local provider's store below the workspace root.
const finalizeTwinStore = ".putnami/collaboration/local/state.json"

// finalizeTwinDossier is a JSON document that is not a dossier, in an
// ignored directory of the workspace, so the tree stays clean.
const finalizeTwinDossier = ".context/dossier.json"

// finalizeTwinExtensions is the ignored directory that holds the harness
// manifests, so the committed workspace file names no path of the machine.
const finalizeTwinExtensions = ".harness"

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
	// every run, so every run pushes the same objects. The empty home keeps
	// the user's Git configuration, such as commit signing, out of the run.
	twin.env = append(os.Environ(), "PATH="+harness+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Finalize Test", "GIT_AUTHOR_EMAIL=finalize-test@example.com",
		"GIT_COMMITTER_NAME=Finalize Test", "GIT_COMMITTER_EMAIL=finalize-test@example.com",
		"GIT_AUTHOR_DATE=2026-01-02T03:04:05Z", "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z")
	binding := collab.Binding{Provider: "@putnami/local-collaboration", Version: 1,
		Settings: json.RawMessage(`{"repository":"acme/app"}`)}
	var extensions []string
	for _, name := range []string{"local-collaboration", "contributor"} {
		manifest, err := os.ReadFile(filepath.Join(harness, name, "putnami.extension.json"))
		if err != nil {
			t.Fatal(err)
		}
		writePathFile(t, twin.root, finalizeTwinExtensions+"/"+name+"/putnami.extension.json", string(manifest))
		extensions = append(extensions, "/"+finalizeTwinExtensions+"/"+name)
	}
	workspace := finalizeTwinWorkspace{Name: "finalize-fixture", Extensions: extensions, Includes: []string{"lib", "app"}}
	workspace.Options.Collaboration = collab.Bindings{collab.ContractTasks: binding, collab.ContractProposals: binding}
	workspace.Options.Contributor = json.RawMessage(`{"version":1,` +
		`"verification":{"ciGate":{"checks":["Putnami CI"],"pollSeconds":1}},"publication":{"draft":false}}`)
	config, err := json.Marshal(workspace)
	if err != nil {
		t.Fatal(err)
	}
	writePathFile(t, twin.root, "putnami.workspace.json", string(config)+"\n")
	writePathFile(t, twin.root, ".gitignore", ".putnami/\n.context/\n"+finalizeTwinExtensions+"/\n")
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

// launch runs the putnami program in the workspace root.
func (w finalizeTwin) launch(t *testing.T, program string, args ...string) finalizeRun {
	t.Helper()
	command := exec.Command(program, args...)
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
		t.Fatalf("%s %v did not run: %v", program, args, err)
	}
	return run
}

// finalizeOutcome is what a finalize run leaves behind, as a golden holds it.
type finalizeOutcome struct {
	exit                                 int
	stdout, message, head, pushed, store string
}

// outcome reads what run left in the twin. stderr is the run's stderr
// without any line putnami writes before the launch, and prefix the
// command's message prefix.
func (w finalizeTwin) outcome(t *testing.T, run finalizeRun, stderr, prefix string) finalizeOutcome {
	t.Helper()
	return finalizeOutcome{exit: run.code, stdout: run.stdout, message: w.message(t, stderr, prefix),
		head: w.git(t, "rev-parse", "HEAD"), pushed: w.pushed(t), store: w.store(t)}
}

// render is the golden text of the outcome. The store draws its identity at
// random when it is created, and every reference it issues carries it in its
// source, local:<identity>; render names it <store>.
func (o finalizeOutcome) render(source string) string {
	identity := strings.TrimPrefix(source, "local:")
	return strings.NewReplacer(source, "local:<store>", `"storeId": "`+identity+`"`, `"storeId": "<store>"`).Replace(
		fmt.Sprintf("exit %d\n-- stdout --\n%s-- message --\n%s\n-- head --\n%s\n-- pushed --\n%s\n-- store --\n%s\n",
			o.exit, o.stdout, o.message, o.head, o.pushed, o.store))
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
	var canonical bytes.Buffer
	if err := json.Indent(&canonical, finalizeTwinTimestamp.ReplaceAll(data, []byte(`"$1":"<timestamp>"`)), "", "  "); err != nil {
		t.Fatalf("the store is not JSON: %v\n%s", err, data)
	}
	return canonical.String()
}

// finalizeTwinTimestamp matches a createdAt or updatedAt member of the store.
var finalizeTwinTimestamp = regexp.MustCompile(`"(createdAt|updatedAt)":\s*"[^"]*"`)

// finalizeTwinWorkspace is the putnami.workspace.json of a twin.
type finalizeTwinWorkspace struct {
	Name       string   `json:"name"`
	Extensions []string `json:"extensions"`
	Includes   []string `json:"includes"`
	Options    struct {
		Collaboration collab.Bindings `json:"collaboration"`
		Contributor   json.RawMessage `json:"@putnami/contributor"`
	} `json:"options"`
}
