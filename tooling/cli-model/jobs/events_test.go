package jobs

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestReadJSONLEventsBoundsDetailWithoutChangingObservationOrHandshake(t *testing.T) {
	var stream strings.Builder
	for i := 0; i < 900; i++ {
		fmt.Fprintf(&stream, "{\"v\":2,\"type\":\"log\",\"level\":\"info\",\"message\":\"ordinary-%d\"}\n", i)
	}
	stream.WriteString("{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"FAILED\"}}\n")
	// The runtime handshake is execution-control evidence, so it must survive
	// independently even when the ordinary retention partition is already full.
	stream.WriteString("{\"v\":2,\"type\":\"meta\",\"message\":\"runtime reached\"}\n")

	forwarded := 0
	observed := 0
	events, result := ReadJSONLEventsObserved(
		strings.NewReader(stream.String()),
		func(RawJobEvent) { forwarded++ },
		nil,
		func() { observed++ },
	)

	if forwarded != 902 || observed != 902 {
		t.Fatalf("callbacks = (%d forwarded, %d observed), want every one of 902 valid events", forwarded, observed)
	}
	if len(events) >= forwarded {
		t.Fatalf("retained %d of %d events, want ordinary detail bounded", len(events), forwarded)
	}
	if result == nil || result.Status != "failed" || !result.EmittedMeta {
		t.Fatalf("result = %#v, want failed verdict with independent meta handshake", result)
	}
	foundFailedResult := false
	for _, event := range events {
		if event.Type == EventTypeResult && event.Data["status"] == "FAILED" {
			foundFailedResult = true
		}
	}
	if !foundFailedResult {
		t.Fatal("failure-priority result was crowded out by ordinary detail")
	}
}

func TestBoundedJobEventsKeepsFailureReserveAfterOrdinaryPartitionIsFull(t *testing.T) {
	ordinary, ok := ParseRawEvent(`{"v":2,"type":"log","level":"info","message":"ordinary"}`)
	if !ok {
		t.Fatal("ordinary fixture did not parse")
	}
	failure, ok := ParseRawEvent(`{"v":2,"type":"diagnostic","severity":"error","message":"late failure"}`)
	if !ok {
		t.Fatal("failure fixture did not parse")
	}

	input := make([]RawJobEvent, 900, 901)
	for i := range input {
		input[i] = ordinary
	}
	input = append(input, failure)
	retained := BoundedJobEvents(input)
	if len(retained) >= len(input) {
		t.Fatalf("retained %d events, want a strict cap below %d", len(retained), len(input))
	}
	if got := retained[len(retained)-1]; got.Type != EventTypeDiagnostic || got.Data["message"] != "late failure" {
		t.Fatalf("last retained event = %#v, want late failure from the reserved partition", got)
	}
}

// TestBoundedJobEventsKeepsArtifactsAfterOrdinaryPartitionIsFull pins the
// reserve that keeps a task's declared outputs readable. Artifact records are
// emitted late — after the whole test or build transcript — so on a chatty task
// they are exactly what an exhausted ordinary partition drops. Every consumer
// that resolves them (the spec gate joining verification reports, publish
// resolving images, coverage resolving profiles) reads this projection through
// TaskResultOf, and each one fails silently when the record is gone: the gate
// reports a mapped requirement as `missing` with no diagnostic at all.
func TestBoundedJobEventsKeepsArtifactsAfterOrdinaryPartitionIsFull(t *testing.T) {
	ordinary, ok := ParseRawEvent(`{"v":2,"type":"log","level":"info","message":"ordinary"}`)
	if !ok {
		t.Fatal("ordinary fixture did not parse")
	}
	artifact, ok := ParseRawEvent(
		`{"v":2,"type":"artifact","data":{"id":"putnami-feature-verification","kind":"report",` +
			`"path":".putnami/out/p/test/putnami-feature-verification.json"}}`)
	if !ok {
		t.Fatal("artifact fixture did not parse")
	}

	input := make([]RawJobEvent, 900, 901)
	for i := range input {
		input[i] = ordinary
	}
	input = append(input, artifact)

	retained := BoundedJobEvents(input)
	if len(retained) >= len(input) {
		t.Fatalf("retained %d events, want a strict cap below %d", len(retained), len(input))
	}

	_, artifacts, _, _ := RecordsFromEvents(retained)
	if len(artifacts) != 1 {
		t.Fatalf("recovered %d artifacts from the bounded projection, want the late artifact to survive", len(artifacts))
	}
	if artifacts[0].ID != "putnami-feature-verification" {
		t.Fatalf("artifact ID = %q, want the declared output preserved", artifacts[0].ID)
	}
}

