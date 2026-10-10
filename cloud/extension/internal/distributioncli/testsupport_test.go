package distributioncli

import (
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

func resultPayloadMap(t *testing.T, got any) map[string]any {
	t.Helper()
	if result, ok := got.(protocolcli.ResultV2); ok {
		got = result.Data
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("result payload = %T %v, want map[string]any", got, got)
	}
	return m
}
