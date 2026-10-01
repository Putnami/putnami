package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSessionFilePlacementRoundTrip(t *testing.T) {
	for _, placement := range []SessionPlacement{
		{Requested: "local", Actual: "local"},
		{Requested: "remote", Actual: "remote"},
		{Requested: "remote", Actual: "local"},
	} {
		t.Run(placement.Requested+"-"+placement.Actual, func(t *testing.T) {
			want := sessionFileWithTree()
			want.Placement = &placement
			data, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			var got SessionFile
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip lost session metadata: got %+v, want %+v", got, want)
			}
			if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
				t.Fatalf("ValidateDocument = %v, want none", violations)
			}
		})
	}
}

// Provenance is the executing engine's statement of the bound request it ran:
// every member present when the block is, absent as a whole otherwise, and
// spelled exactly the way the runner contract spells its digests.
func TestSessionPlacementProvenanceIsCompleteOrAbsent(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	provenance := SessionProvenance{SourceDigest: digest, InputDigest: "sha256:" + strings.Repeat("cd", 32), Submission: strings.Repeat("ef", 16)}
	want := sessionFileWithTree()
	want.Placement = &SessionPlacement{Requested: "remote", Actual: "remote", Provenance: &provenance}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got SessionFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip lost provenance: got %+v, want %+v", got.Placement, want.Placement)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("ValidateDocument = %v, want none", violations)
	}
	local := sessionFileWithTree()
	local.Placement = &SessionPlacement{Requested: "remote", Actual: "local"}
	data, _ = json.Marshal(local)
	if strings.Contains(string(data), "provenance") {
		t.Fatalf("unknown provenance must be omitted: %s", data)
	}
	for name, placement := range map[string]string{
		"null provenance":     `{"requested":"remote","actual":"remote","provenance":null}`,
		"partial provenance":  `{"requested":"remote","actual":"remote","provenance":{"sourceDigest":"` + digest + `","inputDigest":"` + digest + `"}}`,
		"bare hex digest":     `{"requested":"remote","actual":"remote","provenance":{"sourceDigest":"` + strings.Repeat("ab", 32) + `","inputDigest":"` + digest + `","submission":"` + strings.Repeat("ef", 16) + `"}}`,
		"uppercase key":       `{"requested":"remote","actual":"remote","provenance":{"sourceDigest":"` + digest + `","inputDigest":"` + digest + `","submission":"` + strings.Repeat("EF", 16) + `"}}`,
		"transport metadata":  `{"requested":"remote","actual":"remote","provenance":{"sourceDigest":"` + digest + `","inputDigest":"` + digest + `","submission":"` + strings.Repeat("ef", 16) + `","attempt":"a-1"}}`,
		"provenance on local": `{"requested":"remote","actual":"local","provenance":{"sourceDigest":"` + digest + `","inputDigest":"` + digest + `","submission":"` + strings.Repeat("ef", 16) + `"}}`,
	} {
		document := strings.Replace(string(data), `"placement":{"requested":"remote","actual":"local"}`, `"placement":`+placement, 1)
		if !strings.Contains(document, placement) {
			t.Fatalf("%s: fixture substitution failed", name)
		}
		if violations := ValidateDocument(DocumentSessionFile, []byte(document)); len(violations) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSessionFilePlacementIsOptional(t *testing.T) {
	data, err := json.Marshal(sessionFileWithTree())
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if _, exists := document["placement"]; exists {
		t.Fatalf("unknown placement must be omitted: %s", data)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("older session no longer validates: %v", violations)
	}
}

func TestSessionPlacementEnumsMatchSchema(t *testing.T) {
	definition := loadResultV2Schema(t).Defs["sessionPlacement"]
	for _, member := range []string{"requested", "actual"} {
		var property struct {
			Enum []string `json:"enum"`
		}
		if err := json.Unmarshal(definition.Properties[member], &property); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(property.Enum, []string{"local", "remote"}) {
			t.Errorf("schema placement.%s enum = %v, want local, remote", member, property.Enum)
		}
	}
}
