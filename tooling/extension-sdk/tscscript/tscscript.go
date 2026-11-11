// Package tscscript reads a package.json build script and says whether it runs
// the TypeScript compiler alone. @putnami/typescript compiles such a project
// with that tsc, and the Nx and Turborepo imports move such a build to it, so
// the three read the rule here and cannot disagree.
package tscscript

import (
	"path"
	"strings"
)

// Mode is how a tsc command names the config it compiles.
type Mode string

const (
	// ModeDefault is a bare tsc: it compiles the tsconfig.json of its
	// working directory.
	ModeDefault Mode = "default"
	// ModeBuild is tsc -b or tsc --build, with at most one config.
	ModeBuild Mode = "build"
	// ModeProject is tsc -p or tsc --project, with one config.
	ModeProject Mode = "project"
)

// Command is a build script that runs tsc alone.
type Command struct {
	// Mode is how the command names its config.
	Mode Mode
	// Config is the config path as the script writes it, relative to the
	// project directory, or empty when the command names none.
	Config string
	// Flags are the presentation flags the script passes, in order.
	Flags []string
}

// presentationFlags change what tsc prints, never what it emits.
var presentationFlags = map[string]bool{"--pretty": true, "--verbose": true}

// shellBytes are the bytes that make a script more than one plain command:
// operators, redirections, substitutions, quotes, escapes and globs.
const shellBytes = "&|;<>$`()\\'\"*?[]{}~#!\n\r"

// Parse reports the tsc command a build script runs, and whether it runs tsc
// alone: the program tsc, then nothing, -b or --build with at most one
// config, or -p or --project with one config, plus --pretty or --verbose.
// Anything else, such as another flag, a second config, an environment
// assignment, a shell operator or a config that leaves the project, is not
// tsc alone.
func Parse(script string) (Command, bool) {
	if strings.ContainsAny(script, shellBytes) {
		return Command{}, false
	}
	fields := strings.Fields(script)
	if len(fields) == 0 || fields[0] != "tsc" {
		return Command{}, false
	}
	cmd := Command{Mode: ModeDefault}
	var positional []string
	for i := 1; i < len(fields); i++ {
		arg := fields[i]
		switch {
		case presentationFlags[arg]:
			cmd.Flags = append(cmd.Flags, arg)
		case arg == "-b" || arg == "--build":
			if cmd.Mode != ModeDefault {
				return Command{}, false
			}
			cmd.Mode = ModeBuild
		case arg == "-p" || arg == "--project":
			if cmd.Mode != ModeDefault || i+1 >= len(fields) {
				return Command{}, false
			}
			cmd.Mode = ModeProject
			i++
			positional = append(positional, fields[i])
		case strings.HasPrefix(arg, "-"):
			return Command{}, false
		default:
			positional = append(positional, arg)
		}
	}
	switch {
	case len(positional) > 1:
		return Command{}, false
	case len(positional) == 1 && cmd.Mode == ModeDefault:
		// tsc file.ts compiles files, not a config.
		return Command{}, false
	case len(positional) == 1:
		if !insideProject(positional[0]) {
			return Command{}, false
		}
		cmd.Config = path.Clean(positional[0])
	}
	return cmd, true
}

// Runs reports whether the script runs tsc alone.
func Runs(script string) bool {
	_, ok := Parse(script)
	return ok
}

// insideProject reports whether a config path is relative and stays inside
// the project directory.
func insideProject(config string) bool {
	if strings.HasPrefix(config, "-") || strings.HasPrefix(config, "/") || strings.Contains(config, ":") {
		return false
	}
	clean := path.Clean(config)
	return clean != ".." && !strings.HasPrefix(clean, "../")
}

// Args returns the arguments the command passes to tsc, with config given in
// place of the script's when it is not empty.
func (c Command) Args() []string {
	var args []string
	switch c.Mode {
	case ModeBuild:
		args = append(args, "-b")
		if c.Config != "" {
			args = append(args, c.Config)
		}
	case ModeProject:
		args = append(args, "-p", c.Config)
	}
	return append(args, c.Flags...)
}
