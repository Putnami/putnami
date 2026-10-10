package clicore

import "strings"

// Positionals returns every non-flag argument in order. It skips the parent
// CLI's --putnamiContext token and the values consumed by string flags, the
// same walk FirstPositional performs.
func Positionals(argv []string) []string {
	indexes := positionalIndexes(argv)
	out := make([]string, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, argv[index])
	}
	return out
}

// DropFirstPositional returns argv without its first positional word, keeping
// every flag and later positional in place. A command that routes
// `putnami cloud <noun> <verb> …` to the handler of the verb uses it to hand the
// handler the arguments it always received.
func DropFirstPositional(argv []string) []string {
	indexes := positionalIndexes(argv)
	if len(indexes) == 0 {
		return append([]string{}, argv...)
	}
	out := make([]string, 0, len(argv)-1)
	out = append(out, argv[:indexes[0]]...)
	return append(out, argv[indexes[0]+1:]...)
}

// ReplaceFirstPositional returns argv with its first positional word replaced
// by word. It appends word when argv has no positional.
func ReplaceFirstPositional(argv []string, word string) []string {
	out := append([]string{}, argv...)
	indexes := positionalIndexes(argv)
	if len(indexes) == 0 {
		return append(out, word)
	}
	out[indexes[0]] = word
	return out
}

// positionalIndexes returns the index in argv of each positional argument.
func positionalIndexes(argv []string) []int {
	var out []int
	skipNext := false
	for i, raw := range argv {
		if skipNext {
			skipNext = false
			continue
		}
		if raw == "--putnamiContext" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(raw, "--") {
			name := strings.TrimPrefix(raw, "--")
			if strings.Contains(name, "=") || strings.HasPrefix(name, "no-") || IsBooleanFlag(name) {
				continue
			}
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
				skipNext = true
			}
			continue
		}
		out = append(out, i)
	}
	return out
}
