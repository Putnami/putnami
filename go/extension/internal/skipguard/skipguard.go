// Package skipguard finds Go test code that hides a failing test: a t.Skip,
// t.Skipf or t.SkipNow that runs unconditionally, or one whose reason or guard
// names flakiness or CI.
//
// A skip inside an if statement, a switch case or a select case is guarded:
// it runs only when a condition holds, such as a platform check
// (runtime.GOOS), a missing dependency or testing.Short(). Any such condition
// is a guard unless it names CI or flakiness. A skip in a file that some
// platform does not build (a //go:build or // +build line such as linux,
// !windows, unix or cgo, or a _windows or _arm64 file name suffix) is guarded
// by that constraint. Another build tag, such as integration, is not a guard,
// and neither is `integration || linux`, which every platform can build. A
// `//go:build ignore` file is never compiled, so it is not scanned. A
// skip that stays on purpose carries a reviewed exception on the same line,
// or on a comment line of its own just above:
//
//	//putnami:allow-skip <reason>
//
// The exception keeps the lint green and is reported as a warning, so the
// review stays visible. An exception without a reason is an error.
package skipguard

import (
	"bytes"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Category is the diagnostic category every finding carries.
const Category = "skip-guard"

// Directive marks a reviewed exception. The rest of the comment is its reason.
const Directive = "putnami:allow-skip"

// Finding is one skip the lint reports. File is absolute.
type Finding struct {
	File     string
	Line     int
	Column   int
	Severity string // "error" or "warning"
	Message  string
}

// reasonPattern matches a skip reason that names flakiness or CI.
var reasonPattern = regexp.MustCompile(`(?i)\bflak(e|es|ed|y|ily|iness)\b|\bci\b|\bgithub actions\b|\bcontinuous integration\b`)

// ciEnvironment holds the environment variables CI services set.
var ciEnvironment = map[string]bool{
	"CI": true, "GITHUB_ACTIONS": true, "GITLAB_CI": true, "BUILDKITE": true,
	"CIRCLECI": true, "JENKINS_URL": true, "TF_BUILD": true,
}

// guardIdentifier matches an identifier in a guard condition that names CI
// (CI, isCI, inCI, onCI, EnvCI, IS_CI) or flakiness (isFlaky, flakyTests).
// The TypeScript scanner uses the same pattern.
var guardIdentifier = regexp.MustCompile(`^CI$|[a-z_]CI$|(?i:^(is|in|on|under|running)_?ci$)|(?i:flak)`)

// skippedDirectories are never scanned: Go ignores testdata, vendor and
// directories whose name starts with "." or "_".
var skippedDirectories = map[string]bool{"testdata": true, "vendor": true, "node_modules": true}

// ScanProject scans every _test.go file under root, without entering a nested
// module (a directory with its own go.mod), a nested Putnami project or a
// directory git ignores. The task's cache key leaves ignored directories out,
// so the lint must not read them either. Findings are sorted by file and
// position.
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
			if skippedDirectories[name] || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				fileExists(filepath.Join(path, "go.mod")) || fileExists(filepath.Join(path, "putnami.json")) {
				return filepath.SkipDir
			}
			if rel, relErr := filepath.Rel(root, path); relErr == nil && ignored[filepath.ToSlash(rel)] {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), "_test.go") {
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

// ScanFile scans one Go test file. A file that does not parse yields no
// finding: the compiler and golangci-lint report it.
func ScanFile(path string, src []byte) []Finding {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	if neverBuilt(file) {
		return nil
	}
	testing := testingImportName(file)
	directives := directiveLines(fset, file, src)
	constrained := hasBuildConstraint(path, file)

	var findings []Finding
	var stack []ast.Node
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		stack = append(stack, node)
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isSkipMethod(selector.Sel.Name) || !isTestingReceiver(selector.X, stack, testing) {
			return true
		}
		message := problem(call, stack, constrained)
		if message == "" {
			return true
		}
		position := fset.Position(call.Pos())
		finding := Finding{File: path, Line: position.Line, Column: position.Column, Severity: "error", Message: message}
		if reason, ok := directiveFor(directives, position.Line); ok {
			if reason == "" {
				finding.Message = "reviewed exception `//" + Directive + "` needs a reason: " + message
			} else {
				finding.Severity = "warning"
				finding.Message = "reviewed exception: " + types.ExprString(call.Fun) + " stays because " + reason
			}
		}
		findings = append(findings, finding)
		return true
	})
	return findings
}

func isSkipMethod(name string) bool {
	return name == "Skip" || name == "Skipf" || name == "SkipNow"
}

