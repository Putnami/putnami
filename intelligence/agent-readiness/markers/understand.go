package markers

import (
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/internal/ci"
	"go.putnami.dev/intelligence/agent-readiness/internal/scope"
	"go.putnami.dev/intelligence/agent-readiness/inventory"
)

// command is a build or test command a tool can discover in the repository,
// with the fragments that show a CI configuration or a document runs it.
type command struct {
	test      bool
	fragments []string
	source    string
}

// commandFiles are the root files that declare commands by name.
var commandFiles = map[string]bool{
	"GNUmakefile": true, "Justfile": true, "Makefile": true, "Taskfile.yaml": true,
	"Taskfile.yml": true, "justfile": true, "makefile": true,
}

var (
	makeTarget = regexp.MustCompile(`(?m)^(build|test|check)[A-Za-z0-9_-]*\s*:`)
	justRecipe = regexp.MustCompile(`(?m)^(build|test)\b[^:\n]*:`)
	taskName   = regexp.MustCompile(`(?m)^\s+(build|test):`)
)

// discoverCommands finds the commands a newcomer, or an agent, would find:
// named targets in root task files, package scripts, and the default
// commands of the languages the repository builds.
func discoverCommands(c *computation) []command {
	var commands []command
	add := func(source string, test bool, fragments ...string) {
		commands = append(commands, command{test: test, fragments: fragments, source: source})
	}
	for _, file := range []string{"Makefile", "GNUmakefile", "makefile"} {
		for _, match := range makeTarget.FindAllStringSubmatch(string(c.Contents[file]), -1) {
			add(file, match[1] != "build", "make "+strings.TrimSpace(strings.TrimSuffix(match[0], ":")))
		}
	}
	for _, file := range []string{"justfile", "Justfile"} {
		for _, match := range justRecipe.FindAllStringSubmatch(string(c.Contents[file]), -1) {
			add(file, match[1] == "test", "just "+match[1])
		}
	}
	for _, file := range []string{"Taskfile.yml", "Taskfile.yaml"} {
		for _, match := range taskName.FindAllStringSubmatch(string(c.Contents[file]), -1) {
			add(file, match[1] == "test", "task "+match[1])
		}
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(c.Contents["package.json"], &manifest) == nil {
		if _, ok := manifest.Scripts["build"]; ok {
			add("package.json", false, "npm run build", "pnpm build", "pnpm run build", "yarn build", "bun run build", "turbo build", "turbo run build")
		}
		if _, ok := manifest.Scripts["test"]; ok {
			add("package.json", true, "npm test", "npm run test", "pnpm test", "pnpm run test", "yarn test", "bun test", "bun run test", "turbo test", "turbo run test")
		}
	}
	languages := []struct {
		manifest    string
		build, test []string
	}{
		{"putnami.workspace.json", []string{"putnami build"}, []string{"putnami test"}},
		{"go.mod", []string{"go build", "go vet"}, []string{"go test"}},
		{"Cargo.toml", []string{"cargo build"}, []string{"cargo test"}},
		{"pyproject.toml", nil, []string{"pytest"}},
		{"build.gradle", []string{"gradle build", "gradlew build"}, []string{"gradle test", "gradlew test"}},
		{"build.gradle.kts", []string{"gradle build", "gradlew build"}, []string{"gradle test", "gradlew test"}},
		{"pom.xml", []string{"mvn package", "mvn verify"}, []string{"mvn test", "mvn verify"}},
	}
	for _, language := range languages {
		source := c.firstNamed(language.manifest)
		if source == "" {
			continue
		}
		if len(language.build) > 0 {
			add(source, false, language.build...)
		}
		add(source, true, language.test...)
	}
	return commands
}

// firstNamed returns the shallowest tracked file with that base name.
func (c *computation) firstNamed(name string) string {
	best := ""
	for _, file := range c.Files {
		if path.Base(file.Path) != name || scope.Skipped(file.Path) {
			continue
		}
		if best == "" || strings.Count(file.Path, "/") < strings.Count(best, "/") {
			best = file.Path
		}
	}
	return best
}

func (c *computation) fragments(test bool) []string {
	var fragments []string
	for _, command := range c.commands {
		if command.test == test {
			fragments = append(fragments, command.fragments...)
		}
	}
	return fragments
}

func (c *computation) allFragments() []string {
	return append(c.fragments(false), c.fragments(true)...)
}

func (c *computation) discoverable() contract.Marker {
	const id = "understand.commands"
	if len(c.commands) == 0 {
		return marker(id, contract.MarkerAbsent, nil,
			fileList("git ls-files --", []string{"Makefile", "package.json", "justfile", "Taskfile.yml", "go.mod", "Cargo.toml", "pyproject.toml"}), nil)
	}
	var sources []string
	seen := map[string]bool{}
	for _, command := range c.commands {
		if !seen[command.source] {
			seen[command.source] = true
			sources = append(sources, command.source)
		}
	}
	jobs := ci.Runs(c.configs, c.allFragments()...)
	if len(jobs) == 0 {
		return marker(id, contract.MarkerExists, nil, fileList("git ls-files --", sources), sources)
	}
	paths := jobPaths(jobs)
	return marker(id, contract.MarkerEnforced, days(c.Now, c.enforcedSince(nil, jobs)),
		fileList("git log --diff-filter=A --format=%cs --", paths), append(sources, paths...))
}

// instructionFiles returns the instruction files that describe an area, or
// the repository's own for -1.
func (c *computation) instructionFiles(area int) []string {
	var found []string
	for _, file := range c.Files {
		if !inventory.IsInstructionFile(file.Path) || scope.Skipped(file.Path) {
			continue
		}
		dir := path.Dir(file.Path)
		switch {
		case area < 0 && (dir == "." || strings.HasPrefix(file.Path, ".github/") || strings.HasPrefix(file.Path, ".cursor/")):
			found = append(found, file.Path)
		case area >= 0 && dir == c.Layout.Areas[area].Path:
			found = append(found, file.Path)
		}
	}
	return found
}

func (c *computation) instructions(area int) contract.Marker {
	const id = "understand.instructions"
	files := c.instructionFiles(area)
	if len(files) == 0 {
		names := []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md", ".github/copilot-instructions.md", ".cursorrules"}
		candidates := make([]string, 0, len(names))
		for _, name := range names {
			candidates = append(candidates, c.dir(area)+name)
		}
		return marker(id, contract.MarkerAbsent, nil, fileList("git ls-files --", candidates), nil)
	}
	newest := files[0]
	for _, file := range files {
		if c.Dates.Changed[file] > c.Dates.Changed[newest] {
			newest = file
		}
	}
	var naming []string
	for _, file := range files {
		// "./putnamiw lint,test,build" names "putnami test" and "putnami
		// build", the commands a Putnami workspace declares.
		text := ci.SpellCommands(strings.ToLower(string(c.Contents[file])))
		if mentionsAny(text, c.fragments(false)) && mentionsAny(text, c.fragments(true)) {
			naming = append(naming, file)
		}
	}
	value := lag(c.activity(area).LastCode, c.Dates.Changed[newest])
	command := "git log -1 --format=%cs -- " + newest
	if len(naming) == 0 {
		return marker(id, contract.MarkerExists, value, command, files)
	}
	// The file has named the commands since it was added, as far as the
	// collector can tell: it reads the file at HEAD only.
	return enforcedFor(marker(id, contract.MarkerEnforced, value, command, files), days(c.Now, c.oldestAdded(naming)))
}

// enforcedFor sets how many days the marker's treatment has been in place.
func enforcedFor(result contract.Marker, held *float64) contract.Marker {
	result.EnforcedDays = held
	return result
}

func mentionsAny(text string, fragments []string) bool {
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}

// docsChecks are the tools that check documentation in CI. Putnami's lint
// checks the links of every project's README and doc tree (lint-docs), and
// validate-workspace checks the documents outside any project.
var docsChecks = []string{
	"lychee", "markdown-link-check", "linkinator", "linkcheck", "markdownlint", "vale ",
	"mkdocs build", "docusaurus build", "sphinx-build", "putnami lint", "putnami validate-workspace",
}

func isReadme(base string) bool {
	lower := strings.ToLower(base)
	return lower == "readme.md" || lower == "readme" || lower == "readme.rst" || lower == "readme.txt"
}

func (c *computation) readme(area int) string {
	dir := c.dir(area)
	for _, file := range c.Files {
		if isReadme(path.Base(file.Path)) && strings.HasPrefix(file.Path, dir) && !strings.Contains(strings.TrimPrefix(file.Path, dir), "/") {
			return file.Path
		}
	}
	return ""
}

func (c *computation) areaDocs(area int) contract.Marker {
	const id = "understand.area-docs"
	readme := c.readme(area)
	if readme == "" {
		return marker(id, contract.MarkerAbsent, nil, "git ls-files -- "+c.dir(area)+"README.md", nil)
	}
	value := lag(c.activity(area).LastCode, c.Dates.Changed[readme])
	command := "git log -1 --format=%cs -- " + readme
	jobs, tasks := c.docsJobs()
	if len(jobs) == 0 {
		return marker(id, contract.MarkerExists, value, command, []string{readme})
	}
	return enforcedFor(marker(id, contract.MarkerEnforced, value, command, append(append([]string{readme}, jobPaths(jobs)...), tasks...)),
		days(c.Now, c.enforcedSince([]string{readme}, jobs)))
}

func (c *computation) docsJobs() ([]ci.Config, []string) {
	jobs := ci.Runs(c.configs, docsChecks...)
	tasks := c.tasks()
	// docs maps each docs task's name to the pattern that invokes it.
	docs := map[string]*regexp.Regexp{}
	var sources []string
	for changed := true; changed; {
		changed = false
		for _, task := range tasks {
			if docs[task.name] != nil {
				continue
			}
			runs := mentionsAny(task.command, docsChecks)
			for _, invoked := range docs {
				runs = runs || invoked.MatchString(task.command)
			}
			if runs {
				docs[task.name], changed = invokes(task.name), true
			}
		}
	}
	seen := map[string]bool{}
	for _, job := range jobs {
		seen[job.Path] = true
	}
	for _, invoked := range docs {
		for _, job := range ci.RunsPattern(c.configs, invoked) {
			if !seen[job.Path] {
				seen[job.Path] = true
				jobs = append(jobs, job)
			}
		}
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	for _, task := range tasks {
		if docs[task.name] != nil && !slices.Contains(sources, task.source) {
			sources = append(sources, task.source)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Path < jobs[j].Path })
	sort.Strings(sources)
	return jobs, sources
}

// task is a named command a build tool runs: a package.json script, an Nx
// target or a Makefile target, with its lowercase command text.
type task struct {
	name, command, source string
}

var makeRule = regexp.MustCompile(`^([A-Za-z0-9_.:-]+)\s*:([^=]|$)`)

// tasks reads the tasks the repository declares: the scripts of every
// package.json, the targets of every Nx project.json, and the root
// Makefile's targets.
func (c *computation) tasks() []task {
	var tasks []task
	for _, file := range c.Files {
		if scope.Skipped(file.Path) {
			continue
		}
		switch path.Base(file.Path) {
		case "package.json":
			var manifest struct {
				Scripts map[string]string `json:"scripts"`
			}
			if json.Unmarshal(c.Contents[file.Path], &manifest) == nil {
				for name, script := range manifest.Scripts {
					tasks = append(tasks, task{name: strings.ToLower(name), command: ci.SpellCommands(strings.ToLower(script)), source: file.Path})
				}
			}
		case "project.json":
			var project struct {
				Targets map[string]json.RawMessage `json:"targets"`
			}
			if json.Unmarshal(c.Contents[file.Path], &project) == nil {
				for name, target := range project.Targets {
					tasks = append(tasks, task{name: strings.ToLower(name), command: ci.SpellCommands(strings.ToLower(string(target))), source: file.Path})
				}
			}
		}
	}
	for _, file := range []string{"Makefile", "GNUmakefile", "makefile"} {
		current := -1
		for _, line := range strings.Split(string(c.Contents[file]), "\n") {
			switch {
			case strings.HasPrefix(line, "\t") && current >= 0:
				tasks[current].command += "\n" + ci.SpellCommands(strings.ToLower(line))
			case makeRule.MatchString(line):
				tasks = append(tasks, task{name: strings.ToLower(makeRule.FindStringSubmatch(line)[1]), source: file})
				current = len(tasks) - 1
			case strings.TrimSpace(line) != "":
				current = -1
			}
		}
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].source != tasks[j].source {
			return tasks[i].source < tasks[j].source
		}
		return tasks[i].name < tasks[j].name
	})
	return tasks
}

