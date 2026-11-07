package swift

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// MaxProjectBytes bounds the project.pbxproj the detector reads.
const MaxProjectBytes = 64 << 20

// Project is the Xcode projects of one directory.
type Project struct {
	// Dir is the repository directory that holds the .xcodeproj.
	Dir string
	// Name is the name of the first .xcodeproj, without its extension.
	Name string
	// LocalPackages are the repository directories of the local Swift
	// packages the projects reference, sorted.
	LocalPackages []string
	// TestTargets are the names of the unit and UI test targets, sorted.
	TestTargets []string
}

// testProductTypes are the product types of Xcode test targets.
var testProductTypes = map[string]bool{
	"com.apple.product-type.bundle.unit-test":   true,
	"com.apple.product-type.bundle.ui-testing":  true,
	"com.apple.product-type.bundle.ocunit-test": true,
}

// readProject reads the .xcodeproj directories of one repository directory.
func readProject(root, dir string, xcodeprojs []string) (Project, []diag.Diagnostic) {
	project := Project{Dir: dir, Name: strings.TrimSuffix(path.Base(xcodeprojs[0]), ".xcodeproj")}
	var diags []diag.Diagnostic
	locals, tests := map[string]bool{}, map[string]bool{}
	for _, xcodeproj := range xcodeprojs {
		file := path.Join(xcodeproj, PBXProject)
		objects, err := readPBXObjects(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			diags = append(diags, diag.Warningf(CodeProjectUnreadable, file,
				"%s cannot be read, so the project %s has no edge: %v", file, xcodeproj, err))
			continue
		}
		for _, object := range objects {
			switch object["isa"] {
			case "XCLocalSwiftPackageReference":
				rel, _ := object["relativePath"].(string)
				if local, ok := join(dir, rel); ok && local != dir {
					locals[local] = true
				}
			case "PBXNativeTarget":
				productType, _ := object["productType"].(string)
				if name, _ := object["name"].(string); name != "" && testProductTypes[productType] {
					tests[name] = true
				}
			}
		}
	}
	project.LocalPackages = sortedKeys(locals)
	project.TestTargets = sortedKeys(tests)
	return project, diags
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// readPBXObjects reads the objects table of a project.pbxproj.
func readPBXObjects(file string) ([]map[string]any, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxProjectBytes {
		return nil, errors.New("it is larger than 64 MiB")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	doc, err := parsePlist(data)
	if err != nil {
		return nil, err
	}
	top, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("it is not a property list dictionary")
	}
	table, ok := top["objects"].(map[string]any)
	if !ok {
		return nil, errors.New("it has no objects table")
	}
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(table))
	for _, key := range keys {
		if object, ok := table[key].(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out, nil
}

// parsePlist parses an OpenStep property list, the format of
// project.pbxproj: dictionaries, arrays and strings, quoted or bare, with //
// and /* */ comments. A dictionary is a map[string]any, an array a []any.
func parsePlist(data []byte) (any, error) {
	p := &plistParser{src: string(data)}
	value, err := p.value()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.pos < len(p.src) {
		return nil, p.errorf("unexpected content after the property list")
	}
	return value, nil
}

type plistParser struct {
	src string
	pos int
}

func (p *plistParser) errorf(format string, args ...any) error {
	line := strings.Count(p.src[:p.pos], "\n") + 1
	return fmt.Errorf("line %d: %s", line, fmt.Sprintf(format, args...))
}

// space skips white space and comments.
func (p *plistParser) space() {
	for p.pos < len(p.src) {
		switch {
		case strings.ContainsRune(" \t\r\n", rune(p.src[p.pos])):
			p.pos++
		case strings.HasPrefix(p.src[p.pos:], "//"):
			end := strings.IndexByte(p.src[p.pos:], '\n')
			if end < 0 {
				p.pos = len(p.src)
				return
			}
			p.pos += end + 1
		case strings.HasPrefix(p.src[p.pos:], "/*"):
			end := strings.Index(p.src[p.pos+2:], "*/")
			if end < 0 {
				p.pos = len(p.src)
				return
			}
			p.pos += end + 4
		default:
			return
		}
	}
}

func (p *plistParser) value() (any, error) {
	p.space()
	if p.pos >= len(p.src) {
		return nil, p.errorf("unexpected end of the property list")
	}
	switch p.src[p.pos] {
	case '{':
		return p.dictionary()
	case '(':
		return p.array()
	}
	return p.str()
}

func (p *plistParser) dictionary() (any, error) {
	p.pos++
	out := map[string]any{}
	for {
		p.space()
		if p.pos >= len(p.src) {
			return nil, p.errorf("unterminated dictionary")
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return out, nil
		}
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		if err := p.expect('='); err != nil {
			return nil, err
		}
		value, err := p.value()
		if err != nil {
			return nil, err
		}
		if err := p.expect(';'); err != nil {
			return nil, err
		}
		out[key] = value
	}
}

func (p *plistParser) array() (any, error) {
	p.pos++
	var out []any
	for {
		p.space()
		if p.pos >= len(p.src) {
			return nil, p.errorf("unterminated array")
		}
		if p.src[p.pos] == ')' {
			p.pos++
			return out, nil
		}
		value, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, value)
		p.space()
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
		}
	}
}

func (p *plistParser) expect(c byte) error {
	p.space()
	if p.pos >= len(p.src) || p.src[p.pos] != c {
		return p.errorf("expected %q", c)
	}
	p.pos++
	return nil
}

// bare are the characters of an unquoted string.
func bare(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_$+/:.-", c) >= 0
}

func (p *plistParser) str() (string, error) {
	p.space()
	if p.pos >= len(p.src) {
		return "", p.errorf("unexpected end of the property list")
	}
	if c := p.src[p.pos]; c == '"' || c == '\'' {
		p.pos++
		var b strings.Builder
		for p.pos < len(p.src) {
			r := p.src[p.pos]
			switch {
			case r == c:
				p.pos++
				return b.String(), nil
			case r == '\\' && p.pos+1 < len(p.src):
				p.pos++
				switch e := p.src[p.pos]; e {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteByte(e)
				}
			default:
				b.WriteByte(r)
			}
			p.pos++
		}
		return "", p.errorf("unterminated string")
	}
	start := p.pos
	for p.pos < len(p.src) && bare(p.src[p.pos]) {
		p.pos++
	}
	if p.pos == start {
		return "", p.errorf("unexpected %q", p.src[p.pos])
	}
	return p.src[start:p.pos], nil
}
