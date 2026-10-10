// Agent-content contribution: the optional `agentContent` section of an
// extension manifest.
//
// An extension may ship the instructions that belong to it — skills, worker
// profiles, references and helper scripts, projected onto every supported
// agent host — under the same resolved version as its commands and tools. The
// content is an agent-artifact tree (protocols/workspace agent-artifact
// manifest v1: `putnami.agent-artifact.json` plus the files it declares), so
// the CLI materializes it with the ownership rules it already applies to every
// agent artifact, and never with a second set.
//
// The section has two forms, and a manifest carries exactly one of them:
//
//   - AUTHORED: `source` names the extension-relative directory holding the
//     closed authoring layout (`skills/<name>/…`, `agents/<name>/…`). A local
//     extension is built from it on every run, and the package step builds
//     it into `path`.
//   - PACKAGED: `manifestSha256` binds the built tree under `path`. The
//     package step writes it, so the chain lock → extension manifest →
//     content manifest → file digests pins every byte the CLI may write to
//     the exact extension release that ships it.
//
// Declaring the section is additive vocabulary: the manifest must carry
// cliContract AgentContentContract (RequiredCLIContract), which a reader that
// predates the section refuses instead of loading the extension without its
// instructions.

package extension

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

// AgentContentContribution is the manifest's `agentContent` section.
type AgentContentContribution struct {
	// Path is the extension-relative directory of the BUILT content tree: its
	// agent-artifact manifest and every file that manifest declares. It is
	// required in both forms, because the package step builds the authored
	// source into it.
	Path string `json:"path"`
	// Source is the extension-relative directory of the authoring layout
	// (`skills/`, `agents/`) the SDK builder turns into Path. Present only in
	// the authored form.
	Source string `json:"source,omitempty"`
	// ManifestSHA256 is the lowercase hex SHA-256 of the agent-artifact
	// manifest under Path. Present only in the packaged form; the package step
	// writes it.
	ManifestSHA256 string `json:"manifestSha256,omitempty"`
	// Supersedes names the agent-artifact identities this content replaces:
	// separately declared artifacts (registry or in-tree) whose workflows the
	// extension now ships itself. It is a statement by the extension's author,
	// and it has two consumers: a workspace that opts into this content while
	// still declaring a superseded artifact is refused rather than given two
	// competing sets of instructions, and `putnami migrate agent-content`
	// moves the superseded declarations, pins and ownership records to this
	// content. Present in both forms; the package step keeps it.
	Supersedes []string `json:"supersedes,omitempty"`
}

// supersededNameFormat is the grammar of an agent-artifact identity a
// contribution may supersede: the registry name grammar extensions and
// artifacts share (@scope/name or name, lowercase letters, digits, hyphens).
var supersededNameFormat = regexp.MustCompile(`^(@[a-z0-9-]+/[a-z0-9-]+|[a-z0-9][a-z0-9-]*)$`)

// DeclaresAgentContent reports whether the manifest carries an agent-content
// section at all. An empty section counts: it is a declaration that says
// nothing, which validation rejects rather than treating as absent.
func (m *Manifest) DeclaresAgentContent() bool {
	return m != nil && m.AgentContent != nil
}

// Packaged reports whether the contribution is in its packaged form: a built
// tree bound by its manifest digest.
func (c AgentContentContribution) Packaged() bool {
	return c.ManifestSHA256 != ""
}

// RequiredCLIContract returns the lowest CLI contract whose vocabulary covers
// the manifest: ReleaseBaselineInputContract for a task that declares the
// releaseBaseline runtime input, GitInputModeContract (the same rung) for a
// task that declares a `git:` file pattern, GoEmbedInputsContract for Go embed
// task inputs/cache-key files, AgentContentContract for an agent-content
// contribution, CurrentContract otherwise. The highest rung any task reaches
// wins.
//
// It is the stamp a packager writes, and the floor LoadManifest enforces. A
// manifest stamped below it uses vocabulary its stamp does not cover; a
// manifest stamped at or above it (up to LatestContract) loads.
func RequiredCLIContract(m *Manifest) int {
	required := protocolcli.CurrentContract
	if m.DeclaresAgentContent() {
		required = protocolcli.AgentContentContract
	}
	if m == nil {
		return required
	}
	for _, task := range m.Tasks {
		if declaresReleaseBaseline(task) {
			required = max(required, protocolcli.ReleaseBaselineInputContract)
		}
		if declaresGitInput(task) {
			required = max(required, protocolcli.GitInputModeContract)
		}
		if declaresGoEmbedSelector(task) {
			required = max(required, protocolcli.GoEmbedInputsContract)
		}
	}
	return required
}

// declaresGitInput reports whether a task selects files with a `git:` pattern
// in an input port or its cache key's files. An exclusion ("!git:…") selects
// nothing, so it does not count.
func declaresGitInput(task TaskDefinition) bool {
	isGitPattern := func(file string) bool { return strings.HasPrefix(file, "git:") }
	for _, input := range task.Inputs {
		if slices.ContainsFunc(input.Files, isGitPattern) {
			return true
		}
	}
	if task.Cache == nil || task.Cache.Key == nil {
		return false
	}
	key := task.Cache.Key
	return slices.ContainsFunc(key.Files, isGitPattern) || slices.ContainsFunc(key.WorkspaceFiles, isGitPattern) ||
		slices.ContainsFunc(key.ClosureFiles, isGitPattern)
}

