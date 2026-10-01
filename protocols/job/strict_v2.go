package job

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// contextPresence is strict-parser state for optional lifecycle members whose
// Go zero values erase absent vs empty/null. Programmatically built Context
// values remain supported by combining these bits with their non-zero values.
type contextPresence struct {
	workspaceOptions         bool
	extensionRuntimePath     bool
	extensionCacheRoot       bool
	projectMetadata          bool
	projectType              bool
	projectDependencyClosure bool
	invocation               bool
	selection                bool
	workspaceProjects        bool
	userScope                bool
}

// v2 validation rules. Validate (strict.go) checks every document against the
// v1 requirements — those are unchanged and still apply at v2 — and then calls
// validateVersioned for the version-scoped ones below.

// Job context error codes for the version-scoped failures.
const (
	// ErrorCodeInvalidVersion marks a protocolVersion this package cannot read.
	ErrorCodeInvalidVersion = "job.invalid_version"
	// ErrorCodeUnexpectedField marks a member the document's version forbids.
	ErrorCodeUnexpectedField = "job.unexpected_field"
	// ErrorCodeInvalidValue marks a member with the right type and a disallowed
	// value.
	ErrorCodeInvalidValue = "job.invalid_value"
	// ErrorCodeInvalidKey marks an identity key that disagrees with the
	// structured identity it is derived from.
	ErrorCodeInvalidKey = "job.invalid_key"
	// ErrorCodeInvalidMetadata marks a namespaced project-metadata block whose
	// key or shape the contract does not allow.
	ErrorCodeInvalidMetadata = "job.invalid_metadata"
)

