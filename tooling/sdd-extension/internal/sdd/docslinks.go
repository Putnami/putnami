package sdd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/docslinks"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The documentation link gate of `validate-workspace`.
//
// Each language extension's `lint-docs` task checks the README.md files and
// doc/ trees inside its project, which is the feedback a contributor gets
// while working on it. This gate checks every document of the workspace with
// the same rule, once per run whatever the selection: the documents no
// project owns (the workspace README, scope directories, projects without a
// language extension), and a link from one project into another that a change
// to the other breaks, which no per-project task of that change would run
// for. A link broken inside a selected project is reported by both.
//
// It walks the workspace once. Inside a project it reads what that project's
// lint-docs reads, and it honors the project's opt-out: `docs-links: false`
// under `options.lint`, or under the key of one of the project's own
// extensions or its `<extension>:lint` key, in putnami.json, or those keys
// and `*` in putnami.workspace.json. The task
// is UNCACHEABLE for the reason lint-docs is: a link may name any file of the
// workspace.

// DocsLinksReport is the result of the documentation link gate.
type DocsLinksReport struct {
	// Documents is the number of documents checked.
	Documents int `json:"documents"`
	// Findings are the broken links, sorted by file, line and column.
	Findings []DocsLinkFinding `json:"findings"`
}

// DocsLinkFinding is one broken link, with a workspace-relative slash path.
type DocsLinkFinding struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Target  string `json:"target"`
	Message string `json:"message"`
}

// BuildDocsLinksResult checks every document of ws: each project's documents,
// unless the project turns the check off, then the documents outside every
// project. workspaceOptions are the workspace's option blocks, whose
// `docs-links` a project's own options override. It returns the broken links
// as a findings list and as the SDK's findings, which the task reports with
// line and column.
func BuildDocsLinksResult(ws *workspace.Workspace, workspaceOptions map[string]map[string]any) (DocsLinksReport, []docslinks.Finding, error) {
	report := DocsLinksReport{Findings: []DocsLinkFinding{}}
	files, err := workspaceDocuments(ws, workspaceOptions)
	if err != nil {
		return report, nil, protocolcli.Classify(fmt.Errorf("list the workspace documents: %w", err),
			protocolcli.ErrInvalidConfig)
	}
	report.Documents = len(files)
	findings, err := docslinks.Check(ws.Root, files)
	if err != nil {
		return report, nil, protocolcli.Classify(fmt.Errorf("check documentation links: %w", err),
			protocolcli.ErrInvalidConfig)
	}
	for _, finding := range findings {
		rel, relErr := filepath.Rel(ws.Root, finding.File)
		if relErr != nil {
			rel = finding.File
		}
		report.Findings = append(report.Findings, DocsLinkFinding{
			File:    filepath.ToSlash(rel),
			Line:    finding.Line,
			Column:  finding.Column,
			Target:  finding.Target,
			Message: finding.Message,
		})
	}
	if len(findings) > 0 {
		return report, findings, WithResultData(protocolcli.Classify(
			fmt.Errorf("docs-links: %d broken link(s) in the workspace documents", len(findings)),
			protocolcli.ErrInvalidConfig), report)
	}
	return report, findings, nil
}

// workspaceDocuments returns the documents the gate checks, sorted, from one
// walk of the workspace: those of every project that keeps the check on, as
// its lint-docs task sees them, and those outside every project. A root
// project owns the documents outside the others, so its opt-out covers them.
func workspaceDocuments(ws *workspace.Workspace, workspaceOptions map[string]map[string]any) ([]string, error) {
	root := filepath.Clean(ws.Root)
	names := extensionNames{root: root, cache: map[string]string{}}
	off := map[string]bool{}
	dirs := make([]string, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		if project == nil || project.Path == "" {
			continue
		}
		dir := filepath.Join(root, filepath.FromSlash(project.Path))
		dirs = append(dirs, dir)
		off[dir] = docsLinksOff(names.resolve(project.Extensions), workspaceOptions, projectOptions(dir))
	}
	grouped, err := docslinks.WorkspaceDocuments(root, dirs)
	if err != nil {
		return nil, err
	}
	var files []string
	for owner, documents := range grouped {
		if !off[owner] {
			files = append(files, documents...)
		}
	}
	sort.Strings(files)
	return files, nil
}

// projectOptions reads the option blocks of the putnami.json in dir. A file
// this step cannot read or parse sets nothing: the configuration gates report
// it, and the link check stays on.
func projectOptions(dir string) map[string]map[string]any {
	data, err := os.ReadFile(filepath.Join(dir, "putnami.json"))
	if err != nil {
		return nil
	}
	var document struct {
		Options map[string]map[string]any `json:"options"`
	}
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &document) != nil {
		return nil
	}
	return document.Options
}

// extensionNames resolves extension references to manifest names, reading
// each manifest once.
type extensionNames struct {
	root  string
	cache map[string]string
}

// resolve returns the manifest names of refs, which key option blocks. A
// path reference is named by the `name` of the putnami.extension.json it
// points at; a reference this step cannot resolve keeps itself as its name.
func (n extensionNames) resolve(refs []string) []string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		name, ok := n.cache[ref]
		if !ok {
			name = ref
			if strings.HasPrefix(ref, "/") {
				var manifest struct {
					Name string `json:"name"`
				}
				data, err := os.ReadFile(filepath.Join(n.root, filepath.FromSlash(ref), "putnami.extension.json"))
				if err == nil && json.Unmarshal(data, &manifest) == nil && manifest.Name != "" {
					name = manifest.Name
				}
			}
			n.cache[ref] = name
		}
		names = append(names, name)
	}
	return names
}

// docsLinksOff reports whether the lint `docs-links` option is off for a
// project, given the manifest names of its extensions. It reads the option
// blocks the CLI merges into a lint task's parameters, in the CLI's order:
// the workspace's `*`, `lint`, each extension name and `<extension>:lint`,
// then the project's own `lint` and the same extension blocks. Only the
// project's own extensions count: a block keyed by `@putnami/go` does not
// reach a TypeScript project. A block keyed by an extension's path is not
// read, as the CLI reads it for file and environment inputs only. Only a
// language extension defines the flag, and a project runs lint through one,
// so the blocks of all its extensions are read.
func docsLinksOff(names []string, workspaceOptions, projectOptions map[string]map[string]any) bool {
	on := true
	read := func(options map[string]map[string]any, keys []string) {
		for _, key := range keys {
			if value, ok := options[key][docslinks.Param].(bool); ok {
				on = value
			}
		}
	}
	read(workspaceOptions, optionLayers([]string{"*", "lint"}, names))
	read(projectOptions, optionLayers([]string{"lint"}, names))
	return !on
}

// optionLayers returns base, then each extension key, then each
// `<extension>:lint` key.
func optionLayers(base, extensions []string) []string {
	keys := append(append([]string(nil), base...), extensions...)
	for _, extension := range extensions {
		keys = append(keys, extension+":lint")
	}
	return keys
}
