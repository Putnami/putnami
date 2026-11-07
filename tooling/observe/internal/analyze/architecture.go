package analyze

import (
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	readers "go.putnami.dev/evidence/readers"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/reports"
)

// CodeArchitectureBounded is a run whose architecture rules or violations
// exceed the evidence bounds.
const CodeArchitectureBounded = "observe.architecture_bounded"

// maxSourceBytes caps what is read of a test source to find an ArchUnit
// import.
const maxSourceBytes = 1 << 20

// architecture merges the rules of every architecture result: one tool and
// rule read twice is one rule, broken when either is, its counts summed.
type architecture struct {
	order []string
	rules map[string]*evidence.ArchitectureRule
	// dropped counts the violations and rules left out by the bounds.
	droppedViolations, droppedRules int
}

func newArchitecture() *architecture {
	return &architecture{rules: map[string]*evidence.ArchitectureRule{}}
}

// add merges one rule a reader read, each violation placed by place.
func (a *architecture) add(tool string, rule readers.ArchitectureRule, place func(readers.Violation) evidence.ArchitectureViolation) {
	name := bounded(strings.TrimSpace(rule.Name))
	if name == "" {
		name = "(unnamed)"
	}
	key := tool + "\x00" + name
	entry := a.rules[key]
	if entry == nil {
		if len(a.order) == evidence.MaxArchitectureRules {
			a.droppedRules++
			return
		}
		entry = &evidence.ArchitectureRule{Tool: tool, Rule: name, Status: evidence.ArchitectureKept}
		a.rules[key] = entry
		a.order = append(a.order, key)
	}
	if rule.Status != readers.RuleBroken {
		return
	}
	entry.Status = evidence.ArchitectureBroken
	entry.ViolationCount += max(rule.Count, len(rule.Violations), 1)
	for _, v := range rule.Violations {
		if len(entry.Violations) == evidence.MaxViolations {
			a.droppedViolations++
			continue
		}
		entry.Violations = append(entry.Violations, place(v))
	}
}

func (a *architecture) result() []evidence.ArchitectureRule {
	if len(a.order) == 0 {
		return nil
	}
	out := make([]evidence.ArchitectureRule, 0, len(a.order))
	for _, key := range a.order {
		out = append(out, *a.rules[key])
	}
	return out
}

// readArchitecture reads one deptrac or import-linter report and places its
// violations.
func (r *Result) readArchitecture(report reports.Report, abs string, m *mapper, arch *architecture) {
	var tool string
	var read func([]byte) (readers.ArchitectureReport, []diag.Diagnostic)
	switch report.Format {
	case reports.FormatDeptrac:
		tool, read = evidence.ToolDeptrac, readers.ReadDeptrac
	case reports.FormatImportLinter:
		tool, read = evidence.ToolImportLinter, readers.ReadImportLinter
	default:
		return
	}
	result, diags := readers.ReadFile(abs, read)
	if r.unread(report, diags) {
		return
	}
	for _, rule := range result.Rules {
		arch.add(tool, rule, func(v readers.Violation) evidence.ArchitectureViolation {
			file := ""
			if tool == evidence.ToolDeptrac {
				file = m.phpClassFile(v.Subject)
			} else {
				file = m.pythonModuleFile(v.Subject)
			}
			return m.violation(v, file)
		})
	}
}

// archUnitCase reads a JUnit case as an ArchUnit rule when its test source
// imports ArchUnit or its class name matches a pattern of in, and reports
// whether it did.
func (r *Result) archUnitCase(in Input, report reports.Report, c readers.TestCase, file string, m *mapper, arch *architecture) bool {
	if !matchesClass(in.ArchUnitClasses, c.Classname) && (file == "" || !m.usesArchUnit(file)) {
		return false
	}
	rule, ok := readers.ArchUnitRule(c)
	if !ok {
		return true
	}
	arch.add(evidence.ToolArchUnit, rule, func(v readers.Violation) evidence.ArchitectureViolation {
		return m.violation(v, m.jvmClassFile(report.Path, v.Subject, v.File))
	})
	return true
}

// matchesClass reports whether a class name matches one of the patterns:
// shell patterns where * matches any run of characters, dots included.
func matchesClass(patterns []string, classname string) bool {
	if classname == "" {
		return false
	}
	for _, pattern := range patterns {
		// path.Match stops * at a slash; a class name has none.
		if ok, err := path.Match(pattern, classname); err == nil && ok {
			return true
		}
	}
	return false
}

// violation places a violation on the file the caller located and the
// component that holds it. A line needs a file.
func (m *mapper) violation(v readers.Violation, file string) evidence.ArchitectureViolation {
	out := evidence.ArchitectureViolation{Message: bounded(strings.TrimSpace(v.Message))}
	if out.Message == "" {
		out.Message = "(no message)"
	}
	if file == "" {
		return out
	}
	out.File = file
	if v.Line > 0 {
		out.Line = v.Line
	}
	if owner, ok := m.set.Owner(file); ok {
		out.Component = owner
	}
	return out
}

