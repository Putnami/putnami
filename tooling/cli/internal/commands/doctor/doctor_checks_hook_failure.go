package doctor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/sdk/extension/putnamihome"
)

// The agent session hooks of @putnami/contributor log each failure as one
// JSON line in contributor/sessions/hook.log under the Putnami home, and
// rotate the log to hook.log.1 past 1 MiB. Each line names the workspace root
// the hook ran in.
const (
	hookLogRelative = "contributor/sessions/hook.log"
	// hookFailureMaxAge is how old a failure doctor still reports.
	hookFailureMaxAge = 24 * time.Hour
	// hookFailureErrorMax bounds the error a finding repeats.
	hookFailureErrorMax = 512
)

// hookFailure is one line of the hook log, and the log that holds it.
type hookFailure struct {
	Path   string
	At     time.Time
	Engine string
	Event  string
	Error  string
}

// hookLogPath is the hook log of the Putnami home the environment names,
// else of .putnami under the workspace root, as the hooks resolve it.
func hookLogPath(wsRoot string, lookup func(string) string) string {
	home := putnamihome.Resolve(lookup)
	if home == "" {
		home = filepath.Join(wsRoot, putnamihome.DirName)
	}
	return filepath.Join(home, filepath.FromSlash(hookLogRelative))
}

// readLastHookFailure answers the last failure of the hook log at path, or
// of its rotated copy when the log holds none, whose root is the directory
// wsRoot names, through any directory link, and that is less than
// hookFailureMaxAge old at now. A line that is not JSON, names no root or
// another directory, or has no valid time is skipped.
func readLastHookFailure(path, wsRoot string, now time.Time) (hookFailure, bool) {
	root := newRootMatcher(wsRoot)
	for _, candidate := range []string{path, path + ".1"} {
		if failure, ok := lastHookFailureIn(candidate, root); ok {
			failure.Path = candidate
			age := now.Sub(failure.At)
			return failure, age < hookFailureMaxAge && age > -time.Minute
		}
	}
	return hookFailure{}, false
}

// rootMatcher tells whether a logged root is the workspace's directory: the
// same path once cleaned, else the same directory once both resolve their
// links, symbolic links and Windows junctions alike (dirlink.Resolve). It
// resolves each distinct logged root once.
type rootMatcher struct {
	clean, resolved string
	seen            map[string]bool
}

func newRootMatcher(wsRoot string) *rootMatcher {
	clean := filepath.Clean(wsRoot)
	resolved, err := dirlink.Resolve(clean)
	if err != nil {
		resolved = clean
	}
	return &rootMatcher{clean: clean, resolved: resolved, seen: map[string]bool{}}
}

func (m *rootMatcher) matches(logged string) bool {
	if logged == "" {
		return false
	}
	logged = filepath.Clean(logged)
	if logged == m.clean || logged == m.resolved {
		return true
	}
	same, known := m.seen[logged]
	if !known {
		resolved, err := dirlink.Resolve(logged)
		same = err == nil && resolved == m.resolved
		m.seen[logged] = same
	}
	return same
}

// lastHookFailureIn answers the last line of the log at path whose root
// matches.
func lastHookFailureIn(path string, root *rootMatcher) (hookFailure, bool) {
	file, err := os.Open(path) // #nosec G304 -- the hook log under the Putnami home.
	if err != nil {
		return hookFailure{}, false
	}
	defer func() { _ = file.Close() }()
	var last hookFailure
	found := false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var line struct {
			At     string `json:"at"`
			Root   string `json:"root"`
			Engine string `json:"engine"`
			Event  string `json:"event"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) != nil || !root.matches(line.Root) {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, line.At)
		if err != nil {
			continue
		}
		last, found = hookFailure{At: at, Engine: line.Engine, Event: line.Event, Error: line.Error}, true
	}
	return last, found
}

// checkHookFailure reports the last agent session hook failure of the
// workspace from the last day.
func checkHookFailure(profile doctor.Profile, probe workstationProbe) []doctor.Finding {
	if probe.lastHookFailure == nil {
		return nil
	}
	failure, ok := probe.lastHookFailure()
	if !ok {
		return nil
	}
	return []doctor.Finding{workstationFinding(doctor.CheckAgentHookFailed, profile, displayHomePath(failure.Path), "",
		fmt.Sprintf("the last agent session hook failure of this workspace, at %s, on the %s event of %s: %s",
			failure.At.UTC().Format(time.RFC3339), oneLine(failure.Event, 64), oneLine(failure.Engine, 64), oneLine(failure.Error, hookFailureErrorMax)))}
}

// oneLine is value on one line, cut to at most limit characters.
func oneLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "unknown"
	}
	if utf8.RuneCountInString(value) > limit {
		value = string([]rune(value)[:limit]) + "…"
	}
	return value
}
