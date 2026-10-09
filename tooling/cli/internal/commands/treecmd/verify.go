package treecmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// VerifyUsage is the one-line usage `putnami tree verify --help` prints.
const VerifyUsage = "usage: putnami tree verify (--record FILE | --snapshot | --ref FILE | --gate SESSION --report REPORT) [--base REV]\n"

// Verifier checks local workflow evidence against the worktree it runs in. It
// is a consistency check, not a signed review or an access boundary, and it
// never mutates the tree it reads.
//
// The two seams exist for tests: production leaves both nil.
type Verifier struct {
	// Dir is where the check starts; Run moves to the Git top level of Dir.
	Dir string
	// Fingerprint returns the worktree fingerprint of root. Nil reads it with
	// git.FingerprintTree, the calculation `putnami tree fingerprint` prints.
	Fingerprint func(root string) (string, error)
	// Plan returns the task identity keys of the native, unfiltered impacted
	// plan of commands with flags against baseSHA. Nil runs the workspace CLI
	// (`./putnamiw` when it is executable, `putnami` otherwise, `putnami` on
	// Windows) with `<commands joined by ,> --impacted --baseline <baseSHA>
	// <flags> --plan --output=json`.
	Plan func(root string, commands, flags []string, baseSHA string) ([]string, error)
	// Stdout receives exactly one JSON document, or the usage line for --help.
	Stdout io.Writer
}

// ErrNotVerified is returned by Run after it wrote a not-verified verdict.
// The caller exits 1 and prints nothing else: stdout already holds the reason.
var ErrNotVerified = errors.New("not verified")

// Run parses args, the flags after `tree verify`, and writes the verdict.
//
// Success writes the mode's document and returns nil. Any failure, including a
// usage error, writes {"verdict":"not-verified","reason":"<message>"} on one
// line and returns ErrNotVerified.
func (v Verifier) Run(args []string) error {
	stdout := v.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	if slices.ContainsFunc(args, func(arg string) bool { return arg == "--help" || arg == "-h" }) {
		_, err := io.WriteString(stdout, VerifyUsage)
		return err
	}
	value, err := v.verdict(args)
	if err != nil {
		failure := obj([]string{"verdict", "reason"}, "not-verified", err.Error())
		if _, writeErr := io.WriteString(stdout, stringify(failure, "")+"\n"); writeErr != nil {
			return fmt.Errorf("write verdict: %w", writeErr)
		}
		return ErrNotVerified
	}
	if _, err := io.WriteString(stdout, stringify(value, "  ")+"\n"); err != nil {
		return fmt.Errorf("write verdict: %w", err)
	}
	return nil
}

// The checks run in a fixed order, and each failure names a fixed reason:
// finalize-pr.sh and the execute skill read those reasons. The evidence is read
// with JavaScript semantics (see verify_json.go), the semantics its producers
// and the skill's earlier JavaScript verifier share. A failed check, and a read
// that JavaScript answers with a TypeError, unwinds with a verifyFailure panic;
// verdict recovers every panic into the not-verified reason, so none leaves
// Run.

// verifyFailure carries a not-verified reason up to verdict.
type verifyFailure struct{ reason string }

func fail(reason string) { panic(verifyFailure{reason: reason}) }

func failErr(err error) { fail(err.Error()) }

func require(ok bool, reason string) {
	if !ok {
		fail(reason)
	}
}

// recoverFailure turns a panic in the caller into *err: a verifyFailure into
// its reason, anything else into an internal error.
func recoverFailure(err *error) {
	switch recovered := recover().(type) {
	case nil:
	case verifyFailure:
		*err = errors.New(recovered.reason)
	default:
		*err = fmt.Errorf("internal verifier error: %v", recovered)
	}
}

// verifyRun is one verification: the Git top level every path resolves
// against, and every evidence file checked so far with the digest it had.
type verifyRun struct {
	v       Verifier
	root    string
	checked []checkedRef
}

type checkedRef struct{ path, digest string }

// verifyArgs is the parsed command line. base and report are nil when absent.
type verifyArgs struct {
	mode, input  string
	base, report *string
}

func (a verifyArgs) baseOr(fallback string) string {
	if a.base == nil {
		return fallback
	}
	return *a.base
}