// validateVersioned applies the rules that depend on the document's version.
//
// The shape of these rules is what keeps v2 additive: a v1 document (no
// protocolVersion member) is checked exactly as before and cannot carry a v2
// member, while a v2 document must carry the identity that makes it v2. There
// is no in-between state where a reader has to guess which contract it holds.
func validateVersioned(ctx *Context) []diag.Diagnostic {
	var diags []diag.Diagnostic

	switch ctx.ContextVersion() {
	case ProtocolVersion:
		// A v1 document that carries v2 members is ambiguous: a v1 consumer
		// would ignore them and a v2 consumer would trust them, so the two
		// would disagree about the same bytes.
		if ctx.presence.workspaceOptions || ctx.Workspace.Options != nil {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "workspace.options",
				"workspace.options requires protocolVersion %d", ProtocolVersion2))
		}
		if ctx.Identity != nil {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "identity",
				"identity requires protocolVersion %d", ProtocolVersion2))
		}
		if ctx.Staging != nil {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "staging",
				"staging requires protocolVersion %d", ProtocolVersion2))
		}
		if ctx.presence.extensionRuntimePath || ctx.Extension.RuntimePath != "" {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "extension.runtimePath",
				"extension.runtimePath requires protocolVersion %d", ProtocolVersion2))
		}
		if ctx.presence.extensionCacheRoot || ctx.Extension.CacheRoot != "" {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "extension.cacheRoot",
				"extension.cacheRoot requires protocolVersion %d", ProtocolVersion2))
		}
		if ctx.presence.projectMetadata || ctx.Project.Metadata != nil {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "project.metadata",
				"project.metadata requires protocolVersion %d", ProtocolVersion2))
		}
		if ctx.presence.projectType || ctx.Project.Type != "" {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "project.type",
				"project.type requires protocolVersion %d", ProtocolVersion2))
		}
		if ctx.presence.projectDependencyClosure || ctx.Project.DependencyClosure != nil {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "project.dependencyClosure",
				"project.dependencyClosure requires protocolVersion %d", ProtocolVersion2))
		}
		if ctx.presence.invocation || ctx.Invocation != nil {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "invocation",
				"invocation requires protocolVersion %d", ProtocolVersion2))
		}
		if ctx.presence.selection || ctx.Selection != nil {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "selection",
				"selection requires protocolVersion %d", ProtocolVersion2))
		}
		if ctx.presence.workspaceProjects || ctx.WorkspaceProjects != nil {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "workspaceProjects",
				"workspaceProjects requires protocolVersion %d", ProtocolVersion2))
		}
		if ctx.presence.userScope || ctx.UserScope != nil {
			diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField, "userScope",
				"userScope requires protocolVersion %d", ProtocolVersion2))
		}
		// The v2-only members of ProjectRef are v2-only for the same reason as
		// every member above, and the cost of getting it wrong is concrete: a
		// v1 consumer ignores `sourceName` and renames the manifest anyway,
		// while a v2 consumer reads it and declines — the two disagree about the
		// same bytes, and the disagreement is a workspace-wide rename that
		// orphans every sibling reference to the old name. `version` splits the
		// same way: a v1 consumer resolves a package reference by name alone
		// where a v2 consumer resolves it by name AND version, so the two answer
		// a package-scoped selector differently. Only
		// project.dependencyClosure carries the other ProjectRef, and that
		// member is already v2-only, so this loop is the whole surface.
		// `dependencies` splits the same way once more, and with the widest
		// consequence of the three: a v1 consumer sees no edges and reports no
		// undeclared dependency, while a v2 consumer sees them and fails the run.
		// `config` can carry a reviewed featureAuthority answer; ignoring it makes
		// a v1 consumer report a completeness gap a v2 consumer suppresses.
		for i, project := range ctx.SelectedProjects {
			for _, member := range []struct {
				name  string
				value string
			}{
				{"sourceName", project.SourceName},
				{"version", project.Version},
			} {
				if member.value == "" {
					continue
				}
				diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField,
					fmt.Sprintf("selectedProjects[%d].%s", i, member.name),
					"selectedProjects[].%s requires protocolVersion %d", member.name, ProtocolVersion2))
			}
			if project.Dependencies != nil {
				diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField,
					fmt.Sprintf("selectedProjects[%d].dependencies", i),
					"selectedProjects[].dependencies requires protocolVersion %d", ProtocolVersion2))
			}
			if project.Config != nil {
				diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField,
					fmt.Sprintf("selectedProjects[%d].config", i),
					"selectedProjects[].config requires protocolVersion %d", ProtocolVersion2))
			}
			if project.Extensions != nil {
				diags = append(diags, diag.Errorf(ErrorCodeUnexpectedField,
					fmt.Sprintf("selectedProjects[%d].extensions", i),
					"selectedProjects[].extensions requires protocolVersion %d", ProtocolVersion2))
			}
		}
	case ProtocolVersion2:
		diags = append(diags, validateWorkspaceOptions(
			ctx.Workspace.Options, ctx.presence.workspaceOptions)...)
		diags = append(diags, validateIdentity(ctx.Identity)...)
		if ctx.Staging != nil {
			diags = append(diags, validateStaging(ctx.Staging)...)
		}
		diags = append(diags, validateExtensionPaths(
			&ctx.Extension,
			ctx.presence.extensionRuntimePath,
			ctx.presence.extensionCacheRoot,
		)...)
		diags = append(diags, validateProjectMetadata(ctx.Project.Metadata, ctx.presence.projectMetadata)...)
		diags = append(diags, validateDependencyClosure(
			ctx.Project.DependencyClosure, ctx.presence.projectDependencyClosure)...)
		diags = append(diags, validateInvocation(ctx.Invocation, ctx.presence.invocation)...)
		diags = append(diags, validateUserScope(ctx.UserScope, ctx.presence.userScope)...)
		diags = append(diags, validateSelection(ctx.Selection, ctx.presence.selection)...)
		for i, project := range ctx.SelectedProjects {
			diags = append(diags, validateProjectRefConfig(
				fmt.Sprintf("selectedProjects[%d]", i), project.Config)...)
		}
		diags = append(diags, validateWorkspaceProjects(
			ctx.WorkspaceProjects, ctx.presence.workspaceProjects)...)
	default:
		diags = append(diags, diag.Errorf(ErrorCodeInvalidVersion, "protocolVersion",
			"unknown job context protocol version %d; this package reads %d and %d",
			ctx.ProtocolVersion, ProtocolVersion, ProtocolVersion2))
	}

	return diags
}

