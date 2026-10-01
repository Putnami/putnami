package workspace

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// THE ONE LEGAL RESIDUE.
//
// Every other piece of Go knowledge left core in this slice. This one could not,
// and the reason is a genuine ordering dependency rather than an oversight.
//
// The Go extension owns the workspace-replace closure and maintains it in its
// own `workspace-sync` task (slice C4a). That task runs on the extension's
// PREPARED runtime, and in a workspace that builds its extensions from source —
// this repository — preparation compiles the extension's own module with
// `GOWORK=off` (go/extension/bin/prepare, deliberately, so the artifact is a
// function of its declared inputs rather than of ambient workspace state).
// Module mode is exactly the mode go.work replaces do not apply in, so a missing
// replace in the EXTENSION's own go.mod makes its runtime unbuildable: the
// placeholder v0.0.0 require escapes to the proxy as a doomed 404.
// Handing the write over unconditionally would make `projects sync` —
// the command that exists to repair this — unable to repair the one failure that
// stops it from running.
//
// So core keeps the repair, and keeps it as SMALL as the ordering requires:
//
//   - It writes only the go.mod files of the module directories a caller names,
//     and `projects sync` names exactly the local extension roots whose runtime
//     it must prepare. Every other module's closure belongs to the Go
//     extension's `workspace-sync` task, which runs on every sync.
//   - It is append-only, deterministic and convergent, so core's write and the
//     extension's identical implementation over one tree cannot oscillate; the
//     shared closure corpus pins the two byte-for-byte.
//   - It never invokes the Go toolchain. Settling require lines and go.sum with
//     `go mod tidy` moved to the extension in C4a and did not come back.
//
// Removing this residue means removing the ordering: a `prepare` that resolved
// through the governing go.work would not need the closure at all. That is a
// change to an artifact-digest-relevant script and belongs to its own slice with
// its own evidence, not to the one that deletes the parsers.

// GoModReplaceChange records the workspace replace directives appended to one
// go.work member's go.mod.
type GoModReplaceChange struct {
	// Dir is the member directory relative to the workspace root, as written
	// in go.work's use directive (slash-separated, cleaned).
	Dir string
	// Added holds the module paths of the appended replaces, sorted.
	Added []string
}

// RepairBootstrapGoModClosure completes the workspace-replace closure of the
// named module directories, and of nothing else.
//
// Reachability is computed over the whole go.work graph — so a not-yet-tidied
// module still picks up what its dependencies require — while the WRITE is
// confined to dirs. Missing replaces are appended as single-line directives
// after the existing content, sorted by module path for a stable diff; existing
// directives are never removed, reordered or reformatted, so manually maintained
// replaces survive. The computation is offline and deterministic. With dryRun
// set the changes are computed and reported but nothing is written.
//
// An empty dirs is a no-op, as is a workspace without go.work: a repository with
// no Go modules must not pay for this, and a directory that is not a go.work
// member has no closure to complete.
func RepairBootstrapGoModClosure(wsRoot string, dirs []string, dryRun bool) ([]GoModReplaceChange, error) {
	if len(dirs) == 0 {
		return nil, nil
	}
	members, err := loadGoWorkMembers(wsRoot)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, nil
	}

	wanted := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		if cleaned := cleanWorkspacePath(dir); cleaned != "" {
			wanted[cleaned] = true
		}
	}

	byModule := make(map[string]*goWorkMember, len(members))
	for _, m := range members {
		byModule[m.module] = m
	}

	var changes []GoModReplaceChange
	for _, m := range members {
		if !wanted[cleanWorkspacePath(m.rel)] {
			continue
		}
		missing := missingWorkspaceReplaces(m, byModule)
		if len(missing) == 0 {
			continue
		}
		updated, err := appendWorkspaceReplaces(m, missing, byModule)
		if err != nil {
			return nil, err
		}
		if !dryRun {
			if err := os.WriteFile(filepath.Join(m.dir, "go.mod"), []byte(updated), 0o644); err != nil {
				return nil, fmt.Errorf("write %s/go.mod: %w", m.rel, err)
			}
		}
		changes = append(changes, GoModReplaceChange{Dir: m.rel, Added: missing})
	}
	return changes, nil
}

// goWorkMember is one `use` directive of go.work plus the parsed pieces of
// its go.mod that the replace-closure computation needs.
type goWorkMember struct {
	rel      string          // use path relative to wsRoot, slash-separated, cleaned
	dir      string          // absolute member directory
	module   string          // module path declared by go.mod
	content  string          // raw go.mod content
	requires []string        // required module paths (block + single-line, incl. indirect)
	replaced map[string]bool // LHS module paths of existing replace directives
}

// loadGoWorkMembers parses go.work's use directives and each member's go.mod.
// A missing go.work yields no members (pure-TS workspace); a member whose
// go.mod is unreadable is skipped — it cannot participate in the require
// graph, and a replace pointing at it would be broken anyway.
func loadGoWorkMembers(wsRoot string) ([]*goWorkMember, error) {
	rels, err := parseGoWorkUses(filepath.Join(wsRoot, "go.work"))
	if err != nil {
		return nil, err
	}
	members := make([]*goWorkMember, 0, len(rels))
	for _, rel := range rels {
		dir := filepath.Join(wsRoot, filepath.FromSlash(rel))
		data, readErr := os.ReadFile(filepath.Join(dir, "go.mod"))
		if readErr != nil {
			continue
		}
		m := &goWorkMember{rel: rel, dir: dir, content: string(data)}
		m.module, m.requires, m.replaced = parseGoModDirectives(m.content)
		if m.module == "" {
			continue
		}
		members = append(members, m)
	}
	return members, nil
}

