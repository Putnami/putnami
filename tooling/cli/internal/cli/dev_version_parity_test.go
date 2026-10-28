package cli

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
	"slices"
	"strconv"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
)

// The acceptance of moving `putnami version get` and `putnami version tag`
// to @putnami/dev: on the same fixture, the extension's executable, launched
// through the core's dispatch of an interactive extension subcommand with the
// shipped manifest, answers what the core's built-in commands answered. The
// launch is the real one: the job context, the host RPC channel the command
// reads the version lines through, and the subprocess. Only the runtime's
// location is the test's, a binary built from tooling/dev.
//
// # The oracle is recorded
//
// testdata/dev-version-parity holds one answer of the core per case: the exit
// code, stdout, stderr, and the state of the repository after the run.
// TestRecordDevVersionParity wrote them with a putnami binary built from the
// tree whose core still served both commands. A later binary has no such
// command, so it cannot record them again: it answers that the command is
// unknown, and the comparison then fails.
//
// # What differs, by decision
//
// A recording is compared after these substitutions, and only these:
//   - Each stderr line loses the "putnami: " prefix of a core error.
//   - `version tag` under structured output no longer lists the core's
//     structured-output commands after its refusal.
//   - A usage error exits 2 where the core exited 1 (devVersionCase.usage).
//
// Both sides normalize what depends on the moment a case runs: the release
// date of a changelog, the release commit's ID that git prints, and the
// fixture's directory. Every other byte is compared verbatim, version strings
// and fixture commit IDs included: the fixture's commits carry fixed
// identities and dates.

// devVersionCase is one invocation, compared against its recorded answer.
type devVersionCase struct {
	// name identifies the subtest and, through paritySlug, the recording.
	name string
	// args is the whole CLI invocation.
	args []string
	// before prepares the fixture, whose root it receives, ahead of the run.
	before func(t *testing.T, root string)
	// env returns the variables the run adds to its environment.
	env func(t *testing.T, root string) []string
	// usage marks an argument error: the core exited 1, the extension exits 2.
	usage bool
	// wantExit is the exit code the extension must return. It is stated so
	// a recording edited into another verdict shows here.
	wantExit int
}

func devVersionCases() []devVersionCase {
	return []devVersionCase{
		{name: "get", args: []string{"version", "get"}},
		{name: "bare version", args: []string{"version"}},
		{name: "get scope", args: []string{"version", "get", "--scope", "tooling"}},
		{name: "get scope with slashes", args: []string{"version", "get", "--scope", "/typescript/"}},
		{name: "get inline scope", args: []string{"version", "get", "--scope=typescript"}},
		{name: "get jsonl", args: []string{"version", "get", "--output=jsonl"}},
		{name: "get json", args: []string{"version", "get", "--output=json"}},
		{name: "get json one line", args: []string{"version", "get", "--scope", "tooling", "--json"}},
		{name: "get cloud-logging", args: []string{"version", "get", "--output=cloud-logging"}},
		{name: "get tagged", args: []string{"version", "get"}, before: tagDevVersionHead},
		{name: "get tagged jsonl", args: []string{"version", "get", "--output=jsonl"}, before: tagDevVersionHead},
		{name: "get bound to another commit", args: []string{"version", "get"}, env: bindDevVersionParent},
		{name: "get unknown scope", args: []string{"version", "get", "--scope", "nope"}, wantExit: 2},
		{name: "get unknown scope json", args: []string{"version", "get", "--scope", "nope", "--output=json"}, wantExit: 2},
		{name: "get extra argument", args: []string{"version", "get", "extra"}, usage: true, wantExit: 2},
		{name: "get scope without value", args: []string{"version", "get", "--scope"}, usage: true, wantExit: 2},
		{name: "tag several lines", args: []string{"version", "tag", "--yes"}, wantExit: 2},
		{name: "tag dry-run", args: []string{"version", "tag", "--scope", "typescript", "--dry-run"}},
		{name: "tag dry-run tooling", args: []string{"version", "tag", "--scope", "tooling", "--dry-run"}},
		{name: "tag explicit", args: []string{"version", "tag", "ts/v1.0.0", "--scope", "typescript", "--dry-run"}},
		{name: "tag off-pattern", args: []string{"version", "tag", "v9", "--scope", "typescript", "--yes"}, wantExit: 2},
		{name: "tag two positionals", args: []string{"version", "tag", "a", "b", "--scope", "tooling"}, usage: true, wantExit: 2},
		{name: "tag without yes", args: []string{"version", "tag", "--scope", "typescript"}},
		{name: "tag tagged head", args: []string{"version", "tag", "--scope", "typescript", "--yes"}, before: tagDevVersionHead, wantExit: 2},
		{name: "tag dirty tree", args: []string{"version", "tag", "--scope", "typescript", "--yes"}, before: dirtyDevVersionTree, wantExit: 2},
		{name: "tag existing tag", args: []string{"version", "tag", "ts/v0.4.0", "--scope", "typescript", "--yes"}, wantExit: 2},
		{name: "tag bound to another commit", args: []string{"version", "tag", "--scope", "tooling", "--yes"}, env: bindDevVersionParent, wantExit: 2},
		{name: "tag structured output", args: []string{"version", "tag", "--scope", "tooling", "--output=json"}, wantExit: 2},
		{name: "tag yes", args: []string{"version", "tag", "--scope", "typescript", "--yes"}},
		{name: "tag yes push", args: []string{"version", "tag", "--scope", "tooling", "--yes", "--push"}, before: addDevVersionOrigin},
	}
}

