package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	// AgentArtifactManifestFilename is the manifest stored at the root of an
	// agent-workflow artifact archive.
	AgentArtifactManifestFilename = "putnami.agent-artifact.json"
	// AgentArtifactManifestProtocolVersion is the only manifest vocabulary this
	// package currently accepts.
	AgentArtifactManifestProtocolVersion = 1
)

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AgentArtifactManifest describes the workspace-relative files carried by one
// agent-workflow artifact. Paths are destinations as well as archive member
// names, so a materializer never has to infer where a packaged file belongs.
type AgentArtifactManifest struct {
	Schema          string              `json:"$schema,omitempty"`
	ProtocolVersion int                 `json:"protocolVersion"`
	Name            string              `json:"name"`
	Version         string              `json:"version"`
	Files           []AgentArtifactFile `json:"files"`
}

// AgentArtifactFile binds one canonical workspace-relative path to the exact
// content bytes the artifact is allowed to materialize.
type AgentArtifactFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ParseAgentArtifactManifest strictly decodes one manifest document. Unknown
// fields and trailing JSON are rejected so every accepted byte has a defined
// meaning.
func ParseAgentArtifactManifest(data []byte) (*AgentArtifactManifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var manifest AgentArtifactManifest
	if err := dec.Decode(&manifest); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse agent artifact manifest: %v", err),
		}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON documents")
		}
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse agent artifact manifest: trailing data: %v", err),
		}
	}
	return &manifest, nil
}

// ValidateAgentArtifactManifest checks the minimum contract needed for safe,
// reproducible materialization: an understood format, stable identity, a
// non-empty unique file set, contained canonical paths, and exact content
// digests.
func ValidateAgentArtifactManifest(manifest *AgentArtifactManifest) []diag.Diagnostic {
	if manifest == nil {
		return []diag.Diagnostic{diag.Errorf("nil-manifest", "", "agent artifact manifest is nil")}
	}

	var diags []diag.Diagnostic
	if manifest.ProtocolVersion != AgentArtifactManifestProtocolVersion {
		diags = append(diags, diag.Errorf("invalid-protocol-version", "protocolVersion",
			"unsupported agent artifact manifest protocol version %d (want %d)",
			manifest.ProtocolVersion, AgentArtifactManifestProtocolVersion))
	}
	if strings.TrimSpace(manifest.Name) == "" {
		diags = append(diags, diag.Errorf("required-field", "name", "agent artifact manifest requires a name"))
	}
	if strings.TrimSpace(manifest.Version) == "" {
		diags = append(diags, diag.Errorf("required-field", "version", "agent artifact manifest requires a version"))
	}
	if len(manifest.Files) == 0 {
		diags = append(diags, diag.Errorf("required-field", "files", "agent artifact manifest requires at least one file"))
	}

	seen := make(map[string]int, len(manifest.Files))
	for index, file := range manifest.Files {
		field := fmt.Sprintf("files.%d", index)
		if !ValidAgentArtifactPath(file.Path) {
			diags = append(diags, diag.Errorf("invalid-path", field+".path",
				"agent artifact file path %q must be canonical, slash-separated, and workspace-relative", file.Path))
		}
		if first, ok := seen[file.Path]; ok {
			diags = append(diags, diag.Errorf("duplicate-path", field+".path",
				"agent artifact file path %q duplicates files.%d.path", file.Path, first))
		} else {
			seen[file.Path] = index
		}
		if !sha256HexPattern.MatchString(file.SHA256) {
			diags = append(diags, diag.Errorf("invalid-integrity", field+".sha256",
				"agent artifact file SHA-256 must be 64 lowercase hexadecimal characters"))
		}
	}
	return diags
}

// CanonicalAgentArtifactManifest returns a deep, non-mutating copy with files
// sorted by path. That ordering plus encoding/json's stable struct field order
// defines the manifest's canonical bytes.
func CanonicalAgentArtifactManifest(manifest *AgentArtifactManifest) *AgentArtifactManifest {
	if manifest == nil {
		return nil
	}
	out := *manifest
	out.Files = append([]AgentArtifactFile(nil), manifest.Files...)
	sort.Slice(out.Files, func(i, j int) bool {
		if out.Files[i].Path != out.Files[j].Path {
			return out.Files[i].Path < out.Files[j].Path
		}
		return out.Files[i].SHA256 < out.Files[j].SHA256
	})
	return &out
}

// MarshalAgentArtifactManifest renders canonical two-space-indented JSON with
// exactly one trailing newline.
func MarshalAgentArtifactManifest(manifest *AgentArtifactManifest) ([]byte, error) {
	if diags := ValidateAgentArtifactManifest(manifest); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid agent artifact manifest: %v", diag.Errors(diags))
	}
	data, err := json.MarshalIndent(CanonicalAgentArtifactManifest(manifest), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal agent artifact manifest: %w", err)
	}
	return append(data, '\n'), nil
}

// ParseAndValidateAgentArtifactManifest combines strict parsing and semantic
// validation without normalizing authored order. Call Marshal to obtain the
// canonical representation.
func ParseAndValidateAgentArtifactManifest(data []byte) (*AgentArtifactManifest, []diag.Diagnostic) {
	manifest, diags := ParseAgentArtifactManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return manifest, append(diags, ValidateAgentArtifactManifest(manifest)...)
}

// ValidAgentArtifactPath reports whether value is a path an agent artifact may
// name: non-empty, canonical, slash-separated, workspace-relative, and free of
// anything that would make its destination depend on the host — a leading
// slash, a backslash or drive separator, a control character, surrounding
// whitespace, or a parent-directory escape.
//
// It is EXPORTED because it is a security control with more than one consumer.
// A materializer re-checks every path it is about to touch, and re-checks the
// paths recorded in its own ownership state, which a hand edit or a corrupted
// write could otherwise turn into a delete outside the workspace. Both must
// apply the same rule as the manifest validator: a second, privately-spelled
// copy of this predicate is how the two drift apart, and the direction they
// drift in is "the writer accepts what the validator rejected".
func ValidAgentArtifactPath(value string) bool {
	return value != "" &&
		value != "." &&
		value != ".." &&
		value == strings.TrimSpace(value) &&
		!strings.HasPrefix(value, "/") &&
		!strings.Contains(value, `\`) &&
		!strings.Contains(value, ":") &&
		!strings.ContainsFunc(value, unicode.IsControl) &&
		path.Clean(value) == value &&
		!strings.HasPrefix(value, "../")
}
