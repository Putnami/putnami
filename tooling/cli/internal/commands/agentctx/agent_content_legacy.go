package agentctx

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// Legacy `agentArtifacts` entries: the forms earlier releases installed — a
// registry artifact (`name` or `name:constraint`, pinned under the lock's
// `agentArtifacts`) and an in-tree project (`/path`, built from its source).
// This CLI installs neither. It reads them for three things only, and none of
// them writes an agent file: the refusal that names the migration, the merge
// `putnami init --force` performs, and `putnami migrate agent-content`, which
// moves them to the extension whose content supersedes them.

// legacyAgentArtifact is one legacy entry, read for its identity.
type legacyAgentArtifact struct {
	// Name is the artifact identity: the registry name, or the name the
	// in-tree project's putnami.json declares.
	Name string
	// Declared is the entry exactly as authored.
	Declared string
}

// parseLegacyAgentArtifact reads the identity of one legacy entry. An in-tree
// entry is resolved to its project inside the workspace and named by its
// putnami.json; a registry entry is named by what precedes its constraint.
func parseLegacyAgentArtifact(wsRoot, declared string) (legacyAgentArtifact, error) {
	declared = strings.TrimSpace(declared)
	if !strings.HasPrefix(declared, "/") {
		name, _, _ := strings.Cut(declared, ":")
		name = strings.TrimSpace(name)
		if name == "" {
			return legacyAgentArtifact{}, fmt.Errorf("agent artifact entry %q names no artifact", declared)
		}
		return legacyAgentArtifact{Name: name, Declared: declared}, nil
	}
	if strings.ContainsAny(declared, `\:`) {
		return legacyAgentArtifact{}, fmt.Errorf("agent artifact path %q must use / separators and no volume name", declared)
	}
	for _, segment := range strings.Split(declared, "/") {
		if segment == ".." {
			return legacyAgentArtifact{}, fmt.Errorf("agent artifact path %q leaves the workspace", declared)
		}
	}
	clean := path.Clean(declared)
	if clean == "/" {
		return legacyAgentArtifact{}, fmt.Errorf("agent artifact path %q names the workspace root, not a project", declared)
	}
	dir := filepath.Join(wsRoot, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	// A `..`-free path can still leave the workspace through a symlinked
	// directory, and the project it names is read.
	if err := requireInsideWorkspace(wsRoot, dir); err != nil {
		return legacyAgentArtifact{}, fmt.Errorf("agent artifact path %s %w", clean, err)
	}
	cfg := wsproto.LoadProjectConfig(dir)
	if cfg == nil {
		return legacyAgentArtifact{}, &missingLegacyProjectError{path: clean}
	}
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return legacyAgentArtifact{}, fmt.Errorf("agent artifact path %s declares no project name", clean)
	}
	return legacyAgentArtifact{Name: name, Declared: declared}, nil
}

// missingLegacyProjectError reports an in-tree entry whose project has no
// readable project config: the project was deleted, or never existed.
type missingLegacyProjectError struct {
	// path is the entry's cleaned workspace path.
	path string
}

func (e *missingLegacyProjectError) Error() string {
	return fmt.Sprintf(
		"agent artifact path %s has no readable project config, so the artifact it declared cannot be named; remove the entry, then rerun", e.path)
}

// nameMissingLegacyProject names the artifact an in-tree entry declared after
// its project is gone. An in-tree artifact's project is deleted once its
// content moves to an extension, so the entry is named by the one candidate
// whose unscoped name is the entry's last path segment: /tooling/agent-workflows
// names @putnami/agent-workflows. It returns "" when no candidate, or more
// than one, matches.
func nameMissingLegacyProject(missing *missingLegacyProjectError, candidates []string) string {
	segment := path.Base(missing.path)
	match := ""
	for _, candidate := range candidates {
		unscoped := candidate
		if strings.HasPrefix(candidate, "@") {
			_, unscoped, _ = strings.Cut(candidate, "/")
		}
		if unscoped != segment || candidate == match {
			continue
		}
		if match != "" {
			return ""
		}
		match = candidate
	}
	return match
}

// requireInsideWorkspace resolves both ends and refuses a directory that is not
// under the workspace root. Both ends follow directory links, a junction on
// Windows included, so a link under the workspace that names a directory
// outside it is refused.
func requireInsideWorkspace(wsRoot, dir string) error {
	root, err := dirlink.Resolve(wsRoot)
	if err != nil {
		return fmt.Errorf("cannot be checked against the workspace root (%w)", err)
	}
	target, err := dirlink.Resolve(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // the missing-project error that follows is the clearer one
		}
		return fmt.Errorf("cannot be resolved (%w)", err)
	}
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return fmt.Errorf("resolves outside the workspace")
	}
	return nil
}