// TestDevVersionCommandsMatchTheRecordedCore is the acceptance: same fixture,
// same arguments, the same exit code, stdout, stderr and repository the
// core's built-in commands produced, with the documented substitutions.
func TestDevVersionCommandsMatchTheRecordedCore(t *testing.T) {
	t.Parallel()
	runtimePath := devVersionRuntime(t)
	for _, tc := range devVersionCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			limitParityConcurrency(t)
			root := devVersionFixture(t)
			if tc.before != nil {
				tc.before(t, root)
			}
			got := devVersionLaunch(t, root, runtimePath, tc)
			want := devVersionExpected(t, tc)
			if got.exit != want.exit {
				t.Errorf("exit code: extension %d, recorded core %d after the substitutions", got.exit, want.exit)
			}
			for _, part := range []struct{ name, got, want string }{
				{"stdout", got.stdout, want.stdout},
				{"stderr", got.stderr, want.stderr},
				{"repository", got.repository, want.repository},
			} {
				if part.got != part.want {
					t.Errorf("%s differs from the recorded core answer.\n--- recorded ---\n%s\n--- extension ---\n%s", part.name, part.want, part.got)
				}
			}
		})
	}
}

// devVersionExpected is the recorded answer of tc with the documented
// substitutions applied.
func devVersionExpected(t *testing.T, tc devVersionCase) devVersionAnswer {
	t.Helper()
	recorded := readDevVersionAnswer(t, filepath.Join(devVersionRecordingDir(), paritySlug(tc.name)+".txt"))
	if tc.usage {
		if recorded.exit != 1 {
			t.Fatalf("the case is marked as a usage error the core exited 1 for, but the recording exits %d", recorded.exit)
		}
		recorded.exit = 2
	}
	if recorded.exit != tc.wantExit {
		t.Errorf("the recording exits %d after the substitutions, but the case claims %d", recorded.exit, tc.wantExit)
	}
	var stderr []string
	for _, line := range strings.SplitAfter(recorded.stderr, "\n") {
		if strings.HasPrefix(line, "  structured-output commands: ") {
			continue
		}
		stderr = append(stderr, strings.TrimPrefix(line, "putnami: "))
	}
	recorded.stderr = strings.Join(stderr, "")
	return recorded
}

// TestDevVersionRecordingsCoverEveryCaseAndNothingElse keeps the recordings
// and the case table in step: a recording no case reads is a comparison that
// silently stopped.
func TestDevVersionRecordingsCoverEveryCaseAndNothingElse(t *testing.T) {
	t.Parallel()
	expected := map[string]bool{}
	for _, tc := range devVersionCases() {
		expected[paritySlug(tc.name)+".txt"] = true
	}
	if len(expected) != len(devVersionCases()) {
		t.Fatalf("the table names %d cases but only %d distinct recordings: paritySlug collided", len(devVersionCases()), len(expected))
	}
	entries, err := os.ReadDir(devVersionRecordingDir())
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		found = append(found, entry.Name())
		if !expected[entry.Name()] {
			t.Errorf("%s is a recording no case compares against", entry.Name())
		}
	}
	for name := range expected {
		if !slices.Contains(found, name) {
			t.Errorf("%s has no recording", name)
		}
	}
}

