// Package skipguard finds TypeScript and JavaScript test code that hides a
// failing test: a focused test (`.only(`, `fit(`, `fdescribe(`), a skip
// without a guard (`.skip(`, `xit(`, `xtest(`, `xdescribe(`), or a guard whose
// condition names flakiness or CI.
//
// A guard is a condition the skip depends on: `test.skipIf(<condition>)`,
// `test.if(<condition>)`, or a `.skip(` that runs inside an if statement, an
// else branch, a switch, a ternary or a short-circuit (`&&`, `||`, `??`). Any
// such condition is a guard unless it names CI or flakiness. A skip that stays
// on purpose carries a reviewed exception on the same line, or on a comment
// line of its own just above:
//
//	// putnami:allow-skip <reason>
//
// The exception keeps the lint green and is reported as a warning, so the
// review stays visible. An exception without a reason is an error.
//
// The scanner reads tokens, not a syntax tree: it skips comments, strings,
// template literals and regular expressions, then matches member calls on a
// test API. A `.skip(` or `.only(` counts when its chain starts at a test API
// name (test, it, describe, suite, bench, context, specify, or a name ending in
// Test), or when it registers a named case (a string first argument and a
// function literal body). On another wrapper, a body passed by reference
// (`wrapper.skip('a', body)`) is not detected. The test name is not a skip reason, so only the guard condition is
// read for flakiness or CI. JSX text is not parsed: an apostrophe in JSX text
// reads as the start of a string to the end of its line.
package skipguard

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Category is the diagnostic category every finding carries.
const Category = "skip-guard"

// Directive marks a reviewed exception. The rest of the comment is its reason.
const Directive = "putnami:allow-skip"

// Finding is one skip or focused test the lint reports. File is absolute.
type Finding struct {
	File     string
	Line     int
	Column   int
	Severity string // "error" or "warning"
	Message  string
}

// testFilePattern matches the test files bun discovers.
var testFilePattern = regexp.MustCompile(`[._](test|spec)\.(ts|tsx|js|jsx|mts|cts|mjs|cjs)$`)

// ciEnvironment holds the environment variables CI services set.
var ciEnvironment = map[string]bool{
	"CI": true, "GITHUB_ACTIONS": true, "GITLAB_CI": true, "BUILDKITE": true,
	"CIRCLECI": true, "JENKINS_URL": true, "TF_BUILD": true,
}

// guardIdentifier matches an identifier in a guard condition that names CI
// (CI, isCI, inCI, onCI, EnvCI, IS_CI) or flakiness (isFlaky, flakyTests).
// The Go scanner uses the same pattern.
var guardIdentifier = regexp.MustCompile(`^CI$|[a-z_]CI$|(?i:^(is|in|on|under|running)_?ci$)|(?i:flak)`)

// testAPINames are the test registration functions of bun, Jest, Vitest and
// Mocha. A name ending in Test (specTest) is a test API too.
var testAPINames = map[string]bool{
	"test": true, "it": true, "describe": true, "suite": true, "bench": true, "context": true, "specify": true,
}

// testModifiers are the members that may follow `.skip` or `.only` on a test
// API, as in `test.skip.each(cases)`.
var testModifiers = map[string]bool{
	"each": true, "if": true, "skipIf": true, "todo": true, "todoIf": true, "concurrent": true,
	"failing": true, "only": true, "skip": true, "serial": true, "sequential": true,
}

// directSkips and directFocuses are the global aliases Jest and bun define:
// `xit('a', fn)` skips a test, `fit('a', fn)` focuses one.
var (
	directSkips   = map[string]bool{"xit": true, "xtest": true, "xdescribe": true}
	directFocuses = map[string]bool{"fit": true, "fdescribe": true}
)

// skippedDirectories are never scanned: dependencies and build output.
var skippedDirectories = map[string]bool{
	"node_modules": true, "dist": true, "out": true, "vendor": true,
}

