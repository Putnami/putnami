// Package manifest authors extension manifests in Go and refuses to emit one
// that violates the extension protocol.
//
// A putnami.extension.json is normally hand-written, and under the v2 task
// contract a mistake in it was cheap: the CLI inferred a task's filesystem
// footprint anyway, so a wrong or missing statement about outputs cost at most
// a special case. The v3 task contract makes the manifest load-bearing — a
// declared output is captured and restored verbatim, and two tasks claiming one
// path is a correctness bug that surfaces in a CONSUMER's workspace, at plan
// time, far from the extension that caused it.
//
// This package moves that verdict to authoring time. Build runs exactly the
// gate `putnami dev extension validate` and the package job run
// (extension.FullValidateManifest, whose v3 half is
// extension.ValidateTaskContracts), so an authored manifest that violates
//
//   - EXACTNESS — a declared path must be a concrete file or subtree under a
//     named root: no globs, no template variables, no escapes;
//   - ONE OWNER PER OUTPUT — no two tasks may declare the same path, and a file
//     nested in another task's declared subtree is the same path;
//   - HONEST EFFECTS — a task whose consequences a cache hit cannot reproduce
//     must declare the effect and must not be cacheable;
//
// fails in the authoring program with a diagnostic naming the field, instead of
// in a user's plan. The rules themselves are never restated here: they live in
// protocols/extension, which is also what the planner enforces, so the SDK and
// the CLI cannot drift apart.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
)

// Builder accumulates an extension manifest. The zero value is not usable; call
// New. Nothing is validated as members are added — a manifest is only
// meaningful as a whole, since ownership of an output is a property of every
// task at once — so the verdict happens in Build.
type Builder struct {
	manifest proto.Manifest
}

// New starts a manifest for the named extension.
func New(name, version string) *Builder {
	return &Builder{manifest: proto.Manifest{
		Schema:   "https://putnami.dev/schemas/putnami-extension.json",
		Name:     name,
		Version:  version,
		Commands: map[string]proto.CommandDefinition{},
		Tasks:    map[string]proto.TaskDefinition{},
	}}
}

// Tool adds an MCP tool contributed by the extension.
func (b *Builder) Tool(name string, tool proto.ToolDefinition) *Builder {
	if b.manifest.Tools == nil {
		b.manifest.Tools = map[string]proto.ToolDefinition{}
	}
	b.manifest.Tools[name] = tool
	return b
}

// AgentContent declares the extension's agent-content contribution. A later
// call replaces the earlier one: a manifest carries at most one.
func (b *Builder) AgentContent(content proto.AgentContentContribution) *Builder {
	b.manifest.AgentContent = &content
	return b
}

// Command adds a command. A later call with the same name replaces the earlier
// one, so a generator can compose a manifest without checking what it already
// wrote.
func (b *Builder) Command(name string, command proto.CommandDefinition) *Builder {
	b.manifest.Commands[name] = command
	return b
}

// Task adds a task. Pass Declares(...) as the task's Declares member to give it
// a v3 contract; a task without one keeps v2 semantics, exactly as in a
// hand-written manifest.
func (b *Builder) Task(name string, task proto.TaskDefinition) *Builder {
	b.manifest.Tasks[name] = task
	return b
}

// Build validates the accumulated manifest and returns an independent copy of
// it. The error is a *ValidationError carrying every protocol diagnostic, so a
// generator can report all of an author's mistakes in one pass rather than one
// per run.
//
// A manifest that passes stamps `cliContract` at the contract its vocabulary
// requires (proto.RequiredCLIContract): the base CLI contract, or the additive
// agent-content contract for a manifest that declares agent content.
// The stamp stays EARNED rather than claimed: this is the same verdict the
// package-time gate applies (FullValidateManifest), run by the same code, so
// passing it is exactly what the field asserts. Stamping
// here is what makes an SDK-authored manifest loadable at all — since contract 3
// a manifest that declares a contract surface without a matching stamp is
// rejected — and it costs nothing at package time, where the gate re-validates
// and re-stamps the staged copy anyway.
//
// The returned manifest never aliases the builder: a caller may keep building
// after a Build without rewriting what it already produced.
func (b *Builder) Build() (*proto.Manifest, error) {
	built, err := copyManifest(&b.manifest)
	if err != nil {
		return nil, err
	}
	if err := Validate(built); err != nil {
		return nil, err
	}
	if proto.DeclaresContractSurface(built) {
		built.CLIContract = proto.RequiredCLIContract(built)
	}
	return built, nil
}

// WriteFile validates the manifest and writes it to path as canonical JSON.
// Nothing is written when validation fails — a rejected manifest must not leave
// a half-valid file behind for a packager to pick up.
//
// The bytes are deterministic: Go marshals map keys in sorted order, so two
// runs of the same generator produce byte-identical output and the file is
// diffable in review.
func (b *Builder) WriteFile(path string) error {
	built, err := b.Build()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(built, "", "  ")
	if err != nil {
		return fmt.Errorf("encode extension manifest: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write extension manifest %s: %w", path, err)
	}
	return nil
}

// Validate applies the protocol's strict validation — structural rules, the v3
// task contract, the pipeline DAG, and schema references — to a manifest built
// by any means. It is exported for authors who assemble a proto.Manifest
// directly or load one from disk and want the same authoring-time verdict a
// Builder gives.
func Validate(m *proto.Manifest) error {
	diags := proto.FullValidateManifest(m)
	if errs := diag.Errors(diags); len(errs) > 0 {
		return &ValidationError{Diagnostics: errs}
	}
	return nil
}

// ValidationError reports every protocol violation in an authored manifest.
type ValidationError struct {
	Diagnostics []diag.Diagnostic
}

// Error renders one line per violation, each naming the manifest field, so the
// message points at what to edit.
func (e *ValidationError) Error() string {
	lines := make([]string, 0, len(e.Diagnostics)+1)
	lines = append(lines, fmt.Sprintf("extension manifest has %d protocol violation(s):", len(e.Diagnostics)))
	for _, d := range e.Diagnostics {
		lines = append(lines, "  - "+d.String())
	}
	return strings.Join(lines, "\n")
}

// Codes returns the diagnostic codes in order, for a caller that branches on
// which rule was broken instead of printing the message.
func (e *ValidationError) Codes() []string {
	codes := make([]string, 0, len(e.Diagnostics))
	for _, d := range e.Diagnostics {
		codes = append(codes, d.Code)
	}
	return codes
}

// copyManifest deep-copies through JSON so a built manifest never shares a map
// or slice with the builder that produced it.
func copyManifest(m *proto.Manifest) (*proto.Manifest, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode extension manifest: %w", err)
	}
	var copied proto.Manifest
	if err := json.Unmarshal(raw, &copied); err != nil {
		return nil, fmt.Errorf("decode extension manifest: %w", err)
	}
	return &copied, nil
}
