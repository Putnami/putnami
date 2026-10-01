package completion

import "go.putnami.dev/tooling/cli/internal/commandmeta"

// StructuredCommandInfo describes usage help for a structured command or subcommand.
type StructuredCommandInfo struct {
	Description string
	Usage       string
	Flags       []StructuredFlagInfo
	Examples    []string
}

// StructuredFlagInfo describes a structured-command flag for help and completion.
type StructuredFlagInfo struct {
	Long        string
	Description string
	ValueName   string
}

// structuredCommandHelp is derived from the command catalog: the catalog owns
// every description, usage line, flag, and example, and this map is only the
// lookup shape the help renderers want. Paths that share another path's help
// (`scopes` ≡ `scopes list`, `init` ≡ `workspace init`) resolve through
// commandmeta.Detail, so both spellings answer with identical bytes.
var structuredCommandHelp = buildStructuredCommandHelp()

func buildStructuredCommandHelp() map[string]StructuredCommandInfo {
	commands := commandmeta.StructuredCommands()
	out := make(map[string]StructuredCommandInfo, len(commands))
	for _, command := range commands {
		detail, ok := commandmeta.Detail(command.Path)
		if !ok {
			continue
		}
		out[command.Path] = StructuredCommandInfo{
			Description: detail.Description,
			Usage:       detail.Usage,
			Flags:       structuredFlagInfos(detail.Flags),
			Examples:    detail.Examples,
		}
	}
	return out
}

func structuredFlagInfos(flags []commandmeta.Flag) []StructuredFlagInfo {
	if len(flags) == 0 {
		return nil
	}
	out := make([]StructuredFlagInfo, 0, len(flags))
	for _, flag := range flags {
		out = append(out, StructuredFlagInfo{
			Long:        flag.Long,
			Description: flag.Description,
			ValueName:   flag.ValueName,
		})
	}
	return out
}

// StructuredCommandHelp returns help metadata for a structured command or
// subcommand.
//
// The returned Flags slice is shared with the table, so callers must not sort or
// otherwise mutate it. Until slice A1b, shell completion seeded itself through
// this accessor and then sorted the result in place, reordering the flags
// `putnami help projects create` prints for any process that had already
// generated a completion script. Completion projects the catalog directly now,
// and its own copy is the one it sorts.
func StructuredCommandHelp(command, subcommand string) (StructuredCommandInfo, bool) {
	key := command
	if subcommand != "" {
		key = command + " " + subcommand
	}
	info, ok := structuredCommandHelp[key]
	return info, ok
}

// StructuredCommandHelpTable returns the full derived help table, keyed by
// command path. Exposed for internal/commands/surface_golden_test.go, which
// pins the table's CONTENT independent of any renderer; ordinary callers
// should use StructuredCommandHelp instead. The returned map is shared with
// the table, so callers must not mutate it.
func StructuredCommandHelpTable() map[string]StructuredCommandInfo {
	return structuredCommandHelp
}
