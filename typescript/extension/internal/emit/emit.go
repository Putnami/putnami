// Package emit writes the emit manifest of a TypeScript project: one line per
// file its dependency closure holds, keyed on what `bun test` executes of it.
//
// A dependent's tests run their dependencies' sources through Bun, which strips
// types and comments before it executes a module. The manifest therefore keys a
// TypeScript module on the JavaScript Bun's transpiler emits for it, under the
// compiler options of the tsconfig that governs it, and every other file on its
// bytes. A test-run that folds the manifest's digest instead of its
// dependencies' cache keys reruns when what it executes moves, and only then
// (ADR 0016).
//
// Every narrowing falls back to bytes. A module that does not transpile, or
// whose tsconfig chain cannot be read, keys on its bytes; a closure in which a
// Bun plugin or a preload script can transform modules before Bun runs them
// keys every module on its bytes, and so does a project whose tests another
// runner than Bun runs, whose transform is not Bun's.
package emit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ManifestVersion is the version of the manifest's shape and of the rule that
// fills it. Changing either changes it, so no consumer folds a digest written
// under another rule.
const ManifestVersion = 1

// Modes of a manifest.
const (
	// ModeEmit keys a TypeScript module on its emitted JavaScript.
	ModeEmit = "emit"
	// ModeBytes keys every file on its bytes.
	ModeBytes = "bytes"
)

// RunnerBun is the test runner whose transform the manifest reproduces. A
// project whose tests another runner runs keys every module on its bytes.
const RunnerBun = "bun"

// Kinds of an entry.
const (
	KindJS     = "js"
	KindBytes  = "bytes"
	KindAbsent = "absent"
)

// Manifest is the emit manifest of one project.
type Manifest struct {
	Version int    `json:"version"`
	Mode    string `json:"mode"`
	// Reason says why the manifest keys every file on its bytes.
	Reason string  `json:"reason,omitempty"`
	Files  []Entry `json:"files"`
}

// Entry is one file of the closure.
type Entry struct {
	// Project is the workspace-relative path of the closure member.
	Project string `json:"project"`
	// Path is the file's slash path relative to the member.
	Path string `json:"path"`
	Kind string `json:"kind"`
	// SHA256 is the digest of the emitted JavaScript for KindJS, of the bytes
	// for KindBytes, and empty for KindAbsent.
	SHA256 string `json:"sha256,omitempty"`
}

// Member is one project of the dependency closure.
type Member struct {
	// Path is the workspace-relative path.
	Path string
	// Root is the absolute directory.
	Root string
}

// File is one module to transpile.
type File struct {
	Path   string `json:"path"`
	Loader string `json:"loader"`
	Config string `json:"config"`
}

// TranspileRequest asks the transpiler for the digest of every file's emit.
type TranspileRequest struct {
	Configs map[string]map[string]any `json:"configs"`
	Files   []File                    `json:"files"`
}

// Transpiler answers the digest of the emitted JavaScript of each file of a
// request, by absolute path. A file it leaves out keys on its bytes.
type Transpiler func(TranspileRequest) (map[string]string, error)

// Options configure Build.
type Options struct {
	WorkspaceRoot string
	// Members is the project's dependency closure, the project included.
	Members []Member
	// ListFiles lists the files of a member Git does not ignore, as slash
	// paths relative to its root. Nil walks the disk (WalkFiles).
	ListFiles func(root string) ([]string, error)
	Transpile Transpiler
	// Runner is the runner the project's tests run under. Only RunnerBun
	// keys a module on its emitted JavaScript: Vitest and Jest transform the
	// sources themselves (ts-jest, Babel, Vite plugins) and may read what
	// Bun strips. Any other value, the empty one included, keys every module
	// on its bytes.
	Runner string
}

// pluginPattern finds a module that can register a Bun plugin: a plugin's
// onLoad hook may read what the transpiler strips.
var pluginPattern = regexp.MustCompile(`\bBun\s*\.\s*plugin\b|import\s*\{[^}]*\bplugin\b[^}]*\}\s*from\s*['"]bun['"]`)

// preloadPattern finds a bunfig.toml preload, which runs before every test
// module and may register a plugin.
var preloadPattern = regexp.MustCompile(`(?m)^\s*preload\s*=`)

