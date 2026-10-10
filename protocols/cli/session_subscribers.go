package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
)

// Session subscriber evidence: the v1 document the engine writes beside a
// session.json once its event-stream subscribers have finished.
const (
	// SessionSubscribersFileName is the evidence document's name inside the
	// session directory, beside session.json and events.jsonl.
	SessionSubscribersFileName = "subscribers.json"
	// SessionSubscribersVersion is the evidence contract version.
	SessionSubscribersVersion = 1
	// SessionSubscribersSchemaID is the published schema's $id.
	SessionSubscribersSchemaID = "https://putnami.dev/schemas/putnami-session-subscribers.json"
	// SessionSubscribersMaxBytes bounds the document a reader accepts.
	SessionSubscribersMaxBytes = 64 * 1024
	// SessionSubscribersMax bounds how many subscribers one document names.
	SessionSubscribersMax = 32
)

// Subscriber evidence vocabulary. It is closed: a reader never has to guess
// what a fourth value would mean.
const (
	// SubscriberEvidenceDelivered: the subscriber acknowledged the stream's
	// terminal final marker, so every record reached it.
	SubscriberEvidenceDelivered = "delivered"
	// SubscriberEvidencePartial: the subscriber acknowledged some bytes but never
	// the final marker.
	SubscriberEvidencePartial = "partial"
	// SubscriberEvidenceLost: the subscriber acknowledged nothing.
	SubscriberEvidenceLost = "lost"
)

// SessionSubscribersFile is subscribers.json. It never changes session.json or
// events.jsonl: it states, per declared subscriber, how much of the session's
// event stream that subscriber acknowledged.
type SessionSubscribersFile struct {
	// ProtocolVersion identifies the evidence contract; currently 1.
	ProtocolVersion int `json:"protocolVersion"`
	// SessionID is the recorded session the evidence belongs to.
	SessionID string `json:"sessionId"`
	// Stream is the final extent of the session's events.jsonl.
	Stream SessionStreamPosition `json:"stream"`
	// Subscribers holds one entry per declared subscriber, sorted by name.
	Subscribers []SessionSubscriberEvidence `json:"subscribers"`
}

// SessionStreamPosition is a position in events.jsonl derived from the log
// itself: no record carries a sequence member.
type SessionStreamPosition struct {
	// Offset is a zero-based byte offset in events.jsonl.
	Offset int64 `json:"offset"`
	// Records counts the LF-terminated records that end at or before Offset.
	Records int64 `json:"records"`
}

// SessionSubscriberEvidence is one subscriber's delivery evidence.
type SessionSubscriberEvidence struct {
	// Name is the subscriber's declared name, for example "session-reporter".
	Name string `json:"name"`
	// Evidence is delivered, partial or lost.
	Evidence string `json:"evidence"`
	// Acknowledged is the last stream position the subscriber acknowledged.
	Acknowledged SessionStreamPosition `json:"acknowledged"`
	// Lost counts the records after Acknowledged that the subscriber never
	// acknowledged within its budget: stream.records minus acknowledged.records.
	Lost int64 `json:"lost"`
}

var subscriberName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// NewSessionSubscriberEvidence classifies one subscriber from the final stream
// extent, its last acknowledged position, and whether it acknowledged the
// terminal final marker.
func NewSessionSubscriberEvidence(name string, stream, acknowledged SessionStreamPosition, final bool) SessionSubscriberEvidence {
	evidence := SubscriberEvidenceLost
	switch {
	case final && acknowledged == stream:
		evidence = SubscriberEvidenceDelivered
	case acknowledged.Offset > 0:
		evidence = SubscriberEvidencePartial
	}
	return SessionSubscriberEvidence{Name: name, Evidence: evidence, Acknowledged: acknowledged, Lost: stream.Records - acknowledged.Records}
}

// Validate checks every rule a reader relies on, including the ones a JSON
// schema cannot express: the lost count, the evidence/position agreement and
// the name order.
func (f SessionSubscribersFile) Validate() error {
	if f.ProtocolVersion != SessionSubscribersVersion || !reportingSessionID.MatchString(f.SessionID) {
		return fmt.Errorf("invalid subscriber evidence identity")
	}
	if !f.Stream.valid() {
		return fmt.Errorf("invalid subscriber evidence stream position")
	}
	if len(f.Subscribers) == 0 || len(f.Subscribers) > SessionSubscribersMax {
		return fmt.Errorf("invalid subscriber evidence count")
	}
	for i, s := range f.Subscribers {
		if !subscriberName.MatchString(s.Name) || i > 0 && f.Subscribers[i-1].Name >= s.Name {
			return fmt.Errorf("subscriber names must be valid, unique and sorted")
		}
		if err := s.validate(f.Stream); err != nil {
			return fmt.Errorf("subscriber %s: %w", s.Name, err)
		}
	}
	return nil
}

