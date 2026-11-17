package collaboration

import "strings"

// MissionKeyTitle is the title a mission key gives, the one a host names
// before it knows a better one: "Issue #<n>" for "issue-<n>" with n a positive
// integer, the branch for "branch:<branch>", else the key itself.
func MissionKeyTitle(key string) string {
	if number, ok := strings.CutPrefix(key, "issue-"); ok && positiveDecimal(number) {
		return "Issue #" + number
	}
	if branch, ok := strings.CutPrefix(key, "branch:"); ok {
		return branch
	}
	return key
}

func positiveDecimal(value string) bool {
	if value == "" || value[0] == '0' {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// SyncedMissionTitle is the title a mission holds after a sync or an ask that
// names title, stored being its title before, "" for a mission the write
// creates. The named title applies to a mission without a title and to one
// still titled by its key, MissionKeyTitle; any other title stays, so a
// host's key title never replaces the title a checkpoint or a session named.
func SyncedMissionTitle(stored, key, title string) string {
	if title != "" && (stored == "" || stored == MissionKeyTitle(key)) {
		return title
	}
	return stored
}
