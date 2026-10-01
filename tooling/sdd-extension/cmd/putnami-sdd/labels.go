package main

import (
	"strings"

	capabilityproto "go.putnami.dev/protocol/capabilities"
	featureproto "go.putnami.dev/protocol/features"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
)

// The label vocabulary every human renderer in this package shares.
//
// Ported verbatim from `tooling/cli/internal/commands/sdd/features.go` — the
// rendering half the engine move deliberately left behind. These are pure
// projections of a value onto one line of text: they read nothing, decide
// nothing, and are the reason a revision or a source selector reads the same in
// `features inspect` and in `specs list`.
//
// They live in the COMMAND layer rather than beside the engine because the
// engine's answer is a value, and how a value is spelled for a person is not
// part of that answer.

// featureRevisionLabel names the revision an answer was derived from: the
// worktree with its HEAD when git could resolve one, an exact commit for a
// historical evaluation, and the bare kind when neither is available.
func featureRevisionLabel(revision featureengine.Revision) string {
	switch revision.Kind {
	case featureengine.RevisionKindWorktree:
		if revision.Head != "" {
			return revision.Kind + " @ " + revision.Head
		}
	case featureengine.RevisionKindGit:
		if revision.Commit != "" {
			return revision.Commit
		}
	}
	return revision.Kind
}

// evidenceKindsLabel joins the evidence kinds a requirement accepts.
func evidenceKindsLabel(kinds []featureproto.EvidenceKind) string {
	values := make([]string, len(kinds))
	for index, kind := range kinds {
		values[index] = string(kind)
	}
	return strings.Join(values, ", ")
}

// sourceSelectorLabel renders the exact source an evidence record is bound to,
// including the package and version when the binding is a package root: a
// version-qualified selector and a versionless one are different bindings, and
// a label that dropped the version would show them as the same.
func sourceSelectorLabel(source featureproto.SourceSelector) string {
	parts := []string{string(source.Root)}
	if source.OwnerProject != "" {
		parts = append(parts, "owner="+source.OwnerProject)
	}
	if source.Package != "" {
		parts = append(parts, "package="+source.Package, "version="+source.Version)
	}
	parts = append(parts, source.Binding)
	if source.Environment != "" {
		parts = append(parts, "environment="+source.Environment)
	}
	return strings.Join(parts, " · ")
}

// locationLabel renders a capability location as "<root>:<path>[#symbol]".
func locationLabel(root capabilityproto.LocationRoot, locationPath, symbol string) string {
	label := string(root) + ":" + locationPath
	if symbol != "" {
		label += "#" + symbol
	}
	return label
}

// contributionIdentityLabel renders the semantic identity of one contribution —
// the tuple that makes two contributions the same fact.
func contributionIdentityLabel(identity capabilityproto.ContributionIdentity) string {
	parts := []string{
		"owner=" + identity.OwnerProject,
		"kind=" + string(identity.Kind),
	}
	if identity.Subkind != "" {
		parts = append(parts, "subkind="+identity.Subkind)
	}
	parts = append(parts, "key="+identity.Key)
	return strings.Join(parts, " · ")
}

// emptyLabel spells an absent value as "unavailable" rather than as nothing, so
// a reader can tell a missing fact from a blank column.
func emptyLabel(value string) string {
	if value == "" {
		return "unavailable"
	}
	return value
}

// specSourceLabel renders a spec's path with its owning project, when one owns
// it. A workspace-root spec has no owner and is named by its path alone.
func specSourceLabel(specPath, project string) string {
	if project == "" {
		return specPath
	}
	return specPath + " (" + project + ")"
}
