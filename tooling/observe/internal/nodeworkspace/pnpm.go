package nodeworkspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// pnpmWorkspace is what the detector reads from pnpm-workspace.yaml.
type pnpmWorkspace struct {
	// declared reports whether the file has a packages key: a file that holds
	// only settings declares no workspace.
	declared              bool
	packages              []string
	linkWorkspacePackages bool
}

// readPNPMWorkspace reads the root pnpm-workspace.yaml. found is false when
// the file does not exist. A file that cannot be read, or whose packages is not
// a list of strings, is a diagnostic and gives no member.
func readPNPMWorkspace(root string) (ws pnpmWorkspace, found bool, diags []diag.Diagnostic) {
	data, err := os.ReadFile(filepath.Join(root, PNPMWorkspace))
	if errors.Is(err, fs.ErrNotExist) {
		return pnpmWorkspace{}, false, nil
	}
	if err != nil {
		return pnpmWorkspace{declared: true}, true, []diag.Diagnostic{diag.Warningf(CodeManifestInvalid, PNPMWorkspace, "pnpm-workspace.yaml cannot be read: %v", err)}
	}
	ws, err = parsePNPMWorkspace(string(data))
	if err != nil {
		return pnpmWorkspace{declared: true}, true, []diag.Diagnostic{diag.Warningf(CodeManifestInvalid, PNPMWorkspace, "pnpm-workspace.yaml cannot be read: %v", err)}
	}
	return ws, true, nil
}

// parsePNPMWorkspace reads the two top-level keys the detector needs from
// pnpm-workspace.yaml: packages, a block or flow list of strings, and
// linkWorkspacePackages. It is not a YAML parser: every other key is skipped,
// and a packages value of another shape is an error.
func parsePNPMWorkspace(text string) (pnpmWorkspace, error) {
	var ws pnpmWorkspace
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := stripComment(lines[i])
		if strings.TrimSpace(line) == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "linkWorkspacePackages":
			ws.linkWorkspacePackages = value == "true" || value == "deep"
		case "packages":
			ws.declared = true
			if value != "" {
				list, err := flowList(value)
				if err != nil {
					return pnpmWorkspace{}, err
				}
				ws.packages = list
				continue
			}
			for i+1 < len(lines) {
				next := stripComment(lines[i+1])
				trimmed := strings.TrimSpace(next)
				if trimmed == "" {
					i++
					continue
				}
				if next[0] != ' ' && next[0] != '\t' && !strings.HasPrefix(next, "-") {
					break
				}
				item, isItem := strings.CutPrefix(trimmed, "-")
				if !isItem {
					return pnpmWorkspace{}, errors.New("packages is not a list of patterns")
				}
				pattern, err := scalar(strings.TrimSpace(item))
				if err != nil {
					return pnpmWorkspace{}, err
				}
				ws.packages = append(ws.packages, pattern)
				i++
			}
		}
	}
	return ws, nil
}

// flowList reads a flow sequence such as ["apps/*", 'packages/*'].
func flowList(value string) ([]string, error) {
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, errors.New("packages is not a list of patterns")
	}
	inner := strings.TrimSpace(value[1 : len(value)-1])
	if inner == "" {
		return nil, nil
	}
	var list []string
	for _, item := range strings.Split(inner, ",") {
		pattern, err := scalar(strings.TrimSpace(item))
		if err != nil {
			return nil, err
		}
		list = append(list, pattern)
	}
	return list, nil
}

// scalar unquotes a plain, single-quoted or double-quoted YAML scalar.
func scalar(value string) (string, error) {
	if value == "" {
		return "", errors.New("packages holds an empty pattern")
	}
	quote := value[0]
	if quote != '"' && quote != '\'' {
		if strings.ContainsAny(value[:1], "[{&*|>") {
			return "", errors.New("packages holds a pattern that is not a string")
		}
		return value, nil
	}
	if len(value) < 2 || value[len(value)-1] != quote {
		return "", errors.New("packages holds an unterminated quoted pattern")
	}
	return value[1 : len(value)-1], nil
}

// stripComment removes a comment that starts outside quotes.
func stripComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return strings.TrimRight(line, " \t")
}
