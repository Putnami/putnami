package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// The Go workspace-sync task.
//
// One mutation moves here from CLI core, and it is the one core could only
// perform by knowing what a Go module is: the WORKSPACE-REPLACE CLOSURE. Every
// go.work member's go.mod must carry its own relative `replace` for every
// workspace module in its TRANSITIVE require graph, because `go mod tidy` runs
// in module mode where go.work replaces do not apply and the placeholder
// versions (v0.0.0/v0.0.1) exist only in this repository, never on a proxy. A
// missing replace rots in silently as modules gain cross-module dependencies
// and surfaces later as an offline-tidy 404.
//
// THIS TASK OWNS THE CLOSURE. Core's copy is narrowed to a BOOTSTRAP
// residue — the extension modules whose runtime core has to prepare before any
// extension can run at all — and left everything else here.
//
// The residue exists because this task runs on a PREPARED runtime, and in a
// workspace that builds its extensions from source, preparation compiles this
// module with `GOWORK=off` (bin/prepare, deliberately, so the artifact is a
// function of its declared inputs). Module mode is exactly the mode go.work
// replaces do not apply in, so a missing replace in THIS module's own go.mod
// makes the runtime unbuildable and `projects sync` unable to repair the one
// failure it exists for. Core therefore repairs the extension modules, and only
// those, before preparing them.
//
// Running both over one module is safe in a way it never is for a name field:
// the codemod is append-only, deterministic and convergent, and the two
// implementations are pinned byte-for-byte identical by the shared closure
// corpus. Removing the residue means removing the ordering — a `prepare` that
// resolved through the governing go.work would not need the closure at all —
// which is a change to an artifact-digest-relevant script and belongs to its own
// slice.
//
// What this task deliberately does NOT do is align the `module` line to the
// resolved project name. That is the one place the TypeScript pilot's shape
// does not transfer: a package.json `name` IS the project's source identity, so
// aligning it is correct, but a Go module path is a BUILD-ADDRESSABLE import
// prefix that every importing file spells out. `@putnami/go` is a project name;
// `go.putnami.dev/go/extension` is its module path, and writing the first over
// the second would break every import in the repository. Core never made that
// write either — its guard is `SourceName != Name`, and a Go project's
// SourceName is whatever named it (putnami.json, else the module path), so the
// two can never diverge for a module. Declaring `go.mod` as this adapter's
// marker hands core's writer over; the honest handover is to state, here, that
// the field has no aligner and why.
//
// Idempotence is a requirement, not a nicety: `projects sync` runs on every
// membership change and the tree must converge. A run that changes nothing
// writes nothing, so a no-op sync leaves file mtimes — and therefore every
// downstream cache key that observes them — untouched.

// workspaceSyncChange is one recorded mutation, in the form the caller reports.
type workspaceSyncChange struct {
	// Path is the repo-relative file that changed.
	Path string `json:"path"`
	// Field names what changed in it.
	Field string `json:"field"`
	// Added holds the module paths of appended replace directives, sorted.
	Added []string `json:"added,omitempty"`
}