// problem returns why the skip hides a test, or "" when it is a legitimate
// guard. constrained reports a file-level build constraint.
func problem(call *ast.CallExpr, stack []ast.Node, constrained bool) string {
	name := types.ExprString(call.Fun)
	fix := "; fix the test, or record a reviewed exception with `//" + Directive + " <reason>`"
	if reason := skipReason(call); reasonPattern.MatchString(reason) {
		return name + " hides a test because it is flaky or fails in CI (" + strconv.Quote(reason) + ")" + fix
	}
	guarded := constrained
	for i := len(stack) - 2; i >= 0; i-- {
		if _, ok := stack[i].(*ast.FuncDecl); ok {
			break
		}
		var conditions []ast.Expr
		switch node := stack[i].(type) {
		case *ast.IfStmt:
			guarded = true
			// The else branch runs when the condition is false: a skip there
			// is not guarded by what the condition names.
			if stack[i+1] == node.Body {
				conditions = append(conditions, node.Cond)
			}
		case *ast.CaseClause:
			guarded = true
			conditions = append(conditions, node.List...)
			if i >= 2 {
				if outer, ok := stack[i-2].(*ast.SwitchStmt); ok && outer.Tag != nil {
					conditions = append(conditions, outer.Tag)
				}
			}
		case *ast.CommClause:
			guarded = true
		}
		for _, condition := range conditions {
			if namesCIOrFlakiness(condition) {
				return name + " is guarded by a CI or flakiness condition (" + types.ExprString(condition) + "), which hides a failing test" + fix
			}
		}
	}
	if guarded {
		return ""
	}
	return name + " runs unconditionally and hides the test; guard it with a platform, dependency or testing.Short() check, or delete the test" +
		", or record a reviewed exception with `//" + Directive + " <reason>`"
}

// skipReason concatenates the string constants passed to the skip.
func skipReason(call *ast.CallExpr) string {
	var parts []string
	for _, arg := range call.Args {
		if text, ok := constantString(arg); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

func constantString(expr ast.Expr) (string, bool) {
	switch node := expr.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(node.Value)
		return text, err == nil
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return "", false
		}
		left, leftOK := constantString(node.X)
		right, rightOK := constantString(node.Y)
		return left + right, leftOK || rightOK
	case *ast.ParenExpr:
		return constantString(node.X)
	}
	return "", false
}

func namesCIOrFlakiness(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.BasicLit:
			if text, ok := constantString(node); ok && (ciEnvironment[text] || strings.Contains(strings.ToLower(text), "flak")) {
				found = true
			}
		case *ast.Ident:
			if ciEnvironment[node.Name] || guardIdentifier.MatchString(node.Name) {
				found = true
			}
		}
		return !found
	})
	return found
}

// isTestingReceiver reports whether expr is a *testing.T, *testing.B,
// *testing.F or testing.TB parameter of an enclosing function, or a
// testify-style suite's T() call.
func isTestingReceiver(expr ast.Expr, stack []ast.Node, testing string) bool {
	switch node := expr.(type) {
	case *ast.CallExpr:
		selector, ok := node.Fun.(*ast.SelectorExpr)
		return ok && selector.Sel.Name == "T" && len(node.Args) == 0
	case *ast.Ident:
		if testing == "" {
			return false
		}
		for i := len(stack) - 1; i >= 0; i-- {
			var params *ast.FieldList
			switch fn := stack[i].(type) {
			case *ast.FuncLit:
				params = fn.Type.Params
			case *ast.FuncDecl:
				params = fn.Type.Params
			default:
				continue
			}
			if declared, ok := parameterType(params, node.Name); ok {
				return isTestingType(declared, testing)
			}
		}
	}
	return false
}

func parameterType(params *ast.FieldList, name string) (ast.Expr, bool) {
	if params == nil {
		return nil, false
	}
	for _, field := range params.List {
		for _, ident := range field.Names {
			if ident.Name == name {
				return field.Type, true
			}
		}
	}
	return nil, false
}

func isTestingType(expr ast.Expr, testing string) bool {
	pointer := false
	if star, ok := expr.(*ast.StarExpr); ok {
		pointer = true
		expr = star.X
	}
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || pkg.Name != testing {
		return false
	}
	switch selector.Sel.Name {
	case "T", "B", "F":
		return pointer
	case "TB":
		return !pointer
	}
	return false
}

