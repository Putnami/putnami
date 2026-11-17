package collaboration

import "testing"

func TestMissionKeyTitle(t *testing.T) {
	for key, want := range map[string]string{
		"issue-5":       "Issue #5",
		"issue-0":       "issue-0",
		"issue-12a":     "issue-12a",
		"issue-":        "issue-",
		"branch:feat/x": "feat/x",
		"branch:":       "",
		"pt-1":          "pt-1",
	} {
		if got := MissionKeyTitle(key); got != want {
			t.Errorf("MissionKeyTitle(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestSyncedMissionTitle(t *testing.T) {
	for _, tc := range []struct {
		name, stored, key, title, want string
	}{
		{"a new mission", "", "branch:feat/x", "feat/x", "feat/x"},
		{"a new mission without a title", "", "branch:feat/x", "", ""},
		{"a mission titled by its key", "feat/x", "branch:feat/x", "Page title", "Page title"},
		{"an issue mission titled by its key", "Issue #5", "issue-5", "Track sessions", "Track sessions"},
		{"a named mission and its key title", "Page title", "branch:feat/x", "feat/x", "Page title"},
		{"a named mission and another name", "Page title", "branch:feat/x", "Other", "Page title"},
		{"no title", "feat/x", "branch:feat/x", "", "feat/x"},
	} {
		if got := SyncedMissionTitle(tc.stored, tc.key, tc.title); got != tc.want {
			t.Errorf("%s: SyncedMissionTitle = %q, want %q", tc.name, got, tc.want)
		}
	}
}