// validateWorkspaceOptions keeps extension-owned values raw while pinning the
// envelope every consumer relies on. A present options member is a non-null
// object whose named blocks are non-null objects. Names are sorted before
// diagnostics so invalid producer bytes have a deterministic report.
func validateWorkspaceOptions(options map[string]json.RawMessage, present bool) []diag.Diagnostic {
	present = present || options != nil
	if !present {
		return nil
	}
	if options == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, "workspace.options",
			"workspace.options must be omitted or contain a non-null object keyed by extension name")}
	}
	names := make([]string, 0, len(options))
	for name := range options {
		names = append(names, name)
	}
	sort.Strings(names)
	var diags []diag.Diagnostic
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "workspace.options",
				"workspace option keys are extension names and cannot be empty"))
			continue
		}
		var block map[string]json.RawMessage
		if err := json.Unmarshal(options[name], &block); err != nil || block == nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "workspace.options."+name,
				"workspace option block %q must be a non-null JSON object", name))
		}
	}
	return diags
}

// validateIdentity checks the typed task identity a v2 document must carry.
func validateIdentity(identity *TaskIdentity) []diag.Diagnostic {
	if identity == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingField, "identity",
			"identity is required at protocolVersion %d", ProtocolVersion2)}
	}

	var diags []diag.Diagnostic
	require := func(field, value string) {
		if value == "" {
			diags = append(diags, diag.Errorf(
				ErrorCodeMissingField, field, "%s is required", field,
			))
		}
	}

	require("identity.project.id", identity.Project.ID)
	require("identity.project.name", identity.Project.Name)
	require("identity.task.name", identity.Task.Name)
	require("identity.task.command", identity.Task.Command)
	require("identity.task.kind", identity.Task.Kind)
	require("identity.provider.extension", identity.Provider.Extension)

	switch identity.Scope {
	case "":
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, "identity.scope",
			"identity.scope is required"))
	case TaskScopeProject, TaskScopeWorkspace:
	default:
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "identity.scope",
			"invalid task scope %q; must be %q or %q",
			identity.Scope, TaskScopeProject, TaskScopeWorkspace))
	}

	// The key is a DERIVED view of the structured identity (protocols/cli).
	// A key that disagrees with its own fields is a contract violation, not an
	// alternative spelling: two consumers would join on two different values.
	switch {
	case identity.Key == "":
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, "identity.key",
			"identity.key is required"))
	case identity.Key != identity.DerivedKey():
		diags = append(diags, diag.Errorf(ErrorCodeInvalidKey, "identity.key",
			"identity.key %q disagrees with the structured identity (%q)",
			identity.Key, identity.DerivedKey()))
	}

	return diags
}

// validateExtensionPaths checks the two v2 extension-owned directories.
//
// Both are OPTIONAL within v2 — an extension that declares no runtime section
// has no runtime path, and a producer that has not wired the cache root yet emits
// neither — but a member that IS present must be a usable absolute path. A
// relative one would resolve against whatever working directory the subprocess
// happened to inherit, which is the class of bug an absolute contract exists to
// remove.
func validateExtensionPaths(extension *Extension, runtimePathPresent, cacheRootPresent bool) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for _, member := range []struct {
		field   string
		value   string
		present bool
	}{
		{"extension.runtimePath", extension.RuntimePath, runtimePathPresent || extension.RuntimePath != ""},
		{"extension.cacheRoot", extension.CacheRoot, cacheRootPresent || extension.CacheRoot != ""},
	} {
		if !member.present {
			continue
		}
		if strings.TrimSpace(member.value) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, member.field,
				"%s must be omitted or contain a non-empty absolute path", member.field))
			continue
		}
		if !isAbsolutePath(member.value) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, member.field,
				"%s must be an absolute path, got %q", member.field, member.value))
		}
	}
	return diags
}

