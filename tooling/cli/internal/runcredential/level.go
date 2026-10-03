package runcredential

import (
	"strconv"
	"strings"
)

// CustodyLevel is the credential custody this CLI implements. A hosted run
// hands the run credential only to a CLI at this level or above: a pin
// relaunch refuses a pinned CLI that advertises a lower level, or none.
//
// Level 2 is the first advertised level. A CLI at level 2 hands a holder only
// to a native executable, starts no registry-token child from a fetch, and
// refuses every command in a credentialed workspace-fetch job (HostedFetchEnv).
// A CLI that accepts Flag but advertises no level, such as an older build,
// is below it. Raise the level when a change to custody must not be
// undone by pinning an older CLI. A variable Capture removes from the
// environment needs no raise: Capture runs before the pin relaunch, so the
// pinned CLI never sees it. Handing the reporters the run credential over
// their protocol stays at level 2 for that reason: a pinned level 2 CLI finds
// no reporter token, and hands its reporters no credential.
const CustodyLevel = 2

// custodyLevelWords precede the level in FlagDescription.
var custodyLevelWords = []string{"custody", "level"}

// FlagDescription is the help description of Flag. Its last words advertise
// CustodyLevel, which a pin relaunch reads from the pinned CLI's help
// (AdvertisedCustodyLevel).
var FlagDescription = "Read the run credential from descriptor <n>; keeps it out of every environment and file; " +
	strings.Join(custodyLevelWords, " ") + " " + strconv.Itoa(CustodyLevel)

// AdvertisedCustodyLevel reads the custody level a CLI's help advertises.
// listed reports whether a line of help starts with Flag as a whole word,
// which is how the help lists a global flag. level is the integer that
// follows the words "custody level" on that line, or 0 when the line
// advertises no level.
func AdvertisedCustodyLevel(help []byte) (level int, listed bool) {
	for _, line := range strings.Split(string(help), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != Flag {
			continue
		}
		return lineCustodyLevel(fields), true
	}
	return 0, false
}

// lineCustodyLevel returns the non-negative integer that follows
// custodyLevelWords in fields, or 0.
func lineCustodyLevel(fields []string) int {
	for i := 0; i+len(custodyLevelWords) < len(fields); i++ {
		if !wordsAt(fields, i, custodyLevelWords) {
			continue
		}
		level, err := strconv.Atoi(fields[i+len(custodyLevelWords)])
		if err != nil || level < 0 {
			return 0
		}
		return level
	}
	return 0
}

func wordsAt(fields []string, at int, words []string) bool {
	for j, word := range words {
		if fields[at+j] != word {
			return false
		}
	}
	return true
}
