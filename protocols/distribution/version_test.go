package distribution

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSchemasPinProtocolVersionAndVocabularies(t *testing.T) {
	schemas := map[string]string{
		"schemas/release-set.json":     ReleaseSetSchemaURL,
		"schemas/provider.json":        ProviderSchemaURL,
		"schemas/publish-outcome.json": PublishOutcomeSchemaURL,
	}
	for filename, schemaURL := range schemas {
		schema := readSchemaObject(t, filename)
		assertEveryProtocolVersionConst(t, filename, schema)
		if schema["$id"] != schemaURL {
			t.Errorf("%s $id = %v, want %s", filename, schema["$id"], schemaURL)
		}
	}

	// The release set is generic: the ecosystem is a pattern, never an enum.
	definitions := readSchemaObject(t, "schemas/release-set.json")["$defs"].(map[string]any)
	ecosystem := definitions["ecosystem"].(map[string]any)
	if ecosystem["pattern"] != EcosystemPattern {
		t.Fatalf("schema ecosystem pattern = %v, want %s", ecosystem["pattern"], EcosystemPattern)
	}
	if _, enumerated := ecosystem["enum"]; enumerated {
		t.Fatalf("the schema still enumerates ecosystems: %v", ecosystem["enum"])
	}
	wantRequired := []string{"ecosystem", "coordinate", "version", "artifactDigest", "dependencies", "sourceRevision", "selectionFingerprint"}
	required := definitions["member"].(map[string]any)["required"].([]any)
	if len(required) != len(wantRequired) {
		t.Fatalf("member required fields drifted: %v", required)
	}
	for index, name := range wantRequired {
		if required[index] != name {
			t.Fatalf("member required field %d = %v, want %s (struct order)", index, required[index], name)
		}
	}

	// Attribution is optional in both halves: absent from the required list
	// above, and closed where it is present.
	kinds := definitions["kind"].(map[string]any)["enum"].([]any)
	if len(kinds) != len(MemberKinds) {
		t.Fatalf("schema kind vocabulary drifted: %v", kinds)
	}
	for index, kind := range MemberKinds {
		if kinds[index] != string(kind) {
			t.Fatalf("schema kind %d = %v, want %s", index, kinds[index], kind)
		}
	}
	project := definitions["project"].(map[string]any)
	if project["pattern"] != MemberProjectPattern || int(project["maxLength"].(float64)) != MaxProjectBytes {
		t.Fatalf("schema project rules drifted: %v", project)
	}
	memberProperties := definitions["member"].(map[string]any)["properties"].(map[string]any)
	for name, ref := range map[string]string{"project": "#/$defs/project", "kind": "#/$defs/kind"} {
		if memberProperties[name].(map[string]any)["$ref"] != ref {
			t.Fatalf("member %s is not bound to %s: %v", name, ref, memberProperties[name])
		}
	}

	provider := readSchemaObject(t, "schemas/provider.json")["$defs"].(map[string]any)
	for _, shape := range []string{"resolveRequest", "resolveResponse", "releaseRequest", "releaseResponse", "channelSetRequest", "channelStatusRequest", "channelStatusResponse"} {
		if _, defined := provider[shape]; !defined {
			t.Fatalf("provider schema does not define %s", shape)
		}
	}
	for _, removed := range []string{"putRequest", "putResponse", "advanceRequest", "advanceResponse", "projections"} {
		if _, defined := provider[removed]; defined {
			t.Fatalf("provider schema still defines %s", removed)
		}
	}
	outcomes := provider["releaseResponse"].(map[string]any)["properties"].(map[string]any)["outcome"].(map[string]any)["enum"].([]any)
	wantOutcomes := []ReleaseOutcome{ReleaseOutcomeReleased, ReleaseOutcomeAlreadyCurrent, ReleaseOutcomeConflict}
	if len(outcomes) != len(wantOutcomes) {
		t.Fatalf("schema release outcomes drifted: %v", outcomes)
	}
	for index, outcome := range wantOutcomes {
		if outcomes[index] != string(outcome) {
			t.Fatalf("schema release outcome %d = %v, want %s", index, outcomes[index], outcome)
		}
	}
	if provider["channel"].(map[string]any)["pattern"] != ChannelPattern {
		t.Fatalf("schema channel pattern drifted: %v", provider["channel"].(map[string]any)["pattern"])
	}
	if provider["ecosystem"].(map[string]any)["pattern"] != EcosystemPattern {
		t.Fatalf("schema ecosystem pattern drifted: %v", provider["ecosystem"].(map[string]any)["pattern"])
	}
	visibility := provider["visibility"].(map[string]any)["enum"].([]any)
	wantLevels := []Visibility{VisibilityInternal, VisibilityPrivate, VisibilityPublic}
	if len(visibility) != len(wantLevels) {
		t.Fatalf("schema visibility levels drifted: %v", visibility)
	}
	for index, level := range wantLevels {
		if visibility[index] != string(level) {
			t.Fatalf("schema visibility level %d = %v, want %s", index, visibility[index], level)
		}
	}
	// A channel with no head is a first-class answer, not a provider failure.
	head := provider["nullableChannelHead"].(map[string]any)["oneOf"].([]any)
	if len(head) != 2 || head[1].(map[string]any)["type"] != "null" {
		t.Fatalf("a channel head must admit null: %v", head)
	}
	if provider["headMap"].(map[string]any)["maxProperties"].(float64) != MaxChannelsPerRelease {
		t.Fatalf("schema channel bound drifted: %v", provider["headMap"])
	}
	mirrors := provider["mirrorTargets"].(map[string]any)
	if mirrors["maxProperties"].(float64) != MaxRegistryKinds || mirrors["propertyNames"].(map[string]any)["$ref"] != "#/$defs/ecosystem" {
		t.Fatalf("schema mirror map vocabulary or bound drifted: %v", mirrors)
	}
	target := mirrors["additionalProperties"].(map[string]any)["properties"].(map[string]any)["to"].(map[string]any)
	if target["pattern"] != MirrorTargetPattern || target["minLength"].(float64) != 1 || target["maxLength"].(float64) != MaxMirrorTargetBytes {
		t.Fatalf("schema mirror destination rules drifted: %v", target)
	}
	releaseProperties := provider["releaseRequest"].(map[string]any)["properties"].(map[string]any)
	if releaseProperties["mirrors"].(map[string]any)["$ref"] != "#/$defs/mirrorTargets" {
		t.Fatalf("release request is not bound to mirror intent: %v", releaseProperties["mirrors"])
	}
}

func readSchemaObject(t *testing.T, filename string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("%s is not JSON: %v", filename, err)
	}
	return schema
}

func assertEveryProtocolVersionConst(t *testing.T, filename string, value any) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		if property, ok := typed["protocolVersion"].(map[string]any); ok {
			if constant, ok := property["const"].(float64); !ok || int(constant) != ProtocolVersion {
				t.Errorf("%s protocolVersion const = %v, want %d", filename, property["const"], ProtocolVersion)
			}
		}
		for _, child := range typed {
			assertEveryProtocolVersionConst(t, filename, child)
		}
	case []any:
		for _, child := range typed {
			assertEveryProtocolVersionConst(t, filename, child)
		}
	}
}
