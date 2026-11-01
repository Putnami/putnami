package calc

import "example.com/calc/internal/fmtx"

func Add(a, b int) int {
	return a + b
}

func Sign(n int) string {
	if n >= 0 {
		return fmtx.Label("positive")
	} else {
		return fmtx.Label("negative")
	}
}

func NeverCalled(x int) int {
	doubled := x * 2
	return doubled + 1
}
