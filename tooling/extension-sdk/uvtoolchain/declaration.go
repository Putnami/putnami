package uvtoolchain

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Pyproject is the workspace root manifest whose [tool.uv] table declares the
// uv release, as required-version.
const Pyproject = "pyproject.toml"

// declarationName names the declaration in a message.
const declarationName = Pyproject + " [tool.uv] required-version"

// Declaration is what a pyproject.toml says about the uv release a workspace
// runs.
type Declaration struct {
	// Present reports whether the [tool.uv] table sets required-version.
	Present bool
	// Spec is the value required-version sets, as written between its quotes,
	// or "" when that value is not a one-line string.
	Spec string
	// Version is the exact release Spec names (see ExactRelease), or "" when
	// it names none: a range, a wildcard, or anything that is not a version.
	Version string
	// Outside names the first key that writes uv settings outside a [tool.uv]
	// table header, as a dotted key or an inline table, such as uv.managed in
	// the [tool] table, or "" when there is none. ReadDeclaration does not read
	// those forms, and a [tool.uv] header added to such a document defines the
	// table twice, which TOML forbids.
	Outside string
}

// ReadWorkspaceDeclaration reads the declaration of the Pyproject at the root
// of workspaceRoot. A workspace without one declares nothing.
func ReadWorkspaceDeclaration(workspaceRoot string) (Declaration, error) {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, Pyproject))
	if errors.Is(err, fs.ErrNotExist) {
		return Declaration{}, nil
	}
	if err != nil {
		return Declaration{}, fmt.Errorf("read %s: %w", Pyproject, err)
	}
	return ReadDeclaration(string(data)), nil
}

// Problem returns why the declaration names no release Putnami can pin and
// run, and nil when it names one or declares nothing. Its message names the
// declaration to write; the caller adds the command to run next.
func (d Declaration) Problem() error {
	if !d.Present {
		return nil
	}
	if d.Version == "" {
		written := "empty or not a one-line string"
		if d.Spec != "" {
			written = strconv.Quote(d.Spec)
		}
		return fmt.Errorf("%s is %s, which names no single uv release: "+
			"declare one exact release, such as required-version = \"==%s\", so Putnami can pin it",
			declarationName, written, DefaultVersion)
	}
	if err := Supported(d.Version); err != nil {
		return fmt.Errorf("%s declares a release Putnami cannot run: %w: "+
			"declare required-version = \"==%s\" or a later release",
			declarationName, err, MinimumVersion)
	}
	return nil
}

// ReadDeclaration reads the uv release a pyproject.toml declares: the
// required-version key of its [tool.uv] table. The key in any other table or
// in an array-of-tables entry is not read. Nor are uv settings written as a
// dotted key or an inline table; Outside reports the first of those.
//
// The reader works line by line. It reads a one-line string value, which is
// how required-version is written, and ignores the content of multi-line
// strings and arrays it does not need.
func ReadDeclaration(pyproject string) Declaration {
	var declaration Declaration
	table := ""
	for _, raw := range strings.Split(pyproject, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, ok := tableHeader(line); ok {
			table = name
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		parts := keyParts(key)
		switch {
		case table == "tool.uv" && len(parts) == 1 && parts[0] == "required-version" && !declaration.Present:
			declaration.Present = true
			declaration.Spec = tomlString(value)
			declaration.Version = ExactRelease(declaration.Spec)
		case declaration.Outside == "" && table == "tool" && parts[0] == "uv",
			declaration.Outside == "" && table == "" && parts[0] == "tool" && (len(parts) == 1 || parts[1] == "uv"):
			declaration.Outside = strings.Join(parts, ".")
		}
	}
	return declaration
}

// tableHeader returns the name of the table a header line opens, with the
// white space around its parts removed. An array-of-tables header gets a name
// no table has. It reports false for a line that is not a header.
func tableHeader(line string) (string, bool) {
	header, _, _ := strings.Cut(line, "#")
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, "[") || !strings.HasSuffix(header, "]") {
		return "", false
	}
	if strings.HasPrefix(header, "[[") {
		return "[[" + strings.Join(strings.Fields(strings.Trim(header, "[]")), "") + "]]", true
	}
	return strings.Join(strings.Fields(strings.Trim(header, "[]")), ""), true
}

// keyParts splits a key into its dotted parts, each without the white space
// and quotes around it.
func keyParts(key string) []string {
	parts := strings.Split(key, ".")
	for i, part := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(part), `"'`)
	}
	return parts
}

// tomlString returns the content of the one-line string value starts with,
// or "" when it starts with none.
func tomlString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || (value[0] != '"' && value[0] != '\'') {
		return ""
	}
	content, _, closed := strings.Cut(value[1:], value[:1])
	if !closed {
		return ""
	}
	return content
}

// ExactRelease returns the release spec names when it names exactly one:
// "==X.Y.Z", or a bare "X.Y.Z", which uv reads the same way, with a leading
// "v" dropped and a ".0" patch added to a MAJOR.MINOR version. It returns ""
// for any other specifier, such as a range, a wildcard, an arbitrary equality
// or a prerelease.
func ExactRelease(spec string) string {
	spec = strings.TrimSpace(spec)
	if rest, ok := strings.CutPrefix(spec, "=="); ok && !strings.HasPrefix(rest, "=") {
		spec = strings.TrimSpace(rest)
	}
	version := strings.TrimPrefix(spec, "v")
	if strings.Count(version, ".") == 1 {
		version += ".0"
	}
	if !IsRelease(version) {
		return ""
	}
	return version
}
