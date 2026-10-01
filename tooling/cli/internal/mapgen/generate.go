package mapgen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/workspace"
)

// Mode is what a build session does with the workspace map. The map
// is ephemeral CLI state, so there is nothing to verify and nothing that can
// drift: a session either refreshes it or leaves it alone.
type Mode string

const (
	// ModeOff disables the build attachment entirely.
	ModeOff Mode = "off"
	// ModeWrite refreshes the selected projects' fragments and writes the two
	// documents when they differ from what is on disk.
	ModeWrite Mode = "write"
)

// ModeEnv is the explicit override / escape hatch for the build attachment:
// off | write. CIEnv is the CI marker every other CI-sensitive default in this
// CLI reads (internal/cli/cache_trust.go, internal/output/live.go).
const (
	ModeEnv = "PUTNAMI_CONTEXT_MAP"
	CIEnv   = "CI"
)

// ResolveMode decides what a build session does with the workspace map, in
// precedence order:
//
//  1. PUTNAMI_CONTEXT_MAP=off|write — the explicit override. An unrecognized
//     value is IGNORED rather than fatal: a typo in an environment variable must
//     not brick every build in a shell.
//  2. CI non-empty ⇒ OFF. Nothing is committed, so there is nothing for a runner
//     to keep honest, and a runner has no map consumer — the map's readers are
//     the human and the agent at the keyboard. Spending a digest sweep per CI
//     build to render a file nobody opens is pure cost.
//  3. otherwise ⇒ WRITE. Locally, `putnami build` just leaves the ephemeral map
//     correct, with no verb to remember.
//
// getenv is injected so the policy is testable without mutating the process
// environment.
func ResolveMode(getenv func(string) string) Mode {
	switch Mode(strings.ToLower(strings.TrimSpace(getenv(ModeEnv)))) {
	case ModeOff:
		return ModeOff
	case ModeWrite:
		return ModeWrite
	}
	if strings.TrimSpace(getenv(CIEnv)) != "" {
		return ModeOff
	}
	return ModeWrite
}

// Outcome values reported by Generate.
const (
	// OutcomeClean means the on-disk documents already matched a fresh render.
	OutcomeClean = "clean"
	// OutcomeUpdated means at least one document was rewritten.
	OutcomeUpdated = "updated"
)

// Report is the machine-readable result of one generation run. It is shared by
// `putnami context map` and by the build attachment, so both surfaces describe
// the same run the same way.
type Report struct {
	// Outcome is OutcomeClean or OutcomeUpdated.
	Outcome string `json:"outcome"`
	// Projects is the number of live projects the map renders.
	Projects int `json:"projects"`
	// FragmentsReused counts live projects whose persisted fragment still
	// matched its inputs digest; FragmentsBuilt counts the ones recomputed.
	FragmentsReused int `json:"fragmentsReused"`
	FragmentsBuilt  int `json:"fragmentsBuilt"`
	// FragmentsWritten lists the workspace-relative fragment paths persisted for
	// the selected projects, in path order.
	FragmentsWritten []string `json:"fragmentsWritten,omitempty"`
	// Outputs lists the workspace-relative documents actually rewritten. Empty
	// when the render matched what was already there.
	Outputs []string `json:"outputs,omitempty"`
}

// Resolution is one in-memory map build: the complete document plus how much of
// it came from validated fragments. It is what the MCP `workspace_map` tool and
// `context map --print` serve — neither may depend on, or disturb, the files on
// disk.
type Resolution struct {
	Map             *RepoMap
	FragmentsReused int
	FragmentsBuilt  int
}

// Generate is the whole feature in one function: refresh the SELECTED projects'
// fragments, reduce over the LIVE project set, and write the two documents.
//
// The two stages are what makes drift unrepresentable rather than
// detected-after-the-fact. Stage 1 recomputes and persists a fragment for every
// selected project. Stage 2 enumerates the live set and, for each project not
// just refreshed, reuses the persisted fragment ONLY when its recorded inputs
// digest still matches the inputs on disk — otherwise it rebuilds in memory. The
// miss path is therefore O(changed) for an --impacted run while the rendered map
// is always complete.
func Generate(ws *workspace.Workspace, selected []*workspace.Project) (Report, error) {
	var report Report

	fragments := make(map[string]*Fragment, len(ws.Projects))
	refreshed, err := refreshSelectedFragments(ws, selected, fragments, &report)
	if err != nil {
		return Report{}, err
	}
	if err := resolveRemainingFragments(ws, refreshed, fragments, &report); err != nil {
		return Report{}, err
	}

	repoMap, err := Reduce(ws, fragments)
	if err != nil {
		return Report{}, err
	}
	report.Projects = len(repoMap.Projects)

	jsonBytes, err := RenderJSON(repoMap)
	if err != nil {
		return Report{}, err
	}
	mdBytes := RenderMarkdown(repoMap)

	written, err := writeDocuments(ws.Root, jsonBytes, mdBytes)
	if err != nil {
		return Report{}, err
	}
	report.Outputs = written
	report.Outcome = OutcomeClean
	if len(written) > 0 {
		report.Outcome = OutcomeUpdated
	}
	return report, nil
}

