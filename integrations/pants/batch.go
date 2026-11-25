package pants

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.putnami.dev/tooling/stacks"
)

// ReportsDir is where pants test --test-report writes the JUnit report of
// each test target, under the workspace root.
const ReportsDir = "dist/test/reports"

var (
	// ansi matches the escape sequences of a colored output.
	ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// testResult is one line of the test results pants test prints:
	// <sigil> <address>[ and <n> other files] succeeded|failed[ (exit code
	// <n>)][ after <n> attempts] [in <t>s][ (<source>)].
	testResult = regexp.MustCompile(`^(✓|✕|\+) (\S+)( and \d+ other files)? (succeeded|failed)(?: \(exit code -?\d+\))?(?: after \d+ attempts)? ?(?:in [0-9.]+s)?(?: \([^)]*\))?\.$`)
)

// ReadBatch reads the test results a failed pants test over several package
// directories printed (ADR 0019 of tooling/stacks): one line per test target,
// and the command --test-show-rerun-command prints, which names every target
// whose test failed, those Pants prints no line for included. A package with
// a failed target fails with its lines; one whose own lines all succeeded,
// and that the rerun command does not name, passes. A line that stands for a
// batch of files passes no package. An output with no rerun command, or one
// that names a target of no package of the command, names no package: the
// core then fails every package with the command's error. pants lint and
// pants package print no line per package: their failure names none either.
func ReadBatch(report stacks.BatchReport) map[string]stacks.BatchOutcome {
	if !report.Failed || report.Task != stacks.TaskTest {
		return nil
	}
	type result struct {
		passed bool
		failed []string
	}
	results := map[string]result{}
	fail := func(dir, message string) bool {
		if !slices.Contains(report.Components, dir) {
			return false
		}
		r := results[dir]
		r.failed = append(r.failed, message)
		results[dir] = r
		return true
	}
	lines := strings.Split(ansi.ReplaceAllString(string(report.Output), ""), "\n")
	rerun := false
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == rerunHeader {
			addresses, ok := rerunAddresses(lines[i+1:])
			if !ok {
				return nil
			}
			for _, address := range addresses {
				dir, ok := addressDir(report.Root, address)
				if !ok || !slices.Contains(report.Components, dir) {
					return nil
				}
				if !slices.ContainsFunc(results[dir].failed, func(m string) bool { return strings.Contains(m, " "+address+" ") }) {
					fail(dir, "pants test failed: "+address+" (no result line)")
				}
			}
			rerun = true
			continue
		}
		m := testResult.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		dir, ok := addressDir(report.Root, m[2])
		switch {
		case !ok:
		case m[4] == "failed":
			if !fail(dir, "pants test: "+line) {
				return nil
			}
		case m[3] == "":
			r := results[dir]
			r.passed = true
			results[dir] = r
		}
	}
	if !rerun {
		return nil
	}
	out := map[string]stacks.BatchOutcome{}
	for _, component := range report.Components {
		r := results[component]
		switch {
		case len(r.failed) > 0:
			out[component] = stacks.BatchOutcome{Failed: true, Message: strings.Join(r.failed, "\n")}
		case r.passed:
			out[component] = stacks.BatchOutcome{}
		}
	}
	return out
}

// rerunHeader opens the command --test-show-rerun-command prints when a test
// failed.
const rerunHeader = "To rerun the failing tests, use:"

// rerunAddresses reads the addresses of the rerun command, the first
// non-blank line after its header: <pants> test <address>..., each address
// quoted as a shell word when it needs it.
func rerunAddresses(lines []string) ([]string, bool) {
	for _, line := range lines {
		words := shellWords(line)
		if len(words) == 0 {
			continue
		}
		if len(words) < 3 || words[1] != "test" {
			return nil, false
		}
		return words[2:], true
	}
	return nil, false
}

// shellWords splits a line into words as a POSIX shell does for the quoting
// Python's shlex.quote writes: single quotes hold their text, and '"'"'
// stands for one quote.
func shellWords(line string) []string {
	var words []string
	var word strings.Builder
	quote, started := byte(0), false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			word.WriteByte(c)
		case c == '\'' || c == '"':
			quote, started = c, true
		case c == ' ' || c == '\t':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteByte(c)
			started = true
		}
	}
	if started {
		words = append(words, word.String())
	}
	return words
}

// addressDir is the directory of the target an address names, as Pants
// prints it: the package of its report and of its result.
func addressDir(root, address string) (string, bool) {
	a, ok := parseAddress(root, address)
	if !ok || a.specPath == "" {
		return "", false
	}
	return a.specPath, true
}

// address is a target address as Pants holds it.
type address struct {
	// specPath is the directory of the target's BUILD file.
	specPath string
	// file is the path of the file a file address names, below specPath.
	file string
	// target is the target's name, empty for the default one, which is
	// named after specPath.
	target string
	// generated and parameters follow a # and an @ in a printed address.
	generated, parameters string
}