// parseGoWorkUses returns the use directives of the go.work file at goWorkPath,
// handling both the `use (...)` block form and the single-line `use ./x` form,
// with // comments stripped. Paths come back slash-separated and cleaned. A
// missing go.work returns no members.
func parseGoWorkUses(goWorkPath string) ([]string, error) {
	data, err := os.ReadFile(goWorkPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rels []string
	inUseBlock := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		var rel string
		switch {
		case line == "use (":
			inUseBlock = true
			continue
		case inUseBlock && line == ")":
			inUseBlock = false
			continue
		case inUseBlock && line != "":
			rel = line
		case strings.HasPrefix(line, "use "):
			rel = strings.TrimSpace(strings.TrimPrefix(line, "use "))
		default:
			continue
		}
		rels = append(rels, path.Clean(rel))
	}
	return rels, nil
}

// parseGoModDirectives extracts the module path, the required module paths
// (block and single-line forms, direct and // indirect), and the LHS module
// paths of the replace directives (block and single-line forms) from go.mod
// content. Comments are stripped; existing content is never interpreted
// beyond these directives.
func parseGoModDirectives(content string) (string, []string, map[string]bool) {
	var module string
	var requires []string
	replaced := make(map[string]bool)
	inRequire, inReplace := false, false
	for line := range strings.SplitSeq(content, "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case inRequire:
			if line == ")" {
				inRequire = false
				continue
			}
			if f := strings.Fields(line); len(f) > 0 {
				requires = append(requires, f[0])
			}
		case inReplace:
			if line == ")" {
				inReplace = false
				continue
			}
			if f := strings.Fields(line); len(f) > 0 {
				replaced[f[0]] = true
			}
		case strings.HasPrefix(line, "module "):
			module = strings.TrimSpace(strings.TrimPrefix(line, "module "))
		case line == "require (":
			inRequire = true
		case strings.HasPrefix(line, "require "):
			if f := strings.Fields(strings.TrimPrefix(line, "require ")); len(f) > 0 {
				requires = append(requires, f[0])
			}
		case line == "replace (":
			inReplace = true
		case strings.HasPrefix(line, "replace "):
			if f := strings.Fields(strings.TrimPrefix(line, "replace ")); len(f) > 0 {
				replaced[f[0]] = true
			}
		}
	}
	return module, requires, replaced
}

// missingWorkspaceReplaces returns the workspace module paths in m's
// transitive require graph that have no replace directive in m's go.mod,
// sorted for a stable diff. Reachability runs over the whole workspace graph,
// so a not-yet-tidied module that just gained a direct edge to X still picks
// up X's transitive workspace deps from X's own (tidied) require block.
func missingWorkspaceReplaces(m *goWorkMember, byModule map[string]*goWorkMember) []string {
	visited := map[string]bool{m.module: true}
	queue := workspaceRequires(m, byModule)
	var missing []string
	for len(queue) > 0 {
		mod := queue[0]
		queue = queue[1:]
		if visited[mod] {
			continue
		}
		visited[mod] = true
		if !m.replaced[mod] {
			missing = append(missing, mod)
		}
		queue = append(queue, workspaceRequires(byModule[mod], byModule)...)
	}
	sort.Strings(missing)
	return missing
}

// workspaceRequires filters m's require directives down to workspace members.
func workspaceRequires(m *goWorkMember, byModule map[string]*goWorkMember) []string {
	var out []string
	for _, req := range m.requires {
		if _, ok := byModule[req]; ok {
			out = append(out, req)
		}
	}
	return out
}

// appendWorkspaceReplaces appends single-line replace directives for the
// missing workspace modules after m's existing go.mod content. Existing bytes
// are left untouched — replaces are only ever added, never removed or
// rewritten. A subsequent `go mod tidy` settles the indirect require lines
// and go.sum; require directives are never emitted here.
func appendWorkspaceReplaces(m *goWorkMember, missing []string, byModule map[string]*goWorkMember) (string, error) {
	var b strings.Builder
	b.WriteString(m.content)
	if !strings.HasSuffix(m.content, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n")
	for _, mod := range missing {
		target, err := filepath.Rel(m.dir, byModule[mod].dir)
		if err != nil {
			return "", fmt.Errorf("relative path from %s to %s: %w", m.rel, byModule[mod].rel, err)
		}
		rel := filepath.ToSlash(target)
		// go.mod filesystem replaces must start with ./ or ../ — a sibling
		// module already yields ../x, a nested one needs the explicit ./.
		if !strings.HasPrefix(rel, ".") {
			rel = "./" + rel
		}
		b.WriteString("replace " + mod + " => " + rel + "\n")
	}
	return b.String(), nil
}
