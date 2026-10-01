package runtime

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ParseEvent decodes a single JSON line into an Event. It parses leniently —
// the runtime event envelope is intentionally additive, so unknown top-level
// fields are preserved as extras rather than rejected. Use ParseAndValidateEvent
// for the canonical parse→validate flow that mirrors the sibling protocols.
func ParseEvent(data []byte) (*Event, error) {
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// ParseAndValidateEvent decodes a single JSON line into an Event and validates
// its structural invariants, returning diagnostics uniformly (like the sibling
// protocols' ParseAndValidate* helpers) rather than a bare error. A decode
// failure yields a single parse-error diagnostic and a nil event; otherwise the
// event is returned alongside any validation diagnostics.
func ParseAndValidateEvent(data []byte) (*Event, []diag.Diagnostic) {
	e, err := ParseEvent(data)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf("parse-error", "", "invalid event JSON: %v", err)}
	}
	return e, ValidateEvent(e)
}

// Decoder reads JSONL event streams line by line.
type Decoder struct {
	scanner *bufio.Scanner
}

// NewDecoder creates a Decoder that reads events from r.
func NewDecoder(r io.Reader) *Decoder {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024) // up to 1MB lines
	return &Decoder{scanner: s}
}

// Next reads the next event from the stream. Returns nil, io.EOF at end.
func (d *Decoder) Next() (*Event, error) {
	if !d.scanner.Scan() {
		if err := d.scanner.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	line := d.scanner.Bytes()
	if len(line) == 0 {
		return d.Next() // skip blank lines
	}
	return ParseEvent(line)
}

// DecodeAll reads all events from r until EOF.
func DecodeAll(r io.Reader) ([]*Event, error) {
	dec := NewDecoder(r)
	var events []*Event
	for {
		e, err := dec.Next()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, e)
	}
}