// validateProjectMetadata checks the namespaced provider metadata map.
//
// The namespace is the whole point: a key names the ONE extension that owns the
// block, so an empty or blank key would make the block ownerless and readable
// by anybody. Each block must be a JSON object, because a namespace whose value
// is a scalar cannot carry a provider's fields and would force every consumer
// to type-switch before it can decode its own data.
//
// Diagnostics come out in sorted key order, so two runs over one document are
// byte-identical.
func validateProjectMetadata(metadata map[string]json.RawMessage, present bool) []diag.Diagnostic {
	present = present || metadata != nil
	if !present {
		return nil
	}
	if metadata == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidMetadata, "project.metadata",
			"project.metadata must be omitted or contain a non-null object keyed by extension name")}
	}
	names := make([]string, 0, len(metadata))
	for name := range metadata {
		names = append(names, name)
	}
	sort.Strings(names)

	var diags []diag.Diagnostic
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidMetadata, "project.metadata",
				"project metadata keys are extension names; an empty key leaves the block unowned"))
			continue
		}
		var block map[string]json.RawMessage
		if err := json.Unmarshal(metadata[name], &block); err != nil || block == nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidMetadata, "project.metadata."+name,
				"project metadata for %q must be a JSON object", name))
		}
	}
	return diags
}

// validateDependencyClosure checks the resolved in-workspace dependency
// closure.
//
// Every entry must locate a project the same way a selected project does —
// name, workspace-relative path, absolute path — because a consumer resolves a
// closure member's files by joining them, and a member missing one of the three
// is a reference to nowhere. A PRESENT-but-null member is rejected for the same
// reason as an empty one elsewhere: the producer either resolved the closure or
// it did not, and null is neither answer.
//
// Diagnostics are emitted in closure order, which the producer states as
// canonical project-id order, so two runs over one document agree byte for byte.
func validateDependencyClosure(closure []ProjectRef, present bool) []diag.Diagnostic {
	present = present || closure != nil
	if !present {
		return nil
	}
	if closure == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, "project.dependencyClosure",
			"project.dependencyClosure must be omitted or contain a non-null array of project references")}
	}
	var diags []diag.Diagnostic
	for i, project := range closure {
		prefix := fmt.Sprintf("project.dependencyClosure[%d]", i)
		for _, member := range []struct {
			field string
			value string
		}{
			{prefix + ".name", project.Name},
			{prefix + ".path", project.Path},
			{prefix + ".fullPath", project.FullPath},
		} {
			if member.value == "" {
				diags = append(diags, diag.Errorf(ErrorCodeMissingField, member.field,
					"%s is required", member.field))
			}
		}
		diags = append(diags, validateProjectRefConfig(prefix, project.Config)...)
	}
	return diags
}

// validateWorkspaceProjects checks the complete resolved workspace membership.
//
// Every entry must locate a project the way every other project reference in
// this contract does — name, workspace-relative path, absolute path — because a
// consumer opens a member's files by joining them.
//
// Two rules are stricter than the dependency closure's, and both come from what
// the member is FOR. An id is required: this listing exists so a consumer can
// decide whether an id named somewhere else (an architecture manifest, a design
// graph) is a real member, and an entry with no id cannot answer that question
// for itself. And the listing must be in sorted, duplicate-free id order,
// because a membership answer must be the same bytes for the same workspace no
// matter which run produced it — the identical rule selection.projects carries.
//
// A PRESENT-but-null member is rejected for the same reason as everywhere else:
// the producer either resolved the workspace or it did not, and null is neither
// answer. An empty array IS an answer — a workspace with no members.
func validateWorkspaceProjects(projects []ProjectRef, present bool) []diag.Diagnostic {
	present = present || projects != nil
	if !present {
		return nil
	}
	if projects == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, "workspaceProjects",
			"workspaceProjects must be omitted or contain a non-null array of project references")}
	}
	var diags []diag.Diagnostic
	for i, project := range projects {
		prefix := fmt.Sprintf("workspaceProjects[%d]", i)
		for _, member := range []struct {
			field string
			value string
		}{
			{prefix + ".id", project.ID},
			{prefix + ".name", project.Name},
			{prefix + ".path", project.Path},
			{prefix + ".fullPath", project.FullPath},
		} {
			if strings.TrimSpace(member.value) == "" {
				diags = append(diags, diag.Errorf(ErrorCodeMissingField, member.field,
					"%s is required", member.field))
			}
		}
		diags = append(diags, validateProjectRefConfig(prefix, project.Config)...)
	}
	for i := 1; i < len(projects); i++ {
		if projects[i-1].ID >= projects[i].ID {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "workspaceProjects",
				"workspaceProjects must be sorted by id and free of duplicates; %q precedes %q",
				projects[i-1].ID, projects[i].ID))
			break
		}
	}
	return diags
}

