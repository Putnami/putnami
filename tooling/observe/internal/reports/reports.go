// Package reports finds the test and coverage reports a CI job produced.
//
// A report is recognized by its content, never by its name alone: a JUnit XML
// report has a testsuites or testsuite root, a Cobertura report a coverage
// root with Cobertura attributes, a Clover report a coverage root with a
// project, an LCOV trace starts with a TN: or SF: record, and a Go cover
// profile starts with its mode line. Named paths replace discovery for their
// kind. A missing, unreadable or unrecognized report is a diagnostic.
package reports

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Report kinds.
const (
	KindJUnit    = "junit"
	KindCoverage = "coverage"
)

// Report formats.
const (
	FormatJUnit     = "junit"
	FormatLCOV      = "lcov"
	FormatCobertura = "cobertura"
	FormatClover    = "clover"
	FormatGoCover   = "go-cover"
)

// Report sources.
const (
	SourceDiscovered = "discovered"
	SourceNamed      = "named"
)

// Diagnostic codes.
const (
	CodeReportMissing      = "observe.report_missing"
	CodeReportUnreadable   = "observe.report_unreadable"
	CodeReportUnrecognized = "observe.report_unrecognized"
	CodeDiscoveryBounded   = "observe.discovery_bounded"
	CodePatternInvalid     = "observe.report_pattern_invalid"
)

// Bounds of discovery: the entries walked, the files sniffed, the reports
// kept, and the bytes read from each file.
const (
	MaxWalkedEntries = 200000
	MaxSniffedFiles  = 10000
	MaxReports       = 1000
	sniffBytes       = 8 << 10
)

// skippedDirs are never walked.
var skippedDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true}

// Report is one report file.
type Report struct {
	// Path is relative to the repository root, slash-separated, or absolute
	// when the file lies outside it.
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Format string `json:"format"`
	Source string `json:"source"`
}

// Request says where to look.
type Request struct {
	// Root is the repository's top directory, where discovery walks.
	Root string
	// JUnit and Coverage are the named paths or glob patterns, resolved
	// against the invocation directory by Resolve. An empty list discovers
	// that kind.
	JUnit    []string
	Coverage []string
	// Resolve turns a named path into an absolute one.
	Resolve func(string) string
}

// Collect returns the reports of req, in path order, and its diagnostics.
func Collect(req Request) ([]Report, []diag.Diagnostic) {
	var reports []Report
	var diags []diag.Diagnostic
	discover := map[string]bool{}
	for _, named := range []struct {
		kind     string
		patterns []string
	}{{KindJUnit, req.JUnit}, {KindCoverage, req.Coverage}} {
		if len(named.patterns) == 0 {
			discover[named.kind] = true
			continue
		}
		found, d := collectNamed(req, named.kind, named.patterns)
		reports = append(reports, found...)
		diags = append(diags, d...)
	}
	if len(discover) > 0 {
		found, d := discoverReports(req.Root, discover)
		reports = append(reports, found...)
		diags = append(diags, d...)
		for _, kind := range []string{KindJUnit, KindCoverage} {
			if discover[kind] && !slices.ContainsFunc(found, func(r Report) bool { return r.Kind == kind }) {
				diags = append(diags, diag.Warningf(CodeReportMissing, kind,
					"no %s report was found in the repository: produce one, or name it with --%s", label(kind), kind))
			}
		}
	}
	slices.SortStableFunc(reports, func(a, b Report) int { return strings.Compare(a.Path, b.Path) })
	reports = slices.CompactFunc(reports, func(a, b Report) bool { return a.Path == b.Path && a.Kind == b.Kind })
	if len(reports) > MaxReports {
		diags = append(diags, diag.Warningf(CodeDiscoveryBounded, "", "%d reports were found; the first %d are kept", len(reports), MaxReports))
		reports = reports[:MaxReports]
	}
	return reports, diags
}

