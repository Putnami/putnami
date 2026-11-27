// Extension runtime ownership: the first of the three lifecycle primitives
// contract v3 adds.
//
// Before it, the CLI decided how to execute an extension by RECOGNIZING it:
// a hardcoded table mapped known first-party names to language-specific shell
// launchers, while an unknown language had no fast path at all.
// Recognition by name is the opposite of a contract — it cannot be validated,
// cannot be extended by a third party, and silently changes meaning when an
// extension is renamed.
//
// `runtime` replaces it with a declaration. An extension states the executable
// it ships (`executable`) and, when it is built from source rather than
// shipped in a platform archive, how to build it (`prepare`). The CLI resolves
// the executable, prepares it once per digest under an inter-process lock, and
// runs it. Nothing about that path depends on the extension's name.
//
// Two invariants make a prepared runtime trustworthy:
//
//   - WORKSPACE-INDEPENDENT PREPARATION. A prepare is a function of the
//     extension's own module and nothing else: it runs with the workspace
//     resolution turned OFF (GOWORK=off for Go) and resolves dependencies from
//     the extension module alone, over the replace closure the extension
//     maintains. Locally prepared and archive-shipped runtimes are therefore
//     the SAME artifact semantically, which is what lets one digest cover both
//     and lets a packaged extension be validated the way a local one is.
//     Declaration validation rejects workspace/project tokens, ambient-cwd
//     commands, and path arguments that are absolute or escape the extension
//     tree. Execution must still isolate an arbitrary executable: set cwd to
//     the extension root, disable ambient workspace resolution, and expose only
//     the maintained extension-module view and the assigned runtime output.
//
//   - THE DIGEST SEES THE SOURCES. `prepare.inputs` is what the artifact digest
//     is taken over, together with the prepare declaration, the platform, the
//     extension identity and the runtime ABI. An empty input set would digest
//     to the same value forever, so a source edit would keep serving a stale
//     binary — which is why inputs are REQUIRED whenever prepare is declared.
//
// Everything here is declaration-only. The resolver, the preparer, the
// `__putnami runtime-info` handshake and the {extensionRuntime} expansion land
// in slice C1; this file fixes the shape they implement and the failure
// vocabulary they report through (see lifecycle.go).

package extension

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Template tokens introduced by the runtime primitive. They use the same
// {token} syntax and the same expansion function (ExpandTemplateVars) as the
// pre-existing vocabulary — {workspaceRoot}, {projectRoot}, {extensionRoot},
// {outputRoot}, {cacheRoot} and the selection variables — so an extension
// author learns one mechanism, not two.
const (
	// TemplateVarExtensionRuntime expands, in a TASK's command, to the absolute
	// path of the resolved runtime executable. It is what replaces a
	// language-specific launcher: a task says {extensionRuntime}, not a
	// conventionally named file below {extensionRoot}.
	TemplateVarExtensionRuntime = "extensionRuntime"
	// TemplateVarRuntimeOutput expands, in a PREPARE command, to the directory
	// the prepare must write its executable into. The CLI owns that directory
	// (it is digest-keyed and published atomically), so the prepare never picks
	// its own destination.
	TemplateVarRuntimeOutput = "runtimeOutput"
	// TemplateVarInvocationArtifactRoot expands only for the producer,
	// consumers, and finalizer named by one finalizes relation. It is the
	// non-secret private root against which invocation-scoped declared-output
	// paths resolve; unrelated steps never receive it.
	TemplateVarInvocationArtifactRoot = "invocationArtifactRoot"
)

// ValidPrepareTemplateVars is the CLOSED set of template tokens a prepare
// command or argument may use, in canonical (sorted) order.
//
// It is short on purpose. {extensionRoot} is the extension's own tree — the
// only source a workspace-independent build may read — and {runtimeOutput} is
// the destination the CLI assigns. Every other token in the manifest vocabulary
// ({workspaceRoot}, {projectRoot}, {outputRoot}, {cacheRoot}, the selection
// variables) names something OUTSIDE the extension module, and reading it would
// make the produced artifact depend on the workspace it was built in — exactly
// what the workspace-independence invariant forbids.
var ValidPrepareTemplateVars = []string{
	"extensionRoot",
	TemplateVarRuntimeOutput,
}

