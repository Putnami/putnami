package markers

import (
	"fmt"
	"sort"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/areas"
	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/history"
	"go.putnami.dev/intelligence/agent-readiness/internal/scope"
	"go.putnami.dev/intelligence/agent-readiness/inventory"
)

// codeownersPaths are the places a forge reads CODEOWNERS from, in the order
// GitHub looks for them.
var codeownersPaths = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

var codeownersFiles = map[string]bool{".github/CODEOWNERS": true, "CODEOWNERS": true, "docs/CODEOWNERS": true}

const codeownersCommand = "git ls-files -- CODEOWNERS .github/CODEOWNERS docs/CODEOWNERS"

type ownerRule struct {
	pattern string
	owned   bool
	line    int
}

func (c *computation) ownerRules() (string, []ownerRule) {
	for _, file := range codeownersPaths {
		data, ok := c.Contents[file]
		if !ok {
			continue
		}
		var rules []ownerRule
		for i, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(stripHash(line))
			if len(fields) == 0 {
				continue
			}
			rules = append(rules, ownerRule{pattern: fields[0], owned: len(fields) > 1, line: i + 1})
		}
		return file, rules
	}
	return "", nil
}

func stripHash(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		return line[:i]
	}
	return line
}

// owner returns the rule that owns an area directory, following the
// CODEOWNERS rule that the last matching pattern wins.
func owner(rules []ownerRule, dir string) (ownerRule, bool) {
	var last ownerRule
	found := false
	for _, rule := range rules {
		if covers(rule.pattern, dir) {
			last, found = rule, true
		}
	}
	return last, found && last.owned
}

// covers reports whether a CODEOWNERS pattern applies to everything under a
// directory.
func covers(pattern, dir string) bool {
	anchored := strings.HasPrefix(pattern, "/")
	pattern = strings.Trim(pattern, "/")
	pattern = strings.TrimSuffix(strings.TrimSuffix(pattern, "/**"), "/*")
	if pattern == "" || pattern == "*" || pattern == "**" {
		return true
	}
	if dir == areas.RootPath {
		return false
	}
	patternSegments := strings.Split(pattern, "/")
	dirSegments := strings.Split(dir, "/")
	if !anchored && len(patternSegments) == 1 {
		patternSegments = append([]string{"**"}, patternSegments...)
	}
	for end := 1; end <= len(dirSegments); end++ {
		if areas.Glob(patternSegments, dirSegments[:end]) {
			return true
		}
	}
	return false
}

// ownership reads recover.ownership. Its value counts the area's
// contributors, people and agents (Activity.Contributors): a solo maintainer
// who works with an agent credited in a trailer has a second contributor.
// EnforcedDays is the age of the CODEOWNERS file, the closest the collector
// gets to the age of the rule without reading its history line by line.
func (c *computation) ownership(area int) contract.Marker {
	const id = "recover.ownership"
	value := number(float64(c.activity(area).Contributors()))
	file, rules := c.ownerRules()
	if file == "" {
		return marker(id, contract.MarkerAbsent, value, codeownersCommand, nil)
	}
	held := days(c.Now, c.Dates.Added[file])
	if area >= 0 {
		rule, ok := owner(rules, c.Layout.Areas[area].Path)
		if !ok {
			return marker(id, contract.MarkerExists, value, codeownersCommand, []string{file})
		}
		return enforcedFor(marker(id, contract.MarkerEnforced, value, codeownersCommand, []string{fmt.Sprintf("%s:%d", file, rule.line)}), held)
	}
	for i, activity := range c.Areas {
		if activity.Commits == 0 {
			continue
		}
		if _, ok := owner(rules, c.Layout.Areas[i].Path); !ok {
			return marker(id, contract.MarkerExists, value, codeownersCommand, []string{file})
		}
	}
	return enforcedFor(marker(id, contract.MarkerEnforced, value, codeownersCommand, []string{file}), held)
}

// recentChanges is how many of the newest changes must land through pull
// requests for recover.small-changes to read enforced: the treatment, a rule
// that requires pull requests, shows as soon as that many changes land.
const recentChanges = 10

