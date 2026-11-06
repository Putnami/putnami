// Package gradle is the Gradle detector of @putnami/observe: it finds the
// projects of a Gradle build, Android included, and the project dependencies
// between them, without running Gradle.
//
// The read is static. The settings file at the root lists the projects with
// include calls; each project named with a string literal is a component, at
// its default directory or at the projectDir the settings set with a literal.
// The edges are the project(":x") references and the projects.x type-safe
// accessors of each project's build file. A settings file that computes its
// includes gets a warning, and keeps what it names with literals.
package gradle

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// Ecosystem is the evidence ecosystem slug of a Gradle component.
const Ecosystem = "gradle"

// The settings and build files the detector reads, Kotlin DSL first.
var (
	SettingsFiles = []string{"settings.gradle.kts", "settings.gradle"}
	BuildFiles    = []string{"build.gradle.kts", "build.gradle"}
)

// Diagnostic codes.
const (
	CodeDynamicSettings    = "observe.gradle_dynamic_settings"
	CodeProjectMissing     = "observe.gradle_project_missing"
	CodeSettingsUnreadable = "observe.gradle_settings_unreadable"
)

// MaxScriptBytes bounds the settings and build files the detector reads.
const MaxScriptBytes = 4 << 20

// RootPath is the Gradle path of the root project.
const RootPath = ":"

// Component is one Gradle project.
type Component struct {
	// ID is the project directory relative to the repository root, or ".".
	ID string
	// Name is the project's Gradle path, such as :libs:core.
	Name string
	// DependsOn lists the ids of the projects it depends on, sorted.
	DependsOn []string
}

// Detection is what the detector found in one repository.
type Detection struct {
	// Components are sorted by ID; empty outside a Gradle build.
	Components  []Component
	Diagnostics []diag.Diagnostic
}

// Detect reads the Gradle build at root. A root without a settings file is
// no Gradle build and gets an empty detection.
func Detect(root string) Detection {
	var out Detection
	settingsFile, data, err := readFirst(root, "", SettingsFiles)
	if settingsFile == "" {
		return out
	}
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, diag.Warningf(CodeSettingsUnreadable, settingsFile, "the Gradle settings %s cannot be read: %v", settingsFile, err))
		return out
	}
	settings := readSettings(stripComments(string(data)))
	if settings.dynamic > 0 {
		out.Diagnostics = append(out.Diagnostics, diag.Warningf(CodeDynamicSettings, settingsFile,
			"%s computes %d include or projectDir value(s) that a static read cannot follow; the projects it names with string literals are kept", settingsFile, settings.dynamic))
	}
	byPath := map[string]string{}
	var paths []string
	if info, err := os.Stat(filepath.Join(root, "src")); err == nil && info.IsDir() {
		byPath[RootPath] = wsproto.ProbeRootPath
		paths = append(paths, RootPath)
	}
	taken := map[string]bool{}
	for _, project := range settings.projects {
		dir, explicit := settings.dirs[project]
		if !explicit {
			dir = strings.ReplaceAll(strings.TrimPrefix(project, ":"), ":", "/")
		}
		id, ok := wsproto.NormalizeProbePath(dir)
		if !ok {
			out.Diagnostics = append(out.Diagnostics, diag.Warningf(CodeProjectMissing, settingsFile, "the project %s lies outside the repository and is no component", project))
			continue
		}
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(id))); err != nil || !info.IsDir() {
			out.Diagnostics = append(out.Diagnostics, diag.Warningf(CodeProjectMissing, settingsFile, "the project %s has no directory %s and is no component", project, id))
			continue
		}
		if taken[id] {
			continue
		}
		taken[id] = true
		byPath[project] = id
		paths = append(paths, project)
	}
	accessors := map[string]string{}
	for _, project := range paths {
		if project != RootPath {
			accessors[accessor(project)] = project
		}
	}
	for _, project := range paths {
		id := byPath[project]
		c := Component{ID: id, Name: project}
		_, build, err := readFirst(root, id, BuildFiles)
		if err == nil && build != nil {
			edges := map[string]bool{}
			for _, target := range references(stripComments(string(build)), project, accessors) {
				if targetID, known := byPath[target]; known && targetID != id {
					edges[targetID] = true
				}
			}
			for target := range edges {
				c.DependsOn = append(c.DependsOn, target)
			}
			sort.Strings(c.DependsOn)
		}
		out.Components = append(out.Components, c)
	}
	sort.Slice(out.Components, func(i, j int) bool { return out.Components[i].ID < out.Components[j].ID })
	return out
}

