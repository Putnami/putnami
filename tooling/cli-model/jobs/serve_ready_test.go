package jobs

import "testing"

// TestIsServeReadyEvent pins the readiness claim a serve acts on: a v2 ready
// event whose payload names a server or a workload, and nothing else.
func TestIsServeReadyEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"server", `{"v":2,"type":"ready","data":{"target":"server"}}`, true},
		{"workload", `{"v":2,"type":"ready","data":{"target":"workload"}}`, true},
		{"another type", `{"v":2,"type":"log","level":"info","message":"up","data":{"target":"server"}}`, false},
		{"no payload", `{"v":2,"type":"ready"}`, false},
		{"unknown target", `{"v":2,"type":"ready","data":{"target":"database"}}`, false},
	} {
		event, ok := ParseRawEvent(tc.line)
		if !ok {
			t.Fatalf("%s: ParseRawEvent refused %s", tc.name, tc.line)
		}
		if got := IsServeReadyEvent(event); got != tc.want {
			t.Errorf("%s: IsServeReadyEvent = %v, want %v", tc.name, got, tc.want)
		}
	}
	v1, _ := ParseRawEvent(`{"v":2,"type":"ready","data":{"target":"server"}}`)
	v1.Version = 1
	if IsServeReadyEvent(v1) {
		t.Error("a ready event stamped v1 is a readiness claim")
	}
}
