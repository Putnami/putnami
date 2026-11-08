package stacks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Diagnostic codes of the toolchain check.
const (
	CodeToolchainMismatch = "stacks.toolchain_mismatch"
	CodeToolchainMissing  = "stacks.toolchain_missing"
	CodeToolchainUnpinned = "stacks.toolchain_unpinned"
)

// Expected is the toolchain version a project expects.
type Expected struct {
	// Version is the expected version; empty when nothing states one.
	Version string `json:"version,omitempty"`
	// Source is the workspace-relative file that states it, or "profile"
	// when the profile or an override sets toolchain.version.
	Source string `json:"source,omitempty"`
}

// ExpectedVersion reads the expected toolchain version of the project at
// projectDir, of the stack root stackRoot, both workspace-relative, in the
// workspace at root. toolchain.version outranks the files; the files are read
// in the project directory first, then at the stack root, in the order the
// profile lists them. A range does not end the search: an exact version a
// later file states wins over it (ADR 0002).
func (t Toolchain) ExpectedVersion(root, projectDir, stackRoot string) (Expected, error) {
	if t.Version != "" {
		return Expected{Version: t.Version, Source: "profile"}, nil
	}
	var firstRange Expected
	for _, dir := range versionDirs(projectDir, stackRoot) {
		for _, f := range t.Files {
			rel := path.Join(dir, f.Path)
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // workspace-relative path under the workspace root
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return Expected{}, err
			}
			version, err := readVersion(data, f.Key)
			if err != nil {
				return Expected{}, fmt.Errorf("%s: %w", rel, err)
			}
			if version == "" {
				continue
			}
			if _, isRange := parseRange(version); isRange && !IsExact(version) {
				if firstRange.Version == "" {
					firstRange = Expected{Version: version, Source: rel}
				}
				continue
			}
			return Expected{Version: version, Source: rel}, nil
		}
	}
	return firstRange, nil
}

// readVersion reads a version from a file's content: the string at a dotted
// key of a JSON object or a TOML table, or the first non-empty line.
func readVersion(data []byte, key string) (string, error) {
	if key == "" {
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
				return line, nil
			}
		}
		return "", nil
	}
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("{")) {
		var doc map[string]any
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return "", err
		}
		var cur any = doc
		for _, k := range strings.Split(key, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				return "", nil
			}
			cur = m[k]
		}
		s, _ := cur.(string)
		return s, nil
	}
	return tomlString(data, key), nil
}

var (
	tomlTableLine = regexp.MustCompile(`^\[\s*([A-Za-z0-9_.\-"]+)\s*\]\s*(#.*)?$`)
	tomlKeyLine   = regexp.MustCompile(`^([A-Za-z0-9_\-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')\s*(#.*)?$`)
)

// tomlString reads the string value of a dotted key ("toolchain.channel") from
// TOML text: the last segment is the key, the others the table. It reads only
// what a toolchain file holds, a table header and a quoted string.
func tomlString(data []byte, key string) string {
	table, name := "", key
	if i := strings.LastIndex(key, "."); i >= 0 {
		table, name = key[:i], key[i+1:]
	}
	current := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := tomlTableLine.FindStringSubmatch(line); m != nil {
			current = strings.ReplaceAll(m[1], `"`, "")
			continue
		}
		if m := tomlKeyLine.FindStringSubmatch(line); m != nil && current == table && m[1] == name {
			if m[2] != "" {
				return m[2]
			}
			return m[3]
		}
	}
	return ""
}

// MachineVersion runs the toolchain command in dir and matches its output:
// the standard output, or the standard error when the standard output does
// not match. A missing command is ErrToolchainMissing.
func (t Toolchain) MachineVersion(ctx context.Context, dir string, env []string) (string, error) {
	re, err := regexp.Compile(t.Pattern)
	if err != nil {
		return "", err
	}
	if _, err := exec.LookPath(t.Command[0]); err != nil {
		return "", fmt.Errorf("%w: %s", ErrToolchainMissing, t.Command[0])
	}
	cmd := exec.CommandContext(ctx, t.Command[0], t.Command[1:]...) //nolint:gosec // the profile's own toolchain command
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", strings.Join(t.Command, " "), err, strings.TrimSpace(stderr.String()))
	}
	idx := re.SubexpIndex("version")
	for _, out := range []string{stdout.String(), stderr.String()} {
		if m := re.FindStringSubmatch(out); m != nil {
			return m[idx], nil
		}
	}
	return "", fmt.Errorf("%s printed no version that matches %q", strings.Join(t.Command, " "), t.Pattern)
}

// Selection is the toolchain a task runs and the outcome of its check.
type Selection struct {
	// Env is the environment of the task's steps.
	Env []string
	// Executable is the path of the pinned toolchain; empty when the toolchain
	// on PATH runs.
	Executable string
	// Verdict is the outcome of the check of the selected toolchain.
	Verdict Verdict
}

