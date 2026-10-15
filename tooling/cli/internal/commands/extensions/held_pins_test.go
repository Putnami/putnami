package extensions

import "testing"

func TestHoldPin(t *testing.T) {
	tests := []struct {
		name, request, pinned, served string
		want                          *heldPin
	}{
		{"an older build on latest", "latest", lockedCanary, olderBuild, &heldPin{channel: "latest", served: olderBuild, older: true}},
		{"an older build on an empty constraint", "", "1.2.0", "1.1.0", &heldPin{channel: "latest", served: "1.1.0", older: true}},
		{"an older build on *", "*", "1.2.0", "1.1.0", &heldPin{channel: "latest", served: "1.1.0", older: true}},
		{"an older build on a channel name", "canary", lockedCanary, olderBuild, &heldPin{channel: "canary", served: olderBuild, older: true}},
		{"a build that does not compare", "latest", "1.0.0", "nightly", &heldPin{channel: "latest", served: "nightly"}},
		{"a pin that does not compare", "latest", "nightly", "1.0.0", &heldPin{channel: "latest", served: "1.0.0"}},
		{"an answer without a version", "latest", "1.0.0", "0.0.0", &heldPin{channel: "latest", served: "0.0.0"}},
		{"a newer build", "latest", lockedCanary, newerBuild, nil},
		{"the same build", "latest", lockedCanary, lockedCanary, nil},
		{"no pin", "latest", "", olderBuild, nil},
		{"a 0.0.0 pin", "latest", "0.0.0", olderBuild, nil},
		{"an exact version", olderBuild, lockedCanary, olderBuild, nil},
		{"a range", "^1.0.0", "1.2.0", "1.1.0", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := holdPin(tc.request, tc.pinned, tc.served)
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Errorf("holdPin(%q, %q, %q) = %+v, want %+v", tc.request, tc.pinned, tc.served, got, tc.want)
			}
		})
	}
}

func TestMoveMark(t *testing.T) {
	tests := []struct{ old, next, want string }{
		{lockedCanary, newerBuild, "↑"},
		{lockedCanary, olderBuild, "↓"},
		{"1.0.0", "v1.0.0", "→"},
		{"1.0.0", "nightly", "→"},
	}
	for _, tc := range tests {
		if got := moveMark(tc.old, tc.next); got != tc.want {
			t.Errorf("moveMark(%q, %q) = %s, want %s", tc.old, tc.next, got, tc.want)
		}
	}
}