// devVersionRecordEnv names the putnami binary TestRecordDevVersionParity
// records the core's answers with.
const devVersionRecordEnv = "PUTNAMI_RECORD_DEV_VERSION_PARITY"

// TestRecordDevVersionParity writes testdata/dev-version-parity: it runs each
// case with the putnami binary devVersionRecordEnv names, which must serve
// `version get` and `version tag` itself, and records its answer. It runs only
// when that variable is set.
func TestRecordDevVersionParity(t *testing.T) {
	t.Parallel()
	core := os.Getenv(devVersionRecordEnv)
	if core == "" {
		t.Skip("recording: " + devVersionRecordEnv + " names no putnami binary")
	}
	if err := os.MkdirAll(devVersionRecordingDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range devVersionCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			limitParityConcurrency(t)
			root := devVersionFixture(t)
			if tc.before != nil {
				tc.before(t, root)
			}
			cmd := exec.CommandContext(t.Context(), core, tc.args...)
			cmd.Dir = root
			cmd.Env = append(devVersionEnv(t, root, tc), "HOME="+t.TempDir())
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			code := 0
			if err := cmd.Run(); err != nil {
				exitErr := &exec.ExitError{}
				if !errors.As(err, &exitErr) {
					t.Fatal(err)
				}
				code = exitErr.ExitCode()
			}
			answer := devVersionAnswer{exit: code, stdout: stdout.String(), stderr: stderr.String(),
				repository: devVersionRepository(t, root)}
			answer = answer.normalized(root)
			path := filepath.Join(devVersionRecordingDir(), paritySlug(tc.name)+".txt")
			if err := os.WriteFile(path, []byte(answer.String()), 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// devVersionRequestEnv carries the launch TestDevVersionParityHelperProcess
// runs, JSON-encoded.
const devVersionRequestEnv = "PUTNAMI_CLI_DEV_VERSION_PARITY"

// devVersionRequest is one launch of the helper process.
type devVersionRequest struct {
	Root    string   `json:"root"`
	Runtime string   `json:"runtime"`
	Args    []string `json:"args"`
	Answer  string   `json:"answer"`
}

// devVersionLaunch runs tc through the extension dispatch in a child of this
// test binary, so the core that answers version.lines reads the case's
// environment, and returns the normalized answer with the repository after
// the run.
func devVersionLaunch(t *testing.T, root, runtimePath string, tc devVersionCase) devVersionAnswer {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(devVersionRequest{Root: root, Runtime: runtimePath, Args: tc.args,
		Answer: filepath.Join(t.TempDir(), "answer.txt")})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestDevVersionParityHelperProcess$")
	cmd.Env = append(devVersionEnv(t, root, tc), devVersionRequestEnv+"="+string(request))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the helper process failed: %v\n%s", err, out)
	}
	var decoded devVersionRequest
	if err := json.Unmarshal(request, &decoded); err != nil {
		t.Fatal(err)
	}
	answer := readDevVersionAnswer(t, decoded.Answer)
	answer.repository = devVersionRepository(t, root)
	return answer.normalized(root)
}

// TestDevVersionParityHelperProcess is the child devVersionLaunch starts: it
// runs the launch devVersionRequestEnv describes and writes the answer.
func TestDevVersionParityHelperProcess(t *testing.T) {
	t.Parallel()
	encoded := os.Getenv(devVersionRequestEnv)
	if encoded == "" {
		t.Skip("helper process: not invoked as a version command launch")
	}
	var request devVersionRequest
	if err := json.Unmarshal([]byte(encoded), &request); err != nil {
		t.Fatal(err)
	}
	// App.Run's dispatch of an extension group: the parse with the group
	// known, the output mode resolved from --output and --json, then the
	// extension command.
	parsed := ParseArgs(request.Args, nil, map[string]bool{request.Args[0]: true})
	if parsed.Err != nil {
		t.Fatalf("the parser rejected %v: %v", request.Args, parsed.Err)
	}
	mode, err := protocolcli.ResolveOutputMode(parsed.Global.Output, parsed.Global.JSON)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Global.Output = string(mode)
	var stdout, stderr bytes.Buffer
	code := (&App{}).runExtensionStructuredCommand(t.Context(), parsed, wsproto.Load(request.Root), request.Root,
		devVersionExtensions(t, request.Root, request.Runtime), os.Stdin, &stdout, &stderr)
	answer := devVersionAnswer{exit: code, stdout: stdout.String(), stderr: stderr.String()}
	if err := os.WriteFile(request.Answer, []byte(answer.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// devVersionEnv is the environment of one run: this process's, without the
// variables that bind a run to a commit, select an output mode, open a host
// channel or point git elsewhere, and with git's global and system
// configuration out of reach.
func devVersionEnv(t *testing.T, root string, tc devVersionCase) []string {
	t.Helper()
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "PUTNAMI_") || strings.HasPrefix(name, "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C", "DO_NOT_TRACK=1")
	if tc.env != nil {
		env = append(env, tc.env(t, root)...)
	}
	return env
}

// devVersionRuntime builds the @putnami/dev executable for the test, as
// tooling/dev/bin/prepare does, and returns its path.
func devVersionRuntime(t *testing.T) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), pkgmeta.ExecutableName(runtime.GOOS, "putnami-dev"))
	cmd := exec.Command("go", "build", "-o", output, "./cmd/putnami-dev")
	cmd.Dir = filepath.Join("..", "..", "..", "dev")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("could not build the @putnami/dev runtime, so parity cannot be measured:\n%s", out)
	}
	return output
}

