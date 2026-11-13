package bazel

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	detect "go.putnami.dev/tooling/observe/detect/bazel"
)

// CollectReportsCommand is the runtime command the test task's report step
// runs: {runtime} collect-reports --events <file> --root <dir>
// [--package <dir>] --into <dir>.
const CollectReportsCommand = "collect-reports"

// Report file names: the JUnit log and the LCOV coverage of a test run, as
// bazel-testlogs holds them, and as the collected copies are named.
const (
	junitFile    = "test.xml"
	coverageFile = "coverage.dat"
)

// testOutputs maps the name a build event gives a test output to the file
// the collected copy is named.
var testOutputs = map[string]string{
	"test.xml":     junitFile,
	"test.lcov":    coverageFile,
	"coverage.dat": coverageFile,
}

// CollectReports copies the JUnit log and the LCOV coverage of each test run
// the build-event file lists into the directory --into, by its log directory
// under bazel-testlogs. A run whose event names a local file is copied from
// it; any other run is read from <root>/bazel-testlogs/<package>/<target>.
// A coverage file that is no LCOV tracefile, such as the raw LLVM profile a
// C++ test can leave, is skipped with a warning on stderr. With --package,
// a coverage copy keeps the records of the sources under that package
// directory alone: the test steps instrument every source of the main
// repository, so a test's coverage also holds the packages it depends on,
// which are projects of their own. Bazel's own outputs are only read, never
// removed: a removed output would make Bazel rerun a cached test. A missing
// build-event file, as when bazel stopped before it wrote one, collects
// nothing and is no error: the task's reports are missing then, and the step
// already failed.
func CollectReports(args []string, _, stderr io.Writer) error {
	if stderr == nil {
		stderr = io.Discard
	}
	fs := flag.NewFlagSet(CollectReportsCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	events := fs.String("events", "", "the build-event file of the test run")
	root := fs.String("root", "", "the Bazel workspace root")
	pkg := fs.String("package", "", "the package whose sources the coverage keeps")
	into := fs.String("into", "", "the directory the reports are copied to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *events == "" || *root == "" || *into == "" || fs.NArg() != 0 {
		return errors.New("usage: collect-reports --events <file> --root <dir> [--package <dir>] --into <dir>")
	}
	runs, err := testRuns(*events)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, run := range runs {
		for name, src := range run.files(*root) {
			rel := path.Join(run.logDir, name)
			if name == coverageFile && !isLCOV(src) {
				fmt.Fprintf(stderr, "warning: %s is not an LCOV tracefile: its coverage is skipped\n", rel)
				continue
			}
			dst := filepath.Join(*into, filepath.FromSlash(rel))
			var keep func(string) bool
			if name == coverageFile && *pkg != "" {
				keep = underPackage(*pkg)
			}
			if err := copyReport(src, dst, keep); err != nil {
				return err
			}
		}
	}
	return nil
}

// testRun is one test run a build-event file lists.
type testRun struct {
	// logDir is the run's directory relative to bazel-testlogs:
	// <package>/<target>, and below it the shard, run and attempt
	// directories a local output names.
	logDir string
	// outputs are the local files of the run's outputs, by collected name.
	outputs map[string]string
}

// files are the report files of the run, by collected name: the local files
// its event names, else the files bazel-testlogs holds for its target.
func (r testRun) files(root string) map[string]string {
	if len(r.outputs) > 0 {
		return r.outputs
	}
	out := map[string]string{}
	for _, name := range []string{junitFile, coverageFile} {
		file := filepath.Join(root, detect.TestLogs, filepath.FromSlash(r.logDir), name)
		if info, err := os.Stat(file); err == nil && info.Mode().IsRegular() {
			out[name] = file
		}
	}
	return out
}

// buildEvent is the part of a build event the collection reads.
type buildEvent struct {
	ID struct {
		TestResult *struct {
			Label string `json:"label"`
		} `json:"testResult"`
	} `json:"id"`
	TestResult *struct {
		TestActionOutput []struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
		} `json:"testActionOutput"`
	} `json:"testResult"`
}