// readFirst reads the first of names that exists in dir, relative to root.
// It returns the repository-relative path of the file it found, "" for none.
func readFirst(root, dir string, names []string) (string, []byte, error) {
	for _, name := range names {
		rel := name
		if dir != "" && dir != wsproto.ProbeRootPath {
			rel = dir + "/" + name
		}
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return rel, nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, MaxScriptBytes+1))
		f.Close()
		if err == nil && len(data) > MaxScriptBytes {
			err = errors.New("the file is larger than 4 MiB")
		}
		return rel, data, err
	}
	return "", nil, nil
}

// --- the settings file --------------------------------------------------------

type settings struct {
	// projects are the Gradle paths the includes name, in order.
	projects []string
	// dirs are the directories the settings set, by Gradle path.
	dirs map[string]string
	// dynamic counts the values a static read cannot follow.
	dynamic int
}

// includeCall finds the word include: a call with parentheses, Groovy's call
// without them, or a function reference such as ::include. includeBuild and
// includeFlat are other words.
var includeCall = regexp.MustCompile(`(?:^|[^A-Za-z0-9_$])include\b`)

// projectDirAssignment finds project(":x").projectDir = ..., with an optional
// find and ?. on the project.
var projectDirAssignment = regexp.MustCompile(`(?:^|[^A-Za-z0-9_$.])(?:findProject|project)\s*\(\s*("[^"$]*"|'[^']*')\s*\)\s*!*\??\.projectDir\s*=\s*([^\n;]*)`)

// anyProjectDir finds every projectDir assignment, so that the ones the
// settings compute are counted.
var anyProjectDir = regexp.MustCompile(`\.projectDir\s*=[^=]`)

// staticDir matches the projectDir values a static read follows:
// file("dir"), and File(settingsDir, "dir") or File(rootDir, "dir"), with an
// optional new or java.io.
var staticDir = regexp.MustCompile(`^(?:file\s*\(\s*("[^"$]*"|'[^']*')\s*\)|(?:new\s+)?(?:java\.io\.)?File\s*\(\s*(?:settingsDir|rootDir|rootProject\.projectDir)\s*,\s*("[^"$]*"|'[^']*')\s*\))\s*$`)

func readSettings(text string) settings {
	s := settings{dirs: map[string]string{}}
	seen := map[string]bool{}
	// The word include inside a string is no call: match on the text with
	// its string contents blanked, read the arguments from the text.
	for _, m := range includeCall.FindAllStringIndex(blankStrings(text), -1) {
		open := m[1]
		for open < len(text) && (text[open] == ' ' || text[open] == '\t') {
			open++
		}
		if open == len(text) || (text[open] != '(' && text[open] != '"' && text[open] != '\'') {
			// include it, include modules, forEach(::include): the
			// projects are computed.
			s.dynamic++
			continue
		}
		args := callArguments(text, open)
		for _, arg := range splitArguments(args) {
			value, ok := literal(arg)
			if !ok {
				s.dynamic++
				continue
			}
			project := normalPath(value)
			if project == RootPath || seen[project] {
				continue
			}
			seen[project] = true
			s.projects = append(s.projects, project)
		}
	}
	assigned := 0
	for _, m := range projectDirAssignment.FindAllStringSubmatch(text, -1) {
		assigned++
		name, _ := literal(m[1])
		value := strings.TrimSpace(m[2])
		dm := staticDir.FindStringSubmatch(value)
		if dm == nil {
			s.dynamic++
			continue
		}
		dir := dm[1]
		if dir == "" {
			dir = dm[2]
		}
		dir, _ = literal(dir)
		s.dirs[normalPath(name)] = path.Clean(strings.TrimPrefix(dir, "./"))
	}
	if extra := len(anyProjectDir.FindAllString(text, -1)) - assigned; extra > 0 {
		s.dynamic += extra
	}
	return s
}

// callArguments returns the argument text of the call whose arguments start
// at open: up to the matching parenthesis, or, for a Groovy call without
// one, up to the end of the line, continued while the line ends with a comma.
func callArguments(text string, open int) string {
	if text[open] == '(' {
		depth := 0
		for i := open; i < len(text); i++ {
			switch c := text[i]; c {
			case '"', '\'':
				i = skipString(text, i)
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					return text[open+1 : i]
				}
			}
		}
		return text[open+1:]
	}
	end := open
	for {
		nl := strings.IndexByte(text[end:], '\n')
		if nl < 0 {
			return text[open:]
		}
		end += nl
		if !strings.HasSuffix(strings.TrimSpace(text[open:end]), ",") {
			return text[open:end]
		}
		end++
	}
}

// splitArguments splits an argument list at its top-level commas.
func splitArguments(args string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case '"', '\'':
			i = skipString(args, i)
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, args[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, args[start:])
	var kept []string
	for _, arg := range out {
		if arg = strings.TrimSpace(arg); arg != "" {
			kept = append(kept, arg)
		}
	}
	return kept
}

