package markers

import (
	"path"
	"regexp"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/internal/ci"
	"go.putnami.dev/intelligence/agent-readiness/inventory"
)

// signalMatch finds each occurrence of SignalPattern on a line.
var signalMatch = regexp.MustCompile(SignalPattern)

// frameworkLanguages names, for each skip and focus token, the languages
// whose test frameworks spell it. The same token in a file of another
// language is text the test writes, such as a Python fixture inside a Go
// test, and hides no failure. A file whose extension names no language keeps
// every token.
var frameworkLanguages = map[string]map[string]bool{
	"t.Skip(":          {"Go": true},
	".skip(":           {"JavaScript": true, "TypeScript": true, "Python": true},
	".only(":           {"JavaScript": true, "TypeScript": true},
	"pytest.mark.skip": {"Python": true},
	"@Disabled":        {"Java": true, "Kotlin": true},
}

// sourceLanguages are the languages frameworkLanguages covers: in their
// files, a token inside a string literal is text, not a call.
var sourceLanguages = map[string]bool{
	"Go": true, "JavaScript": true, "TypeScript": true, "Python": true, "Java": true, "Kotlin": true,
}

// runnerOptions matches a statement that passes options to the test runner:
// Playwright's configure( call, or a test declared with an options object,
// such as Cypress's it('name', { retries: 2 }, ...).
var runnerOptions = regexp.MustCompile(`configure\(|\b(it|test|describe|context|specify|suite)(\.[a-z]+)*\(\s*['"` + "`" + `][^'"` + "`" + `]*['"` + "`" + `]\s*,\s*\{`)

// SignalContext holds the facts of the whole repository that decide whether
// a skip hides a failure: which Go files a build constraint limits to a
// platform, and whether CI runs the Go tests in full. Its zero value knows no
// constraint line and no CI, so every skip it cannot see guarded counts.
type SignalContext struct {
	// Constrained holds the Go files whose //go:build line names a platform.
	Constrained map[string]bool
	// FullSuiteInCI is true when a CI configuration runs the Go tests and
	// none passes -short.
	FullSuiteInCI bool
}

// shortFlag matches the -short flag of go test, on the command line or in
// GOFLAGS, and not --short or -short=false.
var shortFlag = regexp.MustCompile(`(^|[\s='"])-(test\.)?short(=true)?([\s'"]|$)`)

// goTests matches a CI command that runs the Go tests: go test, gotestsum,
// or a putnami command whose task list holds test.
var goTests = regexp.MustCompile(`\bgo test\b|\bgotestsum\b|\bputnami [^\n]*\btest\b`)

// NewSignalContext reads the facts a SignalContext holds: the CI
// configurations among files, read from contents, and the //go:build lines
// of the Go files in signalContents.
func NewSignalContext(files []string, contents, signalContents map[string][]byte) SignalContext {
	configs := ci.Load(files, contents)
	signal := SignalContext{
		Constrained:   map[string]bool{},
		FullSuiteInCI: len(ci.RunsPattern(configs, goTests)) > 0 && len(ci.RunsPattern(configs, shortFlag)) == 0,
	}
	for file, content := range signalContents {
		if strings.HasSuffix(file, ".go") && platformConstraint(content) {
			signal.Constrained[file] = true
		}
	}
	return signal
}

// goPlatforms are the GOOS and GOARCH values, and the unix constraint, a Go
// build constraint can name.
var goPlatforms = regexp.MustCompile(`\b(aix|android|darwin|dragonfly|freebsd|hurd|illumos|ios|js|linux|nacl|netbsd|openbsd|plan9|` +
	`solaris|wasip1|windows|zos|unix|386|amd64|arm|arm64|loong64|mips|mipsle|mips64|mips64le|ppc64|ppc64le|riscv64|s390x|wasm)\b`)

// platformConstraint reports whether a Go file's //go:build line, above its
// package clause, names a platform.
func platformConstraint(content []byte) bool {
	for _, line := range fileLines(content) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			return false
		}
		if constraint, ok := strings.CutPrefix(line, "//go:build "); ok && goPlatforms.MatchString(constraint) {
			return true
		}
	}
	return false
}

// platformSuffix matches a Go file name the go command builds for one
// platform only: name_GOOS_test.go, name_GOARCH_test.go or
// name_GOOS_GOARCH_test.go.
var platformSuffix = regexp.MustCompile(`_(aix|android|darwin|dragonfly|freebsd|hurd|illumos|ios|js|linux|nacl|netbsd|openbsd|plan9|` +
	`solaris|wasip1|windows|zos|386|amd64|arm|arm64|loong64|mips|mipsle|mips64|mips64le|ppc64|ppc64le|riscv64|s390x|wasm)(_test)?\.go$`)

// platformFile reports whether the go command builds file for some
// platforms only, by its name or its //go:build line.
func (s SignalContext) platformFile(file string) bool {
	return platformSuffix.MatchString(path.Base(file)) || s.Constrained[file]
}

func (s SignalContext) guarded(file string, lines []string, i int) bool {
	if GuardedSkip(lines, i) {
		return true
	}
	statement, before, inside, ok := skipContext(lines, i)
	if !ok || stillCounts.MatchString(before+" "+statement) {
		return false
	}
	if s.platformFile(file) {
		return true
	}
	return inside && s.FullSuiteInCI && shortMode.MatchString(before+" "+statement)
}

// CountsSignal reports whether lines[i] of file is a retry, a skip or a
// focused test that can let a failing test pass, with no repository fact:
// it is SignalContext{}.Counts.
func CountsSignal(file string, lines []string, i int) bool {
	return SignalContext{}.Counts(file, lines, i)
}

// Counts reports whether the line contains a test signal that can hide a failure.
func (s SignalContext) Counts(file string, lines []string, i int) bool {
	if i < 0 || i >= len(lines) {
		return false
	}
	line := lines[i]
	language := inventory.Language(file)
	for _, at := range signalMatch.FindAllStringIndex(line, -1) {
		token := line[at[0]:at[1]]
		if languages, ok := frameworkLanguages[token]; ok && language != "" && !languages[language] {
			continue
		}
		if sourceLanguages[language] && inString(line[:at[0]]) {
			continue
		}
		switch token {
		case "t.Skip(", ".skip(", "pytest.mark.skip", "@Disabled":
			if s.guarded(file, lines, i) {
				continue
			}
		case "retries:":
			if testSource(file) && !runnerOptions.MatchString(line) && (i == 0 || !runnerOptions.MatchString(lines[i-1])) {
				continue
			}
		}
		return true
	}
	return false
}

// testSource reports whether file is a test, not a test runner's or a CI
// configuration.
func testSource(file string) bool {
	if !IsTest(file) {
		return false
	}
	base := path.Base(file)
	for _, prefix := range testRunnerConfigs {
		if strings.HasPrefix(base, prefix) {
			return false
		}
	}
	return true
}

// inString reports whether the end of prefix sits inside a string literal
// that opens on the same line. A literal that spans lines is not seen.
func inString(prefix string) bool {
	var quote byte
	for j := 0; j < len(prefix); j++ {
		switch c := prefix[j]; {
		case quote == 0 && (c == '"' || c == '\'' || c == '`'):
			quote = c
		case quote != 0 && c == '\\' && quote != '`':
			j++
		case quote != 0 && c == quote:
			quote = 0
		}
	}
	return quote != 0
}