// SplitList splits a comma-separated flag value, dropping empty entries.
func SplitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func collectNamed(req Request, kind string, patterns []string) ([]Report, []diag.Diagnostic) {
	var reports []Report
	var diags []diag.Diagnostic
	for _, pattern := range patterns {
		abs := filepath.Clean(req.Resolve(pattern))
		var files []string
		if hasMeta(pattern) {
			if _, err := path.Match(filepath.ToSlash(abs), ""); err != nil {
				diags = append(diags, diag.Warningf(CodePatternInvalid, kind, "the --%s pattern %q is invalid: %v", kind, pattern, err))
				continue
			}
			files = glob(abs)
		} else if info, err := os.Stat(abs); err == nil && info.Mode().IsRegular() {
			files = []string{abs}
		}
		if len(files) == 0 {
			diags = append(diags, diag.Warningf(CodeReportMissing, kind, "the --%s report %q matches no file", kind, pattern))
			continue
		}
		for _, file := range files {
			format, err := sniffFile(file)
			display := relative(req.Root, file)
			switch {
			case err != nil:
				diags = append(diags, diag.Warningf(CodeReportUnreadable, kind, "the %s report %s cannot be read: %v", label(kind), display, err))
			case kindOf(format) != kind:
				diags = append(diags, diag.Warningf(CodeReportUnrecognized, kind, "%s is not a %s report%s", display, label(kind), recognizedAs(format)))
			default:
				reports = append(reports, Report{Path: display, Kind: kind, Format: format, Source: SourceNamed})
			}
		}
	}
	return reports, diags
}

func discoverReports(root string, kinds map[string]bool) ([]Report, []diag.Diagnostic) {
	var reports []Report
	var diags []diag.Diagnostic
	walked, sniffed := 0, 0
	errBound := errors.New("bound")
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		walked++
		if walked > MaxWalkedEntries {
			diags = append(diags, diag.Warningf(CodeDiscoveryBounded, "", "discovery stopped after %d entries: name the reports with --junit and --coverage", MaxWalkedEntries))
			return errBound
		}
		if entry.IsDir() {
			if file != root && skippedDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !candidate(entry.Name()) {
			return nil
		}
		sniffed++
		if sniffed > MaxSniffedFiles {
			diags = append(diags, diag.Warningf(CodeDiscoveryBounded, "", "discovery stopped after reading %d candidate files: name the reports with --junit and --coverage", MaxSniffedFiles))
			return errBound
		}
		format, err := sniffFile(file)
		if err != nil || format == "" || !kinds[kindOf(format)] {
			return nil
		}
		reports = append(reports, Report{Path: relative(root, file), Kind: kindOf(format), Format: format, Source: SourceDiscovered})
		return nil
	})
	if err != nil && !errors.Is(err, errBound) {
		diags = append(diags, diag.Warningf(CodeReportUnreadable, "", "the repository cannot be walked: %v", err))
	}
	return reports, diags
}

// candidate reports whether a file name can hold a report, so discovery reads
// only those.
func candidate(name string) bool {
	lower := strings.ToLower(name)
	switch path.Ext(lower) {
	case ".xml", ".info", ".lcov", ".out", ".cov", ".coverprofile", ".gocov":
		return true
	case ".txt":
		return strings.Contains(lower, "cover")
	}
	return false
}

func kindOf(format string) string {
	switch format {
	case FormatJUnit:
		return KindJUnit
	case FormatLCOV, FormatCobertura, FormatClover, FormatGoCover:
		return KindCoverage
	}
	return ""
}

func label(kind string) string {
	if kind == KindJUnit {
		return "JUnit"
	}
	return "coverage"
}

func recognizedAs(format string) string {
	if format == "" {
		return ""
	}
	return " (it reads as " + format + ")"
}

// sniffFile reads the start of a file and returns its report format, "" when
// it is none.
func sniffFile(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head, err := io.ReadAll(io.LimitReader(f, sniffBytes))
	if err != nil {
		return "", err
	}
	return Sniff(head), nil
}