// RuntimeDefinition is the manifest's `runtime` section: the extension's own
// executable and how to obtain it.
type RuntimeDefinition struct {
	// Executable is the runtime binary's path RELATIVE to the extension root
	// (for a published extension) or to the prepared runtime directory (for a
	// prepared one). The same relative path is used in both cases, which is
	// what makes local and packaged extensions interchangeable at the call
	// site. Cleaned slash form, no globs, no template variables, no escape
	// above the root.
	Executable string `json:"executable"`
	// Toolchains declares the lock-resolved executables this runtime can use.
	// Keys are local aliases referenced by Prepare.Toolchains,
	// RunToolchains, and TaskDefinition.Toolchains; the lock field inside each
	// declaration is the workspace lock identity.
	Toolchains map[string]RuntimeToolchain `json:"toolchains,omitempty"`
	// RunToolchains are required by every task executed through this runtime.
	// A task may add narrower requirements through TaskDefinition.Toolchains.
	RunToolchains []string `json:"runToolchains,omitempty"`
	// Resolves lists the workspace lock identities this extension answers the
	// toolchain-resolve control call for: the core asks the extension for
	// their pins through `<executable> __putnami toolchain-resolve`. An
	// identity need not be one of the runtime's own Toolchains, because an
	// extension can pin a toolchain another extension runs. At most 16 valid
	// identities of at most 128 bytes, each named once; normalization sorts
	// them.
	Resolves []string `json:"resolves,omitempty"`
	// InputSelectors lists the selectors this extension expands through
	// input-resolve: the core asks `<executable> __putnami input-resolve`
	// for the files a task input that names one of them reads. Each is a
	// selector name, `<scheme>:<name>` with a scheme other than `git`, of at
	// most 128 bytes, named once, at most 16 of them; normalization sorts
	// them. A task file pattern of this manifest that names a selector names
	// one listed here. Listing one requires protocol/cli
	// InputSelectorsContract.
	InputSelectors []string `json:"inputSelectors,omitempty"`
	// Prepare describes how to BUILD Executable from the extension's sources.
	// Absent means the executable ships ready to run in each platform archive
	// and the CLI never builds anything.
	Prepare *RuntimePrepare `json:"prepare,omitempty"`
	// Entries names further executables the runtime provides beside
	// Executable, by entry name. A task that names an entry in
	// TaskDefinition.RuntimeEntry runs that executable wherever its command or
	// arguments say {extensionRuntime}. Each path follows the rules of
	// Executable and resolves against the same root. Names are lowercase
	// letters, digits and hyphens, starting with a letter, at most 32 bytes.
	Entries map[string]RuntimeEntry `json:"entries,omitempty"`
	// CompiledInputs lists prepare inputs, spelled exactly as in
	// Prepare.Inputs, whose content reaches a task only through the
	// executables prepare builds from them. With it, a task's implementation
	// identity is the bytes of the executable it runs plus every other prepare
	// input, instead of every prepare input: an edit that changes one entry's
	// executable re-keys that entry's tasks only, and an edit that builds the
	// same bytes re-keys none. A file a task reads at run time, from
	// {extensionRoot} or elsewhere, is not compiled and must not be listed.
	CompiledInputs []string `json:"compiledInputs,omitempty"`
}

// RuntimeEntry is one further executable of a runtime (RuntimeDefinition.Entries).
type RuntimeEntry struct {
	// Executable is the entry's path, under the rules of
	// RuntimeDefinition.Executable and relative to the same root.
	Executable string `json:"executable"`
}

// MaxRuntimeEntries bounds RuntimeDefinition.Entries.
const MaxRuntimeEntries = 16

