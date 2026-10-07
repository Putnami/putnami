// Package inventory describes the product a repository builds: its
// languages, tools and agent instruction files. It reads file names and the
// manifests that declare dependencies; it never runs a tool.
package inventory

import (
	"encoding/json"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/internal/ci"
	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
	"go.putnami.dev/intelligence/agent-readiness/internal/scope"
)

const secondsPerDay = 24 * 60 * 60

// Input is what Detect reads.
type Input struct {
	// Files are every tracked file at HEAD, sorted by path.
	Files []gitrepo.File
	// Authored keeps the files whose lines count.
	Authored func(path string) bool
	// Contents holds the files Wanted named, read at HEAD.
	Contents map[string][]byte
	// Changed is when each instruction file last changed, in Unix seconds.
	Changed map[string]int64
	// Now is the collection time in Unix seconds.
	Now int64
}

// Detect fills every inventory field except the history counts.
func Detect(in Input) contract.Inventory {
	inventory := contract.Inventory{
		Languages:        []contract.LanguageShare{},
		InstructionFiles: []contract.InstructionFile{},
	}
	languages := map[string]*contract.LanguageShare{}
	found := newTools()
	var paths []string
	for _, file := range in.Files {
		paths = append(paths, file.Path)
		if in.Authored(file.Path) && file.Text {
			inventory.Files++
			inventory.Lines += file.Lines
			if name := Language(file.Path); name != "" {
				share := languages[name]
				if share == nil {
					share = &contract.LanguageShare{Name: name}
					languages[name] = share
				}
				share.Files++
				share.Lines += file.Lines
			}
		}
		if scope.InSkippedDir(file.Path) {
			continue
		}
		byName(found, file.Path)
		if IsInstructionFile(file.Path) && safeLocation(file.Path) {
			changed, ok := in.Changed[file.Path]
			age := 0
			if ok && in.Now > changed {
				age = int((in.Now - changed) / secondsPerDay)
			}
			inventory.InstructionFiles = append(inventory.InstructionFiles, contract.InstructionFile{Path: file.Path, Bytes: int(file.Size), AgeDays: age})
		}
	}
	for _, share := range languages {
		inventory.Languages = append(inventory.Languages, *share)
	}
	sort.Slice(inventory.Languages, func(i, j int) bool { return inventory.Languages[i].Name < inventory.Languages[j].Name })
	byDependencies(found, in.Contents, manifestOrder(paths))
	byContent(found, in.Contents)
	inventory.Frameworks = found.list("frameworks")
	inventory.PackageManagers = found.list("packageManagers")
	inventory.MonorepoTools = found.list("monorepoTools")
	inventory.CIProviders = found.list("ciProviders")
	inventory.IaC = found.list("iac")
	inventory.Containers = found.list("containers")
	inventory.Databases = found.list("databases")
	inventory.TestFrameworks = found.list("testFrameworks")
	inventory.AgentTools = found.list("agentTools")
	return inventory
}

// Wanted names the files Detect reads: dependency manifests, compose files
// and version pins.
func Wanted(files []gitrepo.File) []string {
	var wanted []string
	for _, file := range files {
		base := path.Base(file.Path)
		if scope.Skipped(file.Path) {
			continue
		}
		if dependencyManifests[base] || isCompose(base) || versionPins[base] != "" {
			wanted = append(wanted, file.Path)
		}
	}
	return wanted
}

// instructionNames are agent instruction files recognized at any depth.
var instructionNames = map[string]bool{"AGENTS.md": true, "CLAUDE.md": true, "GEMINI.md": true}

// IsInstructionFile reports whether a path is an agent instruction file.
func IsInstructionFile(file string) bool {
	switch {
	case instructionNames[path.Base(file)]:
		return true
	case file == ".github/copilot-instructions.md", file == ".cursorrules", file == ".windsurfrules", file == ".clinerules":
		return true
	case strings.HasPrefix(file, ".cursor/rules/"):
		return true
	}
	return false
}

var location = regexp.MustCompile(`^[A-Za-z0-9._@+-]+(/[A-Za-z0-9._@+-]+)*$`)

// safeLocation keeps the paths the contract accepts as a location.
func safeLocation(file string) bool {
	return len(file) <= 512 && location.MatchString(file)
}

