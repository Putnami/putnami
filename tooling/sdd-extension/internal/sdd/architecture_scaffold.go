package sdd

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	archproto "go.putnami.dev/protocol/architecture"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	archsdk "go.putnami.dev/sdk/extension/architecture"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// `architecture init` scaffolds the first manifest of a domain, and nothing
// else.
//
// It is the ARC counterpart of `specs init` (ADR 0007): an exclusive create that
// states only what the caller already stated — the domain identity, its owner,
// and the projects the caller selected — and invents no export, no import, and
// no binding. Exports and imports are agreements between two domains, and a
// generator that guessed one would be writing a contract nobody negotiated.
//
// The document is authored through the extension SDK's builder, so the bytes a
// scaffold writes are the bytes a `Pin`-backed authoring would later produce for
// the same domain, and the protocol's verdict is applied before anything reaches
// disk.

// ArchitectureInitOptions carries what the caller decided: who owns the domain,
// where the manifest goes, and which projects it maps.
type ArchitectureInitOptions struct {
	// Owner is the team or role accountable for the domain. Empty defaults to
	// the domain ID, which is what every domain in this repository uses and what
	// a single-team domain almost always wants.
	Owner string
	// At is the workspace-relative directory the manifest is written to. Empty
	// derives it — see ArchitectureInitReport.Path.
	At string
	// Projects are the Putnami project IDs the domain maps, already resolved by
	// the orchestrator from the caller's selection. Nothing here re-resolves a
	// selector.
	Projects []string
	// DryRun prints the target path and the exact bytes without writing.
	DryRun bool
}

// ArchitectureInitReport describes exactly what `architecture init` did or would
// do. Contents is the canonical document either way, so --dry-run shows the same
// bytes the write path produces.
type ArchitectureInitReport struct {
	Domain   string   `json:"domain"`
	Owner    string   `json:"owner"`
	Path     string   `json:"path,omitempty"`
	Projects []string `json:"projects"`
	Created  bool     `json:"created"`
	DryRun   bool     `json:"dryRun"`
	Contents string   `json:"contents,omitempty"`
	// Existing names the manifest that already declares this domain, when one
	// does. A domain identity is workspace-unique, so the refusal points at the
	// file that owns it rather than at the one that was about to.
	Existing    string            `json:"existing,omitempty"`
	Diagnostics []diag.Diagnostic `json:"diagnostics"`
}

// BuildArchitectureInitResult scaffolds one domain manifest.
//
// The refusals are the point of the command, and each one names the human
// decision it will not make: an already-declared domain (identity is
// workspace-unique), an existing file at the target path (created exclusively,
// so the guarantee is a property of the write), and a domain ID the protocol
// does not accept.
func BuildArchitectureInitResult(ws *workspace.Workspace, domain string, options ArchitectureInitOptions) (ArchitectureInitReport, error) {
	report := ArchitectureInitReport{
		Domain:      strings.TrimSpace(domain),
		Owner:       strings.TrimSpace(options.Owner),
		Projects:    []string{},
		DryRun:      options.DryRun,
		Diagnostics: []diag.Diagnostic{},
	}
	if report.Owner == "" {
		report.Owner = report.Domain
	}
	if ws == nil {
		return report, protocolcli.Usagef("architecture init requires a resolved workspace")
	}
	if ws.HasWarningCode(workspace.WarningCodeProviderViewUnavailable) {
		return report, protocolcli.Usagef(
			"architecture init needs a complete project view to map a domain; this run received a partial one " +
				"(run `putnami projects sync` when a provider probe is the cause)")
	}

	report.Projects = canonicalProjectList(options.Projects)
	for _, project := range report.Projects {
		if ws.ProjectByID(project) == nil {
			report.Diagnostics = append(report.Diagnostics, diag.Errorf(archproto.ErrorCodeUnknownProject, "projects",
				"project %q is not present in the resolved Putnami workspace", project))
			return report, WithResultData(
				protocolcli.NotFoundf("project %q is not a member of this workspace", project), report)
		}
	}

	discovery := Discover(ws.Root)
	for _, source := range discovery.Sources {
		if source.Manifest != nil && source.Manifest.Domain == report.Domain {
			report.Existing = source.Path
			report.Diagnostics = append(report.Diagnostics, diag.Errorf(archproto.ErrorCodeDuplicateDomain, source.Path,
				"domain %q is already declared; edit that manifest instead", report.Domain))
			return report, WithResultData(
				protocolcli.Usagef("domain %q is already declared by %s", report.Domain, source.Path), report)
		}
	}

	builder := archsdk.NewDomain(report.Domain, report.Owner).Projects(report.Projects...)
	contents, err := builder.CanonicalBytes()
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, architectureAuthoringDiagnostics(err)...)
		return report, WithResultData(
			protocolcli.InvalidConfigf("the requested domain cannot produce a valid manifest"), report)
	}
	report.Contents = string(contents)

	root, err := architectureInitRoot(ws, options.At, report.Projects)
	if err != nil {
		return report, WithResultData(err, report)
	}
	report.Path = joinArchitecturePath(root, archproto.ManifestFilename)
	if options.DryRun {
		return report, nil
	}
	if err := createArchitectureManifest(ws.Root, report.Path, contents); err != nil {
		if os.IsExist(err) {
			report.Diagnostics = append(report.Diagnostics, diag.Errorf(archproto.ErrorCodeParseError, report.Path,
				"a file already exists at the target path; architecture init never overwrites user content"))
			return report, WithResultData(
				protocolcli.Usagef("%s already exists; architecture init never overwrites a file", report.Path), report)
		}
		return report, protocolcli.Classify(fmt.Errorf("write %s: %w", report.Path, err), protocolcli.ErrInvalidConfig)
	}
	report.Created = true
	return report, nil
}

