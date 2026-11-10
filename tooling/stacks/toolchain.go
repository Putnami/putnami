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
	"runtime"
	"slices"
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
	// Reading is ReadingPlain when the version reads with no dialect though
	// the check names one: a pub-flutter constraint in a pubspec whose
	// environment.sdk starts at 3.9 or later, which pub reads as stated.
	Reading string `json:"reading,omitempty"`
	// Root is the absolute workspace root Source is relative to, when a task
	// hands the expected version to its stack's Provide. It reaches no key.
	Root string `json:"-"`
}

// ReadingPlain marks an expected version that reads with no dialect.
const ReadingPlain = "plain"

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
			version, err := f.read(data)
			if err != nil {
				return Expected{}, fmt.Errorf("%s: %w", rel, err)
			}
			if version == "" {
				continue
			}
			if _, isRange := t.parseStated(version, t.Ranges); isRange && !IsExact(version) {
				if firstRange.Version == "" {
					firstRange = Expected{Version: version, Source: rel, Reading: t.reading(f, data)}
				}
				continue
			}
			return Expected{Version: version, Source: rel, Reading: t.reading(f, data)}, nil
		}
	}
	return firstRange, nil
}

// reading is how the version a file states reads: a pub-flutter constraint in
// a pubspec whose environment.sdk starts at 3.9 or later reads plain, since
// pub reads it as stated there.
func (t Toolchain) reading(f VersionFile, data []byte) string {
	if t.Ranges != RangesPubFlutter {
		return ""
	}
	sdk, err := readVersion(f.Path, data, "environment.sdk")
	if err != nil || sdk == "" {
		return ""
	}
	r, ok := t.parseStated(sdk, RangesPubSDK)
	if !ok {
		return ""
	}
	if low, ok := r.lowerBound(); ok && compareRelease(low, []int{3, 9}) >= 0 {
		return ReadingPlain
	}
	return ""
}

// read reads the version the file's content states: the "version" group of
// its pattern, else what readVersion reads at its key.
func (f VersionFile) read(data []byte) (string, error) {
	if f.Pattern == "" {
		return readVersion(f.Path, data, f.Key)
	}
	re, err := regexp.Compile(f.Pattern)
	if err != nil {
		return "", err
	}
	m := re.FindSubmatch(data)
	if m == nil {
		return "", nil
	}
	return strings.TrimSpace(string(m[re.SubexpIndex("version")])), nil
}

// readVersion reads a version from the content of the file name: the string
// at a dotted key of a JSON object, a YAML mapping (a .yaml or .yml file) or
// a TOML table, or the first non-empty line.
func readVersion(name string, data []byte, key string) (string, error) {
	if ext := strings.ToLower(path.Ext(name)); key != "" && (ext == ".yaml" || ext == ".yml") {
		return yamlString(data, key), nil
	}
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

// yamlString reads the scalar value of a dotted key ("environment.sdk") from
// YAML text. It reads block mappings indented with spaces, flow mappings
// ("environment: {sdk: ^3.7.0}", on one line or several) and plain or quoted
// scalars: what a pubspec.yaml environment holds. A key it does not find, or
// that holds no scalar, reads as "".
func yamlString(data []byte, key string) string {
	segments := strings.Split(key, ".")
	type level struct{ indent, child int }
	var matched []level
	sc := bufio.NewScanner(bytes.NewReader(data))
	// flow reads the flow collection that starts text, over the lines it
	// spans, and answers the scalar at the rest of the key.
	flow := func(text string, rest []string) string {
		for !yamlFlowClosed(text) && sc.Scan() {
			text += "\n" + sc.Text()
		}
		value, ok := yamlFlow(text)
		if !ok {
			return ""
		}
		for _, k := range rest {
			m, isMap := value.(map[string]any)
			if !isMap {
				return ""
			}
			value = m[k]
		}
		s, _ := value.(string)
		return s
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "---") {
			continue
		}
		indent := len(line) - len(trimmed)
		for len(matched) > 0 && indent <= matched[len(matched)-1].indent {
			matched = matched[:len(matched)-1]
		}
		want := 0
		if n := len(matched); n > 0 {
			if matched[n-1].child < 0 {
				matched[n-1].child = indent
			}
			want = matched[n-1].child
		}
		if indent != want {
			continue
		}
		if strings.HasPrefix(trimmed, "{") {
			// The mapping the key walks is a flow mapping.
			return flow(trimmed, segments[len(matched):])
		}
		k, raw, ok := yamlSplit(trimmed)
		if !ok || k != segments[len(matched)] {
			continue
		}
		if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
			return flow(raw, segments[len(matched)+1:])
		}
		v := yamlScalar(raw)
		if len(matched) == len(segments)-1 {
			return v
		}
		if v != "" {
			return ""
		}
		matched = append(matched, level{indent: indent, child: -1})
	}
	return ""
}

