package extension

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

func TestGoEmbedSelectorRequiresAdditiveCLIContract(t *testing.T) {
	manifest := []byte(`{"name":"@test/go","cliContract":5,"commands":{"build":{"run":[{"id":"b","task":"build"}]}},"tasks":{"build":{"kind":"command","command":"echo","inputs":{"sources":{"from":"project","files":["**/*.go","go-embed:build"]}}}}}`)
	if _, err := NegotiateManifest("go.json", manifest); err == nil || !strings.Contains(err.Error(), "requires 6") {
		t.Fatalf("old CLI stamp accepted selector: %v", err)
	}
	manifest = []byte(strings.Replace(string(manifest), `"cliContract":5`, `"cliContract":6`, 1))
	m, err := NegotiateManifest("go.json", manifest)
	if err != nil || RequiredCLIContract(m) != protocolcli.GoEmbedInputsContract {
		t.Fatalf("selector contract = %v, %v", m, err)
	}
	m.AgentContent = &AgentContentContribution{}
	if got := RequiredCLIContract(m); got != protocolcli.GoEmbedInputsContract {
		t.Fatalf("combined additive floor = %d", got)
	}
	for _, bad := range []string{
		strings.Replace(string(manifest), `"from":"project"`, `"from":"workspace"`, 1),
		strings.Replace(string(manifest), `go-embed:build`, `go-embed:unknown`, 1),
	} {
		if _, err := NegotiateManifest("go.json", []byte(bad)); err == nil {
			t.Fatalf("unsupported selector accepted: %s", bad)
		}
	}
}
