package turbo

import (
	"fmt"
	"strings"
)

// diffContext is the number of unchanged lines a hunk shows around a change.
const diffContext = 3

// UnifiedDiff renders the line diff from before to after of the file name as
// a unified diff; empty when they are equal. A missing file is /dev/null.
func UnifiedDiff(name string, before, after []byte) string {
	if string(before) == string(after) {
		return ""
	}
	a, b := lines(before), lines(after)
	ops := lineOps(a, b)
	from, to := "a/"+name, "b/"+name
	if before == nil {
		from = "/dev/null"
	}
	if after == nil {
		to = "/dev/null"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", from, to)
	for start := 0; start < len(ops); {
		for start < len(ops) && ops[start].kind == ' ' {
			start++
		}
		if start == len(ops) {
			break
		}
		lo := max(start-diffContext, 0)
		end := start
		for i := start; i < len(ops); i++ {
			if ops[i].kind != ' ' {
				end = i
				continue
			}
			if i-end > 2*diffContext {
				break
			}
		}
		hi := min(end+diffContext+1, len(ops))
		aStart, bStart, aLen, bLen := ops[lo].a, ops[lo].b, 0, 0
		for _, op := range ops[lo:hi] {
			if op.kind != '+' {
				aLen++
			}
			if op.kind != '-' {
				bLen++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
		for _, op := range ops[lo:hi] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			out.WriteByte('\n')
		}
		start = hi
	}
	return out.String()
}

func hunkRange(start, length int) string {
	if length == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, length)
}

func lines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// lineOp is one line of the edit script, with its index in each side.
type lineOp struct {
	kind byte
	text string
	a, b int
}

// lineOps is the longest-common-subsequence edit script from a to b.
func lineOps(a, b []string) []lineOp {
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []lineOp
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, lineOp{' ', a[i], i, j})
			i++
			j++
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, lineOp{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, lineOp{'+', b[j], i, j})
			j++
		}
	}
	return ops
}
