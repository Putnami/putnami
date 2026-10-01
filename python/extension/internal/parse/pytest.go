package parse

import (
	"regexp"
	"strconv"
)

var pytestCount = regexp.MustCompile(`(\d+)\s+(passed|failed|skipped)\b`)

// TestCounts holds parsed pytest result counts.
type TestCounts struct {
	Passed  int
	Failed  int
	Skipped int
	Total   int
}

// PytestResults parses pytest output for pass/fail/skip counts.
func PytestResults(output string) TestCounts {
	var c TestCounts
	for _, m := range pytestCount.FindAllStringSubmatch(output, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		switch m[2] {
		case "passed":
			c.Passed += n
		case "failed":
			c.Failed += n
		case "skipped":
			c.Skipped += n
		}
	}
	c.Total = c.Passed + c.Failed + c.Skipped
	return c
}