// invokes matches, in lowercase text, a command that runs the task name:
// "npm run name", "pnpm name", "pnpm --filter web name", "yarn workspace
// site name", "turbo run a,name", "nx affected -t name", "nx run
// project:name", "make name" and the like. The words must follow the tool's
// own name, so a step titled "Run lint" runs no lint task.
func invokes(name string) *regexp.Regexp {
	quoted := regexp.QuoteMeta(name)
	end := `(?:[^\w.:@/-]|$)`
	// pnpm and yarn select workspace packages before the task name.
	selection := `(?:(?:--filter[=\s]+|-f\s+|workspace\s+)[\w.@/*!{}-]+\s+|(?:-r|--recursive|workspaces\s+foreach(?:\s+-\S+)*)\s+)*`
	runners := `\b(?:(?:npm|pnpm|yarn|bun|turbo|nx)\s+` + selection + `run\s+|(?:pnpm|yarn|bun)\s+` + selection + `|(?:make|just|task)\s+(?:-\S+\s+)*)`
	targets := `\bnx\s[^\n]*?(?:\s-t\s+|--targets?[=\s]+)`
	npmShortcut := ""
	if name == "test" || name == "start" {
		npmShortcut = `|\bnpm\s+` + quoted + end
	}
	return regexp.MustCompile(`(?:` + runners + `|` + targets + `)(?:[\w.:@/-]+,)*` + quoted + end +
		`|\bnx\s+run\s+[\w@/.-]+:` + quoted + end + npmShortcut)
}