// ResolveMap renders the complete map in memory, writing NOTHING — not a
// fragment, not a document. Every live project's fragment is validated against
// its inputs on disk and reused, or rebuilt in memory when the digest moved, so
// a warm call is O(changed) and a cold one O(projects) while the render is
// always complete and always current.
//
// It is deliberately independent of the on-disk documents: the MCP tool answers
// from the working tree, not from whatever the last build happened to leave
// behind, and `--print` owes a caller the same guarantee.
func ResolveMap(ws *workspace.Workspace) (Resolution, error) {
	fragments := make(map[string]*Fragment, len(ws.Projects))
	var report Report
	if err := resolveRemainingFragments(ws, nil, fragments, &report); err != nil {
		return Resolution{}, err
	}
	repoMap, err := Reduce(ws, fragments)
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{
		Map:             repoMap,
		FragmentsReused: report.FragmentsReused,
		FragmentsBuilt:  report.FragmentsBuilt,
	}, nil
}

// refreshSelectedFragments rebuilds a fragment for every selected project — they
// are the projects the run just touched, so their persisted fragment is assumed
// stale rather than validated — and persists it. It returns the set of project
// ids it handled.
func refreshSelectedFragments(
	ws *workspace.Workspace,
	selected []*workspace.Project,
	fragments map[string]*Fragment,
	report *Report,
) (map[string]bool, error) {
	refreshed := make(map[string]bool, len(selected))
	ordered := make([]*workspace.Project, 0, len(selected))
	for _, p := range selected {
		// A selection can name a project the live set does not contain (a stale
		// id, a filter over a foreign workspace); rendering it would put a
		// non-project in the map.
		if p == nil || refreshed[p.ID] || ws.ProjectByID(p.ID) == nil {
			continue
		}
		refreshed[p.ID] = true
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	for _, p := range ordered {
		fragment, err := BuildFragment(ws.Root, p)
		if err != nil {
			return nil, err
		}
		fragments[p.ID] = fragment
		report.FragmentsBuilt++
		rel := FragmentPath(p.Path)
		data, err := CanonicalFragment(fragment)
		if err != nil {
			return nil, err
		}
		if err := atomicWriteFile(filepath.Join(ws.Root, filepath.FromSlash(rel)), data); err != nil {
			return nil, err
		}
		report.FragmentsWritten = append(report.FragmentsWritten, rel)
	}
	sort.Strings(report.FragmentsWritten)
	return refreshed, nil
}

// resolveRemainingFragments fills in every live project the selection did not
// cover, reusing a persisted fragment when its digest still matches and
// rebuilding it in memory otherwise. It never writes.
func resolveRemainingFragments(
	ws *workspace.Workspace,
	refreshed map[string]bool,
	fragments map[string]*Fragment,
	report *Report,
) error {
	for _, p := range LiveProjects(ws) {
		if refreshed[p.ID] {
			continue
		}
		fragment, reused, err := ResolveFragment(ws.Root, p)
		if err != nil {
			return err
		}
		fragments[p.ID] = fragment
		if reused {
			report.FragmentsReused++
		} else {
			report.FragmentsBuilt++
		}
	}
	return nil
}

// writeDocuments atomically writes the two documents, skipping a file whose
// bytes already match so an unchanged map does not churn mtimes — a repeat build
// on an unchanged tree costs one digest sweep and zero writes. It returns the
// workspace-relative paths it actually wrote.
func writeDocuments(wsRoot string, jsonBytes, mdBytes []byte) ([]string, error) {
	var written []string
	for _, doc := range []struct {
		rel  string
		data []byte
	}{{JSONPath, jsonBytes}, {MarkdownPath, mdBytes}} {
		abs := filepath.Join(wsRoot, filepath.FromSlash(doc.rel))
		if current, err := os.ReadFile(abs); err == nil && bytes.Equal(current, doc.data) {
			continue
		}
		if err := atomicWriteFile(abs, doc.data); err != nil {
			return nil, err
		}
		written = append(written, doc.rel)
	}
	return written, nil
}

// atomicWriteFile writes data to path via a same-directory temp file and
// rename(2), so a reader never observes a partially written artifact and a
// failed write never truncates the existing good file. A fail-fast cancel
// mid-generation must not strand a half-written document under a concurrent
// reader — an MCP session byte-compares these files to report freshness, and a
// torn read would report a correct map as stale.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create map directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".repo-map-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp map file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the temp has been renamed into place
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp map file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp map file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp map file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("promote map file to %s: %w", path, err)
	}
	return nil
}
