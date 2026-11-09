package swift

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// xcresult converts an Xcode result bundle into a JUnit report, from
// `xcresulttool get test-results tests` (Xcode 16 and later), and a
// Cobertura report, from the line coverage `xccov` archives for each file
// under the project that the app targets cover. Files under DerivedData,
// SourcePackages and .build, and the test bundles, are left out of the
// coverage. When xcodebuild test failed, the command fails unless a case
// failed: the task then reads the failed cases.
//
//	{runtime} xcresult --bundle <Tests.xcresult> --junit <junit.xml> --cobertura <coverage.xml>
func xcresult(args []string, _, stderr io.Writer) error {
	fs := newFlags(CommandXcresult, stderr)
	bundle := fs.String("bundle", "", "the result bundle")
	junit := fs.String("junit", "", "the JUnit report to write")
	cobertura := fs.String("cobertura", "", "the Cobertura report to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bundle == "" || (*junit == "" && *cobertura == "") {
		return errors.New("name the --bundle, and a --junit or --cobertura report")
	}
	if _, err := os.Stat(*bundle); err != nil {
		return fmt.Errorf("no result bundle: %w", err)
	}
	failed, _ := os.ReadFile(failedMarker(*bundle))
	if len(failed) > 0 && *junit == "" {
		return fmt.Errorf("%s", strings.TrimSpace(string(failed)))
	}
	if *junit != "" {
		out, err := run(stderr, "xcrun", "xcresulttool", "get", "test-results", "tests", "--path", *bundle)
		if err != nil {
			return fmt.Errorf("%w (xcresulttool get test-results needs Xcode 16 or later)", err)
		}
		report, err := junitFromTestResults(out)
		if err != nil {
			return err
		}
		if err := writeReport(*junit, report); err != nil {
			return err
		}
		if len(failed) > 0 && !bytes.Contains(report, []byte("<failure")) {
			return fmt.Errorf("%s, and no test case failed", strings.TrimSpace(string(failed)))
		}
	}
	if *cobertura != "" {
		dir, err := os.Getwd()
		if err != nil {
			return err
		}
		files, err := coveredFiles(stderr, *bundle, dir)
		if err != nil {
			return err
		}
		lines := map[string][]coveredLine{}
		for _, f := range files {
			out, err := run(stderr, "xcrun", "xccov", "view", "--archive", "--file", f, "--json", *bundle)
			if err != nil {
				return err
			}
			l, err := archiveLines(out, f)
			if err != nil {
				return err
			}
			lines[f] = l
		}
		report, err := coberturaReport(lines)
		if err != nil {
			return err
		}
		if err := writeReport(*cobertura, report); err != nil {
			return err
		}
	}
	return nil
}

func writeReport(file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, data, 0o644) //nolint:gosec // the task's own report
}

// testNode is a node of `xcresulttool get test-results tests`.
type testNode struct {
	Name              string     `json:"name"`
	NodeType          string     `json:"nodeType"`
	Result            string     `json:"result"`
	DurationInSeconds float64    `json:"durationInSeconds"`
	Children          []testNode `json:"children"`
}

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	ClassName string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr,omitempty"`
}

// junitFromTestResults writes one JUnit suite per test suite, named
// Bundle.Suite, the module-qualified class name the observe Swift detector
// places on its test target.
func junitFromTestResults(data []byte) ([]byte, error) {
	var doc struct {
		TestNodes []testNode `json:"testNodes"`
	}
	if err := json.Unmarshal(jsonStart(data), &doc); err != nil {
		return nil, fmt.Errorf("read xcresulttool test results: %w", err)
	}
	suites := map[string]*junitSuite{}
	var order []string
	var walk func(n testNode, bundle, suite string)
	walk = func(n testNode, bundle, suite string) {
		switch n.NodeType {
		case "Unit test bundle", "UI test bundle":
			bundle, suite = n.Name, ""
		case "Test Suite":
			suite = n.Name
		case "Test Case":
			class := moduleName(bundle)
			if suite != "" {
				class += "." + suite
			}
			s, ok := suites[class]
			if !ok {
				s = &junitSuite{Name: class}
				suites[class] = s
				order = append(order, class)
			}
			c := junitCase{ClassName: class, Name: strings.TrimSuffix(n.Name, "()"), Time: fmt.Sprintf("%.3f", n.DurationInSeconds)}
			switch n.Result {
			case "Failed":
				c.Failure = &junitMessage{Message: failureMessage(n)}
				s.Failures++
			case "Skipped":
				c.Skipped = &junitMessage{}
				s.Skipped++
			}
			s.Tests++
			s.Cases = append(s.Cases, c)
			return
		}
		for _, child := range n.Children {
			walk(child, bundle, suite)
		}
	}
	for _, n := range doc.TestNodes {
		walk(n, "", "")
	}
	out := junitSuites{}
	for _, name := range order {
		out.Suites = append(out.Suites, *suites[name])
	}
	return marshalXML(out)
}

// moduleName is the module a test bundle builds: its name, each character
// that is no identifier character replaced by _.
func moduleName(bundle string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, bundle)
}