// usesArchUnit reports whether a JVM test source imports ArchUnit.
func (m *mapper) usesArchUnit(file string) bool {
	if known, ok := m.archUnit[file]; ok {
		return known
	}
	uses := false
	if ext := path.Ext(file); ext == ".java" || ext == ".kt" {
		if f, err := os.Open(filepath.Join(m.root, filepath.FromSlash(file))); err == nil {
			data, _ := io.ReadAll(io.LimitReader(f, maxSourceBytes))
			f.Close()
			uses = readers.UsesArchUnit(data)
		}
	}
	m.archUnit[file] = uses
	return uses
}

// jvmClassFile finds the source file of a JVM class under the main, then the
// test, source roots: the class's package path and the file name ArchUnit
// states, else the class name as a .java or .kt file.
func (m *mapper) jvmClassFile(reportPath, class, fileName string) string {
	if class == "" {
		return ""
	}
	candidates := jvmClassFiles(class)
	if fileName != "" && !strings.ContainsAny(fileName, `/\`) {
		pkg := ""
		if dot := strings.LastIndex(class, "."); dot > 0 {
			pkg = strings.ReplaceAll(class[:dot], ".", "/") + "/"
		}
		candidates = append([]string{pkg + fileName}, candidates...)
	}
	for _, roots := range [][]string{jvmMainRoots, jvmTestRoots} {
		dirs := m.sourceRoots(reportPath, roots)
		for _, candidate := range candidates {
			if rel, ok := m.underRoots(dirs, candidate); ok {
				return rel
			}
		}
	}
	return ""
}

// psr4Root is one PSR-4 autoload entry: a namespace prefix and a directory.
type psr4Root struct {
	prefix, dir string
}

// phpClassFile finds the source file of a PHP class through the PSR-4
// autoload prefixes of the composer.json files of the repository root and of
// the components, longest prefix first.
func (m *mapper) phpClassFile(class string) string {
	class = strings.TrimLeft(strings.TrimSpace(class), `\`)
	if class == "" {
		return ""
	}
	for _, root := range m.psr4Roots() {
		rest, ok := strings.CutPrefix(class, root.prefix)
		if !ok || rest == "" {
			continue
		}
		candidate := joinDir(root.dir, strings.ReplaceAll(rest, `\`, "/")+".php")
		if validRepositoryPath(candidate) && m.isFile(candidate) {
			return candidate
		}
	}
	return ""
}

func (m *mapper) psr4Roots() []psr4Root {
	if m.psr4 != nil {
		return m.psr4
	}
	m.psr4 = []psr4Root{}
	dirs := []string{"."}
	for _, c := range m.set.Components {
		if c.ID != "." {
			dirs = append(dirs, c.ID)
		}
	}
	for _, dir := range dirs {
		data, err := os.ReadFile(filepath.Join(m.root, filepath.FromSlash(dir), "composer.json"))
		if err != nil {
			continue
		}
		var manifest struct {
			Autoload struct {
				PSR4 map[string]json.RawMessage `json:"psr-4"`
			} `json:"autoload"`
			AutoloadDev struct {
				PSR4 map[string]json.RawMessage `json:"psr-4"`
			} `json:"autoload-dev"`
		}
		if json.Unmarshal(data, &manifest) != nil {
			continue
		}
		for _, entries := range []map[string]json.RawMessage{manifest.Autoload.PSR4, manifest.AutoloadDev.PSR4} {
			for prefix, raw := range entries {
				var paths []string
				var one string
				if json.Unmarshal(raw, &one) == nil {
					paths = []string{one}
				} else if json.Unmarshal(raw, &paths) != nil {
					continue
				}
				for _, p := range paths {
					p = strings.Trim(strings.ReplaceAll(p, `\`, "/"), "/")
					if p == "" || p == "." {
						p = "."
					}
					target := dir
					if p != "." {
						target = path.Clean(joinDir(dir, p))
					}
					m.psr4 = append(m.psr4, psr4Root{prefix: strings.TrimLeft(prefix, `\`), dir: target})
				}
			}
		}
	}
	sort.SliceStable(m.psr4, func(i, j int) bool {
		if len(m.psr4[i].prefix) != len(m.psr4[j].prefix) {
			return len(m.psr4[i].prefix) > len(m.psr4[j].prefix)
		}
		if m.psr4[i].prefix != m.psr4[j].prefix {
			return m.psr4[i].prefix < m.psr4[j].prefix
		}
		return m.psr4[i].dir < m.psr4[j].dir
	})
	return m.psr4
}

// pythonModuleFile finds the source file of a dotted Python module: a .py
// file or a package's __init__.py, under the repository root, a component or
// its src directory.
func (m *mapper) pythonModuleFile(module string) string {
	module = strings.TrimSpace(module)
	if !dottedName.MatchString(module) {
		return ""
	}
	stem := strings.ReplaceAll(module, ".", "/")
	bases := []string{".", "src"}
	for _, c := range m.set.Components {
		if c.ID != "." {
			bases = append(bases, c.ID, c.ID+"/src")
		}
	}
	for _, base := range slices.Compact(bases) {
		for _, candidate := range []string{stem + ".py", stem + "/__init__.py"} {
			if rel := joinDir(base, candidate); m.isFile(rel) {
				return rel
			}
		}
	}
	return ""
}

// validRepositoryPath reports whether a joined path stays in the repository.
func validRepositoryPath(p string) bool {
	return p != "" && !isAbsolute(p) && p != ".." && !strings.HasPrefix(p, "../")
}
