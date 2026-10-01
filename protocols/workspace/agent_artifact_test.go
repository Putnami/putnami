package workspace

import (
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestAgentArtifactManifestCanonicalBytesAreDeterministicAndNonMutating(t *testing.T) {
	manifest := &AgentArtifactManifest{
		ProtocolVersion: AgentArtifactManifestProtocolVersion,
		Name:            "@putnami/agent-workflows",
		Version:         "1.2.3",
		Files: []AgentArtifactFile{
			{Path: ".codex/agents/fix-heavy.toml", SHA256: strings.Repeat("3", 64)},
			{Path: ".agents/skills/fix/SKILL.md", SHA256: strings.Repeat("1", 64)},
			{Path: ".claude/skills/fix/SKILL.md", SHA256: strings.Repeat("2", 64)},
		},
	}
	original := append([]AgentArtifactFile(nil), manifest.Files...)

	first, err := MarshalAgentArtifactManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		got, err := MarshalAgentArtifactManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("iteration %d produced different canonical bytes", i)
		}
	}
	if !reflect.DeepEqual(manifest.Files, original) {
		t.Fatalf("canonical serialization mutated authored file order: got %v want %v", manifest.Files, original)
	}
	if agents := strings.Index(string(first), ".agents/"); agents < 0 || agents > strings.Index(string(first), ".claude/") {
		t.Fatalf("canonical files are not sorted by path:\n%s", first)
	}
	if !strings.HasSuffix(string(first), "\n") || strings.HasSuffix(string(first), "\n\n") {
		t.Fatalf("canonical manifest must carry exactly one trailing newline: %q", first)
	}

	parsed, diags := ParseAndValidateAgentArtifactManifest(first)
	if diag.HasErrors(diags) {
		t.Fatalf("canonical manifest did not round-trip: %v", diags)
	}
	again, err := MarshalAgentArtifactManifest(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(first) {
		t.Fatalf("canonical round trip changed bytes:\nfirst:\n%s\nsecond:\n%s", first, again)
	}
}

func TestAgentArtifactManifestRejectsUnsafeOrAmbiguousFiles(t *testing.T) {
	tests := []struct {
		name string
		data string
		code string
	}{
		{"parent escape", `{"protocolVersion":1,"name":"a","version":"1","files":[{"path":"../AGENTS.md","sha256":"` + strings.Repeat("1", 64) + `"}]}`, "invalid-path"},
		{"absolute", `{"protocolVersion":1,"name":"a","version":"1","files":[{"path":"/tmp/SKILL.md","sha256":"` + strings.Repeat("1", 64) + `"}]}`, "invalid-path"},
		{"backslash", `{"protocolVersion":1,"name":"a","version":"1","files":[{"path":".agents\\skills\\fix.md","sha256":"` + strings.Repeat("1", 64) + `"}]}`, "invalid-path"},
		{"duplicate", `{"protocolVersion":1,"name":"a","version":"1","files":[{"path":".agents/x","sha256":"` + strings.Repeat("1", 64) + `"},{"path":".agents/x","sha256":"` + strings.Repeat("2", 64) + `"}]}`, "duplicate-path"},
		{"bad digest", `{"protocolVersion":1,"name":"a","version":"1","files":[{"path":".agents/x","sha256":"ABC"}]}`, "invalid-integrity"},
		{"unknown field", `{"protocolVersion":1,"name":"a","version":"1","files":[{"path":".agents/x","sha256":"` + strings.Repeat("1", 64) + `","mode":"copy"}]}`, "parse-error"},
		{"trailing document", `{"protocolVersion":1,"name":"a","version":"1","files":[{"path":".agents/x","sha256":"` + strings.Repeat("1", 64) + `"}]} {}`, "parse-error"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := ParseAndValidateAgentArtifactManifest([]byte(tc.data))
			if !hasDiagnosticCode(diags, tc.code) {
				t.Fatalf("diagnostics = %v, want code %q", diags, tc.code)
			}
		})
	}
}

// TestValidAgentArtifactPathIsTheSingleRule pins the exported predicate a
// materializer reuses for the paths it writes, removes, and reads back out of
// its own ownership state. The manifest validator routes through the same
// function, so a relaxed rule here fails both this test and the manifest
// rejection tests above rather than silently widening one consumer.
func TestValidAgentArtifactPathIsTheSingleRule(t *testing.T) {
	accepted := []string{
		".agents/skills/fix/SKILL.md",
		".agents/skills/fix/scripts/finalize-pr.sh",
		".claude/skills/fix/SKILL.md",
		"agents/openai.yaml",
		"AGENTS.md",
	}
	for _, value := range accepted {
		if !ValidAgentArtifactPath(value) {
			t.Errorf("ValidAgentArtifactPath(%q) = false, want true", value)
		}
	}

	rejected := map[string]string{
		"empty":            "",
		"dot":              ".",
		"dotdot":           "..",
		"parent escape":    "../AGENTS.md",
		"absolute":         "/etc/passwd",
		"backslash":        `.agents\skills\fix.md`,
		"drive letter":     "C:/agents/skill.md",
		"non canonical":    ".agents/./skills/fix.md",
		"trailing slash":   ".agents/skills/",
		"inner traversal":  ".agents/skills/../../etc/passwd",
		"leading space":    " .agents/skills/fix.md",
		"trailing space":   ".agents/skills/fix.md ",
		"control char":     ".agents/skills/fix\n.md",
		"nul byte":         ".agents/skills/fix\x00.md",
		"double separator": ".agents//skills/fix.md",
	}
	for name, value := range rejected {
		if ValidAgentArtifactPath(value) {
			t.Errorf("%s: ValidAgentArtifactPath(%q) = true, want false", name, value)
		}
	}
}

func hasDiagnosticCode(diags []diag.Diagnostic, code string) bool {
	for _, diagnostic := range diags {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