// RuntimePrepare is the workspace-independent build of an extension runtime.
//
// The declaration itself is part of the artifact digest, so changing the
// command, an argument, or the input set produces a different runtime rather
// than silently reusing the previous one.
type RuntimePrepare struct {
	// Command is the executable that performs the build: either a bare program
	// name resolved on PATH or {extensionRoot}/<relative-path>. Slash-relative
	// commands are rejected because they depend on an ambient working
	// directory; absolute, drive-qualified, and escaping paths are rejected.
	Command string `json:"command"`
	// Args are the command's arguments, in order. Order is meaningful and is
	// digested as written. Paths must be extension-root-relative or explicitly
	// rooted at {extensionRoot}/{runtimeOutput}; absolute, drive-qualified,
	// backslash-separated, and parent-segment paths are rejected.
	Args []string `json:"args,omitempty"`
	// Inputs are the extension-root-relative globs whose content the artifact
	// digest is taken over. Required: a prepare whose digest cannot see its own
	// sources would serve a stale binary after every source edit. Globs are
	// allowed here — unlike a declared output path, an input set legitimately
	// matches many files — but the patterns stay relative and may not escape
	// the extension root.
	Inputs []string `json:"inputs,omitempty"`
	// Toolchains names runtime toolchain declarations needed only while
	// preparing a mutable local runtime. Published runtimes do not resolve them.
	Toolchains []string `json:"toolchains,omitempty"`
}

// DeclaresRuntime reports whether the manifest declares a runtime section.
func (m *Manifest) DeclaresRuntime() bool {
	return m != nil && m.Runtime != nil
}

// RequiresPreparation reports whether the runtime must be built before it can
// be executed. A published extension ships its executable and returns false.
func (r *RuntimeDefinition) RequiresPreparation() bool {
	return r != nil && r.Prepare != nil
}

// ValidateRuntime checks the manifest's `runtime` section. It returns nothing
// for a manifest without one, so wiring it into the strict path cannot change
// any existing manifest's verdict.
//
// Diagnostics come out in a fixed order — executable, then prepare command,
// arguments in declaration order, inputs in declaration order — so two runs
// over one manifest are byte-identical.
func ValidateRuntime(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}
	diags := validateRuntimeEntries(m)
	if m.Runtime == nil {
		return append(diags, validateRuntimeToolchains(m)...)
	}
	runtime := m.Runtime

	if strings.TrimSpace(runtime.Executable) == "" {
		diags = append(diags, diag.Errorf("required-field", "runtime.executable",
			"runtime executable is required; it is the path the CLI runs instead of classifying the extension by name"))
	} else if _, err := NormalizeRelativePath(runtime.Executable); err != nil {
		diags = append(diags, diag.Errorf("invalid-runtime-path", "runtime.executable",
			"invalid runtime executable %q: %v", runtime.Executable, err))
	}

	if runtime.Prepare == nil {
		if len(runtime.CompiledInputs) > 0 {
			diags = append(diags, diag.Errorf("invalid-value", "runtime.compiledInputs",
				"compiled inputs need runtime.prepare: a runtime that ships ready to run is identified by its version"))
		}
		diags = append(diags, validateRuntimeToolchains(m)...)
		diags = append(diags, validateRuntimeInputSelectors(runtime.InputSelectors)...)
		return diags
	}
	prepare := runtime.Prepare
	base := "runtime.prepare"

	if strings.TrimSpace(prepare.Command) == "" {
		diags = append(diags, diag.Errorf("required-field", base+".command",
			"prepare command is required"))
	} else {
		diags = append(diags, validatePrepareCommand(base+".command", prepare.Command)...)
	}

	for i, arg := range prepare.Args {
		field := fmt.Sprintf("%s.args[%d]", base, i)
		if strings.TrimSpace(arg) == "" {
			diags = append(diags, diag.Errorf("invalid-value", field,
				"prepare argument cannot be empty"))
			continue
		}
		diags = append(diags, validateTemplateVars(field, arg)...)
		diags = append(diags, validatePrepareArgument(field, arg)...)
	}

	if len(prepare.Inputs) == 0 {
		diags = append(diags, diag.Errorf("required-field", base+".inputs",
			"prepare inputs are required; they are what the runtime artifact digest is taken over, "+
				"and an empty set would keep serving a stale binary after every source edit"))
	}
	seen := make(map[string]bool, len(prepare.Inputs))
	for i, input := range prepare.Inputs {
		field := fmt.Sprintf("%s.inputs[%d]", base, i)
		normalized, err := NormalizeInputPattern(input)
		if err != nil {
			diags = append(diags, diag.Errorf("invalid-runtime-path", field,
				"invalid prepare input %q: %v", input, err))
			continue
		}
		if seen[normalized] {
			diags = append(diags, diag.Errorf("duplicate-value", field,
				"prepare input %q is declared more than once", input))
		}
		seen[normalized] = true
	}

	diags = append(diags, validateRuntimeToolchains(m)...)
	diags = append(diags, validateRuntimeInputSelectors(runtime.InputSelectors)...)
	diags = append(diags, validateCompiledInputs(runtime, seen)...)

	return diags
}