// declaresReleaseBaseline reports whether a task keys on the releaseBaseline
// runtime input, through an input port or its cache key's runtime names.
func declaresReleaseBaseline(task TaskDefinition) bool {
	for name, input := range task.Inputs {
		if input.From == TaskInputFromRuntime && name == RuntimeInputReleaseBaseline {
			return true
		}
	}
	return task.Cache != nil && task.Cache.Key != nil && slices.Contains(task.Cache.Key.Runtime, RuntimeInputReleaseBaseline)
}

// declaresGoEmbedSelector reports whether a task names a Go embed selector in
// an input port or its cache key's files.
func declaresGoEmbedSelector(task TaskDefinition) bool {
	isSelector := func(file string) bool { return file == "go-embed:build" || file == "go-embed:test" }
	for _, input := range task.Inputs {
		if slices.ContainsFunc(input.Files, isSelector) {
			return true
		}
	}
	return task.Cache != nil && task.Cache.Key != nil &&
		(slices.ContainsFunc(task.Cache.Key.Files, isSelector) || slices.ContainsFunc(task.Cache.Key.ClosureFiles, isSelector))
}

// ValidateAgentContent checks the agent-content section. It is inert for a
// manifest that declares none, so every manifest written before the section
// existed keeps its exact verdict.
//
// Every path is extension-relative and canonical: the CLI resolves Path and
// Source against the extension root, reads them as they are written, and
// refuses one that escapes the root or names the root itself.
func ValidateAgentContent(m *Manifest) []diag.Diagnostic {
	if !m.DeclaresAgentContent() {
		return nil
	}
	content := m.AgentContent
	const field = "agentContent"
	var diags []diag.Diagnostic

	pathOK := false
	if strings.TrimSpace(content.Path) == "" {
		diags = append(diags, diag.Errorf("required-field", field+".path",
			"agent content path is required: it names the directory that holds the built content"))
	} else if clean, err := NormalizeRelativePath(content.Path); err != nil {
		diags = append(diags, diag.Errorf("invalid-agent-content-path", field+".path",
			"invalid agent content path %q: %v", content.Path, err))
	} else if clean != content.Path {
		diags = append(diags, diag.Errorf("invalid-agent-content-path", field+".path",
			"agent content path %q must be written in canonical form %q", content.Path, clean))
	} else {
		pathOK = true
	}

	sourceOK := false
	if content.Source != "" {
		if clean, err := NormalizeRelativePath(content.Source); err != nil {
			diags = append(diags, diag.Errorf("invalid-agent-content-path", field+".source",
				"invalid agent content source %q: %v", content.Source, err))
		} else if clean != content.Source {
			diags = append(diags, diag.Errorf("invalid-agent-content-path", field+".source",
				"agent content source %q must be written in canonical form %q", content.Source, clean))
		} else {
			sourceOK = true
		}
	}
	if pathOK && sourceOK && pathsOverlap(content.Path, content.Source) {
		diags = append(diags, diag.Errorf("agent-content-overlap", field+".source",
			"agent content source %q and path %q overlap: the build writes path, so it must not contain or sit inside its own source",
			content.Source, content.Path))
	}

	if content.ManifestSHA256 != "" && !isLowerSHA256Hex(content.ManifestSHA256) {
		diags = append(diags, diag.Errorf("invalid-agent-content-digest", field+".manifestSha256",
			"agent content manifestSha256 %q must be 64 lowercase hex characters", content.ManifestSHA256))
	}
	diags = append(diags, validateSupersedes(m.Name, content.Supersedes)...)

	switch {
	case content.Source == "" && content.ManifestSHA256 == "":
		diags = append(diags, diag.Errorf("empty-agent-content", field,
			"an agent-content contribution names its authored source (source) or its built, digest-bound tree (manifestSha256); this one names neither"))
	case content.Source != "" && content.ManifestSHA256 != "":
		diags = append(diags, diag.Errorf("ambiguous-agent-content", field,
			"an agent-content contribution is either authored (source) or packaged (manifestSha256), never both: the package step replaces the source with the digest of what it built"))
	}
	return diags
}

// validateSupersedes checks the superseded identities: each one is a registry
// name, none repeats, and none is the extension itself — the extension's own
// name is already its content's identity, so superseding it would make one
// ownership record both the source and the target of a migration.
func validateSupersedes(extensionName string, names []string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seen := make(map[string]int, len(names))
	for index, name := range names {
		entry := "agentContent.supersedes." + strconv.Itoa(index)
		switch {
		case !supersededNameFormat.MatchString(name):
			diags = append(diags, diag.Errorf("invalid-agent-content-supersedes", entry,
				"superseded agent artifact %q must be a registry name (@scope/name or name: lowercase letters, digits and hyphens)", name))
		case name == strings.TrimSpace(extensionName):
			diags = append(diags, diag.Errorf("invalid-agent-content-supersedes", entry,
				"agent content cannot supersede its own extension %q: that name already identifies this content", name))
		}
		if first, dup := seen[name]; dup {
			diags = append(diags, diag.Errorf("invalid-agent-content-supersedes", entry,
				"superseded agent artifact %q repeats agentContent.supersedes.%d", name, first))
			continue
		}
		seen[name] = index
	}
	return diags
}

// pathsOverlap reports whether two canonical relative paths are equal or one
// contains the other.
func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// isLowerSHA256Hex reports whether value is a bare 64-character lowercase hex
// digest, the shape lockfile digests and agent-artifact manifests use.
func isLowerSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