// validateProjectRefConfig keeps the raw workspace-protocol document raw while
// still enforcing the one wire invariant this contract owns: when present it
// is a non-null JSON object. The workspace protocol remains responsible for the
// object's member shapes and semantics.
func validateProjectRefConfig(prefix string, raw json.RawMessage) []diag.Diagnostic {
	if raw == nil {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, prefix+".config",
			"%s.config must be omitted or contain a non-null project-config object", prefix)}
	}
	return nil
}

// validateInvocation checks the restricted, non-secret path-delivery channel
// used by an invocation-scoped output lifecycle.
func validateInvocation(invocation *Invocation, present bool) []diag.Diagnostic {
	present = present || invocation != nil
	if !present {
		return nil
	}
	if invocation == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, "invocation",
			"invocation must be omitted or contain a non-null object")}
	}
	var diags []diag.Diagnostic
	if strings.TrimSpace(invocation.ID) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, "invocation.id",
			"invocation.id is required"))
	}
	switch {
	case strings.TrimSpace(invocation.ArtifactRoot) == "":
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, "invocation.artifactRoot",
			"invocation.artifactRoot is required"))
	case !isAbsolutePath(invocation.ArtifactRoot):
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "invocation.artifactRoot",
			"invocation.artifactRoot must be an absolute path, got %q", invocation.ArtifactRoot))
	}
	return diags
}

// validateUserScope checks the member that marks a job run outside any
// workspace. A present member is a non-null object whose callerDir is an
// absolute path: a consumer starts from it, and a relative path would resolve
// against a directory the consumer cannot name.
func validateUserScope(scope *UserScope, present bool) []diag.Diagnostic {
	present = present || scope != nil
	if !present {
		return nil
	}
	if scope == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, "userScope",
			"userScope must be omitted or contain a non-null object")}
	}
	switch {
	case strings.TrimSpace(scope.CallerDir) == "":
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingField, "userScope.callerDir",
			"userScope.callerDir is required")}
	case !isAbsolutePath(scope.CallerDir):
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, "userScope.callerDir",
			"userScope.callerDir must be an absolute path, got %q", scope.CallerDir)}
	}
	return nil
}