// TestBoundedJobEventsBoundsArtifactRecordsToo asserts the artifact reserve is
// a reserve and not an exemption: a task that emits artifact records without
// end is still capped, so retention stays bounded in every direction.
func TestBoundedJobEventsBoundsArtifactRecordsToo(t *testing.T) {
	artifact, ok := ParseRawEvent(
		`{"v":2,"type":"artifact","data":{"id":"a","kind":"report","path":".putnami/out/p/test/a.json"}}`)
	if !ok {
		t.Fatal("artifact fixture did not parse")
	}

	input := make([]RawJobEvent, 5000)
	for i := range input {
		input[i] = artifact
	}

	retained := BoundedJobEvents(input)
	if len(retained) >= len(input) {
		t.Fatalf("retained %d artifact events, want the reserve to bound them below %d", len(retained), len(input))
	}
}

func TestReadJSONLEventsDiscardsAnOversizedLineAndKeepsReading(t *testing.T) {
	var stream strings.Builder
	stream.WriteString("{\"v\":2,\"type\":\"log\",\"level\":\"info\",\"message\":\"before\"}\n")
	stream.WriteString("{\"v\":2,\"type\":\"log\",\"message\":\"")
	stream.WriteString(strings.Repeat("x", maxJobEventBytes))
	stream.WriteString("\"}\n")
	stream.WriteString("{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}")

	var forwarded []RawJobEvent
	events, result := ReadJSONLEvents(strings.NewReader(stream.String()), func(event RawJobEvent) {
		forwarded = append(forwarded, event)
	}, nil)

	if len(forwarded) != 3 || forwarded[0].Message != "before" || forwarded[2].Type != EventTypeResult {
		t.Fatalf("forwarded %d events, want before, the discard notice and the result", len(forwarded))
	}
	if notice := forwarded[1]; notice.Type != EventTypeLog || notice.Level != "error" ||
		!strings.Contains(notice.Message, "discarded an event line") {
		t.Fatalf("notice = %+v, want an error log naming the discarded line", notice)
	}
	if result == nil || result.Status != string(TaskStatusSuccess) {
		t.Fatalf("result = %+v, want the result read after the discarded line", result)
	}
	if len(events) != 3 {
		t.Fatalf("retained %d events, want 3", len(events))
	}
}

func TestReadJSONLEventsFailsATaskWhoseResultWasDiscarded(t *testing.T) {
	line := "{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\",\"data\":{\"blob\":\"" +
		strings.Repeat("x", maxJobEventBytes) + "\"}}}\n"

	_, result := ReadJSONLEvents(strings.NewReader(line), nil, nil)

	if result == nil || result.Status != string(TaskStatusFailed) || result.Error == nil ||
		!strings.Contains(result.Error.Message, "no result event") {
		t.Fatalf("result = %+v, want a failure that names the discarded line", result)
	}
}

func TestReadEventLineKeepsALineAtTheLimit(t *testing.T) {
	atLimit := strings.Repeat("y", maxJobEventBytes)
	reader := bufio.NewReaderSize(strings.NewReader(atLimit+"\n"+atLimit+"z"), 64*1024)

	line, oversized, err := readEventLine(reader)
	if err != nil || oversized || len(line) != maxJobEventBytes {
		t.Fatalf("first line: %d bytes, oversized %v, err %v; want the whole line kept", len(line), oversized, err)
	}
	line, oversized, err = readEventLine(reader)
	if err != nil || !oversized || len(line) != 0 {
		t.Fatalf("second line: %d bytes, oversized %v, err %v; want it discarded", len(line), oversized, err)
	}
	if _, _, err := readEventLine(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF after the last line", err)
	}
}