// yamlSplit splits a "key: value" line into its unquoted key and the raw text
// of its value.
func yamlSplit(line string) (string, string, bool) {
	i := strings.Index(line, ":")
	for i >= 0 && i+1 < len(line) && line[i+1] != ' ' {
		next := strings.Index(line[i+1:], ":")
		if next < 0 {
			i = -1
			break
		}
		i += next + 1
	}
	if i <= 0 {
		return "", "", false
	}
	return strings.Trim(strings.TrimSpace(line[:i]), `"'`), strings.TrimSpace(line[i+1:]), true
}

// yamlScalar reads the raw text of a block value as a scalar: unquoted, with
// its comment removed.
func yamlScalar(value string) string {
	if value != "" && (value[0] == '"' || value[0] == '\'') {
		if end := strings.IndexByte(value[1:], value[0]); end >= 0 {
			return value[1 : end+1]
		}
		return ""
	}
	if strings.HasPrefix(value, "#") {
		return ""
	}
	if c := strings.Index(value, " #"); c >= 0 {
		value = strings.TrimSpace(value[:c])
	}
	return value
}

// yamlFlowClosed reports whether text holds a whole flow collection: every
// bracket it opens outside quotes and comments is closed.
func yamlFlowClosed(text string) bool {
	p := &flowParser{s: text}
	depth := 0
	// A quote opens a quoted scalar only where a scalar starts.
	start := true
	for p.skip(); p.i < len(p.s); p.skip() {
		c := p.s[p.i]
		switch {
		case c == '{' || c == '[':
			depth++
			p.i++
		case c == '}' || c == ']':
			depth--
			p.i++
			if depth <= 0 {
				return true
			}
		case (c == '"' || c == '\'') && start:
			if _, ok := p.quoted(); !ok {
				return false
			}
		default:
			p.i++
		}
		start = strings.ContainsRune("{[,:", rune(c))
	}
	return false
}

// yamlFlow reads the flow collection at the start of text: a mapping is a
// map[string]any, a sequence a []any, a scalar a string.
func yamlFlow(text string) (any, bool) {
	p := &flowParser{s: text}
	return p.value()
}

// flowParser reads YAML flow collections.
type flowParser struct {
	s string
	i int
}

// skip passes white space, line breaks and comments.
func (p *flowParser) skip() {
	for p.i < len(p.s) {
		switch c := p.s[p.i]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			p.i++
		case c == '#' && (p.i == 0 || strings.ContainsRune(" \t\n", rune(p.s[p.i-1]))):
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				p.i++
			}
		default:
			return
		}
	}
}

func (p *flowParser) value() (any, bool) {
	p.skip()
	if p.i >= len(p.s) {
		return nil, false
	}
	switch p.s[p.i] {
	case '{':
		return p.mapping()
	case '[':
		return p.sequence()
	case '"', '\'':
		return p.quoted()
	}
	return p.plain(), true
}

func (p *flowParser) mapping() (any, bool) {
	p.i++
	m := map[string]any{}
	for {
		p.skip()
		if p.i >= len(p.s) {
			return nil, false
		}
		if p.s[p.i] == '}' {
			p.i++
			return m, true
		}
		k, ok := p.value()
		key, isString := k.(string)
		if !ok || !isString {
			return nil, false
		}
		p.skip()
		var v any
		if p.i < len(p.s) && p.s[p.i] == ':' {
			p.i++
			p.skip()
			if p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != '}' {
				if v, ok = p.value(); !ok {
					return nil, false
				}
			}
		}
		m[key] = v
		p.skip()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
		} else if p.i >= len(p.s) || p.s[p.i] != '}' {
			return nil, false
		}
	}
}

