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
	Short       string
	Description string
	ValueName   string
}

func structuredFlagInfos(flags []commandmeta.Flag) []StructuredFlagInfo {
	if len(flags) == 0 {
		return nil
	}
	out := make([]StructuredFlagInfo, 0, len(flags))
	for _, flag := range flags {
		out = append(out, StructuredFlagInfo{
			Long:        flag.Long,
			Short:       flag.Short,
			Description: flag.Description,
			ValueName:   flag.ValueName,
		})
	}
	return out
}

// StructuredCommandHelp returns help metadata for a structured command or
// subcommand, derived from the command catalog: the catalog owns every
// description, usage line, flag, and example. Paths that share another path's
// help (`scopes` ≡ `scopes list`, `init` ≡ `workspace init`) resolve through
// commandmeta.Detail, so both spellings answer with identical bytes.
//
// Every call builds its own Flags and Examples slices, so a caller may sort or
// otherwise change them without changing what the next caller reads.
func StructuredCommandHelp(command, subcommand string) (StructuredCommandInfo, bool) {
	path := command
	if subcommand != "" {
		path = command + " " + subcommand
	}
	entry, ok := commandmeta.Lookup(path)
	if !ok || entry.Kind != commandmeta.KindStructured {
		return StructuredCommandInfo{}, false
	}
	detail, ok := commandmeta.Detail(path)
	if !ok {
		return StructuredCommandInfo{}, false
	}
	return StructuredCommandInfo{
		Description: detail.Description,
		Usage:       detail.Usage,
		Flags:       structuredFlagInfos(detail.Flags),
		Examples:    append([]string(nil), detail.Examples...),
	}, true
}