// devVersionExtensions resolves the shipped @putnami/dev manifest with its
// runtime pointed at the built executable.
func devVersionExtensions(t *testing.T, root, runtimePath string) []*extension.ExtensionDescription {
	t.Helper()
	manifest, err := extension.LoadManifest(filepath.Join("..", "..", "..", "dev", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("the shipped manifest did not load: %v", err)
	}
	description := extension.Resolve(manifest, filepath.Join(root, ".dev-extension"))
	description.RuntimeExecutable = runtimePath
	return []*extension.ExtensionDescription{description}
}

// --- the fixture ------------------------------------------------------------

// devVersionFixture builds the workspace every case runs in, in a fresh
// directory: two version lines, tooling and typescript, a support catalog,
// the release ts/v0.4.0, and commits with fixed identities and dates, so
// every commit ID is the same on every machine. It returns the root.
func devVersionFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspace")
	for _, file := range [][2]string{
		{".gitignore", ".putnami/\n"},
		{"putnami.workspace.json", `{"name":"fixture","includes":["tooling","typescript"]}` + "\n"},
		{"tooling/putnami.json", `{"line":{},"includes":["cli"]}` + "\n"},
		{"tooling/cli/putnami.json", `{"name":"@putnami/cli"}` + "\n"},
		{"typescript/putnami.json", `{"line":{"tag":"ts/v{version}"},"includes":["web","lab"]}` + "\n"},
		{"typescript/web/putnami.json", `{"name":"@putnami/web"}` + "\n"},
		{"typescript/lab/putnami.json", `{"name":"@putnami/lab"}` + "\n"},
		{"putnami.support.json", `{
  "protocolVersion": 1,
  "entries": [
    {"id": "@putnami/cli", "kind": "package", "status": "stable"},
    {"id": "@putnami/lab", "kind": "package", "status": "experimental"},
    {"id": "@putnami/web", "kind": "package", "status": "stable"}
  ]
}
`},
	} {
		writeParityFile(t, root, file[0], file[1])
	}
	devVersionGit(t, root, nil, "init", "-q", "--initial-branch=main")
	for _, setting := range [][2]string{
		{"user.name", "Release Fixture"}, {"user.email", "release@example.invalid"},
		{"commit.gpgsign", "false"}, {"tag.gpgsign", "false"}, {"tag.forceSignAnnotated", "false"},
		{"gc.auto", "0"}, {"maintenance.auto", "false"},
	} {
		devVersionGit(t, root, nil, "config", setting[0], setting[1])
	}
	devVersionCommit(t, root, "2026-01-01T00:00:00Z", "feat: the workspace")
	devVersionGit(t, root, devVersionIdentity("2026-01-02T00:00:00Z"), "tag", "-a", "ts/v0.4.0", "-m", "ts 0.4.0")
	type fixtureCommit struct {
		file, content, date string
		message             []string
	}
	for _, commit := range []fixtureCommit{
		{"typescript/web/src.ts", "export const thing = 1\n", "2026-01-03T00:00:00Z", []string{"feat(web): add the thing"}},
		{"typescript/lab/src.ts", "export const flag = 1\n", "2026-01-04T00:00:00Z", []string{"feat(lab)!: drop the flag"}},
		{"tooling/cli/src.go", "package cli\n", "2026-01-05T00:00:00Z", []string{"fix(cli): stop the leak", "The body explains the leak."}},
		{"README.md", "A readme.\n", "2026-01-06T00:00:00Z", []string{"docs: the readme"}},
	} {
		writeParityFile(t, root, commit.file, commit.content)
		devVersionCommit(t, root, commit.date, commit.message...)
	}
	return root
}

