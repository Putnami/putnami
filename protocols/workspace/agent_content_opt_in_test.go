package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// contract4AgentArtifactReference is how a CLI released at contract 4 reads
// one agentArtifacts entry, restated so the compatibility claim is
// executable: "/path" names an in-tree project, and anything else is a
// registry reference spelled name[:channel], split at the first colon.
func contract4AgentArtifactReference(declared string) (name, channel string, inTree bool) {
	declared = strings.TrimSpace(declared)
	if strings.HasPrefix(declared, "/") {
		return "", "", true
	}
	name, channel = declared, "latest"
	if head, tail, found := strings.Cut(declared, ":"); found {
		name, channel = head, tail
	}
	name, channel = strings.TrimSpace(name), strings.TrimSpace(channel)
	if channel == "" {
		channel = "latest"
	}
	return name, channel, false
}

// contract4InstallPin is that CLI's `putnami install` of one registry
// reference: the committed lock is its only version source, and a reference
// the lock does not pin fails the command with this message before anything
// is written.
func contract4InstallPin(lock *Lock, name string) error {
	if _, pinned := lock.AgentArtifacts[name]; !pinned {
		return fmt.Errorf("agent workflows install: %s: workspace lock pins no agent artifact: putnami.lock.json records no %s entry", name, name)
	}
	return nil
}

// TestAgentContentOptIn_OlderReaderFailsClosed is the old-reader half of the
// opt-in's compatibility, executed. A CLI that predates `extension:<name>`
// reads it with its own grammar as an agent artifact named "extension" on the
// channel "<name>". A migrated lock pins no artifact of that name, so that
// CLI's install stops with the message below and writes nothing: it never
// installs the workspace without its content, and never picks a version.
func TestAgentContentOptIn_OlderReaderFailsClosed(t *testing.T) {
	cfg, diags := ParseWorkspaceConfig([]byte(`{"name":"consumer","agentArtifacts":["extension:@putnami/contributor"]}`))
	if diags != nil {
		t.Fatalf("the migrated workspace document does not parse: %v", diags)
	}
	lock, diags := ParseLock([]byte(`{"version":4,"extensions":{},"templates":{}}`))
	if diags != nil {
		t.Fatalf("the migrated lock does not parse: %v", diags)
	}
	for _, declared := range cfg.AgentArtifacts {
		name, channel, inTree := contract4AgentArtifactReference(declared)
		if inTree || name != "extension" || channel != "@putnami/contributor" {
			t.Fatalf("a contract-4 reader reads %q as %q on %q (in-tree %v)", declared, name, channel, inTree)
		}
		err := contract4InstallPin(lock, name)
		const want = "agent workflows install: extension: workspace lock pins no agent artifact: putnami.lock.json records no extension entry"
		if err == nil || err.Error() != want {
			t.Fatalf("a contract-4 install of %q: %v, want %q", declared, err, want)
		}
	}

	// The published description of the opt-in states that reading.
	data, err := os.ReadFile("schemas/workspace.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	description := schema.Properties["agentArtifacts"].Description
	for _, statement := range []string{"`extension:<name>`", "an agent artifact named `extension`, which no lock pins"} {
		if !strings.Contains(description, statement) {
			t.Errorf("the agentArtifacts description does not state %q:\n%s", statement, description)
		}
	}
}