func (p SessionStreamPosition) valid() bool {
	return p.Offset >= 0 && p.Offset <= maxSafeInteger && p.Records >= 0 && p.Records <= p.Offset
}

func (s SessionSubscriberEvidence) validate(stream SessionStreamPosition) error {
	ack := s.Acknowledged
	if !ack.valid() || ack.Offset > stream.Offset || ack.Records > stream.Records {
		return fmt.Errorf("acknowledged position outside the stream")
	}
	if s.Lost != stream.Records-ack.Records {
		return fmt.Errorf("lost count disagrees with the acknowledged position")
	}
	switch s.Evidence {
	case SubscriberEvidenceDelivered:
		if ack != stream {
			return fmt.Errorf("delivered evidence must acknowledge the whole stream")
		}
	case SubscriberEvidencePartial:
		if ack.Offset == 0 {
			return fmt.Errorf("partial evidence must acknowledge some bytes")
		}
	case SubscriberEvidenceLost:
		if ack.Offset != 0 {
			return fmt.Errorf("lost evidence cannot acknowledge bytes")
		}
	default:
		return fmt.Errorf("unknown evidence")
	}
	return nil
}

// ParseSessionSubscribersFile strictly decodes and validates one bounded
// document: every member is required, none may be null, unknown members and
// trailing JSON are refused.
func ParseSessionSubscribersFile(data []byte) (*SessionSubscribersFile, error) {
	if len(data) > SessionSubscribersMaxBytes {
		return nil, fmt.Errorf("subscriber evidence exceeds limit")
	}
	if err := requireEvidenceMembers(data, "protocolVersion", "sessionId", "stream", "subscribers"); err != nil {
		return nil, err
	}
	var fields struct {
		Stream      json.RawMessage   `json:"stream"`
		Subscribers []json.RawMessage `json:"subscribers"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("invalid subscriber evidence JSON")
	}
	if err := requireEvidenceMembers(fields.Stream, "offset", "records"); err != nil {
		return nil, err
	}
	for _, raw := range fields.Subscribers {
		if err := requireEvidenceMembers(raw, "name", "evidence", "acknowledged", "lost"); err != nil {
			return nil, err
		}
		var entry struct {
			Acknowledged json.RawMessage `json:"acknowledged"`
		}
		_ = json.Unmarshal(raw, &entry)
		if err := requireEvidenceMembers(entry.Acknowledged, "offset", "records"); err != nil {
			return nil, err
		}
	}
	var file SessionSubscribersFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("invalid subscriber evidence fields")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing subscriber evidence JSON")
	}
	if err := file.Validate(); err != nil {
		return nil, err
	}
	return &file, nil
}

// requireEvidenceMembers refuses a value that is not an object, omits a required
// member, or sets any member to null.
func requireEvidenceMembers(data []byte, required ...string) error {
	switch fault, name := findMemberFault(data, required...); fault {
	case memberFaultNotObject:
		return fmt.Errorf("invalid subscriber evidence JSON")
	case memberFaultNull:
		return fmt.Errorf("null subscriber evidence member")
	case memberFaultMissing:
		return fmt.Errorf("missing subscriber evidence member %s", name)
	}
	return nil
}

// memberFault is what findMemberFault finds wrong with a value that must be a
// JSON object.
type memberFault int

const (
	// memberFaultNone: the value is an object with every required member and
	// no null member.
	memberFaultNone memberFault = iota
	// memberFaultNotObject: the value is not a JSON object.
	memberFaultNotObject
	// memberFaultNull: a member is null.
	memberFaultNull
	// memberFaultMissing: a required member is absent.
	memberFaultMissing
)

// findMemberFault finds the first fault of a value that must be a JSON object
// holding every required member and no null member, which a struct decode
// cannot tell from an absent or zero one. It checks in that order: the object,
// then a null member, the first in name order, then a missing member, the
// first in required order. It returns the member the fault names.
func findMemberFault(data []byte, required ...string) (memberFault, string) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil {
		return memberFaultNotObject, ""
	}
	for _, name := range slices.Sorted(maps.Keys(members)) {
		if bytes.Equal(bytes.TrimSpace(members[name]), []byte("null")) {
			return memberFaultNull, name
		}
	}
	for _, name := range required {
		if _, ok := members[name]; !ok {
			return memberFaultMissing, name
		}
	}
	return memberFaultNone, ""
}
