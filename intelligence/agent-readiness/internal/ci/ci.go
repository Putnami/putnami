// Package ci finds a repository's CI configurations and answers what they run
// on pull requests. It reads configuration text only; it never runs a job.
package ci

import (
	"encoding/json"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/areas"
)

// Config is one CI configuration file.
type Config struct {
	Path     string
	Provider string
	// OnPullRequest is true when the configuration runs on pull requests.
	// Only GitHub Actions declares it per workflow; the other providers run
	// their pipeline on every proposed change by default.
	OnPullRequest bool
	// text is the lowercase configuration.
	text string
}

const githubActions = "GitHub Actions"

// Provider returns the CI provider a path configures, or "".
func Provider(file string) string {
	dir, base := path.Dir(file), path.Base(file)
	switch {
	case dir == ".github/workflows" && (strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")):
		return githubActions
	case file == ".gitlab-ci.yml":
		return "GitLab CI"
	case file == ".circleci/config.yml":
		return "CircleCI"
	case file == "azure-pipelines.yml":
		return "Azure Pipelines"
	case file == "Jenkinsfile":
		return "Jenkins"
	case file == "bitbucket-pipelines.yml":
		return "Bitbucket Pipelines"
	case dir == ".buildkite" && strings.HasPrefix(base, "pipeline"):
		return "Buildkite"
	case file == ".travis.yml":
		return "Travis CI"
	case file == "putnami.ci.json":
		return "Putnami CI"
	}
	return ""
}

// gitlabRoot is the file GitLab CI starts from; the files it includes hold
// the rest of the pipeline.
const gitlabRoot = ".gitlab-ci.yml"

// IncludeCandidate reports whether a path may be a GitLab CI file that
// .gitlab-ci.yml includes: YAML under .gitlab/ or ci/, or a file named after
// GitLab CI. Load keeps only the ones an include actually names.
func IncludeCandidate(file string) bool {
	base := path.Base(file)
	if !strings.HasSuffix(base, ".yml") && !strings.HasSuffix(base, ".yaml") || file == gitlabRoot {
		return false
	}
	return strings.HasPrefix(file, ".gitlab/") || strings.HasPrefix(file, "ci/") || strings.Contains(base, "gitlab-ci")
}

var (
	gitlabInclude = regexp.MustCompile(`^(\s*)include:\s*(.*)$`)
	gitlabLocal   = regexp.MustCompile(`^(?:-\s*)?local:\s*(.+)$`)
	gitlabItem    = regexp.MustCompile(`^-\s*([^:{}\s]+\.ya?ml)$`)
)

// gitlabIncludes returns the local files a GitLab CI file includes, as
// repository-relative patterns. Remote, project, template and component
// includes name nothing in this repository and are left out.
func gitlabIncludes(text string) []string {
	var patterns []string
	add := func(value string) {
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		value = strings.TrimPrefix(strings.TrimPrefix(value, "./"), "/")
		if value != "" && !strings.Contains(value, "://") && !strings.Contains(value, "$") {
			patterns = append(patterns, value)
		}
	}
	indent := -1
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		depth := len(line) - len(strings.TrimLeft(line, " \t"))
		if match := gitlabInclude.FindStringSubmatch(line); match != nil {
			indent = len(match[1])
			if rest := strings.TrimSpace(match[2]); rest != "" {
				indent = -1
				if strings.HasPrefix(rest, "[") {
					for _, item := range strings.Split(strings.Trim(rest, "[]"), ",") {
						add(item)
					}
				} else if local := gitlabLocal.FindStringSubmatch(strings.Trim(rest, "{} ")); local != nil {
					add(local[1])
				} else if !strings.Contains(rest, ":") {
					add(rest)
				}
			}
			continue
		}
		if indent < 0 {
			continue
		}
		if depth < indent || depth == indent && !strings.HasPrefix(trimmed, "-") {
			indent = -1
			continue
		}
		if local := gitlabLocal.FindStringSubmatch(trimmed); local != nil {
			add(local[1])
		} else if item := gitlabItem.FindStringSubmatch(strings.Trim(trimmed, `"'`)); item != nil {
			add(item[1])
		}
	}
	return patterns
}

// gitlabFiles follows the includes from .gitlab-ci.yml through every file
// they name, and returns the included files, sorted.
func gitlabFiles(files []string, contents map[string][]byte) []string {
	queue := []string{gitlabRoot}
	seen := map[string]bool{gitlabRoot: true}
	var included []string
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, pattern := range gitlabIncludes(string(contents[current])) {
			for _, file := range files {
				if seen[file] || !IncludeCandidate(file) || !areas.Glob(strings.Split(pattern, "/"), strings.Split(file, "/")) {
					continue
				}
				seen[file] = true
				included = append(included, file)
				queue = append(queue, file)
			}
		}
	}
	sort.Strings(included)
	return included
}