func (p *flowParser) sequence() (any, bool) {
	p.i++
	var list []any
	for {
		p.skip()
		if p.i >= len(p.s) {
			return nil, false
		}
		if p.s[p.i] == ']' {
			p.i++
			return list, true
		}
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		list = append(list, v)
		p.skip()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
		} else if p.i >= len(p.s) || p.s[p.i] != ']' {
			return nil, false
		}
	}
}

// quoted reads a single- or double-quoted scalar.
func (p *flowParser) quoted() (any, bool) {
	quote := p.s[p.i]
	var b strings.Builder
	for p.i++; p.i < len(p.s); p.i++ {
		c := p.s[p.i]
		switch {
		case c == quote && quote == '\'' && p.i+1 < len(p.s) && p.s[p.i+1] == '\'':
			b.WriteByte('\'')
			p.i++
		case c == quote:
			p.i++
			return b.String(), true
		case c == '\\' && quote == '"' && p.i+1 < len(p.s):
			p.i++
			b.WriteByte(p.s[p.i])
		default:
			b.WriteByte(c)
		}
	}
	return nil, false
}

// plain reads a plain scalar: up to a flow indicator, a ": " or a comment,
// with its line breaks folded.
func (p *flowParser) plain() string {
	start := p.i
	for ; p.i < len(p.s); p.i++ {
		c := p.s[p.i]
		if c == ',' || c == '{' || c == '}' || c == '[' || c == ']' {
			break
		}
		if c == ':' && (p.i+1 == len(p.s) || strings.ContainsRune(" \t\r\n,{}[]", rune(p.s[p.i+1]))) {
			break
		}
		if c == '#' && p.i > start && strings.ContainsRune(" \t\n", rune(p.s[p.i-1])) {
			break
		}
	}
	return strings.Join(strings.Fields(p.s[start:p.i]), " ")
}