// validateSelection checks the resolved project selection.
//
// The rules are the ones a consumer ACTS on, not decoration. A task reads
// `scoped` to decide whether its verdict may be reported for the whole
// workspace, and `emptyImpact` to tell "nothing changed" from "nothing
// matched"; a producer whose members disagree with each other would license a
// wrong conclusion in exactly the case the member exists to prevent. Sortedness
// is enforced for the reason every collection in this contract is sorted: the
// same flags over the same tree must produce the same bytes.
//
// Baseline and BaselineSource carry no rule beyond being meaningful only at
// SelectionModeImpacted. They are evidence about a resolution, not a claim
// about scope, and a producer that records the ref it measured against on
// another mode misleads nobody.
func validateSelection(selection *Selection, present bool) []diag.Diagnostic {
	present = present || selection != nil
	if !present {
		return nil
	}
	if selection == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, "selection",
			"selection must be omitted or contain a non-null object")}
	}

	var diags []diag.Diagnostic
	knownMode := false
	switch {
	case strings.TrimSpace(selection.Mode) == "":
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, "selection.mode",
			"selection.mode is required"))
	default:
		for _, mode := range SelectionModes {
			if selection.Mode == mode {
				knownMode = true
				break
			}
		}
		if !knownMode {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "selection.mode",
				"invalid selection mode %q; must be one of %s",
				selection.Mode, strings.Join(SelectionModes, ", ")))
		}
	}

	// `scoped` is derivable from `mode` and must agree with it. Both travel
	// because the CLI's own resolved type carries both, and a consumer reads
	// whichever it needs; a document where they disagree tells two consumers two
	// different things about one run.
	if knownMode && selection.Scoped == (selection.Mode == SelectionModeAll) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "selection.scoped",
			"selection.scoped is %t at mode %q; a narrowed run is scoped and %q is not",
			selection.Scoped, selection.Mode, SelectionModeAll))
	}

	if selection.ProjectIDs == nil {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "selection.projects",
			"selection.projects must be a non-null array of project ids; "+
				"an empty selection is an empty array"))
	}
	for i, id := range selection.ProjectIDs {
		if strings.TrimSpace(id) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingField,
				fmt.Sprintf("selection.projects[%d]", i), "selection.projects[%d] is required", i))
		}
	}
	for i := 1; i < len(selection.ProjectIDs); i++ {
		if selection.ProjectIDs[i-1] >= selection.ProjectIDs[i] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "selection.projects",
				"selection.projects must be sorted and free of duplicates; %q precedes %q",
				selection.ProjectIDs[i-1], selection.ProjectIDs[i]))
			break
		}
	}

	// The release-set half is reported beside the whole selection, never
	// instead of it: a consumer subtracts one from the other to learn what the
	// session verified, and an id outside `projects` would make that
	// subtraction describe a project the run never planned.
	selected := make(map[string]struct{}, len(selection.ProjectIDs))
	for _, id := range selection.ProjectIDs {
		selected[id] = struct{}{}
	}
	for i, id := range selection.ReleaseSetProjects {
		if i > 0 && selection.ReleaseSetProjects[i-1] >= id {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "selection.releaseSetProjects",
				"selection.releaseSetProjects must be sorted and free of duplicates; %q precedes %q",
				selection.ReleaseSetProjects[i-1], id))
			break
		}
	}
	for _, id := range selection.ReleaseSetProjects {
		if _, planned := selected[id]; !planned {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "selection.releaseSetProjects",
				"selection.releaseSetProjects names %q, which selection.projects does not select; "+
					"the release-set half is always a subset of what the run plans", id))
			break
		}
	}

	if selection.EmptyImpact {
		if selection.Mode != SelectionModeImpacted {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "selection.emptyImpact",
				"selection.emptyImpact is the %q no-op and cannot be reported at mode %q",
				SelectionModeImpacted, selection.Mode))
		}
		if len(selection.ProjectIDs) > 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "selection.emptyImpact",
				"selection.emptyImpact is set beside %d selected projects; "+
					"the no-op selects nothing", len(selection.ProjectIDs)))
		}
	}

	return diags
}

// validateStaging checks the task-owned staging tree.
func validateStaging(staging *Staging) []diag.Diagnostic {
	var diags []diag.Diagnostic

	requireAbs := func(field, value string) bool {
		if value == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingField, field,
				"%s is required", field))
			return false
		}
		if !isAbsolutePath(value) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field,
				"%s must be an absolute path, got %q", field, value))
			return false
		}
		return true
	}

	rootOK := requireAbs("staging.root", staging.Root)

	type stagingRoot struct {
		field string
		value string
	}
	roots := []stagingRoot{
		{"staging.project", staging.Project},
		{"staging.workspace", staging.Workspace},
		{"staging.commandOutput", staging.CommandOutput},
	}

	valid := make([]stagingRoot, 0, len(roots))
	for _, root := range roots {
		if !requireAbs(root.field, root.value) {
			continue
		}
		// Task-owned means exactly this: everything the task stages is inside
		// the one directory the orchestrator can capture or discard whole.
		if rootOK && !pathUnder(staging.Root, root.value) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, root.field,
				"%s (%q) must be inside staging.root (%q)",
				root.field, root.value, staging.Root))
			continue
		}
		valid = append(valid, root)
	}

	// The task contract's "one owner per output" rule holds across roots only
	// because two roots are two different directories. Staging that resolved
	// two roots to the same tree would let two declared outputs collide without
	// either declaration being wrong.
	for i := 0; i < len(valid); i++ {
		for j := i + 1; j < len(valid); j++ {
			if pathsOverlap(valid[i].value, valid[j].value) {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, valid[j].field,
					"%s (%q) overlaps %s (%q); staging roots must be distinct trees",
					valid[j].field, valid[j].value, valid[i].field, valid[i].value))
			}
		}
	}

	return diags
}
