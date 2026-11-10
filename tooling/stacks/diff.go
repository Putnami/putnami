package stacks

import (
	"strings"
)

// Diff is a line diff of a file from before to after: a header naming the
// file, then each line prefixed with "-" when removed, "+" when added, and a
// space when kept. A nil before is a new file. The files a sync writes are a
// few lines long, so the diff shows them whole.
func Diff(file string, before, after []byte) string {
	var b strings.Builder
	if before == nil {
		b.WriteString("--- /dev/null\n")
	} else {
		b.WriteString("--- a/" + file + "\n")
	}
	b.WriteString("+++ b/" + file + "\n")
	for _, l := range diffLines(lines(before), lines(after)) {
		b.WriteString(l + "\n")
	}
	return b.String()
}

func lines(data []byte) []string {
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// diffLines aligns a and b on their longest common subsequence.
func diffLines(a, b []string) []string {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, " "+a[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, "-"+a[i])
			i++
		default:
			out = append(out, "+"+b[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "-"+a[i])
	}
	for ; j < m; j++ {
		out = append(out, "+"+b[j])
	}
	return out
}