// runWorkspaceSync is the `workspace-sync` job entry point.
func runWorkspaceSync(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	dryRun := ctx.Params.Bool("dryRun", false)

	// FAIL CLOSED on an empty selection. The task declares
	// `activation: workspace-once` precisely so the RESOLVED project selection
	// reaches it; if it did not, that is a wiring bug. Running anyway would
	// mutate go.mod files while reporting on a membership nobody supplied, and
	// the go.work coverage check below — the one that tells a new Go project it
	// is invisible to module-mode tidy — would silently pass by checking
	// nothing. Stopping loudly, having written nothing, is the only safe answer.
	if len(ctx.SelectedProjects) == 0 {
		return "FAILED", nil, errors.New(
			"workspace-sync received no resolved project selection; refusing to rewrite go.mod workspace " +
				"replaces from an unknown membership (the task must run with activation: workspace-once)")
	}

	changes, err := syncGoWorkspace(ctx.WorkspaceRoot, dryRun)
	if err != nil {
		return "FAILED", nil, err
	}
	for _, change := range changes {
		emit.Log("info", fmt.Sprintf("%s: +%d workspace replaces (%s)",
			change.Path, len(change.Added), strings.Join(change.Added, ", ")))
	}
	for _, orphan := range unmanagedGoProjects(ctx.WorkspaceRoot, ctx.SelectedProjects) {
		emit.Diagnostic("warning", fmt.Sprintf(
			"%s/go.mod is not a go.work member; module-mode `go mod tidy` cannot resolve its workspace "+
				"dependencies — run `putnami deps install` to re-sync go.work", orphan), orphan+"/go.mod", 0)
	}

	// Settle the require lines and go.sum of the modules that gained replaces so
	// the tree ends tidy-guard clean. Best effort by design: external
	// dependencies may need the network, and a sync that failed because a proxy
	// was unreachable would make `projects sync` — the repair command —
	// unusable offline.
	if !dryRun {
		for _, warning := range settleGoModules(ctx.WorkspaceRoot, changes) {
			emit.Log("warn", warning)
		}
	}

	encoded := make([]any, 0, len(changes))
	for _, change := range changes {
		encoded = append(encoded, change)
	}
	return "OK", map[string]any{"changes": encoded, "dryRun": dryRun}, nil
}

// syncGoWorkspace maintains the workspace-replace closure of every go.work
// member and returns what it changed, in go.work order.
//
// Missing replaces are appended as single-line directives after the existing
// content, sorted by module path for a stable diff; existing directives are
// never removed, reordered or reformatted, so manually maintained replaces
// survive. The computation is offline and deterministic — no go tooling is
// invoked. With dryRun set the changes are computed and reported but nothing is
// written. A workspace without go.work (or with no members) is a no-op.
func syncGoWorkspace(root string, dryRun bool) ([]workspaceSyncChange, error) {
	members, err := loadGoWorkMembers(root)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, nil
	}

	byModule := make(map[string]*goWorkMember, len(members))
	for _, member := range members {
		byModule[member.module] = member
	}

	var changes []workspaceSyncChange
	for _, member := range members {
		missing := missingWorkspaceReplaces(member, byModule)
		if len(missing) == 0 {
			continue
		}
		updated, err := appendWorkspaceReplaces(member, missing, byModule)
		if err != nil {
			return nil, err
		}
		if !dryRun {
			if err := os.WriteFile(filepath.Join(member.dir, "go.mod"), []byte(updated), 0o644); err != nil {
				return nil, fmt.Errorf("write %s/go.mod: %w", member.rel, err)
			}
		}
		changes = append(changes, workspaceSyncChange{
			Path: member.rel + "/go.mod", Field: "replace", Added: missing,
		})
	}
	return changes, nil
}

// goWorkMember is one `use` directive of go.work plus the parsed pieces of its
// go.mod that the replace-closure computation needs.
type goWorkMember struct {
	rel      string          // use path relative to root, slash-separated, cleaned
	dir      string          // absolute member directory
	module   string          // module path declared by go.mod
	content  string          // raw go.mod content
	requires []string        // required module paths (block + single-line, incl. indirect)
	replaced map[string]bool // LHS module paths of existing replace directives
}

// loadGoWorkMembers parses go.work's use directives and each member's go.mod.
// A missing go.work yields no members; a member whose go.mod is unreadable or
// declares no module is skipped — it cannot participate in the require graph,
// and a replace pointing at it would be broken anyway.
func loadGoWorkMembers(root string) ([]*goWorkMember, error) {
	rels, err := toolchain.ParseGoWorkUses(filepath.Join(root, "go.work"))
	if err != nil {
		return nil, err
	}
	members := make([]*goWorkMember, 0, len(rels))
	for _, rel := range rels {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		data, readErr := os.ReadFile(filepath.Join(dir, "go.mod"))
		if readErr != nil {
			continue
		}
		mod, parseErr := toolchain.ParseGoMod(string(data))
		if parseErr != nil || mod.Module == "" {
			continue
		}
		members = append(members, &goWorkMember{
			rel:      rel,
			dir:      dir,
			module:   mod.Module,
			content:  string(data),
			requires: mod.Requires,
			replaced: mod.ReplacedModules(),
		})
	}
	return members, nil
}