var extensions = map[string]string{
	".bash": "Shell", ".c": "C", ".cc": "C++", ".cjs": "JavaScript", ".cpp": "C++", ".cs": "C#",
	".css": "CSS", ".cxx": "C++", ".dart": "Dart", ".ex": "Elixir", ".exs": "Elixir", ".go": "Go",
	".h": "C", ".hcl": "HCL", ".hpp": "C++", ".html": "HTML", ".java": "Java", ".js": "JavaScript",
	".jsx": "JavaScript", ".kt": "Kotlin", ".kts": "Kotlin", ".lua": "Lua", ".mjs": "JavaScript",
	".php": "PHP", ".proto": "Protocol Buffers", ".py": "Python", ".rb": "Ruby", ".rs": "Rust",
	".scala": "Scala", ".scss": "SCSS", ".sh": "Shell", ".sql": "SQL", ".svelte": "Svelte",
	".swift": "Swift", ".tf": "HCL", ".ts": "TypeScript", ".tsx": "TypeScript", ".vue": "Vue",
}

// Language names the programming language of a path, or "".
func Language(file string) string {
	return extensions[strings.ToLower(path.Ext(file))]
}

type tools map[string]map[string]string

func newTools() tools { return tools{} }

func (t tools) add(category, name, version string) {
	if t[category] == nil {
		t[category] = map[string]string{}
	}
	if current, ok := t[category][name]; !ok || current == "" {
		t[category][name] = cleanVersion(version)
	}
}