// Sniff returns the report format of a file that starts with head, "" when it
// is none.
func Sniff(head []byte) string {
	head = bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	trimmed := bytes.TrimLeft(head, " \t\r\n")
	if bytes.HasPrefix(trimmed, []byte("<")) {
		return sniffXML(trimmed)
	}
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	if !scanner.Scan() {
		return ""
	}
	line := strings.TrimSpace(scanner.Text())
	switch {
	case strings.HasPrefix(line, "TN:"), strings.HasPrefix(line, "SF:"):
		return FormatLCOV
	case line == "mode: set", line == "mode: count", line == "mode: atomic":
		return FormatGoCover
	}
	return ""
}

func sniffXML(head []byte) string {
	decoder := xml.NewDecoder(bytes.NewReader(head))
	decoder.Strict = false
	root, ok := nextStart(decoder)
	if !ok {
		return ""
	}
	switch root.Name.Local {
	case "testsuites", "testsuite":
		return FormatJUnit
	case "coverage":
	default:
		return ""
	}
	for _, attr := range root.Attr {
		switch attr.Name.Local {
		case "line-rate", "lines-valid", "branch-rate":
			return FormatCobertura
		case "clover", "generated":
			return FormatClover
		}
	}
	child, ok := nextStart(decoder)
	if !ok {
		return ""
	}
	switch child.Name.Local {
	case "project":
		return FormatClover
	case "sources", "packages":
		return FormatCobertura
	}
	return ""
}

func nextStart(decoder *xml.Decoder) (xml.StartElement, bool) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return xml.StartElement{}, false
		}
		if start, ok := token.(xml.StartElement); ok {
			return start, true
		}
	}
}

// hasMeta reports whether a pattern holds glob syntax.
func hasMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

// glob returns the regular files that match an absolute pattern, where **
// matches any number of directories, walking from its longest literal prefix.
func glob(pattern string) []string {
	pattern = filepath.ToSlash(pattern)
	segments := strings.Split(pattern, "/")
	literal := 0
	for literal < len(segments) && !hasMeta(segments[literal]) {
		literal++
	}
	start := strings.Join(segments[:literal], "/")
	if start == "" {
		start = "/"
	}
	start = filepath.FromSlash(start)
	rest := segments[literal:]
	var files []string
	walked := 0
	_ = filepath.WalkDir(start, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		walked++
		if walked > MaxWalkedEntries || len(files) >= MaxReports {
			return filepath.SkipAll
		}
		if entry.IsDir() {
			if file != start && skippedDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(start, file)
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		if matchSegments(rest, strings.Split(filepath.ToSlash(rel), "/")) {
			files = append(files, file)
		}
		return nil
	})
	return files
}

// matchSegments matches path segments against pattern segments, where a **
// segment matches zero or more path segments.
func matchSegments(pattern, segments []string) bool {
	if len(pattern) == 0 {
		return len(segments) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(segments); i++ {
			if matchSegments(pattern[1:], segments[i:]) {
				return true
			}
		}
		return false
	}
	if len(segments) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], segments[0])
	return err == nil && ok && matchSegments(pattern[1:], segments[1:])
}

// relative returns file relative to root, slash-separated, or file itself when
// it lies outside root. Symbolic links in either path are resolved when the
// paths as given do not nest.
func relative(root, file string) string {
	if rel, ok := within(root, file); ok {
		return rel
	}
	realRoot, rootErr := filepath.EvalSymlinks(root)
	realFile, fileErr := filepath.EvalSymlinks(file)
	if rootErr == nil && fileErr == nil {
		if rel, ok := within(realRoot, realFile); ok {
			return rel
		}
	}
	return filepath.ToSlash(file)
}

func within(root, file string) (string, bool) {
	rel, err := filepath.Rel(root, file)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