// ScanProject scans every test file under root, without entering a nested
// Putnami project or a directory git ignores. The task's cache key leaves
// ignored directories out, so the lint must not read them either. Findings
// are sorted by file and position.
func ScanProject(root string) ([]Finding, error) {
	var findings []Finding
	ignored := ignoredDirectories(root)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if entry.IsDir() {
			if path == root {
				return nil
			}
			name := entry.Name()
			if skippedDirectories[name] || strings.HasPrefix(name, ".") || fileExists(filepath.Join(path, "putnami.json")) {
				return filepath.SkipDir
			}
			if rel, relErr := filepath.Rel(root, path); relErr == nil && ignored[filepath.ToSlash(rel)] {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !testFilePattern.MatchString(entry.Name()) || strings.HasSuffix(entry.Name(), ".d.ts") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		findings = append(findings, ScanFile(path, src)...)
		return nil
	})
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Column < findings[j].Column
	})
	return findings, err
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ignoredDirectories returns the directories under root that git ignores
// entirely, as slash-separated paths relative to root. Outside a git
// repository, or without git, it returns nil and nothing is ignored.
func ignoredDirectories(root string) map[string]bool {
	cmd := exec.Command("git", "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	dirs := make(map[string]bool)
	for _, entry := range bytes.Split(output, []byte{0}) {
		if name := string(entry); strings.HasSuffix(name, "/") {
			dirs[strings.TrimSuffix(name, "/")] = true
		}
	}
	return dirs
}

// ScanFile scans one test file.
func ScanFile(path string, src []byte) []Finding {
	tokens, comments := lex(src)
	match := matchBrackets(tokens)
	directives := directiveLines(comments)

	var findings []Finding
	for i, tok := range tokens {
		if tok.kind == punctToken {
			// A bracket member, `test['only'](...)`, calls the same API as
			// `test.only(...)`.
			if access, ok := bracketAccess(tokens, match, i); ok {
				if root, message := memberProblem(tokens, match, tokens[i+1].text, access, i+2); message != "" {
					findings = append(findings, finding(path, tokens, directives, root, i+2, message))
				}
			}
			continue
		}
		if tok.kind != identToken {
			continue
		}
		if i == 0 || !isMemberAccess(tokens[i-1]) {
			if message := directCall(tokens, match, i); message != "" {
				findings = append(findings, finding(path, tokens, directives, i, i, message))
			}
			continue
		}
		if root, message := memberProblem(tokens, match, tok.text, i-1, i); message != "" {
			findings = append(findings, finding(path, tokens, directives, root, i, message))
		}
	}
	return findings
}

// memberProblem returns the start of the expression and why the member name
// hides a test, or "" when it does not. The member access starts at access
// (`.`, `?.` or `[`) and ends at end (the name, or the closing `]`).
func memberProblem(tokens []token, match []int, name string, access, end int) (int, string) {
	root := chainStart(tokens, match, access)
	switch name {
	case "only":
		if isTestRegistration(tokens, match, root, end) {
			return root, "`" + chainText(tokens, root, end) + "` focuses the run, so other tests can stop running; remove `.only`"
		}
	case "skip":
		if isTestRegistration(tokens, match, root, end) {
			return root, skipProblem(tokens, match, root, end)
		}
	case "skipIf", "if", "todoIf":
		open := callOpen(tokens, match, end+1)
		if open < 0 ||
			!isTestAPIName(tokens[root]) && !(registersNamedCase(tokens, match, match[open]+1) && hasFunctionArgument(tokens, match, match[open]+1)) {
			break
		}
		if condition := tokens[open+1 : match[open]]; namesCIOrFlakiness(condition) {
			return root, "`" + chainText(tokens, root, end) + "(" + joinTokens(condition) + ")` is guarded by a CI or flakiness condition, which hides a failing test; fix the test"
		}
		if _, condition := guardContext(tokens, match, root); condition != "" {
			return root, "`" + chainText(tokens, root, end) + "` is guarded by a CI or flakiness condition (" + condition + "), which hides a failing test; fix the test"
		}
	}
	return root, ""
}

// bracketAccess reports whether the bracket at i reads a test member by a
// string key, `test['skip']` or `test?.['only']`, and returns where the
// member access starts. An array literal, `['skip']`, reads nothing.
func bracketAccess(tokens []token, match []int, i int) (int, bool) {
	if i == 0 || match[i] != i+2 {
		return 0, false
	}
	key := tokens[i+1]
	if key.kind != stringToken && key.kind != templateToken || !memberNames[key.text] {
		return 0, false
	}
	access := i
	if tokens[i-1].kind == punctToken && tokens[i-1].text == "?." {
		access = i - 1
	}
	if access == 0 {
		return 0, false
	}
	receiver := tokens[access-1]
	switch receiver.kind {
	case identToken:
		return access, !regexKeywords[receiver.text]
	case punctToken:
		return access, (receiver.text == ")" || receiver.text == "]") && match[access-1] >= 0
	}
	return 0, false
}

// memberNames are the test API members the guard reads.
var memberNames = map[string]bool{"only": true, "skip": true, "skipIf": true, "if": true, "todoIf": true}

// finding builds the report for the member or call at i, whose expression
// starts at root, and applies a reviewed exception.
func finding(path string, tokens []token, directives map[int]directive, root, i int, message string) Finding {
	result := Finding{File: path, Line: tokens[i].line, Column: tokens[i].column, Severity: "error",
		Message: message + ", or record a reviewed exception with `// " + Directive + " <reason>`"}
	if reason, ok := directiveFor(directives, tokens[root].line, tokens[i].line); ok {
		if reason == "" {
			result.Message = "reviewed exception `// " + Directive + "` needs a reason: " + result.Message
		} else {
			result.Severity = "warning"
			result.Message = "reviewed exception: `" + chainText(tokens, root, i) + "` stays because " + reason
		}
	}
	return result
}

// skipProblem returns why the skip whose chain runs from root to i hides a
// test, or "" when a guard holds it.
func skipProblem(tokens []token, match []int, root, i int) string {
	guarded, condition := guardContext(tokens, match, root)
	if condition != "" {
		return "`" + chainText(tokens, root, i) + "` is guarded by a CI or flakiness condition (" + condition + "), which hides a failing test; fix the test"
	}
	if !guarded {
		return "`" + chainText(tokens, root, i) + "` skips unconditionally and hides the test; guard it with `skipIf(<platform or dependency check>)`, or delete the test"
	}
	return ""
}

// directCall reports a global skip or focus alias called as a function,
// `xit('a', fn)` or `fit('a', fn)`, and returns why it hides a test. A declaration (`function xit(`) or a method definition is
// not a call: the first argument of a registration is the test name.
func directCall(tokens []token, match []int, i int) string {
	name := tokens[i].text
	if !directSkips[name] && !directFocuses[name] {
		return ""
	}
	if i > 0 && tokens[i-1].kind == identToken && tokens[i-1].text == "function" {
		return ""
	}
	open := callOpen(tokens, match, i+1)
	if open < 0 || !registersNamedCase(tokens, match, open) || !hasFunctionArgument(tokens, match, open) {
		return ""
	}
	if directFocuses[name] {
		return "`" + name + "` focuses the run, so other tests can stop running; use `" + strings.TrimPrefix(name, "f") + "`"
	}
	return skipProblem(tokens, match, i, i)
}

// isTestRegistration reports whether the `.skip` or `.only` member ending at
// i is a test API: it is called or followed by a test modifier, and its chain
// starts at a test API name or the call registers a named case. A query
// builder's `.skip(10)` or a property named `only` is not one. An uncalled
// `test.skip` handed on to register a test is the API itself.
func isTestRegistration(tokens []token, match []int, root, i int) bool {
	if i+1 >= len(tokens) {
		return handedOn(tokens, match, root, i)
	}
	if open := callOpen(tokens, match, i+1); open >= 0 {
		return isTestAPIName(tokens[root]) || registersNamedCase(tokens, match, open) && hasFunctionArgument(tokens, match, open)
	}
	next := tokens[i+1]
	if next.kind == punctToken {
		switch next.text {
		case ".", "?.":
			return i+2 < len(tokens) && tokens[i+2].kind == identToken && testModifiers[tokens[i+2].text]
		case "[":
			return match[i+1] == i+3 && (tokens[i+2].kind == stringToken || tokens[i+2].kind == templateToken) && testModifiers[tokens[i+2].text]
		}
	}
	return handedOn(tokens, match, root, i)
}

// callOpen returns the parenthesis that opens a call at j, stepping over a
// non-null assertion (`test.skip!(`), an optional call (`test.skip?.(`) and
// type arguments (`test.skip<Ctx>(`); -1 when no call starts there.
func callOpen(tokens []token, match []int, j int) int {
	for j < len(tokens) && tokens[j].kind == punctToken && (tokens[j].text == "!" || tokens[j].text == "?.") {
		j++
	}
	if j < len(tokens) && tokens[j].kind == punctToken && tokens[j].text == "<" {
		depth := 0
	typeArguments:
		for ; j < len(tokens); j++ {
			tok := tokens[j]
			if tok.kind != punctToken {
				continue
			}
			switch tok.text {
			case "(", "[", "{":
				if match[j] < 0 {
					return -1
				}
				j = match[j]
			case "<":
				depth++
			case ">", ">>", ">>>":
				depth -= len(tok.text)
				if depth <= 0 {
					j++
					break typeArguments
				}
			case ";", ")", "]", "}":
				return -1
			}
		}
	}
	if j < len(tokens) && tokens[j].kind == punctToken && tokens[j].text == "(" && match[j] > j {
		return j
	}
	return -1
}

// handedOn reports whether the uncalled test API member whose chain runs
// from root to i is handed on to register tests: assigned, returned, or
// chosen by a condition, as in `const later = test.skip` or
// `(isFlaky ? test.skip : test)('a', fn)`. An argument, `expect(test.skip)`,
// an assignment target, `test.skip = fn`, and `typeof test.skip` are not.
func handedOn(tokens []token, match []int, root, i int) bool {
	if !isTestAPIName(tokens[root]) || root == 0 {
		return false
	}
	if i+1 < len(tokens) && tokens[i+1].kind == punctToken && isAssignment(tokens[i+1].text) {
		return false
	}
	prev := tokens[root-1]
	if prev.kind == identToken {
		return prev.text == "return" || prev.text == "yield" || prev.text == "await"
	}
	if prev.kind != punctToken {
		return false
	}
	switch prev.text {
	case "?", ":", "||", "??", "&&", "=>":
		return true
	case "(":
		return !isCallParen(tokens, match, root-1)
	}
	return isAssignment(prev.text)
}

func isAssignment(text string) bool {
	return text == "=" || strings.HasSuffix(text, "=") && !comparisonOperators[text] && text != "=>"
}

// isCallParen reports whether the parenthesis at open starts the arguments
// of a call rather than a grouping.
func isCallParen(tokens []token, match []int, open int) bool {
	if open == 0 {
		return false
	}
	prev := tokens[open-1]
	switch prev.kind {
	case identToken:
		return !regexKeywords[prev.text] && !controlKeywords[prev.text] || open > 1 && isMemberAccess(tokens[open-2])
	case punctToken:
		return prev.text == ")" && !isControlParen(tokens, match, open-1) || prev.text == "]" || prev.text == "?."
	}
	return false
}

// unaryKeywords start an expression that holds the one after them.
var unaryKeywords = map[string]bool{"new": true, "typeof": true, "delete": true}

// controlKeywords open a statement with a parenthesized head.
var controlKeywords = map[string]bool{"if": true, "while": true, "for": true, "switch": true, "catch": true, "with": true}

// hasFunctionArgument reports whether a later argument of the call whose
// parenthesis opens at open is a function literal: the test body.
// `fit('linear', points)` or `p.skip('(', 1)` has none.
func hasFunctionArgument(tokens []token, match []int, open int) bool {
	closing := match[open]
	for j := open + 1; j < closing; j++ {
		tok := tokens[j]
		if tok.kind == punctToken && (tok.text == "(" || tok.text == "[" || tok.text == "{") && match[j] > j {
			j = match[j]
			continue
		}
		if tok.kind != punctToken || tok.text != "," || j+1 >= closing {
			continue
		}
		next := tokens[j+1]
		switch {
		case next.kind == identToken && (next.text == "function" || next.text == "async"):
			return true
		case next.kind == identToken && j+2 < closing && tokens[j+2].text == "=>":
			return true
		case next.kind == punctToken && next.text == "(" && match[j+1] > j+1 && match[j+1]+1 < closing:
			if arrowAfterParameters(tokens, match, match[j+1]+1, closing) {
				return true
			}
		}
	}
	return false
}

// arrowAfterParameters reports whether the tokens from i, right after an
// arrow function's parameter list, reach `=>`, stepping over a return type
// annotation (`(): void =>`) up to the next argument or the end of the call.
func arrowAfterParameters(tokens []token, match []int, i, closing int) bool {
	if tokens[i].text == "=>" {
		return true
	}
	if tokens[i].kind != punctToken || tokens[i].text != ":" {
		return false
	}
	for j := i + 1; j < closing; j++ {
		tok := tokens[j]
		if tok.kind != punctToken {
			continue
		}
		switch tok.text {
		case "=>":
			return true
		case ",":
			return false
		case "(", "[", "{":
			if match[j] > j {
				j = match[j]
			}
		}
	}
	return false
}

func isTestAPIName(tok token) bool {
	return tok.kind == identToken && (testAPINames[tok.text] || strings.HasSuffix(tok.text, "Test"))
}

// registersNamedCase reports whether the call whose parenthesis opens at open
// takes a string or template name followed by more arguments, as every test
// registration does.
func registersNamedCase(tokens []token, match []int, open int) bool {
	if open >= len(tokens) || tokens[open].kind != punctToken || tokens[open].text != "(" || match[open] < 0 {
		return false
	}
	closing := match[open]
	if open+1 >= closing || (tokens[open+1].kind != stringToken && tokens[open+1].kind != templateToken) {
		return false
	}
	for j := open + 1; j < closing; j++ {
		tok := tokens[j]
		if tok.kind == punctToken && (tok.text == "(" || tok.text == "[" || tok.text == "{") && match[j] > j {
			j = match[j]
			continue
		}
		if tok.kind == punctToken && tok.text == "," {
			return true
		}
	}
	return false
}

func isMemberAccess(tok token) bool {
	return tok.kind == punctToken && (tok.text == "." || tok.text == "?.")
}

// chainStart walks back from the member access at access (`.`, `?.` or `[`)
// over the receiver chain (`describe.each(cases).skip`) and returns the index
// of its first token.
func chainStart(tokens []token, match []int, access int) int {
	start := access + 1
	j := access - 1
	if j >= 0 && tokens[j].text == "?." && tokens[access].text != "?." {
		j-- // an optional call, `test?.(...)`
	}
	for j >= 0 {
		tok := tokens[j]
		if tok.kind == punctToken && (tok.text == ")" || tok.text == "]") && match[j] >= 0 {
			start = match[j]
			j = match[j] - 1
			if tok.text == "]" && j >= 0 && tokens[j].kind == punctToken && tokens[j].text == "?." {
				j--
			}
			continue
		}
		if tok.kind != identToken {
			break
		}
		start = j
		j--
		if j >= 0 && isMemberAccess(tokens[j]) {
			j--
			continue
		}
		break
	}
	return start
}

func chainText(tokens []token, from, to int) string {
	return joinTokens(tokens[from : to+1])
}

func joinTokens(tokens []token) string {
	var builder strings.Builder
	for i, tok := range tokens {
		if i > 0 && tok.kind != punctToken && tokens[i-1].kind != punctToken {
			builder.WriteByte(' ')
		}
		builder.WriteString(tok.display())
	}
	return builder.String()
}

// prefixKeywords may stand between a braceless guard and the expression it
// holds: `if (!hasDb) return ctx.skip();`.
var prefixKeywords = map[string]bool{"return": true, "await": true, "void": true, "yield": true}

// statementPrefix returns the index of the token before the expression at
// root, stepping back over return, await, void and yield; -1 when none.
func statementPrefix(tokens []token, root int) int {
	j := root - 1
	for j >= 0 && tokens[j].kind == identToken && prefixKeywords[tokens[j].text] {
		j--
	}
	return j
}

// guardContext walks out from the expression that starts at root, through
// every expression, statement and block that holds it, to the top of the
// file. It reports whether a condition holds the expression (an if, an else,
// a switch case, a ternary branch, or an operand of `&&`, `||` or `??`), and
// returns the innermost condition that names CI or flakiness and must be
// true for the expression to run. Like an else branch, the second branch of
// a ternary and the right operand of `||` or `??` run when their condition
// is false, so their condition is not read.
func guardContext(tokens []token, match []int, root int) (guarded bool, ciCondition string) {
	check := func(condition []token) bool {
		if namesCIOrFlakiness(condition) {
			ciCondition = joinTokens(condition)
			return true
		}
		return false
	}
	// opener steps out of the bracket at open, which holds the token at
	// inside, and returns where the expression holding the bracket starts.
	opener := func(open, inside int) int {
		if tokens[open].text != "{" {
			if isCallParen(tokens, match, open) {
				return chainStart(tokens, match, open)
			}
			if open > 0 && tokens[open-1].kind == identToken && controlKeywords[tokens[open-1].text] {
				return open - 1
			}
			return open
		}
		if open == 0 {
			return -1
		}
		before := tokens[open-1]
		switch {
		case before.kind == identToken && before.text == "else":
			guarded = true
			return ifBefore(tokens, match, open-1)
		case before.kind == punctToken && before.text == "=>":
			return arrowStart(tokens, match, open-1)
		case before.kind == identToken && (before.text == "try" || before.text == "catch" || before.text == "finally" || before.text == "do"):
			return open - 1
		case before.kind != punctToken || before.text != ")" || match[open-1] < 0:
			return open
		}
		head := match[open-1]
		switch {
		case isKeywordParen(tokens, match, open-1, "if"):
			guarded = true
			if check(tokens[head+1 : open-1]) {
				return -1
			}
		case isKeywordParen(tokens, match, open-1, "switch"):
			guarded = true
			if check(tokens[head+1:open-1]) || check(caseLabels(tokens, match, open, inside)) {
				return -1
			}
		}
		return head - 1
	}
	pos := root
	for steps := 0; pos > 0 && steps <= len(tokens); steps++ {
		p := statementPrefix(tokens, pos)
		if p < 0 {
			return
		}
		tok := tokens[p]
		var next int
		switch {
		case tok.kind == identToken && tok.text == "else":
			guarded = true
			next = ifBefore(tokens, match, p)
		case tok.kind == identToken && (unaryKeywords[tok.text] || tok.text == "do"):
			// `new Runner(...)`, or the braceless body of `do ... while (x)`.
			next = p
		case tok.kind != punctToken:
			next = enclosingOpener(tokens, match, p)
			if next >= 0 {
				next = opener(next, p)
			}
		case tok.text == "(" || tok.text == "[" || tok.text == "{":
			next = opener(p, pos)
		case tok.text == "&&" || tok.text == "?":
			guarded = true
			next = operandStart(tokens, match, p)
			if check(tokens[next:p]) {
				return
			}
		case tok.text == "||" || tok.text == "??":
			guarded = true
			next = operandStart(tokens, match, p)
		case tok.text == ":" && ternaryQuestion(tokens, match, p) >= 0:
			guarded = true
			next = operandStart(tokens, match, ternaryQuestion(tokens, match, p))
		case tok.text == ")" && isKeywordParen(tokens, match, p, "if"):
			guarded = true
			if check(tokens[match[p]+1 : p]) {
				return
			}
			next = match[p] - 1
		case tok.text == ")" && isControlParen(tokens, match, p):
			// The braceless body of a loop: `for (...) test.skip(...)`.
			next = match[p] - 1
		case tok.text == ":" && isLabel(tokens, p):
			next = p - 1
		case isAssignment(tok.text):
			// The assigned value: `x = onWin && test.skip(...)`.
			next = operandStart(tokens, match, p)
		case tok.text == "=>":
			next = arrowStart(tokens, match, p)
		case tok.text == "!" || tok.text == "~" || tok.text == "...":
			next = p
		case tok.text == ",":
			// The next item of a list or a comma expression: `a(), test.skip(...)`.
			// No operator in an earlier item guards this one.
			next = itemStart(tokens, match, p)
		case tok.text == "}" && match[p] > 0 && tokens[pos].kind == identToken && (tokens[pos].text == "catch" || tokens[pos].text == "finally"):
			// A catch or finally clause belongs to the try statement before it.
			next = tryClauseStart(tokens, match, match[p])
		default:
			next = enclosingOpener(tokens, match, p)
			if next >= 0 {
				next = opener(next, p)
			}
		}
		if next < 0 || next >= pos {
			return
		}
		pos = next
	}
	return
}

// enclosingOpener returns the innermost bracket that opens before the token
// at from and closes after it; -1 at the top of the file.
func enclosingOpener(tokens []token, match []int, from int) int {
	for j := from; j >= 0; j-- {
		tok := tokens[j]
		if tok.kind != punctToken {
			continue
		}
		switch tok.text {
		case ")", "]", "}":
			if match[j] >= 0 {
				j = match[j]
			}
		case "(", "[", "{":
			return j
		}
	}
	return -1
}

// itemStart returns where the list item or comma operand that ends before
// the comma at comma starts, stepping over every operator inside it.
func itemStart(tokens []token, match []int, comma int) int {
	start := comma
	for j := comma - 1; j >= 0; j-- {
		tok := tokens[j]
		if tok.kind == punctToken && tok.text == ")" && match[j] >= 0 && isControlParen(tokens, match, j) {
			break
		}
		if tok.kind == punctToken && (tok.text == ")" || tok.text == "]" || tok.text == "}") && match[j] >= 0 {
			j = match[j]
			start = j
			continue
		}
		if tok.kind == punctToken && (tok.text == "," || tok.text == ";" || tok.text == "=>" || tok.text == "(" || tok.text == "[" || tok.text == "{" || isAssignment(tok.text)) ||
			tok.kind == punctToken && tok.text == ":" && ternaryQuestion(tokens, match, j) < 0 ||
			tok.kind == identToken && (operandKeywords[tok.text] || declarationKeywords[tok.text]) {
			break
		}
		start = j
	}
	return start
}

// tryClauseStart returns where the try statement starts whose block, or
// catch block, opens at open: `try {...} catch (e) {...} finally {...}`.
func tryClauseStart(tokens []token, match []int, open int) int {
	for open > 0 {
		before := open - 1
		switch {
		case tokens[before].kind == identToken && tokens[before].text == "try":
			return before
		case tokens[before].kind == identToken && tokens[before].text == "catch" && before > 0 && tokens[before-1].text == "}" && match[before-1] >= 0:
			open = match[before-1]
		case tokens[before].text == ")" && isKeywordParen(tokens, match, before, "catch") && match[before] > 1 && tokens[match[before]-2].text == "}":
			open = match[match[before]-2]
		default:
			return open
		}
		if open < 0 {
			return -1
		}
	}
	return open
}

// operandStart returns where the operand that ends before the operator at
// op starts.
func operandStart(tokens []token, match []int, op int) int {
	start := op
	for j := op - 1; j >= 0; j-- {
		tok := tokens[j]
		if tok.kind == punctToken && tok.text == ")" && match[j] >= 0 && isControlParen(tokens, match, j) {
			break
		}
		if tok.kind == punctToken && (tok.text == ")" || tok.text == "]") && match[j] >= 0 {
			j = match[j]
			start = j
			continue
		}
		if endsOperand(tok) || declarationKeywords[tok.text] && tok.kind == identToken || lineBreakEnds(tokens[j], tokens[start]) && start != op {
			break
		}
		start = j
	}
	return start
}

// declarationKeywords start a statement.
var declarationKeywords = map[string]bool{"const": true, "let": true, "var": true}

// lineBreakEnds reports whether a line break between prev and next ends a
// statement, as it does without semicolons: `a = b` and `const c = d` on two
// lines.
func lineBreakEnds(prev, next token) bool {
	if prev.line >= next.line {
		return false
	}
	endsExpression := prev.kind != punctToken && !operandKeywords[prev.text] || prev.text == ")" || prev.text == "]"
	return endsExpression && next.kind != punctToken
}

// isControlParen reports whether the closing parenthesis at closing ends the
// head of an if, a loop, a switch, a catch or a with statement.
func isControlParen(tokens []token, match []int, closing int) bool {
	open := match[closing]
	return open > 0 && open < closing && tokens[open-1].kind == identToken && controlKeywords[tokens[open-1].text] &&
		(open < 2 || !isMemberAccess(tokens[open-2]))
}

// isLabel reports whether the colon at colon ends a statement label,
// `outer: for (...)`, rather than a ternary branch or a case label.
func isLabel(tokens []token, colon int) bool {
	if colon == 0 || tokens[colon-1].kind != identToken || operandKeywords[tokens[colon-1].text] || tokens[colon-1].text == "default" {
		return false
	}
	if colon == 1 {
		return true
	}
	prev := tokens[colon-2]
	return prev.kind == punctToken && (prev.text == ";" || prev.text == "{" || prev.text == "}" || prev.text == ")") ||
		prev.kind == identToken && prev.text == "else"
}

// ternaryQuestion returns the `?` whose second branch starts after the colon
// at colon; -1 when the colon ends a case label, a property name or a type.
func ternaryQuestion(tokens []token, match []int, colon int) int {
	colons := 0
	for j := colon - 1; j >= 0; j-- {
		tok := tokens[j]
		if tok.kind == identToken && tok.text == "case" {
			return -1
		}
		if tok.kind != punctToken {
			continue
		}
		switch tok.text {
		case ")", "]", "}":
			if match[j] < 0 {
				return -1
			}
			j = match[j]
		case ":":
			colons++
		case "?":
			if colons == 0 {
				return j
			}
			colons--
		case ";", ",", "(", "[", "{", "=>":
			return -1
		}
	}
	return -1
}

// ifBefore returns where the if statement whose else is at elseAt starts, or
// the start of the statement that holds it.
func ifBefore(tokens []token, match []int, elseAt int) int {
	if end := elseAt - 1; end > 0 && tokens[end].text == "}" && match[end] > 0 {
		if head := match[end] - 1; tokens[head].text == ")" && isKeywordParen(tokens, match, head, "if") {
			return match[head] - 1
		}
	}
	if end := elseAt - 1; end > 0 && tokens[end].text == ";" {
		// A braceless then-statement: `if (a) x(); else test.skip(...)`.
		for j := end - 1; j >= 0; j-- {
			tok := tokens[j]
			if tok.kind != punctToken {
				continue
			}
			if tok.text == ")" && match[j] >= 0 && isKeywordParen(tokens, match, j, "if") {
				return match[j] - 1
			}
			if (tok.text == ")" || tok.text == "]" || tok.text == "}") && match[j] >= 0 {
				j = match[j]
				continue
			}
			if tok.text == ";" || tok.text == "(" || tok.text == "[" || tok.text == "{" {
				break
			}
		}
	}
	if open := enclosingOpener(tokens, match, elseAt); open >= 0 {
		return open + 1
	}
	return -1
}

// arrowStart returns where the arrow function whose `=>` is at arrow starts:
// its parameter list, or its single parameter.
func arrowStart(tokens []token, match []int, arrow int) int {
	j := arrow - 1
	if j < 0 {
		return -1
	}
	switch {
	case tokens[j].text == ")" && match[j] >= 0:
		j = match[j]
	case tokens[j].kind == identToken:
	default:
		if open := enclosingOpener(tokens, match, arrow); open >= 0 {
			return open + 1
		}
		return -1
	}
	if j > 0 && tokens[j-1].kind == identToken && tokens[j-1].text == "async" {
		j--
	}
	return j
}

// caseLabels returns the labels of the switch clause that holds the token at
// inside, in the switch body whose brace opens at open, with the labels of
// every clause that falls through into it; nil for a default clause.
func caseLabels(tokens []token, match []int, open, inside int) []token {
	var labels []token
	ended := false
	for j := open + 1; j < inside; j++ {
		tok := tokens[j]
		if tok.kind == punctToken && (tok.text == "(" || tok.text == "[" || tok.text == "{") {
			if match[j] < 0 || match[j] >= inside {
				break
			}
			if tok.text == "{" && plainBlock(tokens, j, open) && terminates(tokens, match, j) {
				ended = true
			}
			j = match[j]
			continue
		}
		if tok.kind != identToken {
			continue
		}
		switch tok.text {
		case "break", "return", "throw", "continue":
			if unconditional(tokens, match, j, open) {
				ended = true
			}
		case "case", "default":
			if ended {
				labels, ended = nil, false
			}
			if tok.text == "default" {
				continue
			}
			colon := j + 1
			for colon < inside && (tokens[colon].kind != punctToken || tokens[colon].text != ":") {
				if match[colon] > colon {
					colon = match[colon]
				}
				colon++
			}
			labels = append(labels, tokens[j+1:colon]...)
			j = colon
		}
	}
	return labels
}

// unconditional reports whether the statement starting at j, inside the
// block that opens at open, runs whenever the statement before it does: it
// is not the braceless body of an if, an else, a loop or a do.
func unconditional(tokens []token, match []int, j, open int) bool {
	if j-1 <= open {
		return true
	}
	prev := tokens[j-1]
	switch {
	case prev.kind == identToken:
		return prev.text != "else" && prev.text != "do"
	case prev.kind == punctToken && prev.text == ")" && match[j-1] >= 0:
		return !isControlParen(tokens, match, j-1)
	}
	return true
}

// plainBlock reports whether the brace at j, inside the block that opens at
// open, opens a statement block of its own, not a function body, an object
// or the body of an if, an else or a loop.
func plainBlock(tokens []token, j, open int) bool {
	if j-1 <= open {
		return true
	}
	prev := tokens[j-1]
	return prev.kind == punctToken && (prev.text == ";" || prev.text == ":" || prev.text == "{" || prev.text == "}")
}

// terminates reports whether the plain block that opens at open always runs
// a break, return, throw or continue.
func terminates(tokens []token, match []int, open int) bool {
	for j := open + 1; j < match[open]; j++ {
		tok := tokens[j]
		if tok.kind == punctToken && (tok.text == "(" || tok.text == "[" || tok.text == "{") {
			if match[j] < 0 {
				return false
			}
			if tok.text == "{" && plainBlock(tokens, j, open) && terminates(tokens, match, j) {
				return true
			}
			j = match[j]
			continue
		}
		if tok.kind == identToken && (tok.text == "break" || tok.text == "return" || tok.text == "throw" || tok.text == "continue") &&
			unconditional(tokens, match, j, open) {
			return true
		}
	}
	return false
}

// endsOperand reports whether tok, walking back, ends the operand of an
// `&&` or a ternary test: a lower-precedence operator, a separator, an
// assignment, an opening bracket or a statement keyword.
func endsOperand(tok token) bool {
	if tok.kind == identToken {
		return operandKeywords[tok.text]
	}
	if tok.kind != punctToken {
		return false
	}
	switch tok.text {
	case "||", "??", "?", ":", ";", ",", "=>", "(", "[", "{", "}":
		return true
	}
	return tok.text == "=" || strings.HasSuffix(tok.text, "=") && !comparisonOperators[tok.text]
}

var operandKeywords = map[string]bool{"return": true, "yield": true, "else": true, "case": true, "throw": true, "do": true}

var comparisonOperators = map[string]bool{"==": true, "===": true, "!=": true, "!==": true, "<=": true, ">=": true}

// isKeywordParen reports whether the closing parenthesis at closing belongs
// to `keyword (...)`.
func isKeywordParen(tokens []token, match []int, closing int, keyword string) bool {
	open := match[closing]
	return open > 0 && open < closing && tokens[open-1].kind == identToken && tokens[open-1].text == keyword &&
		(open < 2 || !isMemberAccess(tokens[open-2])) // `p.catch(...)` is a method call
}

func namesCIOrFlakiness(tokens []token) bool {
	for _, tok := range tokens {
		switch tok.kind {
		case identToken:
			if ciEnvironment[tok.text] || guardIdentifier.MatchString(tok.text) {
				return true
			}
		case stringToken:
			if ciEnvironment[tok.text] || strings.Contains(strings.ToLower(tok.text), "flak") {
				return true
			}
		}
	}
	return false
}

// matchBrackets pairs every bracket with its partner; -1 marks an unmatched one.
func matchBrackets(tokens []token) []int {
	match := make([]int, len(tokens))
	var open []int
	for i, tok := range tokens {
		match[i] = -1
		if tok.kind != punctToken {
			continue
		}
		switch tok.text {
		case "(", "[", "{":
			open = append(open, i)
		case ")", "]", "}":
			if n := len(open); n > 0 {
				match[i] = open[n-1]
				match[open[n-1]] = i
				open = open[:n-1]
			}
		}
	}
	return match
}

// directive is one reviewed-exception comment. A standalone one has no code
// before it on its line.
type directive struct {
	reason     string
	standalone bool
}

// directiveLines maps the last line of each reviewed-exception comment to its
// reason.
func directiveLines(comments []comment) map[int]directive {
	directives := make(map[int]directive)
	for _, c := range comments {
		text := strings.TrimSpace(c.text)
		if !strings.HasPrefix(text, Directive) {
			continue
		}
		rest := strings.TrimPrefix(text, Directive)
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' && rest[0] != ':' {
			continue
		}
		directives[c.endLine] = directive{
			reason:     strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ":")),
			standalone: c.standalone,
		}
	}
	return directives
}

// directiveFor finds a reviewed exception on the line the expression starts
// or the member is named, or a standalone one on the line above either. A
// trailing exception covers its own line only.
func directiveFor(directives map[int]directive, lines ...int) (string, bool) {
	for _, line := range lines {
		if d, ok := directives[line]; ok {
			return d.reason, true
		}
		if d, ok := directives[line-1]; ok && d.standalone {
			return d.reason, true
		}
	}
	return "", false
}