// validateCompiledInputs checks runtime.compiledInputs against the prepare
// inputs whose normalized forms are declared. Each must name one of them, once.
func validateCompiledInputs(runtime *RuntimeDefinition, declared map[string]bool) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seen := make(map[string]bool, len(runtime.CompiledInputs))
	for i, input := range runtime.CompiledInputs {
		field := fmt.Sprintf("runtime.compiledInputs[%d]", i)
		normalized, err := NormalizeInputPattern(input)
		if err != nil {
			diags = append(diags, diag.Errorf("invalid-runtime-path", field,
				"invalid compiled input %q: %v", input, err))
			continue
		}
		if !declared[normalized] {
			diags = append(diags, diag.Errorf("invalid-value", field,
				"compiled input %q is not one of runtime.prepare.inputs; a compiled input names a prepare input whose content reaches tasks only through the executables", input))
		}
		if seen[normalized] {
			diags = append(diags, diag.Errorf("duplicate-value", field,
				"compiled input %q is declared more than once", input))
		}
		seen[normalized] = true
	}
	return diags
}

// validateRuntimeEntries checks runtime.entries and every task's runtimeEntry.
// A runtime without entries leaves every task without one.
func validateRuntimeEntries(m *Manifest) []diag.Diagnostic {
	var diags []diag.Diagnostic
	var entries map[string]RuntimeEntry
	if m.Runtime != nil {
		entries = m.Runtime.Entries
	}
	if len(entries) > MaxRuntimeEntries {
		diags = append(diags, diag.Errorf("invalid-value", "runtime.entries",
			"runtime declares %d entries; at most %d are allowed", len(entries), MaxRuntimeEntries))
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	paths := map[string]string{}
	if m.Runtime != nil {
		if main, err := NormalizeRelativePath(m.Runtime.Executable); err == nil {
			paths[main] = "runtime.executable"
		}
	}
	for _, name := range names {
		field := "runtime.entries." + name
		if !IsRuntimeEntryName(name) {
			diags = append(diags, diag.Errorf("invalid-value", field,
				"runtime entry name %q must be lowercase letters, digits and hyphens, start with a letter and be at most 32 bytes", name))
		}
		executable := entries[name].Executable
		normalized, err := NormalizeRelativePath(executable)
		if strings.TrimSpace(executable) == "" || err != nil {
			diags = append(diags, diag.Errorf("invalid-runtime-path", field+".executable",
				"invalid runtime entry executable %q", executable))
			continue
		}
		if other, taken := paths[normalized]; taken {
			diags = append(diags, diag.Errorf("duplicate-value", field+".executable",
				"runtime entry executable %q is already %s", executable, other))
			continue
		}
		paths[normalized] = field
	}
	taskNames := make([]string, 0, len(m.Tasks))
	for name := range m.Tasks {
		taskNames = append(taskNames, name)
	}
	sort.Strings(taskNames)
	for _, name := range taskNames {
		entry := m.Tasks[name].RuntimeEntry
		if entry == "" {
			continue
		}
		if _, ok := entries[entry]; !ok {
			diags = append(diags, diag.Errorf("unknown-reference", "tasks."+name+".runtimeEntry",
				"task %q names runtime entry %q, which runtime.entries does not declare", name, entry))
		}
	}
	return diags
}

// IsRuntimeEntryName reports whether name is a valid runtime entry name.
func IsRuntimeEntryName(name string) bool {
	if name == "" || len(name) > 32 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// validatePrepareCommand accepts exactly a bare PATH-resolved program name or
// an {extensionRoot}-rooted path. A slash-relative command such as bin/prepare
// would inherit an ambient cwd that the declaration never names.
func validatePrepareCommand(field, command string) []diag.Diagnostic {
	diags := validateTemplateVars(field, command)
	if len(diags) != 0 {
		return diags
	}
	trimmed := strings.TrimSpace(command)
	if strings.ContainsRune(trimmed, '\\') {
		diags = append(diags, diag.Errorf("invalid-runtime-path", field,
			`prepare command %q must use "/" separators`, command))
	}
	if strings.HasPrefix(trimmed, "/") || hasWindowsDrivePrefix(trimmed) {
		diags = append(diags, diag.Errorf("invalid-runtime-path", field,
			"prepare command %q must be a bare program name or a path rooted at a template variable, not an absolute or drive-qualified path",
			command))
	}
	if hasParentPathSegment(trimmed) {
		diags = append(diags, diag.Errorf("invalid-runtime-path", field,
			"prepare command %q must not escape its root", command))
	}
	if strings.Contains(trimmed, "/") {
		if !strings.HasPrefix(trimmed, "{extensionRoot}/") {
			diags = append(diags, diag.Errorf("invalid-runtime-path", field,
				"prepare command %q contains a path separator; path commands must be rooted at {extensionRoot}",
				command))
		} else if suffix := strings.TrimPrefix(trimmed, "{extensionRoot}/"); suffix != "" {
			if _, err := NormalizeRelativePath(suffix); err != nil {
				diags = append(diags, diag.Errorf("invalid-runtime-path", field,
					"invalid {extensionRoot}-relative prepare command %q: %v", command, err))
			}
		}
	} else if strings.ContainsAny(trimmed, "{}") {
		diags = append(diags, diag.Errorf("invalid-runtime-path", field,
			"prepare command %q must be a bare program name or {extensionRoot}/<relative-path>", command))
	}
	return diags
}

// validatePrepareArgument rejects path spellings that can resolve outside the
// isolated prepare roots. Relative paths are resolved from {extensionRoot};
// explicit rooted paths may use {extensionRoot} or {runtimeOutput}. This is a
// declaration check, not a claim that arbitrary executables cannot open other
// paths — the preparer must still enforce the execution isolation contract.
func validatePrepareArgument(field, argument string) []diag.Diagnostic {
	trimmed := strings.TrimSpace(argument)
	if strings.ContainsRune(trimmed, '\\') {
		return []diag.Diagnostic{diag.Errorf("invalid-runtime-path", field,
			"prepare argument %q must use slash separators; backslash paths are not portable", argument)}
	}
	if hasParentPathSegment(trimmed) {
		return []diag.Diagnostic{diag.Errorf("invalid-runtime-path", field,
			"prepare argument %q contains a parent path segment; external or escaping paths are not allowed",
			argument)}
	}

	value := trimmed
	if strings.HasPrefix(trimmed, "-") {
		if equals := strings.IndexByte(trimmed, '='); equals >= 0 {
			value = trimmed[equals+1:]
		} else if strings.Contains(trimmed, "/") || strings.ContainsRune(trimmed, '\\') {
			return []diag.Diagnostic{diag.Errorf("invalid-runtime-path", field,
				"prepare argument %q embeds a path in an option; pass the relative or template-rooted path as a separate argument",
				argument)}
		}
	}
	if value == "" {
		return nil
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\`) || hasWindowsDrivePrefix(value) {
		return []diag.Diagnostic{diag.Errorf("invalid-runtime-path", field,
			"prepare argument %q names an external absolute or drive-qualified path", argument)}
	}
	return nil
}

// hasParentPathSegment is segment-aware, so it catches both interior and
// terminal parents ("a/../b", "a/..") without rejecting names such as "..x".
func hasParentPathSegment(value string) bool {
	for _, segment := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if segment == ".." || strings.HasSuffix(segment, "=..") {
			return true
		}
	}
	return false
}

// validateTemplateVars rejects any {token} outside the closed prepare
// vocabulary. Tokens are reported in the order they appear, so the diagnostics
// are stable.
func validateTemplateVars(field, value string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for _, name := range templateVarNames(value) {
		if isValidPrepareTemplateVar(name) {
			continue
		}
		diags = append(diags, diag.Errorf("invalid-template-var", field,
			"template variable %q is not available during preparation; preparation is workspace-independent, "+
				"so only %s may be used", "{"+name+"}", renderTemplateVarList()))
	}
	return diags
}

func isValidPrepareTemplateVar(name string) bool {
	for _, candidate := range ValidPrepareTemplateVars {
		if candidate == name {
			return true
		}
	}
	return false
}

func renderTemplateVarList() string {
	rendered := make([]string, 0, len(ValidPrepareTemplateVars))
	for _, name := range ValidPrepareTemplateVars {
		rendered = append(rendered, "{"+name+"}")
	}
	return strings.Join(rendered, ", ")
}

// templateVarNames returns the {token} names in a string, in order of
// appearance. An unterminated "{" contributes nothing: it is not a template
// variable, and reporting it as one would rename the author's typo.
func templateVarNames(value string) []string {
	var names []string
	for i := 0; i < len(value); i++ {
		if value[i] != '{' {
			continue
		}
		end := strings.IndexByte(value[i:], '}')
		if end < 0 {
			break
		}
		names = append(names, value[i+1:i+end])
		i += end
	}
	return names
}

// NormalizeRuntime canonicalizes the runtime section in place: the executable
// path and the prepare inputs are cleaned, and the input set is sorted. Args
// and command keep their authored form because their order and spelling are
// meaningful. Values that cannot be normalized are left untouched for
// validation to report, exactly as declared output paths are.
//
// Canonicalization is what lets the artifact digest be taken over the parsed
// declaration instead of the manifest bytes: two spellings of the same prepare
// must produce one runtime, not two.
func NormalizeRuntime(m *Manifest) {
	if m == nil || m.Runtime == nil {
		return
	}
	if normalized, err := NormalizeRelativePath(m.Runtime.Executable); err == nil {
		m.Runtime.Executable = normalized
	}
	for name, entry := range m.Runtime.Entries {
		if normalized, err := NormalizeRelativePath(entry.Executable); err == nil {
			entry.Executable = normalized
			m.Runtime.Entries[name] = entry
		}
	}
	m.Runtime.CompiledInputs = normalizePatternList(m.Runtime.CompiledInputs)
	m.Runtime.RunToolchains = normalizeReferenceList(m.Runtime.RunToolchains)
	m.Runtime.Resolves = normalizeReferenceList(m.Runtime.Resolves)
	m.Runtime.InputSelectors = normalizeReferenceList(m.Runtime.InputSelectors)
	for name, task := range m.Tasks {
		task.Toolchains = normalizeReferenceList(task.Toolchains)
		m.Tasks[name] = task
	}
	prepare := m.Runtime.Prepare
	if prepare == nil {
		return
	}
	prepare.Inputs = normalizePatternList(prepare.Inputs)
	prepare.Toolchains = normalizeReferenceList(prepare.Toolchains)
}

// normalizeReferenceList sorts a toolchain reference, lock identity or input
// selector list and drops repeats. Authoring order carries no meaning here,
// and an entry named twice is the same requirement: leaving the repeat in
// would give one declaration two canonical forms, so two spellings of one
// runtime would produce two artifact digests. ValidateRuntime still reports
// the duplicate, because validation runs before normalization and an
// authored repeat is an authoring mistake.
func normalizeReferenceList(values []string) []string {
	if len(values) == 0 {
		return values
	}
	normalized := append([]string(nil), values...)
	sort.Strings(normalized)
	return slices.Compact(normalized)
}
