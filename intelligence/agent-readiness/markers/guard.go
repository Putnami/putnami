package markers

import (
	"regexp"
	"strings"
)

var (
	// guardSkip matches the signal lines the rule may leave out. Retries and
	// focused tests always count.
	guardSkip = regexp.MustCompile(`t\.Skip\(|\.skip\(|pytest\.mark\.skip|@Disabled`)
	// conditionalSkip matches a skip that carries its own condition: pytest's
	// skipif and JUnit's conditional @Disabled annotations.
	conditionalSkip = regexp.MustCompile(`skipif|@disabledon|@disabledif|@disabledfor`)
	// opensCondition matches a line that opens the block the skip sits in.
	opensCondition = regexp.MustCompile(`\b(if|elif|unless|when)\b`)
	// platformCheck matches an expression that tests the platform or probes
	// for a dependency.
	platformCheck = regexp.MustCompile(`runtime\.goos|runtime\.goarch|os\.pathseparator|os\.geteuid|os\.getuid|sys\.platform|` +
		`sys\.version_info|platform\.(system|python_implementation|machine)|os\.name\b|process\.platform|os\.platform\(|` +
		`@disabledon(os|jre)|@disabledforjrerange|exec\.lookpath|shutil\.which|find_spec|importorskip|os\.getenv|` +
		`os\.lookupenv|os\.environ|process\.env\.`)
	// missingWord matches a reason that names a platform, or a dependency that
	// is missing.
	missingWord = regexp.MustCompile(`\b(windows|darwin|macos|linux|unix|posix|plan9|wasm|emscripten|pypy|graalpy)\b|` +
		`unavailable|not available|not installed|\binstalled\b|not found|not on path|no usable|\bno [a-z ]{0,40}binding\b|` +
		`unreachable|mode=skip|not set\b|\bset [a-z][a-z0-9]*_[a-z0-9_]+|\brequire[sd]?\b|\bneeds?\b|\blacks?\b|\babsent\b|\bmissing\b`)
	// stillCounts matches a reason that names a flaky or failing test, or the
	// CI environment: such a skip hides a failure wherever it runs.
	stillCounts = regexp.MustCompile(`flak|\bci\b|github_actions|todo|fixme|broken|\bfail(s|ing|ure)?\b|unreliable|unstable|intermittent`)
)

// maxContinuation bounds how many lines a skip's statement may continue on.
const maxContinuation = 3

// GuardedSkip reports whether lines[i] is a skip that only a missing platform
// or dependency triggers. It holds when three things are true:
//
//   - the line is a skip: t.Skip(, .skip(, pytest.mark.skip or @Disabled;
//   - the skip is conditional: it is a skipif or a conditional @Disabled
//     annotation, its line or one of the two lines before it opens an if,
//     elif, unless or when, or its statement tests the platform itself; a
//     blank line or a closing brace above the skip ends that search;
//   - the statement or those two lines test the platform, probe for a
//     dependency, or name a platform or a missing dependency, and none of
//     them names a flaky or failing test or CI.
//
// Every comparison is on lowercase text.
func GuardedSkip(lines []string, i int) bool {
	statement, before, inside, ok := skipContext(lines, i)
	if !ok {
		return false
	}
	if !inside && !conditionalSkip.MatchString(statement) && !platformCheck.MatchString(statement) {
		return false
	}
	text := before + " " + statement
	if stillCounts.MatchString(text) {
		return false
	}
	return platformCheck.MatchString(text) || missingWord.MatchString(text)
}

// skipContext reads what the guard rules read around a skip on lines[i]: its
// statement, up to maxContinuation more lines while a parenthesis is open;
// the two lines before it, up to a blank line or a closed block; and whether
// the skip sits in a condition its own line or those lines open. All of it is
// lowercase. ok is false when lines[i] is not a skip.
func skipContext(lines []string, i int) (statement, before string, inside, ok bool) {
	if i < 0 || i >= len(lines) || !guardSkip.MatchString(lines[i]) {
		return "", "", false, false
	}
	statement = strings.ToLower(lines[i])
	for j, open := i, strings.Count(lines[i], "(")-strings.Count(lines[i], ")"); open > 0 && j+1 < len(lines) && j < i+maxContinuation; j++ {
		statement += " " + strings.ToLower(lines[j+1])
		open += strings.Count(lines[j+1], "(") - strings.Count(lines[j+1], ")")
	}
	// The skip sits in a condition that its own line opens before it, or
	// that one of the two lines above opens. A blank line or a closed block
	// ends the search.
	at := guardSkip.FindStringIndex(lines[i])[0]
	inside = opensCondition.MatchString(strings.ToLower(lines[i][:at]))
	for j := i - 1; j >= max(0, i-2); j-- {
		line := strings.ToLower(strings.TrimSpace(lines[j]))
		if line == "" || line == "end" || (strings.HasPrefix(line, "}") && !strings.HasSuffix(line, "{")) {
			break
		}
		before = line + " " + before
		inside = inside || opensCondition.MatchString(line)
	}
	return statement, before, inside, true
}

// shortMode matches a testing.Short() check that Go's -short flag answers,
// not its negation: a skip under !testing.Short() fires on the full run.
var shortMode = regexp.MustCompile(`(^|[^!\w.])testing\.short\(\)`)

// fileLines splits a file's content into lines.
func fileLines(content []byte) []string {
	return strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
}
