package command

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// levelPhrases name, for each level, the agents a codebase supports. The
// levels are returned by the hosted scoring service.
var levelPhrases = map[string]string{
	"L1": "agents that assist",
	"L2": "supervised agents",
	"L3": "delegated agents",
	"L4": "agents reviewed by exception",
}

// readLine is the first terminal line: what the collector read and how long
// it took.
func readLine(built Built) string {
	return fmt.Sprintf("Reading %s commits, %s areas… done (%d s)",
		thousands(built.Commits), thousands(built.Areas), seconds(built.Elapsed))
}

// verdictLine is the second terminal line: the level the service scored and
// the share of recent change that lands below L3.
func verdictLine(level string, shareBlocked float64) string {
	verdict := "This codebase is at level " + level + "."
	if phrase, ok := levelPhrases[level]; ok {
		verdict = fmt.Sprintf("This codebase supports %s (%s).", phrase, level)
	}
	return fmt.Sprintf("%s %d%% of recent changes land where an agent cannot yet work alone.",
		verdict, int(math.Round(shareBlocked*100)))
}

// sentLine is the third terminal line: how much was sent, what it holds, and
// how to see it.
func sentLine(bytes int) string {
	return fmt.Sprintf("Sent: %d KB — counts, area names and paths, no file contents. Inspect: %s",
		kilobytes(bytes), InspectCommand)
}

func kilobytes(bytes int) int {
	return max(1, int(math.Round(float64(bytes)/1024)))
}

func seconds(elapsed time.Duration) int {
	return max(1, int(math.Round(elapsed.Seconds())))
}

// thousands formats n with a comma between groups of three digits.
func thousands(n int) string {
	digits := strconv.Itoa(n)
	if n < 0 {
		return "-" + thousands(-n)
	}
	out := make([]byte, 0, len(digits)+len(digits)/3)
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, digits[i])
	}
	return string(out)
}
