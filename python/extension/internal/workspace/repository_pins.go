package workspace

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A repository adopted with its own tools can pin the uv release they run in a
// file of its own. A [tool.uv] required-version of another release would
// contradict it: uv refuses to run under a required-version it does not
// match, so the repository's own setup would fail.

// uvPinFiles are the files, relative to the workspace root, in which a
// repository pins uv for its own tools, each with the test that finds the pin.
var uvPinFiles = []struct {
	path string
	pins func(content string) bool
}{
	{".prototools", protoUV},
	{"mise.toml", miseUV},
	{"mise.local.toml", miseUV},
	{".mise.toml", miseUV},
	{".mise.local.toml", miseUV},
	{".config/mise.toml", miseUV},
	{".tool-versions", toolVersionsUV.MatchString},
	{".moon/toolchains.yml", moonUVVersion},
	{".moon/toolchain.yml", moonUVVersion},
}

// tomlUVKey is a uv key with a value: uv itself, or a mise backend such as
// aqua:astral-sh/uv.
var tomlUVKey = regexp.MustCompile(`^\s*"?(?:[a-z]+:)?(?:astral-sh/)?uv"?\s*=\s*\S`)

// tomlTable is a TOML table header.
var tomlTable = regexp.MustCompile(`^\s*\[+\s*([^\]]+?)\s*\]+`)

// tomlTableHasUV reports whether the table named table ("" for the keys ahead
// of the first header) holds a uv key.
func tomlTableHasUV(content, table string) bool {
	current := ""
	for _, line := range strings.Split(content, "\n") {
		if m := tomlTable.FindStringSubmatch(line); m != nil {
			current = strings.Trim(m[1], `"`)
			continue
		}
		if current == table && tomlUVKey.MatchString(line) {
			return true
		}
	}
	return false
}

// protoUV reports whether a .prototools pins uv: proto reads the tool versions
// ahead of its first table; [plugins] and the others configure proto itself.
func protoUV(content string) bool { return tomlTableHasUV(content, "") }

// miseUV reports whether a mise file pins uv in its [tools] table; a [tasks]
// entry named uv is a command, no pin.
func miseUV(content string) bool { return tomlTableHasUV(content, "tools") }

// toolVersionsUV is the uv line of an asdf .tool-versions file.
var toolVersionsUV = regexp.MustCompile(`(?m)^\s*uv\s+\S`)

// moonUVKey is the uv key of a moon toolchain file, whose nested version is
// the release moon installs.
var moonUVKey = regexp.MustCompile(`^(\s*)uv:\s*(#.*)?$`)

// moonUVFlow is a uv key whose version is written inline, as in
// uv: { version: '0.8.17' }.
var moonUVFlow = regexp.MustCompile(`(?m)^\s*uv:\s*\{[^}]*\bversion:\s*['"]?[^\s,'"}]`)

// moonUVVersion reports whether a moon toolchain file sets a version under a
// uv key.
func moonUVVersion(content string) bool {
	if moonUVFlow.MatchString(content) {
		return true
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		m := moonUVKey.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent := len(m[1])
		for _, nested := range lines[i+1:] {
			trimmed := strings.TrimSpace(nested)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if len(nested)-len(strings.TrimLeft(nested, " \t")) <= indent {
				break
			}
			if strings.HasPrefix(trimmed, "version:") && strings.TrimSpace(strings.TrimPrefix(trimmed, "version:")) != "" {
				return true
			}
		}
	}
	return false
}

// RepositoryUVPin returns the file, relative to the workspace root, in which
// the repository pins the uv release of its own tools, "" when no file does.
func RepositoryUVPin(workspaceRoot string) string {
	for _, file := range uvPinFiles {
		data, err := os.ReadFile(filepath.Join(workspaceRoot, filepath.FromSlash(file.path))) //nolint:gosec // a file at the workspace root
		if err != nil {
			continue
		}
		if file.pins(string(data)) {
			return file.path
		}
	}
	return ""
}