// pullRequestShare is the share of changes that must land through pull
// requests: a release or a revert pushed directly now and then does not
// undo the practice.
const pullRequestShare = 0.9

// pullRequestsHeld reads the practice both recover markers share: the state
// reads the newest changes, so requiring pull requests reaches enforced at
// once; the days are how far back the practice holds: the age of the oldest
// change before the share of changes landed through pull requests, counted
// newest first, first falls below 90%, and the whole window, or the
// repository's age, when it never does. It answers absent with no change or
// no pull request, and exists when the newest changes fall below the share.
func (c *computation) pullRequestsHeld() (contract.MarkerState, *float64) {
	if len(c.Changes) == 0 {
		return contract.MarkerAbsent, nil
	}
	// Changes come newest first. The run starts with the newest changes read
	// together and ends at the first change where the share falls below 90%:
	// older pull requests do not cover a stretch of direct pushes.
	recent := min(recentChanges, len(c.Changes))
	viaPR, held, broken := 0, -1, false
	for i, change := range c.Changes {
		if change.ViaPullRequest {
			viaPR++
		}
		if broken || i+1 < recent {
			continue
		}
		if float64(viaPR) >= pullRequestShare*float64(i+1) {
			held = i
		} else {
			broken = true
		}
	}
	switch {
	case viaPR == 0:
		return contract.MarkerAbsent, nil
	case held < 0:
		return contract.MarkerExists, nil
	}
	since := c.Changes[held].Time
	if held == len(c.Changes)-1 {
		// The whole window holds the practice: it has held since the window
		// opened, or since the repository was born when it is younger.
		since = max(c.Now-int64(history.Window.Seconds()), c.Born)
	}
	return contract.MarkerEnforced, days(c.Now, since)
}

// smallChanges reads recover.small-changes, the median size of a change,
// with the pull request practice as its state. Method 0.4 scores
// recover.contained-changes instead; the payload still carries both.
func (c *computation) smallChanges() contract.Marker {
	const id = "recover.small-changes"
	var value *float64
	if len(c.Changes) > 0 {
		value = number(Median(c.Changes))
	}
	state, held := c.pullRequestsHeld()
	result := marker(id, state, value, history.ChangesCommand, nil)
	if state == contract.MarkerEnforced {
		return enforcedFor(result, held)
	}
	return result
}

// containedWindow is how long after a change a revert still counts against
// it: a week.
const containedWindow = 7 * secondsPerDay

// containedCommand lists the window's reverts the collector reads, merges
// and message bodies included, so a reader can check the count by hand.
const containedCommand = `git log --since=90.days -i --grep=revert --format='%h %cs %s'`

func (c *computation) containedChanges() contract.Marker {
	const id = "recover.contained-changes"
	state, held := c.pullRequestsHeld()
	eligible, reverted := 0, 0
	var sample []string
	for _, change := range c.Changes {
		if c.Now-change.Time < containedWindow {
			continue
		}
		eligible++
		if change.RevertedAt > 0 && change.RevertedAt-change.Time <= containedWindow {
			reverted++
			if len(sample) < maxSample {
				sample = append(sample, codeFiles(change.Files)...)
				sample = sample[:min(len(sample), maxSample)]
			}
		}
	}
	result := marker(id, state, ratio(reverted, eligible), containedCommand, sample)
	if state == contract.MarkerEnforced {
		return enforcedFor(result, held)
	}
	return result
}

// codeFiles keeps the source files a change touched: code and tests, not
// documentation, configuration or lockfiles, which many unrelated changes
// share.
func codeFiles(files []string) []string {
	var code []string
	for _, file := range files {
		if inventory.Language(file) != "" && !scope.Skipped(file) {
			code = append(code, file)
		}
	}
	return code
}

// Median returns the median changed lines of the changes.
func Median(changes []history.Change) float64 {
	if len(changes) == 0 {
		return 0
	}
	lines := make([]int, len(changes))
	for i, change := range changes {
		lines[i] = change.Lines
	}
	sort.Ints(lines)
	middle := len(lines) / 2
	if len(lines)%2 == 1 {
		return float64(lines[middle])
	}
	return float64(lines[middle-1]+lines[middle]) / 2
}