// parseAddress reads an address as Pants prints it: a path, then a colon and
// the target name, a # and the generated name, an @ and the parameters. A
// path that names a file under root makes a file address, whose target name
// climbs one directory from the file's per ../ it starts with, as
// src/a/sub/x_test.py:../tests names the file sub/x_test.py of the target
// tests of src/a; a target named after its directory is the default one.
func parseAddress(root, printed string) (address, bool) {
	var a address
	rest, parameters, _ := strings.Cut(strings.TrimPrefix(printed, "//"), "@")
	rest, a.generated, _ = strings.Cut(rest, "#")
	spec, target, _ := strings.Cut(rest, ":")
	a.parameters = parameters
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(spec)))
	if spec == "" || err != nil || !info.Mode().IsRegular() {
		a.specPath = spec
	} else {
		a.specPath, a.file = path.Dir(spec), path.Base(spec)
		for t, ok := strings.CutPrefix(target, "../"); ok; t, ok = strings.CutPrefix(t, "../") {
			if a.specPath == "." || a.specPath == "" {
				return address{}, false
			}
			a.file = path.Base(a.specPath) + "/" + a.file
			a.specPath, target = path.Dir(a.specPath), t
		}
		if a.specPath == "." {
			a.specPath = ""
		}
	}
	if target != path.Base(a.specPath) || a.specPath == "" {
		a.target = target
	}
	return a, true
}

// ReportOwner answers, for one pants test command, the function that names
// the package of a JUnit report under dist/test/reports among the components
// of its batch; it reads the command's output once. Pants names each after the address of its
// test target, as pathSafeSpec writes it. The owner is the package of the address the
// command's output names under that file name. A name no address of the
// output matches is owned by the one component that prefixes it; when none
// or several do, as for a nested package, or for a pytest batch report, the
// file has no owner, and neither has any other file, as the coverage report of
// the whole command.
func ReportOwner(report stacks.BatchReport) func(file string) (string, bool) {
	addresses := outputAddresses(report)
	return func(file string) (string, bool) {
		name, ok := strings.CutPrefix(file, ReportsDir+"/")
		if !ok || strings.Contains(name, "/") || !strings.HasSuffix(name, ".xml") {
			return "", false
		}
		name = strings.TrimSuffix(name, ".xml")
		if dir, ok := addresses[name]; ok {
			if !slices.Contains(report.Components, dir) {
				return "", false
			}
			return dir, true
		}
		owner := ""
		for _, c := range report.Components {
			dotted := strings.ReplaceAll(c, "/", ".")
			if c == "" || !(name == dotted || strings.HasPrefix(name, dotted+".") || strings.HasPrefix(name, dotted+"@")) {
				continue
			}
			if owner != "" {
				return "", false
			}
			owner = c
		}
		return owner, owner != ""
	}
}

// outputAddresses maps the report file name of each address the output of a
// pants test names, on a result line or in the rerun command, to the
// directory of its target.
func outputAddresses(report stacks.BatchReport) map[string]string {
	out := map[string]string{}
	add := func(address string) {
		dir, ok := addressDir(report.Root, address)
		if !ok {
			return
		}
		if name, ok := pathSafeSpec(report.Root, address); ok {
			out[name] = dir
		}
	}
	lines := strings.Split(ansi.ReplaceAllString(string(report.Output), ""), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == rerunHeader {
			addresses, _ := rerunAddresses(lines[i+1:])
			for _, a := range addresses {
				add(a)
			}
			continue
		}
		if m := testResult.FindStringSubmatch(line); m != nil && m[3] == "" {
			add(m[2])
		}
	}
	return out
}

// pathSafeSpec is the name Pants gives the report of an address, as
// Address.path_safe_spec writes it: the directory with dots, then for a file
// a dot and its path with dots, then the target name after a dot, or after
// one @ per directory of the file's path; then @ and the generated name, and
// @@ and the parameters. src/a:tests is src.a.tests, src/a/x_test.py is
// src.a.x_test.py, and src/a/sub/x_test.py:../tests is
// src.a.sub.x_test.py@tests.
func pathSafeSpec(root, printed string) (string, bool) {
	a, ok := parseAddress(root, printed)
	if !ok {
		return "", false
	}
	dotted := func(s string) string { return strings.ReplaceAll(s, "/", ".") }
	name := dotted(a.specPath)
	switch up := strings.Count(a.file, "/"); {
	case a.file != "" && up > 0:
		target := a.target
		if target == "" {
			target = path.Base(a.specPath)
		}
		name += "." + dotted(a.file) + strings.Repeat("@", up) + target
	case a.file != "":
		name += "." + dotted(a.file)
		if a.target != "" {
			name += "." + a.target
		}
	case a.target != "":
		name += "." + a.target
	}
	if a.generated != "" {
		name += "@" + dotted(a.generated)
	}
	if a.parameters != "" {
		name += "@@" + dotted(a.parameters)
	}
	return name, true
}