// AgentArtifactDeclarationName names what one `agentArtifacts` entry declares:
// the extension of an `extension:<name>` opt-in, or the artifact of a legacy
// entry. It returns "" for an entry that names nothing readable. It installs
// nothing; `putnami init --force` uses it to merge declarations by what they
// name rather than by how they are spelled.
func AgentArtifactDeclarationName(wsRoot, declared string) string {
	declared = strings.TrimSpace(declared)
	if declared == "" {
		return ""
	}
	if ref, ok, err := parseExtensionAgentContentReference(declared); ok {
		if err != nil {
			return ""
		}
		return ref.Name
	}
	legacy, err := parseLegacyAgentArtifact(wsRoot, declared)
	if err != nil {
		return ""
	}
	return legacy.Name
}

// firstPartyAgentContentSuccessors names the extension whose agent content
// supersedes each agent artifact Putnami published as a separate artifact, so
// a workspace that has not declared that extension yet is told which one to
// declare.
var firstPartyAgentContentSuccessors = map[string]string{
	"@putnami/agent-workflows":      "@putnami/contributor",
	"@putnami/maintainer-workflows": "@putnami/contributor",
}

// legacyAgentArtifactDeclarationsError refuses legacy entries with nothing
// written, and names the migration that moves them. The extension it names is
// a declared one whose content supersedes them when there is one, else the
// first-party successor, else a placeholder.
func legacyAgentArtifactDeclarationsError(wsRoot string, cfg *wsproto.Config, legacy []string) error {
	quoted := make([]string, 0, len(legacy))
	names := make(map[string]bool, len(legacy))
	var missing []*missingLegacyProjectError
	for _, declared := range legacy {
		quoted = append(quoted, fmt.Sprintf("%q", declared))
		artifact, err := parseLegacyAgentArtifact(wsRoot, declared)
		var gone *missingLegacyProjectError
		switch {
		case err == nil:
			names[artifact.Name] = true
		case errors.As(err, &gone):
			missing = append(missing, gone)
		}
	}
	successor, found := supersedingAgentContent(wsRoot, cfg, names, missing)
	target := successor
	if target == "" {
		target = "<extension>"
	}
	var step string
	switch found {
	case successorInstalled:
		step = fmt.Sprintf("Move the declarations, pins and ownership records to extension %s, whose agent content supersedes them", successor)
	case successorDeclared:
		step = fmt.Sprintf("Run `putnami install` to install extension %s, then move the declarations, pins and ownership records to it", successor)
	case successorNamed:
		step = fmt.Sprintf("Declare extension %s in extensions, run `putnami install`, then move the declarations, pins and ownership records to it", successor)
	default:
		step = "Declare the extension whose agent content supersedes them, run `putnami install`, then move the declarations, pins and ownership records to it"
	}
	return protocolcli.WithNext(protocolcli.Classify(fmt.Errorf(
		"agentArtifacts declares %s in a form this CLI no longer installs: nothing was changed. "+
			"Agent content is installed only by an extension, through an extension:<name> entry. %s",
		strings.Join(quoted, ", "), step), protocolcli.ErrInvalidConfig),
		"putnami migrate agent-content "+target)
}

// How far a workspace is from the extension that supersedes its legacy
// entries.
const (
	successorUnknown   = iota // no extension is known to supersede them
	successorNamed            // a first-party successor, not declared here
	successorDeclared         // declared, but its content cannot be located yet
	successorInstalled        // declared and installed; its content supersedes them
)

// supersedingAgentContent finds the extension whose agent content supersedes
// one of names, or one of the artifacts whose in-tree project is missing, and
// how far the workspace is from it.
func supersedingAgentContent(wsRoot string, cfg *wsproto.Config, names map[string]bool, missing []*missingLegacyProjectError) (string, int) {
	declared := extension.DeclaredExtensionNames(wsRoot, cfg)
	for _, candidate := range declared {
		source, err := extension.LocateAgentContent(wsRoot, cfg, candidate)
		if err != nil {
			continue
		}
		for _, superseded := range source.Contribution.Supersedes {
			if names[superseded] {
				return source.Name, successorInstalled
			}
		}
		for _, gone := range missing {
			if nameMissingLegacyProject(gone, source.Contribution.Supersedes) != "" {
				return source.Name, successorInstalled
			}
		}
	}
	firstParty := make([]string, 0, len(firstPartyAgentContentSuccessors))
	for name := range firstPartyAgentContentSuccessors {
		firstParty = append(firstParty, name)
	}
	sort.Strings(firstParty)
	for _, gone := range missing {
		if name := nameMissingLegacyProject(gone, firstParty); name != "" {
			names[name] = true
		}
	}
	for _, name := range sortedNames(names) {
		successor := firstPartyAgentContentSuccessors[name]
		if successor == "" {
			continue
		}
		for _, candidate := range declared {
			if candidate == successor {
				return successor, successorDeclared
			}
		}
		return successor, successorNamed
	}
	return "", successorUnknown
}