func putnamiText(data []byte) string {
	var config struct {
		Commands []string `json:"commands"`
		Flags    []string `json:"flags"`
		Runner   struct {
			Selection string `json:"selection"`
		} `json:"runner"`
	}
	if json.Unmarshal(data, &config) != nil {
		return ""
	}
	lines := []string{"putnami install --frozen"}
	for _, command := range config.Commands {
		line := "putnami " + command + " " + strings.Join(config.Flags, " ")
		if config.Runner.Selection != "" {
			line += " --" + config.Runner.Selection
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// Load keeps the CI configurations among files, reading each from contents.
func Load(files []string, contents map[string][]byte) []Config {
	var configs []Config
	for _, file := range files {
		provider := Provider(file)
		if provider == "" {
			continue
		}
		text := configText(contents[file])
		if provider == "Putnami CI" {
			text = configText([]byte(putnamiText(contents[file])))
		}
		onPR := true
		if provider == githubActions {
			onPR = strings.Contains(text, "pull_request") || strings.Contains(text, "merge_group")
		}
		configs = append(configs, Config{Path: file, Provider: provider, OnPullRequest: onPR, text: text})
		if file == gitlabRoot {
			for _, included := range gitlabFiles(files, contents) {
				configs = append(configs, Config{
					Path: included, Provider: provider, OnPullRequest: true, text: configText(contents[included]),
				})
			}
		}
	}
	configs = followLocalUses(files, contents, configs)
	sort.Slice(configs, func(i, j int) bool { return configs[i].Path < configs[j].Path })
	return configs
}

// ActionCandidate reports whether a path may be a local GitHub action, the
// action.yml that a workflow step calls with "uses: ./<dir>". Load keeps only
// the ones a workflow actually calls.
func ActionCandidate(file string) bool {
	base := path.Base(file)
	return base == "action.yml" || base == "action.yaml"
}

// localUse matches a "uses:" that names a path in this repository: a local
// action's directory or a reusable workflow file.
var localUse = regexp.MustCompile(`(?m)^[\s-]*uses:\s*["']?\./([^"'\s#@]+)`)

// localTarget returns the file a local "uses:" names: the reusable workflow
// itself, or the action.yml in the action's directory.
func localTarget(target string, present map[string]bool) string {
	target = path.Clean(target)
	if strings.HasPrefix(target, "..") {
		return ""
	}
	if present[target] && Provider(target) == githubActions {
		return target
	}
	for _, name := range []string{"action.yml", "action.yaml"} {
		if file := path.Join(target, name); present[file] {
			return file
		}
	}
	return ""
}

// followLocalUses adds the local actions that the GitHub workflows call, and
// the local actions those call in turn, as GitHub Actions configurations. A
// local action or a reusable workflow runs on pull requests when any of its
// callers does: its steps run in that caller's job.
func followLocalUses(files []string, contents map[string][]byte, configs []Config) []Config {
	present := make(map[string]bool, len(files))
	for _, file := range files {
		present[file] = true
	}
	index := map[string]int{}
	var queue []int
	for i, config := range configs {
		if config.Provider == githubActions {
			index[config.Path] = i
			queue = append(queue, i)
		}
	}
	for len(queue) > 0 {
		caller := queue[0]
		queue = queue[1:]
		for _, match := range localUse.FindAllStringSubmatch(string(contents[configs[caller].Path]), -1) {
			file := localTarget(match[1], present)
			if file == "" {
				continue
			}
			onPR := configs[caller].OnPullRequest
			called, known := index[file]
			switch {
			case !known:
				configs = append(configs, Config{Path: file, Provider: githubActions, OnPullRequest: onPR, text: configText(contents[file])})
				index[file] = len(configs) - 1
				queue = append(queue, len(configs)-1)
			case onPR && !configs[called].OnPullRequest:
				configs[called].OnPullRequest = true
				queue = append(queue, called)
			}
		}
	}
	return configs
}

// putnamiCall matches a Putnami invocation, through the ./putnamiw wrapper or
// not, with its comma-separated commands: "./putnamiw lint,test,build".
var putnamiCall = regexp.MustCompile(`(?:\./|\b)putnamiw?[ \t]+([a-z][a-z0-9-]*(?:,[a-z][a-z0-9-]*)*)`)

// SpellCommands rewrites each Putnami invocation in lowercase text as one
// "putnami <command>" per command it runs, so that "./putnamiw lint,test"
// matches the fragments "putnami lint" and "putnami test". It leaves other
// text as it is.
func SpellCommands(text string) string {
	return putnamiCall.ReplaceAllStringFunc(text, func(call string) string {
		commands := strings.Split(putnamiCall.FindStringSubmatch(call)[1], ",")
		for i, command := range commands {
			commands[i] = "putnami " + command
		}
		return strings.Join(commands, " ")
	})
}

func configText(data []byte) string {
	return SpellCommands(strings.ToLower(string(data)))
}

// Unless keeps the configurations that mention none of the given lowercase
// fragments.
func Unless(configs []Config, fragments ...string) []Config {
	var kept []Config
	for _, config := range configs {
		mentioned := false
		for _, fragment := range fragments {
			mentioned = mentioned || strings.Contains(config.text, fragment)
		}
		if !mentioned {
			kept = append(kept, config)
		}
	}
	return kept
}

// Runs returns the configurations that run on pull requests and mention
// any of the given lowercase fragments.
func Runs(configs []Config, fragments ...string) []Config {
	var matched []Config
	for _, config := range configs {
		if !config.OnPullRequest {
			continue
		}
		for _, fragment := range fragments {
			if strings.Contains(config.text, fragment) {
				matched = append(matched, config)
				break
			}
		}
	}
	return matched
}

// RunsPattern returns the configurations that run on pull requests and whose
// lowercase text matches pattern.
func RunsPattern(configs []Config, pattern *regexp.Regexp) []Config {
	var matched []Config
	for _, config := range configs {
		if config.OnPullRequest && pattern.MatchString(config.text) {
			matched = append(matched, config)
		}
	}
	return matched
}