// knownPlatforms holds the GOOS and GOARCH values a file name suffix
// constrains, as go/build reads them.
var knownPlatforms = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true,
	"illumos": true, "ios": true, "js": true, "linux": true, "nacl": true, "netbsd": true,
	"openbsd": true, "plan9": true, "solaris": true, "wasip1": true, "windows": true, "zos": true,
	"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true, "arm64": true,
	"arm64be": true, "loong64": true, "mips": true, "mipsle": true, "mips64": true, "mips64le": true,
	"mips64p32": true, "mips64p32le": true, "ppc": true, "ppc64": true, "ppc64le": true,
	"riscv": true, "riscv64": true, "s390": true, "s390x": true, "sparc": true, "sparc64": true,
	"wasm": true,
}

// constraintTags are the build tags, besides GOOS and GOARCH values, that
// restrict a file to some platforms.
var constraintTags = map[string]bool{"unix": true, "cgo": true}

// hasBuildConstraint reports whether the file compiles only on some
// platforms: a //go:build or // +build line before the package clause that
// names a GOOS, a GOARCH, unix or cgo, or a GOOS or GOARCH file name suffix.
func hasBuildConstraint(path string, file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) && !constraint.IsPlusBuild(comment.Text) {
				continue
			}
			expr, err := constraint.Parse(comment.Text)
			if err == nil && namesPlatform(expr) {
				return true
			}
		}
	}
	parts := strings.Split(strings.TrimSuffix(filepath.Base(path), "_test.go"), "_")
	for i := len(parts) - 1; i >= 1 && i >= len(parts)-2; i-- {
		if knownPlatforms[parts[i]] {
			return true
		}
	}
	return false
}

// neverBuilt reports a file excluded by the `//go:build ignore` convention:
// go test never compiles it, so its skips never run.
func neverBuilt(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) && !constraint.IsPlusBuild(comment.Text) {
				continue
			}
			if expr, err := constraint.Parse(comment.Text); err == nil {
				if tag, ok := expr.(*constraint.TagExpr); ok && tag.Tag == "ignore" {
					return true
				}
			}
		}
	}
	return false
}

// namesPlatform reports whether some platform does not build the file, with
// every other tag set: `linux` and `!windows` do, `integration || linux` and
// `go1.22` do not. It evaluates the expression with no platform tag set, then
// with each platform tag it names set alone.
func namesPlatform(expr constraint.Expr) bool {
	var platforms []string
	expr.Eval(func(tag string) bool {
		if knownPlatforms[tag] || constraintTags[tag] {
			platforms = append(platforms, tag)
		}
		return false
	})
	if len(platforms) == 0 {
		return false
	}
	for _, only := range append([]string{""}, platforms...) {
		builds := expr.Eval(func(tag string) bool {
			if knownPlatforms[tag] || constraintTags[tag] {
				return tag == only
			}
			return true
		})
		if !builds {
			return true
		}
	}
	return false
}

// testingImportName returns the local name of the "testing" import, or "".
func testingImportName(file *ast.File) string {
	for _, spec := range file.Imports {
		if spec.Path.Value != `"testing"` {
			continue
		}
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				return ""
			}
			return spec.Name.Name
		}
		return "testing"
	}
	return ""
}

// directive is one reviewed-exception comment. A standalone one has no code
// before it on its line.
type directive struct {
	reason     string
	standalone bool
}

// directiveLines maps the last line of each reviewed-exception comment to its
// reason.
func directiveLines(fset *token.FileSet, file *ast.File, src []byte) map[int]directive {
	directives := make(map[int]directive)
	for _, group := range file.Comments {
		for _, comment := range group.List {
			text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
			if !strings.HasPrefix(text, Directive) {
				continue
			}
			rest := strings.TrimPrefix(text, Directive)
			if rest != "" && rest[0] != ' ' && rest[0] != '\t' && rest[0] != ':' {
				continue
			}
			start := fset.Position(comment.Pos())
			directives[fset.Position(comment.End()).Line] = directive{
				reason:     strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ":")),
				standalone: blankBefore(src, start.Offset),
			}
		}
	}
	return directives
}

// blankBefore reports whether only spaces and tabs precede offset on its line.
func blankBefore(src []byte, offset int) bool {
	for i := offset - 1; i >= 0 && src[i] != '\n'; i-- {
		if src[i] != ' ' && src[i] != '\t' {
			return false
		}
	}
	return true
}

// directiveFor finds a reviewed exception on the skip's line, or a standalone
// one on the line above. A trailing exception covers its own line only.
func directiveFor(directives map[int]directive, line int) (string, bool) {
	if d, ok := directives[line]; ok {
		return d.reason, true
	}
	if d, ok := directives[line-1]; ok && d.standalone {
		return d.reason, true
	}
	return "", false
}