// devVersionIdentity is the fixed author and committer of a fixture commit
// or tag made at date.
func devVersionIdentity(date string) []string {
	return []string{
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid", "GIT_COMMITTER_DATE=" + date,
	}
}

// devVersionCommit commits every change of root at date, with one -m per
// paragraph of message.
func devVersionCommit(t *testing.T, root, date string, message ...string) {
	t.Helper()
	devVersionGit(t, root, nil, "add", "-A")
	args := []string{"commit", "-q"}
	for _, paragraph := range message {
		args = append(args, "-m", paragraph)
	}
	devVersionGit(t, root, devVersionIdentity(date), args...)
}

// devVersionGit runs git in dir without global or system configuration,
// with env added, and returns its output without trailing newlines.
func devVersionGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(git.Executable(), args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C"), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimRight(string(out), "\n")
}

// tagDevVersionHead releases the typescript line at HEAD.
func tagDevVersionHead(t *testing.T, root string) {
	t.Helper()
	devVersionGit(t, root, nil, "tag", "ts/v0.5.0")
}

// dirtyDevVersionTree changes a committed file without committing it.
func dirtyDevVersionTree(t *testing.T, root string) {
	t.Helper()
	writeParityFile(t, root, "typescript/web/src.ts", "export const thing = 2\n")
}

// addDevVersionOrigin gives the fixture an origin, a bare repository beside
// it that holds main.
func addDevVersionOrigin(t *testing.T, root string) {
	t.Helper()
	origin := devVersionOrigin(root)
	devVersionGit(t, root, nil, "init", "-q", "--bare", origin)
	devVersionGit(t, root, nil, "remote", "add", "origin", origin)
	devVersionGit(t, root, nil, "push", "-q", "origin", "main")
}

// devVersionOrigin is where addDevVersionOrigin puts the origin of root.
func devVersionOrigin(root string) string {
	return filepath.Join(filepath.Dir(root), "origin.git")
}

// bindDevVersionParent binds the run to HEAD's parent, a commit of the
// repository other than HEAD.
func bindDevVersionParent(t *testing.T, root string) []string {
	t.Helper()
	return []string{"PUTNAMI_SOURCE_REVISION=" + devVersionGit(t, root, nil, "rev-parse", "HEAD~1")}
}