// Build reads the closure and returns its manifest.
func Build(opts Options) (Manifest, error) {
	list := opts.ListFiles
	if list == nil {
		list = WalkFiles
	}
	manifest := Manifest{Version: ManifestVersion, Mode: ModeEmit}
	if opts.Runner != RunnerBun {
		manifest.Mode, manifest.Reason = ModeBytes, runnerReason(opts.Runner)
	} else if reason, err := preloadReason(opts.WorkspaceRoot, "bunfig.toml"); err != nil {
		return Manifest{}, err
	} else if reason != "" {
		manifest.Mode, manifest.Reason = ModeBytes, reason
	}
	configs := newConfigResolver(opts.WorkspaceRoot, opts.Members)
	request := TranspileRequest{Configs: map[string]map[string]any{}}
	type pending struct {
		entry int
		abs   string
	}
	var modules []pending
	members := append([]Member(nil), opts.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Path < members[j].Path })
	for _, member := range members {
		files, err := list(member.Root)
		if err != nil {
			return Manifest{}, fmt.Errorf("list the files of %s: %w", member.Path, err)
		}
		files = append(files, genFiles(member.Root)...)
		files = uniqueSorted(files)
		for _, rel := range files {
			if excluded(rel) || unownedGenPath(rel) {
				continue
			}
			abs := filepath.Join(member.Root, filepath.FromSlash(rel))
			data, err := os.ReadFile(abs)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					manifest.Files = append(manifest.Files, Entry{Project: member.Path, Path: rel, Kind: KindAbsent})
					continue
				}
				if isDir(abs) {
					continue
				}
				return Manifest{}, fmt.Errorf("read %s: %w", abs, err)
			}
			entry := Entry{Project: member.Path, Path: rel, Kind: KindBytes, SHA256: digest(data)}
			if manifest.Mode == ModeEmit {
				if path.Base(rel) == "bunfig.toml" && preloadPattern.Match(data) {
					manifest.Mode, manifest.Reason = ModeBytes, member.Path+"/"+rel+" declares a preload"
				}
				if loader := Loader(rel); loader != "" && pluginPattern.Match(data) {
					manifest.Mode, manifest.Reason = ModeBytes, member.Path+"/"+rel+" can register a Bun plugin"
				}
			}
			if Loader(rel) == "" && typeOnly(rel) {
				// A declaration file is never executed: Bun resolves a module
				// to its source, and a .d.ts holds no statement to run.
				continue
			}
			manifest.Files = append(manifest.Files, entry)
			if loader := Loader(rel); loader != "" {
				if id, ok := configs.governing(filepath.Dir(abs)); ok {
					request.Configs[id] = configs.options[id]
					request.Files = append(request.Files, File{Path: abs, Loader: loader, Config: id})
					modules = append(modules, pending{entry: len(manifest.Files) - 1, abs: abs})
				}
			}
		}
	}
	if manifest.Mode == ModeEmit && len(request.Files) > 0 {
		if opts.Transpile == nil {
			return Manifest{}, errors.New("emit: no transpiler")
		}
		emitted, err := opts.Transpile(request)
		if err != nil {
			return Manifest{}, err
		}
		for _, module := range modules {
			if sum, ok := emitted[module.abs]; ok && sum != "" {
				manifest.Files[module.entry].Kind = KindJS
				manifest.Files[module.entry].SHA256 = sum
			}
		}
	}
	return manifest, nil
}

// Loader is the Bun loader of a module Bun transpiles before it runs it, or ""
// for a file Bun runs as it is or does not run.
func Loader(rel string) string {
	if typeOnly(rel) {
		return ""
	}
	switch strings.ToLower(path.Ext(rel)) {
	case ".ts", ".mts", ".cts":
		return "ts"
	case ".tsx":
		return "tsx"
	}
	return ""
}

func typeOnly(rel string) bool {
	lower := strings.ToLower(rel)
	return strings.HasSuffix(lower, ".d.ts") || strings.HasSuffix(lower, ".d.mts") || strings.HasSuffix(lower, ".d.cts")
}

func excluded(rel string) bool {
	return rel == "node_modules" || strings.HasPrefix(rel, "node_modules/") || strings.Contains(rel, "/node_modules/")
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func uniqueSorted(files []string) []string {
	sort.Strings(files)
	out := files[:0]
	for i, f := range files {
		if f == "" || (i > 0 && f == files[i-1]) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// buildStamp is a member's project-relative build stamp.
const buildStamp = ".gen/version.json"

// CededGenPaths are the project-relative paths under .gen that the generate
// step cedes to other tasks, the excludes of the build-generate task's gen
// output: build-infra, package-deployment, the Cloud extension's image layers
// and site content, and config-extract write them, in no order this task waits
// for. A manifest holds none of them, nor anything beneath one.
var CededGenPaths = []string{
	".gen/config-schema.json",
	".gen/config-schema.jsonschema.json",
	".gen/deployment.json",
	".gen/infra/runtime.json",
	".gen/layers",
	".gen/requirements.json",
	".gen/site-content",
}

// unownedGenPath reports whether a member-relative slash path is the build
// stamp, or a ceded .gen path or lies beneath one: a file the member's
// generate step does not write, which a manifest leaves out whichever listing
// names it.
func unownedGenPath(rel string) bool {
	if rel == buildStamp {
		return true
	}
	for _, ceded := range CededGenPaths {
		if rel == ceded || strings.HasPrefix(rel, ceded+"/") {
			return true
		}
	}
	return false
}

// genFiles lists the generated .gen tree of a member, which Git ignores and
// the member's generate task writes before this task runs, minus its build
// stamp.
func genFiles(root string) []string {
	var out []string
	genRoot := filepath.Join(root, ".gen")
	_ = filepath.WalkDir(genRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err == nil && filepath.ToSlash(rel) == buildStamp {
			// The build stamp names the run's version and the commit, so every
			// run writes other bytes. The scheduler rewrites it at the current
			// identity after each restore, and no cache key reads it.
			return nil
		}
		if err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

// WalkFiles lists every file under root outside node_modules and .git, the
// files a key reads in a directory Git does not manage.
func WalkFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (d.Name() == "node_modules" || d.Name() == ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}

// runnerReason is the reason a manifest records when the project's tests run
// under runner, which is not Bun.
func runnerReason(runner string) string {
	if runner == "" {
		return "the test runner is not known to be Bun"
	}
	return "the tests run under " + runner + ", whose transform is not Bun's"
}

func preloadReason(dir, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if preloadPattern.Match(data) {
		return name + " declares a preload", nil
	}
	return "", nil
}

// Encode writes the manifest in its canonical bytes.
func Encode(m Manifest) ([]byte, error) {
	if m.Files == nil {
		m.Files = []Entry{}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