// MachineVersions runs the toolchain command in dir and returns every version
// its output states, in order: every match of the pattern in the standard
// output, or in the standard error when the standard output has none. A
// command that lists the installed toolchains states several.
func (t Toolchain) MachineVersions(ctx context.Context, dir string, env []string) ([]string, error) {
	re, err := regexp.Compile(t.Pattern)
	if err != nil {
		return nil, err
	}
	if _, err := exec.LookPath(t.Command[0]); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrToolchainMissing, t.Command[0])
	}
	cmd := exec.CommandContext(ctx, t.Command[0], t.Command[1:]...) //nolint:gosec // the profile's own toolchain command
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", strings.Join(t.Command, " "), err, strings.TrimSpace(stderr.String()))
	}
	idx := re.SubexpIndex("version")
	for _, out := range []string{stdout.String(), stderr.String()} {
		var versions []string
		for _, m := range re.FindAllStringSubmatch(out, -1) {
			versions = append(versions, m[idx])
		}
		if len(versions) > 0 {
			return versions, nil
		}
	}
	return nil, fmt.Errorf("%s printed no version that matches %q", strings.Join(t.Command, " "), t.Pattern)
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
// that names the toolchain command, a program beside it for a step that names
// one, such as cargo beside rustc, then a tool found on the selection's own
// PATH, which holds the directories of the task's tools, and name otherwise.
// The machine's toolchain command stays a bare name, so a caller tells the
// pinned toolchain, an absolute path, from the machine's. A process resolves
// a bare name on its own PATH, which holds no pinned toolchain and no tool.
func (s Selection) Program(t Toolchain, name string) string {
	if name == t.Command[0] {
		if s.Executable != "" {
			return s.Executable
		}
		return name
	}
	if s.Executable != "" {
		sibling := filepath.Join(filepath.Dir(s.Executable), name+filepath.Ext(s.Executable))
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
			return sibling
		}
	}
	if !strings.ContainsAny(name, `/\`) {
		if found, ok := LookPathIn(s.Env, name); ok {
			return found
		}
	}
	return name
}

// Select chooses the toolchain of a task in dir. The toolchain on PATH runs
// when its check does not fail. Otherwise the toolchain the Provisioned
// variable of env names runs, with its directory first on PATH, when it
// answers and its check passes or nothing is on PATH. The error is the one of
// the toolchain on PATH when no toolchain answers.
func (t Toolchain) Select(ctx context.Context, dir string, expected Expected, env []string) (Selection, error) {
	return t.SelectWith(ctx, dir, expected, env, versionMatches)
}

// SelectWith is Select with a stack's own rule for an exact expected version.
// When the toolchain command fails and the fallback lists versions none of
// which matches, the toolchain on PATH is a mismatch, so a pinned or provided
// toolchain may take its place.
func (t Toolchain) SelectWith(ctx context.Context, dir string, expected Expected, env []string, matches func(expected, machine string) bool) (Selection, error) {
	onPath := Selection{Env: env}
	machine, machineErr := t.MachineVersion(ctx, dir, env)
	if machineErr == nil {
		onPath.Verdict = t.CompareWith(expected, machine, matches)
		if !onPath.Verdict.Fail {
			return onPath, nil
		}
	} else if v, ok := t.fallbackVerdict(ctx, dir, expected, env, machineErr, matches); ok {
		onPath.Verdict, machineErr = v, nil
		if !v.Fail {
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
	verdict := t.CompareWith(expected, version, matches)
	if verdict.Fail && machineErr == nil {
		return onPath, nil
	}
	selected := prependPath(env, filepath.Dir(pinned))
	if t.Home != "" {
		selected = setEnv(selected, t.Home, filepath.Dir(filepath.Dir(pinned)))
	}
	return Selection{Env: selected, Executable: pinned, Verdict: verdict}, nil
}

// setEnv returns env with name set to value, replacing any earlier value.
func setEnv(env []string, name, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, name+"=") {
			out = append(out, entry)
		}
	}
	return append(out, name+"="+value)
}

// ProgramIn returns the program a step runs in env: a program beside the
// toolchain on PATH when none is pinned, then what Program answers, else the
// first executable of that name on the PATH of env, where a task's tools are,
// else name.
func (s Selection) ProgramIn(t Toolchain, name string, env []string) string {
	if strings.ContainsAny(name, `/\`) {
		return s.Program(t, name)
	}
	// A program beside the toolchain on PATH, such as the dart of a Flutter
	// SDK, belongs to it, whatever comes first on PATH.
	if s.Executable == "" && name != t.Command[0] {
		if toolchain, ok := LookPathIn(env, t.Command[0]); ok {
			if sibling, ok := LookPathIn([]string{"PATH=" + filepath.Dir(toolchain)}, name); ok {
				return sibling
			}
		}
	}
	if program := s.Program(t, name); program != name {
		return program
	}
	if found, ok := LookPathIn(env, name); ok {
		return found
	}
	return name
}

// LookPathIn finds an executable file on the PATH of env, with the PATHEXT
// extensions on Windows for a name that has none.
func LookPathIn(env []string, name string) (string, bool) {
	names := []string{name}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		names = nil
		exts := environmentValue(env, "PATHEXT")
		if exts == "" {
			exts = ".com;.exe;.bat;.cmd"
		}
		for _, ext := range strings.Split(strings.ToLower(exts), ";") {
			if ext != "" {
				names = append(names, name+ext)
			}
		}
	}
	for _, dir := range filepath.SplitList(pathValue(env)) {
		// A relative entry would resolve against the step's directory, which
		// Go's own lookup refuses too.
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		for _, n := range names {
			candidate := filepath.Join(dir, n)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0) {
				return candidate, true
			}
		}
	}
	return "", false
}

// pathValue is the PATH of env, whatever the case of its name.
func pathValue(env []string) string {
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok && strings.EqualFold(key, "PATH") {
			return value
		}
	}
	return ""
}

// Pinned selects the toolchain the Provisioned variable of env names, whatever
// the toolchain on PATH: a further check that fails with the toolchain on PATH
// asks for it.
func (t Toolchain) Pinned(ctx context.Context, dir string, expected Expected, env []string) (Selection, error) {
	pinned := environmentValue(env, t.Provisioned)
	if t.Provisioned == "" || !filepath.IsAbs(pinned) {
		return Selection{}, fmt.Errorf("%w: no installed %s", ErrToolchainMissing, t.Command[0])
	}
	withPin := t
	withPin.Command = append([]string{pinned}, t.Command[1:]...)
	version, err := withPin.MachineVersion(ctx, dir, env)
	if err != nil {
		return Selection{}, err
	}
	selected := prependPath(env, filepath.Dir(pinned))
	if t.Home != "" {
		selected = setEnv(selected, t.Home, filepath.Dir(filepath.Dir(pinned)))
	}
	return Selection{Env: selected, Executable: pinned, Verdict: t.Compare(expected, version)}, nil
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

// appendPath returns env with dir last on its PATH: a tool the machine already
// holds keeps coming first.
func appendPath(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	found := false
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && !found && strings.EqualFold(name, "PATH") {
			found = true
			entry = name + "=" + value + string(os.PathListSeparator) + dir
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

// exactVersion matches a version that names one release or one release
// line, such as 1.82.0, 21.0.4, 8.3 or a Java major such as 21: a channel
// name such as "stable" does not.
var exactVersion = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+)*([-+][0-9A-Za-z.\-+]*)?$`)

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

