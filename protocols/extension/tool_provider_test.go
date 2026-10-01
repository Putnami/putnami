package extension

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolCallRequestProviderMemberIsAdditive pins that a tool which is not a
// collaboration provider operation sees the request it always saw, and that a
// routed call carries the operation and the binding settings verbatim.
func TestToolCallRequestProviderMemberIsAdditive(t *testing.T) {
	plain, err := json.Marshal(ToolCallRequest{Name: "acme.search", WorkspaceRoot: "/w", ExtensionRoot: "/e"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "provider") {
		t.Fatalf("a plain tool call carries a provider member: %s", plain)
	}

	routed := ToolCallRequest{
		Name:          "acme.tasks.find",
		Arguments:     json.RawMessage(`{"page":{"size":20}}`),
		WorkspaceRoot: "/w",
		ExtensionRoot: "/e",
		Provider: &ToolProviderCall{
			Contract: "tasks", Version: 1, Operation: "find",
			Settings: json.RawMessage(`{"repository":"acme/app"}`),
		},
	}
	encoded, err := json.Marshal(routed)
	if err != nil {
		t.Fatal(err)
	}
	want := `"provider":{"contract":"tasks","version":1,"operation":"find","settings":{"repository":"acme/app"}}`
	if !strings.Contains(string(encoded), want) {
		t.Fatalf("encoded %s, want %s", encoded, want)
	}
	var decoded ToolCallRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Provider == nil || decoded.Provider.Contract != "tasks" || decoded.Provider.Version != 1 ||
		decoded.Provider.Operation != "find" || string(decoded.Provider.Settings) != `{"repository":"acme/app"}` {
		t.Fatalf("decoded %+v", decoded.Provider)
	}
	withoutSettings, _ := json.Marshal(ToolProviderCall{Contract: "memory", Version: 1, Operation: "context"})
	if strings.Contains(string(withoutSettings), "settings") {
		t.Errorf("absent settings are omitted: %s", withoutSettings)
	}
}