func (t tools) list(category string) []contract.Tool {
	list := make([]contract.Tool, 0, len(t[category]))
	for name, version := range t[category] {
		list = append(list, contract.Tool{Name: name, Version: version})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

var versionShape = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]{0,63}$`)

// cleanVersion keeps a pinned version and drops ranges, tags and local
// references.
func cleanVersion(version string) string {
	version = strings.TrimSpace(version)
	version = strings.TrimLeft(version, "^~>=<v ")
	if versionShape.MatchString(version) {
		return version
	}
	return ""
}

// byName recognizes the tools a file's name reveals.
func byName(found tools, file string) {
	base := path.Base(file)
	if provider := ci.Provider(file); provider != "" {
		found.add("ciProviders", provider, "")
	}
	if manager, ok := lockfileManagers[base]; ok {
		found.add("packageManagers", manager, "")
	}
	if manager, ok := manifestManagers[base]; ok {
		found.add("packageManagers", manager, "")
	}
	if tool, ok := monorepoFiles[file]; ok {
		found.add("monorepoTools", tool, "")
	}
	switch {
	case strings.HasSuffix(base, ".tf"):
		found.add("iac", "Terraform", "")
	case base == "Pulumi.yaml":
		found.add("iac", "Pulumi", "")
	case base == "Chart.yaml":
		found.add("iac", "Helm", "")
	case base == "kustomization.yaml":
		found.add("iac", "Kustomize", "")
	case base == "cdk.json":
		found.add("iac", "AWS CDK", "")
	case strings.HasSuffix(base, ".bicep"):
		found.add("iac", "Bicep", "")
	case base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") || strings.HasSuffix(base, ".Dockerfile") || base == "Containerfile":
		found.add("containers", "Docker", "")
	case isCompose(base):
		found.add("containers", "Docker Compose", "")
	case strings.HasSuffix(base, "_test.go"):
		found.add("testFrameworks", "go test", "")
	}
	for prefix, tool := range agentFiles {
		if file == prefix || strings.HasPrefix(file, prefix+"/") {
			found.add("agentTools", tool, "")
		}
	}
}

var lockfileManagers = map[string]string{
	"Pipfile.lock": "Pipenv", "bun.lock": "Bun", "bun.lockb": "Bun", "package-lock.json": "npm",
	"pnpm-lock.yaml": "pnpm", "poetry.lock": "Poetry", "uv.lock": "uv", "yarn.lock": "Yarn",
}

var manifestManagers = map[string]string{
	"Cargo.toml": "Cargo", "Gemfile": "Bundler", "build.gradle": "Gradle", "build.gradle.kts": "Gradle",
	"composer.json": "Composer", "go.mod": "Go modules", "pom.xml": "Maven", "requirements.txt": "pip",
}

var monorepoFiles = map[string]string{
	"MODULE.bazel": "Bazel", "WORKSPACE": "Bazel", "WORKSPACE.bazel": "Bazel", "go.work": "Go workspaces",
	"lerna.json": "Lerna", "nx.json": "Nx", "pants.toml": "Pants", "pnpm-workspace.yaml": "pnpm workspaces",
	"putnami.workspace.json": "Putnami", "rush.json": "Rush", "turbo.json": "Turborepo",
}

var agentFiles = map[string]string{
	".aider.conf.yml": "Aider", ".claude": "Claude Code", ".clinerules": "Cline", ".codex": "Codex",
	".cursor": "Cursor", ".cursorrules": "Cursor", ".gemini": "Gemini CLI",
	".github/copilot-instructions.md": "GitHub Copilot", ".github/instructions": "GitHub Copilot",
	".windsurf": "Windsurf", ".windsurfrules": "Windsurf", "CLAUDE.md": "Claude Code", "GEMINI.md": "Gemini CLI",
}

func isCompose(base string) bool {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if base == name {
			return true
		}
	}
	return false
}

// dependencyManifests declare dependencies whose names reveal tools.
var dependencyManifests = map[string]bool{
	"Cargo.toml": true, "Gemfile": true, "go.mod": true, "package.json": true,
	"pyproject.toml": true, "requirements.txt": true,
}

// versionPins pin the version of a tool in a file of their own.
var versionPins = map[string]string{".terraform-version": "Terraform"}

// dependency maps a dependency name to the tool it reveals.
type dependency struct{ category, name string }

var dependencies = map[string]dependency{
	// JavaScript and TypeScript.
	"next": {"frameworks", "Next.js"}, "react": {"frameworks", "React"}, "vue": {"frameworks", "Vue"},
	"@angular/core": {"frameworks", "Angular"}, "svelte": {"frameworks", "Svelte"},
	"@sveltejs/kit": {"frameworks", "SvelteKit"}, "nuxt": {"frameworks", "Nuxt"}, "astro": {"frameworks", "Astro"},
	"@remix-run/react": {"frameworks", "Remix"}, "express": {"frameworks", "Express"},
	"fastify": {"frameworks", "Fastify"}, "@nestjs/core": {"frameworks", "NestJS"}, "hono": {"frameworks", "Hono"},
	"jest": {"testFrameworks", "Jest"}, "vitest": {"testFrameworks", "Vitest"}, "mocha": {"testFrameworks", "Mocha"},
	"@playwright/test": {"testFrameworks", "Playwright"}, "cypress": {"testFrameworks", "Cypress"},
	"turbo": {"monorepoTools", "Turborepo"}, "nx": {"monorepoTools", "Nx"}, "lerna": {"monorepoTools", "Lerna"},
	"pg": {"databases", "PostgreSQL"}, "postgres": {"databases", "PostgreSQL"}, "mysql2": {"databases", "MySQL"},
	"mysql": {"databases", "MySQL"}, "redis": {"databases", "Redis"}, "ioredis": {"databases", "Redis"},
	"mongodb": {"databases", "MongoDB"}, "mongoose": {"databases", "MongoDB"},
	"better-sqlite3": {"databases", "SQLite"}, "sqlite3": {"databases", "SQLite"},
	// Go.
	"github.com/gin-gonic/gin": {"frameworks", "Gin"}, "github.com/labstack/echo/v4": {"frameworks", "Echo"},
	"github.com/go-chi/chi/v5": {"frameworks", "chi"}, "github.com/gofiber/fiber/v2": {"frameworks", "Fiber"},
	"google.golang.org/grpc": {"frameworks", "gRPC"}, "github.com/jackc/pgx/v5": {"databases", "PostgreSQL"},
	"github.com/lib/pq": {"databases", "PostgreSQL"}, "github.com/go-sql-driver/mysql": {"databases", "MySQL"},
	"github.com/redis/go-redis/v9": {"databases", "Redis"}, "go.mongodb.org/mongo-driver": {"databases", "MongoDB"},
	"github.com/mattn/go-sqlite3": {"databases", "SQLite"}, "modernc.org/sqlite": {"databases", "SQLite"},
	// Python.
	"django": {"frameworks", "Django"}, "flask": {"frameworks", "Flask"}, "fastapi": {"frameworks", "FastAPI"},
	"pytest": {"testFrameworks", "pytest"}, "psycopg": {"databases", "PostgreSQL"},
	"psycopg2": {"databases", "PostgreSQL"}, "asyncpg": {"databases", "PostgreSQL"},
	// Rust.
	"actix-web": {"frameworks", "Actix Web"}, "axum": {"frameworks", "Axum"}, "rocket": {"frameworks", "Rocket"},
	// Ruby.
	"rails": {"frameworks", "Rails"}, "rspec": {"testFrameworks", "RSpec"},
}

// manifestOrder puts shallower manifests first so the root's pinned version
// wins over a member's.
func manifestOrder(paths []string) []string {
	var manifests []string
	for _, file := range paths {
		if dependencyManifests[path.Base(file)] && !scope.Skipped(file) {
			manifests = append(manifests, file)
		}
	}
	sort.SliceStable(manifests, func(i, j int) bool {
		return strings.Count(manifests[i], "/") < strings.Count(manifests[j], "/")
	})
	return manifests
}

func byDependencies(found tools, contents map[string][]byte, manifests []string) {
	for _, file := range manifests {
		data, ok := contents[file]
		if !ok {
			continue
		}
		for name, version := range declaredDependencies(path.Base(file), data) {
			if tool, known := dependencies[name]; known {
				found.add(tool.category, tool.name, version)
			}
		}
		if path.Base(file) == "package.json" {
			packageManagerPin(found, data)
		}
		if path.Base(file) == "go.mod" {
			if match := goDirective.FindSubmatch(data); match != nil {
				found.add("packageManagers", "Go modules", string(match[1]))
			}
		}
	}
}

func packageManagerPin(found tools, data []byte) {
	var manifest struct {
		PackageManager string `json:"packageManager"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return
	}
	name, version, ok := strings.Cut(manifest.PackageManager, "@")
	if !ok {
		return
	}
	version, _, _ = strings.Cut(version, "+")
	display := map[string]string{"pnpm": "pnpm", "npm": "npm", "yarn": "Yarn", "bun": "Bun"}[name]
	if display != "" {
		found.add("packageManagers", display, version)
	}
}