func (v Verifier) verdict(args []string) (value any, err error) {
	defer recoverFailure(&err)
	parsed := parseVerifyArgs(args)
	top, err := capture("git", gitCommand(v.Dir, "rev-parse", "--show-toplevel"))
	if err != nil {
		return nil, err
	}
	r := &verifyRun{v: v, root: jsTrim(top)}
	switch parsed.mode {
	case "--snapshot":
		return r.snapshot(parsed.baseOr("origin/main")), nil
	case "--ref":
		return r.ref(parsed.input), nil
	case "--gate":
		return r.reusableGate(parsed.input, *parsed.report, parsed.baseOr("origin/main")), nil
	}
	before := r.digest(parsed.input)
	result := r.verify(r.readJSON(parsed.input), parsed.base)
	require(r.digest(parsed.input) == before, "dossier changed during verification")
	return result, nil
}

const chooseOneMode = "choose exactly one of --record, --snapshot, --ref and --gate"

func parseVerifyArgs(args []string) verifyArgs {
	var parsed verifyArgs
	flagValue := func(index int, reason string) string {
		value := ""
		if index < len(args) {
			value = args[index]
		}
		require(text(value) && !strings.HasPrefix(value, "--"), reason)
		return value
	}
	for index := 0; index < len(args); index++ {
		switch arg := args[index]; arg {
		case "--record", "--snapshot", "--ref", "--gate":
			require(parsed.mode == "", chooseOneMode)
			parsed.mode = arg
			if arg != "--snapshot" {
				index++
				parsed.input = flagValue(index, "missing "+arg+" value")
			}
		case "--report":
			index++
			report := flagValue(index, "missing --report value")
			parsed.report = &report
		case "--base":
			index++
			base := flagValue(index, "missing --base value")
			parsed.base = &base
		default:
			fail("unknown argument: " + arg)
		}
	}
	require(parsed.mode != "", chooseOneMode)
	require((parsed.mode == "--gate") == (parsed.report != nil), "--report goes with --gate, and --gate needs it")
	return parsed
}

// snapshot is the dossier skeleton for the current tree: its binding, changed
// files and policy references, with every judgment still empty.
func (r *verifyRun) snapshot(base string) *jsObject {
	baseSHA := r.git("merge-base", base, "HEAD")
	binding := obj([]string{"fingerprint", "headSHA", "baseSHA"}, r.fingerprint(), r.git("rev-parse", "HEAD"), baseSHA)
	changed := r.changedFiles(baseSHA)
	policies := []any{}
	for _, path := range r.policyPaths() {
		policies = append(policies, r.ref(path))
	}
	return obj([]string{"version", "binding", "changedFiles", "policies", "objective", "scope",
		"implementers", "scopes", "review", "gate", "qualification", "acceptance"},
		2.0, binding, stringList(changed), policies, jsNull{}, jsNull{},
		[]any{}, []any{}, jsNull{}, jsNull{},
		obj([]string{"required", "results", "notApplicable"}, []any{}, []any{}, jsNull{}), []any{})
}

// reusableGate checks a gate the finalizer reuses instead of running its own:
// the dossier's gate rules without the dossier. The finalizer's own four
// commands are required on top of the CI policy's, so a reused gate is never
// narrower than the gate the finalizer would run.
func (r *verifyRun) reusableGate(sessionPath, reportPath, base string) *jsObject {
	current := r.fingerprint()
	baseSHA := r.git("merge-base", base, "HEAD")
	session := r.readJSON(sessionPath)
	r.checkGate(session, r.readJSON(reportPath), current, baseSHA, false, []string{"lint", "test", "build", "validate"})
	require(r.fingerprint() == current, "tree changed during verification")
	return obj([]string{"verdict", "gateSession", "session", "report", "fingerprint", "baseSHA"},
		"gate reusable", at(session, "sessionId"), sessionPath, reportPath, current, baseSHA)
}

var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (r *verifyRun) fingerprint() string {
	read := r.v.Fingerprint
	if read == nil {
		read = treeFingerprint
	}
	value, err := read(r.root)
	if err != nil {
		failErr(err)
	}
	require(fingerprintPattern.MatchString(value), "CLI returned no tree fingerprint")
	return value
}

func treeFingerprint(root string) (string, error) {
	tree, err := git.FingerprintTree(root)
	if err != nil {
		return "", err
	}
	return tree.Fingerprint, nil
}

// policyPaths lists the policy files that exist in the tree, sorted.
func (r *verifyRun) policyPaths() []string {
	names := []string{"AGENTS.md", "CLAUDE.md", ".agents/constraints.md",
		"putnami.ci.json", "putnami.workspace.json", "putnami.lock.json"}
	// Every decision registry binds: the root one binds every project, a
	// project's own binds that project.
	names = append(names, r.fileList("ls-files", "-z", "**/putnami.architecture.json", "putnami.architecture.json",
		"**/decisions.json", "decisions.json")...)
	kept := []string{}
	for _, name := range names {
		if info, err := os.Stat(r.path(name)); err == nil && info.Mode().IsRegular() {
			kept = append(kept, name)
		}
	}
	return uniqueSorted(kept)
}