// architectureInitRoot decides where a scaffold lands.
//
// An explicit --at wins and is checked for containment. Otherwise the manifest
// goes beside the FIRST project the domain maps, in canonical order — a rule
// that is deterministic and easy to correct, rather than a guess at a common
// parent directory that would be right for some domain shapes and wrong for
// others. A domain that maps no project yet lands at the workspace root.
func architectureInitRoot(ws *workspace.Workspace, at string, projects []string) (string, error) {
	if trimmed := strings.TrimSpace(at); trimmed != "" {
		cleaned := path.Clean(filepath.ToSlash(trimmed))
		if cleaned == "." {
			return "", nil
		}
		if code := validateArchitectureWritePath(cleaned); code != "" {
			return "", protocolcli.Usagef("--at %s is not a contained workspace-relative directory", at)
		}
		return cleaned, nil
	}
	if len(projects) == 0 {
		return "", nil
	}
	project := ws.ProjectByID(projects[0])
	if project == nil || project.Path == "" || project.Path == "." {
		return "", nil
	}
	return filepath.ToSlash(project.Path), nil
}

func joinArchitecturePath(root, name string) string {
	if root == "" {
		return name
	}
	return root + "/" + name
}

// canonicalProjectList sorts and de-duplicates the caller's project IDs so the
// scaffold is the same document whichever order a selector resolved in.
func canonicalProjectList(input []string) []string {
	seen := make(map[string]bool, len(input))
	output := make([]string, 0, len(input))
	for _, value := range input {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		output = append(output, trimmed)
	}
	sort.Strings(output)
	return output
}

// architectureAuthoringDiagnostics unwraps the SDK builder's verdict so a
// refusal names the manifest field the caller has to change, instead of one
// opaque line.
func architectureAuthoringDiagnostics(err error) []diag.Diagnostic {
	var violation *archsdk.ValidationError
	if errors.As(err, &violation) {
		return copyArchitectureDiagnostics(violation.Diagnostics)
	}
	return []diag.Diagnostic{diag.Errorf(archproto.ErrorCodeParseError, "", "%s", err.Error())}
}

// createArchitectureManifest creates the document exclusively: O_EXCL is what
// makes "never overwrite" a property of the write itself rather than of a check
// that raced.
func createArchitectureManifest(wsRoot, relative string, contents []byte) error {
	if code := validateArchitectureWritePath(relative); code != "" {
		return fmt.Errorf("%s", code)
	}
	root, err := os.OpenRoot(wsRoot)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // the write result is reported below

	target := filepath.FromSlash(relative)
	if parent := filepath.Dir(target); parent != "." {
		// os.Root refuses to traverse a symlink while creating parents, so a
		// pre-existing link cannot redirect this write outside the workspace.
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	file, err := root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		file.Close() //nolint:errcheck // the write error is the one to report
		return err
	}
	return file.Close()
}

// validateArchitectureWritePath refuses anything but a contained
// workspace-relative path. The same guard `specs init` applies, for the same
// reason: the one caller writes to disk.
func validateArchitectureWritePath(relative string) string {
	if relative == "" || strings.HasPrefix(relative, "/") || strings.ContainsAny(relative, "\\\x00") {
		return "architecture manifest path is not a contained workspace-relative path"
	}
	if path.Clean(relative) != relative {
		return "architecture manifest path is not canonical"
	}
	for _, segment := range strings.Split(relative, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "architecture manifest path is not a contained workspace-relative path"
		}
	}
	return ""
}
