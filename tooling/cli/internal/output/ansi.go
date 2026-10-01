package output

import "sync"

// ANSI color/style escape codes.
const (
	Reset     = "\033[0m"
	Bold      = "\033[1m"
	Dim       = "\033[2m"
	Italic    = "\033[3m"
	Underline = "\033[4m"

	Red     = "\033[31m"
	Green   = "\033[32m"
	Yellow  = "\033[33m"
	Blue    = "\033[34m"
	Magenta = "\033[35m"
	Cyan    = "\033[36m"
	Gray    = "\033[90m"
	White   = "\033[97m"

	BgRed   = "\033[41m"
	BgGreen = "\033[42m"

	ClearLine  = "\033[2K"
	ClearToEOL = "\033[K"
	MoveUp     = "\033[1A"
	HideCursor = "\033[?25l"
	ShowCursor = "\033[?25h"
)

var (
	colorMu        sync.Mutex
	colorsDisabled bool
)

// SetColorsEnabled controls whether colorize() emits ANSI codes.
func SetColorsEnabled(enabled bool) {
	colorMu.Lock()
	colorsDisabled = !enabled
	colorMu.Unlock()
}

// ColorsEnabled reports whether ANSI color output is active.
func ColorsEnabled() bool {
	colorMu.Lock()
	defer colorMu.Unlock()
	return !colorsDisabled
}

// colorize wraps a string in ANSI color codes.
// No-ops if color is empty or colors are globally disabled.
func colorize(s, color string) string {
	if color == "" || colorsDisabled {
		return s
	}
	return color + s + Reset
}