var (
	goDirective   = regexp.MustCompile(`(?m)^go\s+([0-9][0-9.]*)\s*$`)
	goRequire     = regexp.MustCompile(`(?m)^\s*(?:require\s+)?([A-Za-z0-9._~/-]+\.[A-Za-z0-9._~/-]+)\s+(v[0-9][^\s]*)`)
	cargoLine     = regexp.MustCompile(`(?m)^([A-Za-z0-9_-]+)\s*=\s*(?:"([^"]+)"|\{[^}\n]*version\s*=\s*"([^"]+)")`)
	gemLine       = regexp.MustCompile(`(?m)^\s*gem\s+['"]([^'"]+)['"](?:\s*,\s*['"]([^'"]+)['"])?`)
	pythonRequire = regexp.MustCompile(`^([A-Za-z0-9_.-]+)(?:\[[^\]]*\])?\s*(?:==\s*([0-9][^\s,;]*))?`)
	quotedString  = regexp.MustCompile(`"([^"]+)"`)
	composeImage  = regexp.MustCompile(`(?m)^\s*image:\s*["']?([a-z0-9./_-]+)(?::([A-Za-z0-9._-]+))?`)
)

// declaredDependencies returns dependency names and their declared
// versions.
func declaredDependencies(base string, data []byte) map[string]string {
	deps := map[string]string{}
	switch base {
	case "package.json":
		var manifest struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
			PeerDeps        map[string]string `json:"peerDependencies"`
		}
		if json.Unmarshal(data, &manifest) == nil {
			for _, group := range []map[string]string{manifest.PeerDeps, manifest.DevDependencies, manifest.Dependencies} {
				for name, version := range group {
					deps[name] = version
				}
			}
		}
	case "go.mod":
		for _, match := range goRequire.FindAllSubmatch(data, -1) {
			deps[string(match[1])] = string(match[2])
		}
	case "Cargo.toml":
		for _, match := range cargoLine.FindAllSubmatch(data, -1) {
			deps[string(match[1])] = string(match[2]) + string(match[3])
		}
	case "Gemfile":
		for _, match := range gemLine.FindAllSubmatch(data, -1) {
			deps[string(match[1])] = string(match[2])
		}
	case "requirements.txt":
		for _, line := range strings.Split(string(data), "\n") {
			addPythonRequirement(deps, line)
		}
	case "pyproject.toml":
		for _, match := range quotedString.FindAllSubmatch(data, -1) {
			addPythonRequirement(deps, string(match[1]))
		}
	}
	return deps
}

func addPythonRequirement(deps map[string]string, requirement string) {
	requirement = strings.TrimSpace(requirement)
	if requirement == "" || strings.HasPrefix(requirement, "#") || strings.HasPrefix(requirement, "-") {
		return
	}
	if match := pythonRequire.FindStringSubmatch(requirement); match != nil {
		deps[strings.ToLower(match[1])] = match[2]
	}
}

var composeDatabases = map[string]string{
	"mariadb": "MariaDB", "mongo": "MongoDB", "mysql": "MySQL", "postgres": "PostgreSQL", "redis": "Redis",
}

// byContent reads compose images and version pin files.
func byContent(found tools, contents map[string][]byte) {
	files := make([]string, 0, len(contents))
	for file := range contents {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		base := path.Base(file)
		if tool := versionPins[base]; tool != "" {
			found.add("iac", tool, strings.TrimSpace(string(contents[file])))
		}
		if !isCompose(base) {
			continue
		}
		for _, match := range composeImage.FindAllSubmatch(contents[file], -1) {
			image := path.Base(string(match[1]))
			if name, ok := composeDatabases[image]; ok {
				found.add("databases", name, string(match[2]))
			}
		}
	}
}