// missingWorkspaceReplaces returns the workspace module paths in m's TRANSITIVE
// require graph that have no replace directive in m's go.mod, sorted for a
// stable diff. Reachability runs over the whole workspace graph, so a
// not-yet-tidied module that just gained a direct edge to X still picks up X's
// transitive workspace dependencies from X's own (tidied) require block.
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
	if m == nil {
		return nil
	}
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
// rewritten. A subsequent `go mod tidy` settles the indirect require lines and
// go.sum; require directives are never emitted here.
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

// unmanagedGoProjects returns the selected project paths that carry a go.mod
// but are not go.work members, sorted.
//
// This is what the resolved selection is FOR: the closure walks go.work, so a
// Go project missing from go.work is invisible to it — its module-mode tidy
// will keep failing to resolve workspace dependencies and nothing in the
// closure report would say why. Reporting it is cheap and turns a confusing
// offline-tidy 404 into a one-line instruction.
func unmanagedGoProjects(root string, selected []pctx.ProjectRef) []string {
	rels, err := toolchain.ParseGoWorkUses(filepath.Join(root, "go.work"))
	if err != nil || len(rels) == 0 {
		// No go.work at all is an ordinary state (a workspace with no Go
		// modules, or one that has not run `deps install` yet); flagging every
		// Go project in it would be noise, not a finding.
		return nil
	}
	members := make(map[string]bool, len(rels))
	for _, rel := range rels {
		members[rel] = true
	}

	var orphans []string
	seen := make(map[string]bool, len(selected))
	for _, ref := range selected {
		rel := filepath.ToSlash(filepath.Clean(ref.Path))
		if rel == "." || rel == "" || rel == "/" || seen[rel] || members[rel] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel), goWorkspaceMarker)); err != nil {
			continue
		}
		seen[rel] = true
		orphans = append(orphans, rel)
	}
	sort.Strings(orphans)
	return orphans
}

// goModTidy runs `go mod tidy` in one module directory. It is a package
// variable so tests exercise the sync without spawning the toolchain; the
// deterministic replace edit above is the tested core, this pass only settles
// what tidy owns (indirect requires, go.sum).
var goModTidy = func(goBinary, dir string) ([]byte, error) {
	cmd := exec.Command(goBinary, "mod", "tidy")
	cmd.Dir = dir
	// An inherited GOWORK=off (leaked from a parent that built standalone)
	// disables workspace resolution, so each placeholder require escapes to the
	// proxy as a doomed 404. WorkspaceBuildEnv strips the leaked entry
	// and re-points GOWORK at the governing go.work.
	cmd.Env = toolchain.WorkspaceBuildEnv(os.Environ(), dir, goBinary)
	return cmd.CombinedOutput()
}

// settleGoModules tidies each module whose go.mod gained workspace replaces and
// returns the warnings a caller should surface. The pass is skipped entirely
// when the Go toolchain cannot be resolved.
func settleGoModules(root string, changes []workspaceSyncChange) []string {
	if len(changes) == 0 {
		return nil
	}
	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		return []string{"go toolchain unavailable; skipping go mod tidy for updated modules: " + err.Error()}
	}
	var warnings []string
	for _, change := range changes {
		dir := filepath.Dir(filepath.Join(root, filepath.FromSlash(change.Path)))
		if out, err := goModTidy(goBinary, dir); err != nil {
			warnings = append(warnings, fmt.Sprintf("go mod tidy failed in %s: %s",
				filepath.Dir(change.Path), strings.TrimSpace(string(out))))
		}
	}
	return warnings
}
