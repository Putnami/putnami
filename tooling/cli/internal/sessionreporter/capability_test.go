package sessionreporter

import (
	"reflect"
	"testing"
	"time"
)

// TestCapabilitiesAreIndependentProtocolSubscribers pins the capability table:
// the protocol-owned names, the artifacts each transmits, its batching, and a
// checkpoint and lock of its own. workspace_state's reportingCheckpoints lists
// the same file names for session retention.
func TestCapabilitiesAreIndependentProtocolSubscribers(t *testing.T) {
	type row struct {
		name, label, activity, selector, token string
		artifacts                              []string
		interval                               time.Duration
		state, lock                            string
	}
	want := []row{
		{"session-reporter", "session reporter", "session reporting", "PUTNAMI_SESSION_REPORTER", "PUTNAMI_SESSION_REPORTER_TOKEN", []string{"plan.json", "session.json", "events.jsonl"}, 10 * time.Second, "reporting.json", "reporting.lock"},
		{"log-reporter", "log reporter", "log reporting", "PUTNAMI_LOG_REPORTER", "PUTNAMI_LOG_REPORTER_TOKEN", []string{"events.jsonl"}, 2 * time.Second, "log-reporting.json", "log-reporting.lock"},
	}
	var got []row
	for _, c := range Capabilities() {
		got = append(got, row{c.Name, c.Label, c.Activity, c.SelectorEnv, c.TokenEnv, c.Artifacts, c.BatchInterval, c.StateFile, c.LockFile})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities = %+v\nwant %+v", got, want)
	}
	if !SessionReporter.sends(sessionFile) || LogReporter.sends(sessionFile) || !LogReporter.sends("events.jsonl") {
		t.Fatal("only the session reporter transmits session.json; both transmit events.jsonl")
	}
	if !SessionReporter.sends(planFile) || LogReporter.sends(planFile) {
		t.Fatal("only the session reporter transmits plan.json")
	}
}