// changedFiles lists the files that differ from base, untracked ones included,
// sorted.
func (r *verifyRun) changedFiles(base any) []string {
	changed := r.fileList("diff", "--name-only", "--no-renames", "-z", jsToString(base), "--")
	changed = append(changed, r.fileList("ls-files", "--others", "--exclude-standard", "-z")...)
	return uniqueSorted(changed)
}

// path resolves a path the evidence or the command line names against the Git
// top level, the directory the paths are relative to.
func (r *verifyRun) path(name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return r.root + string(filepath.Separator) + name
}

// ref is the reference to a file: its path as given and the SHA-256 of its
// bytes.
func (r *verifyRun) ref(name string) *jsObject {
	return obj([]string{"path", "sha256"}, name, r.digest(name))
}

func (r *verifyRun) digest(name string) string {
	file, err := os.Open(r.path(name))
	if err != nil {
		failErr(fileError(err, name))
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		failErr(fileError(err, name))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (r *verifyRun) readJSON(name string) any {
	file, err := os.Open(r.path(name))
	if err != nil {
		failErr(fileError(err, name))
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxEvidenceBytes+1))
	if err != nil {
		failErr(fileError(err, name))
	}
	value, err := parseJSON(data)
	if err != nil {
		failErr(err)
	}
	return value
}

// fileError words a failed read the way Node.js does, with the path as the
// evidence names it.
func fileError(err error, name string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("ENOENT: no such file or directory, open '%s'", name)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("EACCES: permission denied, open '%s'", name)
	case errors.Is(err, syscall.EISDIR):
		return errors.New("EISDIR: illegal operation on a directory, read")
	}
	return err
}

// checkRef checks an evidence reference: a non-empty regular file whose bytes
// still have the recorded digest. It returns the path.
func (r *verifyRun) checkRef(value any) string {
	reference, isObject := value.(*jsObject)
	require(isObject && text(reference.get("path")), "missing evidence reference")
	name, _ := reference.get("path").(string)
	info, err := os.Stat(r.path(name))
	require(err == nil && info.Mode().IsRegular() && info.Size() > 0, "missing or empty evidence file")
	digest := r.digest(name)
	require(strictEqual(digest, reference.get("sha256")), "evidence digest changed: "+name)
	r.checked = append(r.checked, checkedRef{path: name, digest: digest})
	return name
}

// git runs git in the top level and returns its trimmed output.
func (r *verifyRun) git(args ...string) string {
	out, err := capture("git", gitCommand(r.root, args...))
	if err != nil {
		failErr(err)
	}
	return jsTrim(out)
}

// fileList runs git in the top level and returns the paths of its
// NUL-separated output.
func (r *verifyRun) fileList(args ...string) []string {
	out, err := capture("git", gitCommand(r.root, args...))
	if err != nil {
		failErr(err)
	}
	var paths []string
	for _, path := range strings.Split(out, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// gitCommand builds git with args, to run in dir. The verifier runs read verbs
// only (rev-parse, merge-base, diff --name-only, ls-files), which run no hook.
func gitCommand(dir string, args ...string) *exec.Cmd {
	command := exec.Command("git", args...)
	command.Dir = dir
	return command
}

// capture runs command and returns its standard output, decoded as fatal
// UTF-8. name is how failures name the command. Standard error is discarded;
// either stream may carry at most 32 MiB.
func capture(name string, command *exec.Cmd) (string, error) {
	stdout := &cappedBuffer{keep: true, kill: func() { killProcess(command) }}
	stderr := &cappedBuffer{kill: stdout.kill}
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if stdout.over || stderr.over {
		return "", fmt.Errorf("spawnSync %s ENOBUFS", name)
	}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		return "", fmt.Errorf("%s %s failed (%s)", name, strings.Join(command.Args[1:], " "), exitStatus(exitErr))
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("spawnSync %s ENOENT", name)
	case errors.Is(err, fs.ErrPermission):
		return "", fmt.Errorf("spawnSync %s EACCES", name)
	case err != nil:
		return "", err
	}
	return decodeUTF8(stdout.Bytes())
}

func killProcess(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}

// cappedBuffer takes at most maxEvidenceBytes, keeping them when keep is set.
// Past the cap it refuses the write and stops the process.
type cappedBuffer struct {
	bytes.Buffer
	keep bool
	size int
	over bool
	kill func()
}

var errOutputCap = errors.New("process output exceeds 32 MiB")

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.size+len(p) > maxEvidenceBytes {
		if !b.over {
			b.over = true
			b.kill()
		}
		return 0, errOutputCap
	}
	b.size += len(p)
	if b.keep {
		return b.Buffer.Write(p)
	}
	return len(p), nil
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT", syscall.SIGILL: "SIGILL",
	syscall.SIGTRAP: "SIGTRAP", syscall.SIGABRT: "SIGABRT", syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE",
	syscall.SIGKILL: "SIGKILL", syscall.SIGSEGV: "SIGSEGV", syscall.SIGPIPE: "SIGPIPE", syscall.SIGALRM: "SIGALRM",
	syscall.SIGTERM: "SIGTERM",
}