// testRuns reads the test runs of a build-event file written with
// --build_event_json_file, one JSON event per line.
func testRuns(file string) ([]testRun, error) {
	f, err := os.Open(file) //nolint:gosec // the build-event file the task's own step wrote
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var runs []testRun
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e buildEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("%s is no build-event file: %w", file, err)
		}
		if e.ID.TestResult == nil {
			continue
		}
		dir, ok := labelLogDir(e.ID.TestResult.Label)
		if !ok {
			continue
		}
		run := testRun{logDir: dir}
		if e.TestResult != nil {
			for _, o := range e.TestResult.TestActionOutput {
				name, ok := testOutputs[o.Name]
				if !ok {
					continue
				}
				local, under, ok := localOutput(o.URI)
				if !ok {
					continue
				}
				if run.outputs == nil {
					run.outputs = map[string]string{}
				}
				run.outputs[name] = local
				if under != "" {
					run.logDir = under
				}
			}
		}
		runs = append(runs, run)
	}
	return runs, scanner.Err()
}

// labelLogDir is the log directory of a test target of the main repository,
// <package>/<target> relative to bazel-testlogs.
func labelLogDir(label string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimLeft(label, "@"), "//")
	if !ok {
		return "", false
	}
	pkg, name, explicit := strings.Cut(rest, ":")
	if !explicit {
		name = path.Base(pkg)
	}
	if name == "" {
		return "", false
	}
	return path.Join(pkg, name), true
}

// localOutput reads a file URI of a test output: the local file, and its
// directory relative to the testlogs directory that holds it, empty when no
// testlogs directory does.
func localOutput(uri string) (string, string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Path == "" {
		return "", "", false
	}
	slashed := u.Path
	// file:///C:/x names C:/x: the slash before a drive letter is no root.
	if len(slashed) > 1 && filepath.VolumeName(filepath.FromSlash(slashed[1:])) != "" {
		slashed = slashed[1:]
	}
	file := filepath.FromSlash(slashed)
	if i := strings.LastIndex(slashed, "/testlogs/"); i >= 0 {
		return file, path.Dir(slashed[i+len("/testlogs/"):]), true
	}
	return file, "", true
}

// lcovStart are the records an LCOV tracefile starts with.
var lcovStart = []string{"TN:", "SF:", "VER:"}

// isLCOV reports whether a coverage file is an LCOV tracefile: empty, or text
// whose first non-blank line opens one. A file that cannot be read is left to
// the copy, which reports it.
func isLCOV(file string) bool {
	f, err := os.Open(file) //nolint:gosec // a test output the build-event file names
	if err != nil {
		return true
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return true
	}
	text := strings.TrimLeft(string(head[:n]), " \t\r\n")
	if text == "" {
		return true
	}
	for _, start := range lcovStart {
		if strings.HasPrefix(text, start) {
			return true
		}
	}
	return false
}

// underPackage keeps an LCOV record whose source file lies under the
// package directory pkg, as the filter ^//<pkg>[/:] Bazel derives for one
// package does.
func underPackage(pkg string) func(string) bool {
	prefix := strings.Trim(pkg, "/") + "/"
	return func(source string) bool { return strings.HasPrefix(source, prefix) }
}

// copyReport copies one report, read-only in Bazel's output tree, to a new
// writable file; with keep, an LCOV tracefile whose records keep accepts
// by their source file.
func copyReport(src, dst string, keep func(string) bool) error {
	in, err := os.Open(src) //nolint:gosec // a test output the build-event file names
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst) //nolint:gosec // a path under the task's own output directory
	if err != nil {
		return err
	}
	var werr error
	if keep == nil {
		_, werr = io.Copy(out, in)
	} else {
		werr = filterLCOV(out, in, keep)
	}
	if cerr := out.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// filterLCOV copies the LCOV records of in whose SF source keep accepts, each
// from its first line to its end_of_record, to out.
func filterLCOV(out io.Writer, in io.Reader, keep func(string) bool) error {
	reader := bufio.NewReader(in)
	var record strings.Builder
	kept := true
	flush := func() error {
		if kept && record.Len() > 0 {
			if _, err := io.WriteString(out, record.String()); err != nil {
				return err
			}
		}
		record.Reset()
		kept = true
		return nil
	}
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			record.WriteString(line)
			text := strings.TrimRight(line, "\r\n")
			if source, ok := strings.CutPrefix(text, "SF:"); ok {
				kept = keep(source)
			}
			if text == "end_of_record" {
				if ferr := flush(); ferr != nil {
					return ferr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return flush()
		}
		if err != nil {
			return err
		}
	}
}