// literal reads a string literal without interpolation: "x" or 'x'.
func literal(arg string) (string, bool) {
	arg = strings.TrimSpace(arg)
	if len(arg) < 2 || strings.HasPrefix(arg, `"""`) {
		return "", false
	}
	quote := arg[0]
	if (quote != '"' && quote != '\'') || arg[len(arg)-1] != quote {
		return "", false
	}
	value := arg[1 : len(arg)-1]
	if strings.ContainsAny(value, `\`+string(quote)) || (quote == '"' && strings.Contains(value, "$")) {
		return "", false
	}
	return value, true
}

// normalPath makes a Gradle path absolute: app is :app.
func normalPath(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, ":") {
		p = ":" + p
	}
	return p
}

// --- the build files ----------------------------------------------------------

// projectReference finds project(":x"), project(path: ":x"),
// project(path = ":x") and findProject(":x").
var projectReference = regexp.MustCompile(`(?:^|[^A-Za-z0-9_$])(?:findProject|project)\s*\(\s*(?:path\s*[:=]\s*)?("[^"$]*"|'[^']*')`)

// typeSafeAccessor finds projects.x.y, the type-safe project accessors.
var typeSafeAccessor = regexp.MustCompile(`(?:^|[^A-Za-z0-9_$])projects((?:\.[A-Za-z_][A-Za-z0-9_]*)+)`)

// references returns the Gradle paths a build file names.
func references(text, project string, accessors map[string]string) []string {
	var out []string
	for _, m := range projectReference.FindAllStringSubmatch(text, -1) {
		value, _ := literal(m[1])
		if value == "" {
			continue
		}
		if !strings.HasPrefix(value, ":") {
			// A relative path is relative to the project.
			if project == RootPath {
				value = ":" + value
			} else {
				value = project + ":" + value
			}
		}
		out = append(out, value)
	}
	for _, m := range typeSafeAccessor.FindAllStringSubmatch(text, -1) {
		parts := strings.Split(strings.TrimPrefix(m[1], "."), ".")
		for n := len(parts); n > 0; n-- {
			if target, ok := accessors[strings.Join(parts[:n], ".")]; ok {
				out = append(out, target)
				break
			}
		}
	}
	return out
}

// accessor is the type-safe accessor of a Gradle path: :libs:foo-bar is
// libs.fooBar, kebab and snake case turned to camel case.
func accessor(project string) string {
	segments := strings.Split(strings.TrimPrefix(project, ":"), ":")
	for i, segment := range segments {
		words := strings.FieldsFunc(segment, func(r rune) bool { return r == '-' || r == '_' })
		for j := 1; j < len(words); j++ {
			words[j] = strings.ToUpper(words[j][:1]) + words[j][1:]
		}
		segments[i] = strings.Join(words, "")
	}
	return strings.Join(segments, ".")
}

// --- comments -----------------------------------------------------------------

// stripComments blanks the // and /* */ comments of a Groovy or Kotlin
// script, keeping its strings and its line breaks.
func stripComments(text string) string {
	b := []byte(text)
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '"' || b[i] == '\'':
			i = skipString(text, i)
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '/':
			for ; i < len(b) && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			end := strings.Index(text[i+2:], "*/")
			stop := len(b)
			if end >= 0 {
				stop = i + 2 + end + 2
			}
			for ; i < stop; i++ {
				if b[i] != '\n' {
					b[i] = ' '
				}
			}
			i--
		}
	}
	return string(b)
}

// blankStrings replaces the contents of the strings of a script with spaces,
// keeping their quotes, their length and the line breaks.
func blankStrings(text string) string {
	b := []byte(text)
	for i := 0; i < len(b); i++ {
		if b[i] != '"' && b[i] != '\'' {
			continue
		}
		end := skipString(text, i)
		quotes := 1
		if strings.HasPrefix(text[i:], strings.Repeat(string(b[i]), 3)) {
			quotes = 3
		}
		for j := i + quotes; j <= end-quotes && j < len(b); j++ {
			if b[j] != '\n' {
				b[j] = ' '
			}
		}
		i = end
	}
	return string(b)
}

// skipString returns the index of the closing quote of the string that opens
// at i, a triple-quoted one included; the last index when it does not close.
func skipString(text string, i int) int {
	quote := text[i]
	if strings.HasPrefix(text[i:], strings.Repeat(string(quote), 3)) {
		if end := strings.Index(text[i+3:], strings.Repeat(string(quote), 3)); end >= 0 {
			return i + 3 + end + 2
		}
		return len(text) - 1
	}
	for j := i + 1; j < len(text); j++ {
		switch text[j] {
		case '\\':
			j++
		case quote:
			return j
		case '\n':
			return j - 1
		}
	}
	return len(text) - 1
}