// exitStatus is the exit code, or the name of the signal that ended the
// process.
func exitStatus(exitErr *exec.ExitError) string {
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		if name, known := signalNames[status.Signal()]; known {
			return name
		}
		return "signal " + strconv.Itoa(int(status.Signal()))
	}
	return strconv.Itoa(exitErr.ExitCode())
}

// workspaceCLI names the CLI that answers the native plan in root: the
// putnamiw wrapper when it is an executable regular file, the putnami on PATH
// otherwise. Windows runs no shell script as a process, so it always takes
// the putnami on PATH.
func workspaceCLI(root, goos string) string {
	if goos == "windows" {
		return "putnami"
	}
	info, err := os.Stat(filepath.Join(root, "putnamiw"))
	if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		return "./putnamiw"
	}
	return "putnami"
}

// workspacePlan is the default Verifier.Plan: it asks the workspace CLI for
// the native impacted plan with flags as a dry run and returns its task
// identity keys.
func workspacePlan(root string, commands, flags []string, baseSHA string) (keys []string, err error) {
	name := workspaceCLI(root, runtime.GOOS)
	executable := name
	if name == "./putnamiw" {
		executable = filepath.Join(root, "putnamiw")
	}
	// The workspace CLI may be a build of the workspace's source.
	runcredential.MarkRepositoryCodeStarted("tree verify native plan")
	args := append([]string{strings.Join(commands, ","), "--impacted", "--baseline", baseSHA}, flags...)
	command := exec.Command(executable, append(args, "--plan", "--output=json")...)
	command.Dir = root
	out, err := capture(name, command)
	if err != nil {
		return nil, err
	}
	planned, err := parseJSON([]byte(jsTrim(out)))
	if err != nil {
		return nil, err
	}
	defer recoverFailure(&err)
	return plannedKeys(planned), nil
}

// plannedKeys checks that planned is a successful dry-run plan and returns the
// identity key of each of its tasks.
func plannedKeys(planned any) []string {
	require(strictEqual(at(planned, "protocolVersion"), 2.0) && strictEqual(at(planned, "status"), "success") &&
		strictEqual(at(planned, "exitCode"), 0.0) && strictEqual(at(at(planned, "plan"), "dryRun"), true),
		"native gate plan failed or is not a dry run")
	keys := []string{}
	for _, task := range array(at(at(planned, "plan"), "tasks"), "planned.plan.tasks") {
		key, isString := at(at(task, "identity"), "key").(string)
		require(isString, "native gate plan names a task without a string identity key")
		keys = append(keys, key)
	}
	return keys
}

// at is value[key], failing like the TypeError JavaScript throws on undefined
// or null.
func at(value any, key string) any {
	member, err := get(value, key)
	if err != nil {
		failErr(err)
	}
	return member
}

// hasOwnMember is Object.hasOwn(value, key).
func hasOwnMember(value any, key string) bool {
	has, err := hasOwn(value, key)
	if err != nil {
		failErr(err)
	}
	return has
}

// each is the sequence a for...of loop over value visits.
func each(value any) []any {
	items, err := iterate(value)
	if err != nil {
		failErr(err)
	}
	return items
}

// array is value as the array a .map call needs.
func array(value any, name string) []any {
	items, isArray := value.([]any)
	if !isArray {
		if nullish(value) {
			failErr(typeError(value, "map"))
		}
		fail(name + ".map is not a function")
	}
	return items
}

func stringList(values []string) []any {
	items := make([]any, len(values))
	for i, value := range values {
		items[i] = value
	}
	return items
}