// Program returns the program a step runs: the pinned toolchain for a step
// that names the toolchain command, and name otherwise.
func (s Selection) Program(t Toolchain, name string) string {
	if s.Executable != "" && name == t.Command[0] {
		return s.Executable
	}
	return name
}

// Select chooses the toolchain of a task in dir. The toolchain on PATH runs
// when its check does not fail. Otherwise the toolchain the Provisioned
// variable of env names runs, with its directory first on PATH, when it
// answers and its check passes or nothing is on PATH. The error is the one of
// the toolchain on PATH when no toolchain answers.
func (t Toolchain) Select(ctx context.Context, dir string, expected Expected, env []string) (Selection, error) {
	onPath := Selection{Env: env}
	machine, machineErr := t.MachineVersion(ctx, dir, env)
	if machineErr == nil {
		onPath.Verdict = t.Compare(expected, machine)
		if !onPath.Verdict.Fail {
			return onPath, nil
		}
	}
	pinned := environmentValue(env, t.Provisioned)
	if t.Provisioned == "" || !filepath.IsAbs(pinned) {
		return onPath, machineErr
	}
	withPin := t
	withPin.Command = append([]string{pinned}, t.Command[1:]...)
	version, err := withPin.MachineVersion(ctx, dir, env)
	if err != nil {
		return onPath, machineErr
	}
	verdict := t.Compare(expected, version)
	if verdict.Fail && machineErr == nil {
		return onPath, nil
	}
	return Selection{Env: prependPath(env, filepath.Dir(pinned)), Executable: pinned, Verdict: verdict}, nil
}

func environmentValue(env []string, name string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value
		}
	}
	return ""
}

// prependPath returns env with dir first on its PATH.
func prependPath(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	found := false
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && !found && strings.EqualFold(name, "PATH") {
			found = true
			entry = name + "=" + dir + string(os.PathListSeparator) + value
		}
		out = append(out, entry)
	}
	if !found {
		out = append(out, "PATH="+dir)
	}
	return out
}

// ErrToolchainMissing is the error of a toolchain command that is not on PATH.
var ErrToolchainMissing = errors.New("the toolchain command is missing")

// exactVersion matches a version that names one release, such as 1.82.0,
// 21.0.4 or 8.3: a channel name such as "stable" does not.
var exactVersion = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+)+([-+][0-9A-Za-z.\-+]*)?$`)

// IsExact reports whether a stated version names one release.
func IsExact(version string) bool {
	return exactVersion.MatchString(version)
}

// Verdict is the outcome of a toolchain check.
type Verdict struct {
	// Code is the diagnostic code, empty when the check passes silently.
	Code string
	// Fail says the task must not run.
	Fail bool
	// Message is the diagnostic's text.
	Message string
}

// Compare decides the check of an expected version against the machine's.
func (t Toolchain) Compare(expected Expected, machine string) Verdict {
	if expected.Version == "" {
		return Verdict{Code: CodeToolchainUnpinned, Message: fmt.Sprintf("no toolchain file states the expected version (machine: %s): the cache key carries no toolchain version", machine)}
	}
	if !IsExact(expected.Version) {
		// A stated range, such as composer.json's require.php ">=8.4", is
		// checked: the machine's release must satisfy it (ADR 0002).
		r, ok := parseRange(expected.Version)
		if !ok {
			return Verdict{Code: CodeToolchainUnpinned, Message: fmt.Sprintf("%s states %q, which names no exact version and no range (machine: %s): the cache key carries no toolchain version", expected.Source, expected.Version, machine)}
		}
		if r.satisfiedBy(machine) {
			return Verdict{}
		}
		msg := fmt.Sprintf("%s expects a version in %s, the machine has %s; install or select one, or set toolchain.check to warn", expected.Source, expected.Version, machine)
		return Verdict{Code: CodeToolchainMismatch, Fail: t.Check != CheckWarn, Message: msg}
	}
	if versionMatches(expected.Version, machine) {
		return Verdict{}
	}
	msg := fmt.Sprintf("%s expects version %s, the machine has %s; install or select %s, or set toolchain.check to warn", expected.Source, expected.Version, machine, expected.Version)
	return Verdict{Code: CodeToolchainMismatch, Fail: t.Check != CheckWarn, Message: msg}
}

// versionMatches reports whether the machine's version is the expected one:
// equal, or, for an expected version with fewer components such as a rustup
// "1.82" channel, a stable release that version names (1.82.0, 1.82.3, not
// 1.82.0-nightly).
func versionMatches(expected, machine string) bool {
	e := strings.TrimPrefix(expected, "v")
	m := strings.TrimPrefix(machine, "v")
	if e == m {
		return true
	}
	rest, ok := strings.CutPrefix(m, e+".")
	return ok && rest != "" && strings.Trim(rest, "0123456789.") == ""
}
