package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The subscriber evidence document has three representations that must move
// together: these Go types, schemas/session-subscribers.json, and the
// TypeScript twin in @putnami/cli-protocol. The corpus below is executed by
// both runtimes, and both check their member sets against the schema.

func TestSessionSubscribersSharedCorpus(t *testing.T) {
	data, err := os.ReadFile("conformance/session-subscribers.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus []struct {
		Name     string          `json:"name"`
		Valid    bool            `json:"valid"`
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	valid, invalid := 0, 0
	for _, c := range corpus {
		t.Run(c.Name, func(t *testing.T) {
			_, err := ParseSessionSubscribersFile(c.Document)
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v error=%v", c.Valid, err)
			}
		})
		if c.Valid {
			valid++
		} else {
			invalid++
		}
	}
	if valid == 0 || invalid == 0 {
		t.Fatalf("corpus must pin both verdicts: %d valid, %d invalid", valid, invalid)
	}
}

func TestSessionSubscribersSchemaDrift(t *testing.T) {
	data, err := os.ReadFile("schemas/session-subscribers.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		ID   string `json:"$id"`
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.ID != SessionSubscribersSchemaID {
		t.Fatalf("schema $id = %q, constant = %q", schema.ID, SessionSubscribersSchemaID)
	}
	for name, value := range map[string]any{
		"document": SessionSubscribersFile{}, "position": SessionStreamPosition{}, "evidence": SessionSubscriberEvidence{},
	} {
		def := schema.Defs[name]
		want := jsonFieldNames(t, value)
		if got := keys(def.Properties); !reflect.DeepEqual(got, want) {
			t.Errorf("%s members: schema %v, Go %v", name, got, want)
		}
		required := append([]string(nil), def.Required...)
		if got := sortedStrings(required); !reflect.DeepEqual(got, want) {
			t.Errorf("%s required: schema %v, Go %v (every member is required)", name, got, want)
		}
	}
	var bounds struct {
		Defs struct {
			Document struct {
				Properties struct {
					Subscribers struct {
						MaxItems int `json:"maxItems"`
					} `json:"subscribers"`
					ProtocolVersion struct {
						Const int `json:"const"`
					} `json:"protocolVersion"`
				} `json:"properties"`
			} `json:"document"`
			Evidence struct {
				Properties struct {
					Evidence struct {
						Enum []string `json:"enum"`
					} `json:"evidence"`
				} `json:"properties"`
			} `json:"evidence"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &bounds); err != nil {
		t.Fatal(err)
	}
	if got := bounds.Defs.Document.Properties.Subscribers.MaxItems; got != SessionSubscribersMax {
		t.Errorf("schema maxItems = %d, SessionSubscribersMax = %d", got, SessionSubscribersMax)
	}
	if got := bounds.Defs.Document.Properties.ProtocolVersion.Const; got != SessionSubscribersVersion {
		t.Errorf("schema protocolVersion const = %d, SessionSubscribersVersion = %d", got, SessionSubscribersVersion)
	}
	vocabulary := []string{SubscriberEvidenceDelivered, SubscriberEvidencePartial, SubscriberEvidenceLost}
	if got := bounds.Defs.Evidence.Properties.Evidence.Enum; !reflect.DeepEqual(got, vocabulary) {
		t.Errorf("schema evidence enum = %v, Go vocabulary = %v", got, vocabulary)
	}
}

// TestNewSessionSubscriberEvidenceClassifiesAndValidates pins the one
// classification every producer uses, and proves its output always validates.
func TestNewSessionSubscriberEvidenceClassifiesAndValidates(t *testing.T) {
	stream := SessionStreamPosition{Offset: 900, Records: 3}
	for _, c := range []struct {
		name  string
		ack   SessionStreamPosition
		final bool
		want  string
		lost  int64
	}{
		{"final marker acknowledged", stream, true, SubscriberEvidenceDelivered, 0},
		{"every byte without the final marker", stream, false, SubscriberEvidencePartial, 0},
		{"killed mid-record", SessionStreamPosition{Offset: 450, Records: 1}, false, SubscriberEvidencePartial, 2},
		{"never acknowledged", SessionStreamPosition{}, false, SubscriberEvidenceLost, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := NewSessionSubscriberEvidence("session-reporter", stream, c.ack, c.final)
			if got.Evidence != c.want || got.Lost != c.lost {
				t.Fatalf("evidence = %+v, want %s with %d lost", got, c.want, c.lost)
			}
			file := SessionSubscribersFile{ProtocolVersion: SessionSubscribersVersion, SessionID: "20260917-120000-abc123", Stream: stream, Subscribers: []SessionSubscriberEvidence{got}}
			wire, err := json.Marshal(file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseSessionSubscribersFile(wire); err != nil {
				t.Fatalf("producer output refused: %v", err)
			}
		})
	}
	oversized := bytes.Repeat([]byte(" "), SessionSubscribersMaxBytes+1)
	if _, err := ParseSessionSubscribersFile(oversized); err == nil {
		t.Fatal("oversized document accepted")
	}
}
