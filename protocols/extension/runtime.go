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
	// Prepare describes how to BUILD Executable from the extension's sources.
	// Absent means the executable ships ready to run in each platform archive
	// and the CLI never builds anything.
	Prepare *RuntimePrepare `json:"prepare,omitempty"`
}

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
	if m.Runtime == nil {
		return validateRuntimeToolchains(m)
	}
	runtime := m.Runtime
	var diags []diag.Diagnostic

	if strings.TrimSpace(runtime.Executable) == "" {
		diags = append(diags, diag.Errorf("required-field", "runtime.executable",
			"runtime executable is required; it is the path the CLI runs instead of classifying the extension by name"))
	} else if _, err := NormalizeRelativePath(runtime.Executable); err != nil {
		diags = append(diags, diag.Errorf("invalid-runtime-path", "runtime.executable",
			"invalid runtime executable %q: %v", runtime.Executable, err))
	}

	if runtime.Prepare == nil {
		diags = append(diags, validateRuntimeToolchains(m)...)
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

	return diags
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
	m.Runtime.RunToolchains = normalizeReferenceList(m.Runtime.RunToolchains)
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

// normalizeReferenceList sorts a toolchain reference list and drops repeats.
// Authoring order carries no meaning here, and a reference named twice is the
// same requirement: leaving the repeat in would give one declaration two
// canonical forms, so two spellings of one runtime would produce two artifact
// digests. ValidateRuntime still reports the duplicate, because validation runs
// before normalization and an authored repeat is an authoring mistake.
func normalizeReferenceList(values []string) []string {
	if len(values) == 0 {
		return values
	}
	normalized := append([]string(nil), values...)
	sort.Strings(normalized)
	return slices.Compact(normalized)
}