func failureMessage(n testNode) string {
	var out []string
	var walk func(testNode)
	walk = func(c testNode) {
		if c.NodeType == "Failure Message" {
			out = append(out, c.Name)
		}
		for _, child := range c.Children {
			walk(child)
		}
	}
	walk(n)
	if len(out) == 0 {
		return "failed"
	}
	return strings.Join(out, "\n")
}

func marshalXML(v any) ([]byte, error) {
	data, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(data, '\n')...), nil
}

// skippedCoverage are the path segments of files that are no source of the
// repository.
var skippedCoverage = []string{"/DerivedData/", "/SourcePackages/", "/.build/"}

// coveredFiles lists the files of the bundle's coverage report under dir,
// the project's directory, outside the test bundles, sorted. A local package
// the app links is another component, with its own coverage.
func coveredFiles(stderr io.Writer, bundle, dir string) ([]string, error) {
	out, err := run(stderr, "xcrun", "xccov", "view", "--report", "--json", bundle)
	if err != nil {
		return nil, err
	}
	var report struct {
		Targets []struct {
			Name  string `json:"name"`
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(jsonStart(out), &report); err != nil {
		return nil, fmt.Errorf("read xccov view --report --json: %w", err)
	}
	seen := map[string]bool{}
	var files []string
	for _, t := range report.Targets {
		if strings.HasSuffix(t.Name, ".xctest") {
			continue
		}
		for _, f := range t.Files {
			skip := f.Path == "" || seen[f.Path] || !under(f.Path, dir)
			for _, s := range skippedCoverage {
				skip = skip || strings.Contains(filepath.ToSlash(f.Path), s)
			}
			if !skip {
				seen[f.Path] = true
				files = append(files, f.Path)
			}
		}
	}
	sort.Strings(files)
	return files, nil
}

type coveredLine struct {
	Line           int  `json:"line"`
	IsExecutable   bool `json:"isExecutable"`
	ExecutionCount int  `json:"executionCount"`
}

// archiveLines reads `xccov view --archive --file <f> --json`: the file's
// lines, as an array or under the file's path.
func archiveLines(data []byte, file string) ([]coveredLine, error) {
	data = jsonStart(data)
	var lines []coveredLine
	if err := json.Unmarshal(data, &lines); err == nil {
		return lines, nil
	}
	var byFile map[string][]coveredLine
	if err := json.Unmarshal(data, &byFile); err != nil {
		return nil, fmt.Errorf("read xccov view --archive --file %s: %w", file, err)
	}
	if l, ok := byFile[file]; ok {
		return l, nil
	}
	for _, l := range byFile {
		return l, nil
	}
	return nil, nil
}

type coberturaDoc struct {
	XMLName      xml.Name           `xml:"coverage"`
	LineRate     string             `xml:"line-rate,attr"`
	LinesCovered int                `xml:"lines-covered,attr"`
	LinesValid   int                `xml:"lines-valid,attr"`
	Version      string             `xml:"version,attr"`
	Packages     []coberturaPackage `xml:"packages>package"`
}

type coberturaPackage struct {
	Name    string           `xml:"name,attr"`
	Classes []coberturaClass `xml:"classes>class"`
}

type coberturaClass struct {
	Name     string          `xml:"name,attr"`
	Filename string          `xml:"filename,attr"`
	LineRate string          `xml:"line-rate,attr"`
	Lines    []coberturaLine `xml:"lines>line"`
}

type coberturaLine struct {
	Number int `xml:"number,attr"`
	Hits   int `xml:"hits,attr"`
}

// coberturaReport writes the executable lines of each file, one package per
// directory and one class per file.
func coberturaReport(files map[string][]coveredLine) ([]byte, error) {
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	doc := coberturaDoc{Version: "@putnami/swift"}
	byDir := map[string]int{}
	for _, f := range names {
		class := coberturaClass{Name: strings.TrimSuffix(path.Base(filepath.ToSlash(f)), ".swift"), Filename: filepath.ToSlash(f)}
		covered, valid := 0, 0
		for _, l := range files[f] {
			if !l.IsExecutable {
				continue
			}
			valid++
			if l.ExecutionCount > 0 {
				covered++
			}
			class.Lines = append(class.Lines, coberturaLine{Number: l.Line, Hits: l.ExecutionCount})
		}
		class.LineRate = rate(covered, valid)
		doc.LinesCovered += covered
		doc.LinesValid += valid
		dir := path.Dir(filepath.ToSlash(f))
		i, ok := byDir[dir]
		if !ok {
			i = len(doc.Packages)
			byDir[dir] = i
			doc.Packages = append(doc.Packages, coberturaPackage{Name: strings.ReplaceAll(strings.Trim(dir, "/"), "/", ".")})
		}
		doc.Packages[i].Classes = append(doc.Packages[i].Classes, class)
	}
	doc.LineRate = rate(doc.LinesCovered, doc.LinesValid)
	return marshalXML(doc)
}

func rate(covered, valid int) string {
	if valid == 0 {
		return "1"
	}
	return fmt.Sprintf("%.4f", float64(covered)/float64(valid))
}