// fallbackVerdict runs the fallback after the toolchain command failed with
// err: when the versions it lists include none that satisfies an exact
// expected version, the check is a mismatch that names them. ok is false
// otherwise, and err stands.
func (t Toolchain) fallbackVerdict(ctx context.Context, dir string, expected Expected, env []string, err error, matches func(expected, machine string) bool) (Verdict, bool) {
	if len(t.Fallback) == 0 || errors.Is(err, ErrToolchainMissing) || !IsExact(expected.Version) {
		return Verdict{}, false
	}
	fallback := t
	fallback.Command = t.Fallback
	installed, ferr := fallback.MachineVersions(ctx, dir, env)
	if ferr != nil || slices.ContainsFunc(installed, func(m string) bool { return matches(expected.Version, m) }) {
		return Verdict{}, false
	}
	msg := fmt.Sprintf("%v; the machine has %s, and none satisfies version %s that %s states; install or select it, or set toolchain.check to warn", err, strings.Join(installed, ", "), expected.Version, expected.Source)
	return Verdict{Code: CodeToolchainMismatch, Fail: t.Check != CheckWarn, Message: msg}, true
}

// Compare decides the check of an expected version against the machine's.
func (t Toolchain) Compare(expected Expected, machine string) Verdict {
	return t.CompareWith(expected, machine, versionMatches)
}

// CompareWith is Compare with a stack's own rule for an exact expected
// version.
func (t Toolchain) CompareWith(expected Expected, machine string, matches func(expected, machine string) bool) Verdict {
	if expected.Version == "" {
		return Verdict{Code: CodeToolchainUnpinned, Message: fmt.Sprintf("no toolchain file states the expected version (machine: %s): the cache key carries no toolchain version", machine)}
	}
	// A dialect reads a constraint a file states; toolchain.version, set by
	// the profile or a project, reads plain (ADR 0013).
	dialect := t.Ranges
	switch {
	case expected.Source == "profile":
		dialect = ""
	case expected.Reading == ReadingPlain:
		// Pub's syntax, with none of its SDK rules.
		dialect = pubSyntax
	}
	if !IsExact(expected.Version) || dialect == RangesPubFlutter {
		// A stated range, such as composer.json's require.php ">=8.4", is
		// checked: the machine's release must satisfy it (ADR 0002).
		r, ok := t.parseStated(expected.Version, dialect)
		if !ok {
			return Verdict{Code: CodeToolchainUnpinned, Message: fmt.Sprintf("%s states %q, which names no exact version and no range (machine: %s): the cache key carries no toolchain version", expected.Source, expected.Version, machine)}
		}
		if r.satisfiedBy(machine) {
			return Verdict{}
		}
		reading := ""
		if dialect != "" {
			reading = " as pub reads it"
		}
		msg := fmt.Sprintf("%s expects a version in %s%s, the machine has %s; install or select one, or set toolchain.check to warn", expected.Source, expected.Version, reading, machine)
		return Verdict{Code: CodeToolchainMismatch, Fail: t.Check != CheckWarn, Message: msg}
	}
	if matches(expected.Version, machine) {
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