// devVersionRepository describes the repository after a run: the tree's
// status, HEAD's message, the tags on HEAD with their messages, every
// changelog, and what origin holds when the fixture has one.
func devVersionRepository(t *testing.T, root string) string {
	t.Helper()
	var out strings.Builder
	if status := devVersionGit(t, root, nil, "status", "--porcelain"); status != "" {
		fmt.Fprintf(&out, "status:\n%s\n", status)
	} else {
		out.WriteString("status: clean\n")
	}
	fmt.Fprintf(&out, "head:\n%s\n", devVersionGit(t, root, nil, "log", "-1", "--format=%B"))
	tags := strings.Fields(devVersionGit(t, root, nil, "tag", "--points-at", "HEAD"))
	fmt.Fprintf(&out, "tags at head: %s\n", devVersionList(tags))
	for _, tag := range tags {
		fmt.Fprintf(&out, "tag %s:\n%s\n", tag, devVersionGit(t, root, nil, "tag", "-l", "--format=%(contents)", tag))
	}
	for _, rel := range []string{"CHANGELOG.md", "tooling/CHANGELOG.md", "typescript/CHANGELOG.md"} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "file %s:\n%s", rel, data)
	}
	if origin := devVersionOrigin(root); devVersionExists(origin) {
		head := devVersionGit(t, root, nil, "rev-parse", "HEAD")
		fmt.Fprintf(&out, "origin main is HEAD: %t\n", devVersionGit(t, origin, nil, "rev-parse", "main") == head)
		fmt.Fprintf(&out, "origin tags at HEAD: %s\n", devVersionList(strings.Fields(devVersionGit(t, origin, nil, "tag", "--points-at", head))))
	}
	return out.String()
}

// devVersionList joins names with spaces, or says none.
func devVersionList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

func devVersionExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- the recordings ---------------------------------------------------------

// devVersionRecordingDir holds the core's recorded answers.
func devVersionRecordingDir() string { return filepath.Join("testdata", "dev-version-parity") }

// devVersionRepositoryMarker opens the repository section of an answer, after
// its stdout and stderr sections.
const devVersionRepositoryMarker = "--- repository ---\n"

// devVersionAnswer is what one run answered, and the repository after it.
type devVersionAnswer struct {
	exit                       int
	stdout, stderr, repository string
}

func (a devVersionAnswer) String() string {
	text := "exit " + strconv.Itoa(a.exit) + "\n" + recordedStdoutMarker + a.stdout + recordedStderrMarker + a.stderr
	if a.repository != "" {
		text += devVersionRepositoryMarker + a.repository
	}
	return text
}

var (
	// devVersionReleaseDate is the date of a changelog heading, the day the
	// release ran.
	devVersionReleaseDate = regexp.MustCompile(`(## [^ \n]+ — )\d{4}-\d{2}-\d{2}`)
	// devVersionReleaseCommit is the release commit's ID in the line git
	// commit prints.
	devVersionReleaseCommit = regexp.MustCompile(`\[main [0-9a-f]{7,64}\]`)
)

// normalized replaces what depends on the moment of the run: the release
// date, the release commit's ID and the fixture's directory.
func (a devVersionAnswer) normalized(root string) devVersionAnswer {
	normalize := func(text string) string {
		text = devVersionReleaseDate.ReplaceAllString(text, "${1}{{DATE}}")
		text = devVersionReleaseCommit.ReplaceAllString(text, "[main {{RELEASE_COMMIT}}]")
		return normalizeParityBytes(text, root)
	}
	a.stdout, a.stderr, a.repository = normalize(a.stdout), normalize(a.stderr), normalize(a.repository)
	return a
}

// readDevVersionAnswer reads the answer at path: an `exit <code>` line, then
// the stdout and stderr sections, then the repository section if any.
func readDevVersionAnswer(t *testing.T, path string) devVersionAnswer {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the answer %s: %v", path, err)
	}
	head, rest, found := strings.Cut(string(data), recordedStdoutMarker)
	if !found {
		t.Fatalf("%s carries no stdout section", path)
	}
	stdout, rest, found := strings.Cut(rest, recordedStderrMarker)
	if !found {
		t.Fatalf("%s carries no stderr section", path)
	}
	stderr, repository, _ := strings.Cut(rest, devVersionRepositoryMarker)
	for _, section := range []string{stdout, stderr, repository} {
		if strings.Contains(section, recordedStdoutMarker) || strings.Contains(section, recordedStderrMarker) ||
			strings.Contains(section, devVersionRepositoryMarker) {
			t.Fatalf("%s repeats a section marker, so its framing is ambiguous", path)
		}
	}
	exit, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(head), "exit")))
	if err != nil {
		t.Fatalf("%s does not start with an `exit <code>` line: %v", path, err)
	}
	return devVersionAnswer{exit: exit, stdout: stdout, stderr: stderr, repository: repository}
}
