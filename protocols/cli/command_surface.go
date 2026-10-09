package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// Command surface: the document a CLI commits to state the commands and flags
// its users type, and the comparison that finds an incompatible change between
// two of them.
const (
	// CommandSurfaceVersion is the command-surface contract version.
	CommandSurfaceVersion = 1
	// CommandSurfaceSchemaID is the published schema's $id.
	CommandSurfaceSchemaID = "https://putnami.dev/schemas/putnami-cli-command-surface.json"
	// CommandSurfaceMaxBytes bounds the document a reader accepts.
	CommandSurfaceMaxBytes = 1 << 20
)

// Flag types, a closed vocabulary.
const (
	// CommandFlagBool is a flag that takes no value ("--dry-run").
	CommandFlagBool = "bool"
	// CommandFlagValue is a flag that takes a value ("--project <selector>").
	CommandFlagValue = "value"
)

// CommandSurface is the command surface of one CLI: its global flags and its
// commands, each with its flags and positionals. It holds only what an
// invocation depends on. Descriptions, usage lines and examples are not part
// of it, so editing them never changes the document.
type CommandSurface struct {
	// ProtocolVersion identifies the command-surface contract; currently 1.
	ProtocolVersion int `json:"protocolVersion"`
	// GlobalFlags are the flags the CLI accepts with any command, sorted by
	// long name.
	GlobalFlags []CommandSurfaceFlag `json:"globalFlags"`
	// Commands are the commands users type, sorted by path.
	Commands []CommandSurfaceCommand `json:"commands"`
}

// CommandSurfaceCommand is one command users type.
type CommandSurfaceCommand struct {
	// Path is the space-separated invocation path after the program name, for
	// example "projects list".
	Path string `json:"path"`
	// Flags are the command's own flags, sorted by long name.
	Flags []CommandSurfaceFlag `json:"flags"`
	// Positionals are the command's positional arguments, in invocation order.
	Positionals []CommandSurfacePositional `json:"positionals"`
}

// CommandSurfaceFlag is one flag of a command, or one global flag.
type CommandSurfaceFlag struct {
	// Long is the long spelling with its dashes, for example "--dry-run".
	Long string `json:"long"`
	// Short is the single-dash alias, for example "-g"; empty when the flag
	// has none.
	Short string `json:"short,omitempty"`
	// Type is CommandFlagBool or CommandFlagValue.
	Type string `json:"type"`
	// Values is the closed list of values a value flag accepts, sorted; nil
	// when the flag accepts any value.
	Values []string `json:"values,omitempty"`
}

// CommandSurfacePositional is one positional argument of a command.
type CommandSurfacePositional struct {
	// Name is the argument's name as the CLI's usage line spells it, for
	// example "name" or "bash|zsh|fish".
	Name string `json:"name"`
	// Required reports whether an invocation must supply the argument.
	Required bool `json:"required"`
}

// CommandChange is one incompatible change between two command surfaces.
type CommandChange struct {
	// Command is the path of the command the change belongs to; empty for a
	// global flag.
	Command string
	// Flag is the long spelling of the flag the change concerns; empty when
	// the change concerns the command itself or a positional.
	Flag string
	// Message states the change in plain words, naming the command and the
	// flag.
	Message string
}

// The patterns of the command-surface vocabulary. The schema spells the same
// patterns with an end-of-input lookahead in place of "$".
const (
	commandPathPattern  = `^[A-Za-z0-9][A-Za-z0-9_-]*( [A-Za-z0-9][A-Za-z0-9_-]*)*$`
	commandLongPattern  = `^--[A-Za-z0-9][A-Za-z0-9_-]*$`
	commandShortPattern = `^-[A-Za-z0-9]$`
	commandTokenPattern = `^\S+$`
)

var (
	commandPathRE  = regexp.MustCompile(commandPathPattern)
	commandLongRE  = regexp.MustCompile(commandLongPattern)
	commandShortRE = regexp.MustCompile(commandShortPattern)
	commandTokenRE = regexp.MustCompile(commandTokenPattern)
)

// NewCommandSurface returns the canonical command surface of a CLI: its
// commands sorted by path, every flag list sorted by long name, every value
// list sorted, and every list non-nil. It copies its arguments and validates
// the result, so a duplicate command, flag, short alias or value is an error.
func NewCommandSurface(globalFlags []CommandSurfaceFlag, commands []CommandSurfaceCommand) (CommandSurface, error) {
	surface := CommandSurface{
		ProtocolVersion: CommandSurfaceVersion,
		GlobalFlags:     canonicalFlags(globalFlags),
		Commands:        make([]CommandSurfaceCommand, 0, len(commands)),
	}
	for _, command := range commands {
		surface.Commands = append(surface.Commands, CommandSurfaceCommand{
			Path:        command.Path,
			Flags:       canonicalFlags(command.Flags),
			Positionals: append(make([]CommandSurfacePositional, 0, len(command.Positionals)), command.Positionals...),
		})
	}
	slices.SortStableFunc(surface.Commands, func(a, b CommandSurfaceCommand) int { return strings.Compare(a.Path, b.Path) })
	if err := surface.Validate(); err != nil {
		return CommandSurface{}, err
	}
	return surface, nil
}

func canonicalFlags(flags []CommandSurfaceFlag) []CommandSurfaceFlag {
	out := make([]CommandSurfaceFlag, 0, len(flags))
	for _, flag := range flags {
		if flag.Values != nil {
			flag.Values = slices.Clone(flag.Values)
			slices.Sort(flag.Values)
		}
		out = append(out, flag)
	}
	slices.SortStableFunc(out, func(a, b CommandSurfaceFlag) int { return strings.Compare(a.Long, b.Long) })
	return out
}

// Validate checks every rule a reader relies on: the version, the spelling of
// every path, flag, alias and value, and the canonical order that makes each
// one unique. Lists are compared by byte order.
func (s CommandSurface) Validate() error {
	if s.ProtocolVersion != CommandSurfaceVersion {
		return fmt.Errorf("command surface: protocolVersion %d is not %d", s.ProtocolVersion, CommandSurfaceVersion)
	}
	if s.GlobalFlags == nil || s.Commands == nil {
		return errors.New("command surface: globalFlags and commands are required")
	}
	if err := validateFlags(s.GlobalFlags); err != nil {
		return fmt.Errorf("command surface: global flags: %w", err)
	}
	for i, command := range s.Commands {
		if !commandPathRE.MatchString(command.Path) {
			return fmt.Errorf("command surface: invalid command path %q", command.Path)
		}
		if i > 0 && s.Commands[i-1].Path >= command.Path {
			return fmt.Errorf("command surface: commands must be unique and sorted by path: %q follows %q", command.Path, s.Commands[i-1].Path)
		}
		if command.Flags == nil || command.Positionals == nil {
			return fmt.Errorf("command surface: command %q: flags and positionals are required", command.Path)
		}
		if err := validateFlags(command.Flags); err != nil {
			return fmt.Errorf("command surface: command %q: %w", command.Path, err)
		}
		for _, positional := range command.Positionals {
			if !commandTokenRE.MatchString(positional.Name) {
				return fmt.Errorf("command surface: command %q: invalid positional name %q", command.Path, positional.Name)
			}
		}
	}
	return nil
}

func validateFlags(flags []CommandSurfaceFlag) error {
	shorts := map[string]string{}
	for i, flag := range flags {
		if !commandLongRE.MatchString(flag.Long) {
			return fmt.Errorf("invalid flag %q", flag.Long)
		}
		if i > 0 && flags[i-1].Long >= flag.Long {
			return fmt.Errorf("flags must be unique and sorted by long name: %s follows %s", flag.Long, flags[i-1].Long)
		}
		if flag.Short != "" {
			if !commandShortRE.MatchString(flag.Short) {
				return fmt.Errorf("flag %s: invalid short alias %q", flag.Long, flag.Short)
			}
			if other, taken := shorts[flag.Short]; taken {
				return fmt.Errorf("flags %s and %s share the short alias %s", other, flag.Long, flag.Short)
			}
			shorts[flag.Short] = flag.Long
		}
		switch flag.Type {
		case CommandFlagBool:
			if flag.Values != nil {
				return fmt.Errorf("flag %s: a bool flag accepts no values", flag.Long)
			}
		case CommandFlagValue:
		default:
			return fmt.Errorf("flag %s: type %q is not %s or %s", flag.Long, flag.Type, CommandFlagBool, CommandFlagValue)
		}
		if flag.Values != nil && len(flag.Values) == 0 {
			return fmt.Errorf("flag %s: values, when present, lists at least one value", flag.Long)
		}
		for j, value := range flag.Values {
			if !commandTokenRE.MatchString(value) {
				return fmt.Errorf("flag %s: invalid value %q", flag.Long, value)
			}
			if j > 0 && flag.Values[j-1] >= value {
				return fmt.Errorf("flag %s: values must be unique and sorted: %q follows %q", flag.Long, value, flag.Values[j-1])
			}
		}
	}
	return nil
}

// MarshalCommandSurface returns the canonical bytes of a valid command
// surface: two-space indentation, no HTML escaping, and a final newline. The
// same surface always yields the same bytes.
func MarshalCommandSurface(s CommandSurface) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(s); err != nil {
		return nil, fmt.Errorf("command surface: encode: %w", err)
	}
	return buffer.Bytes(), nil
}

// ParseCommandSurface strictly decodes and validates one bounded document:
// every required member must be present, no member may be null or empty when
// optional, and unknown members and trailing JSON are refused.
func ParseCommandSurface(data []byte) (*CommandSurface, error) {
	if len(data) > CommandSurfaceMaxBytes {
		return nil, fmt.Errorf("command surface: the document exceeds %d bytes", CommandSurfaceMaxBytes)
	}
	if err := checkSurfaceMembers(data); err != nil {
		return nil, err
	}
	var surface CommandSurface
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&surface); err != nil {
		return nil, fmt.Errorf("command surface: %w", err)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, errors.New("command surface: trailing data after the document")
	}
	if err := surface.Validate(); err != nil {
		return nil, err
	}
	return &surface, nil
}

// checkSurfaceMembers refuses a missing required member and a null member at
// every level of the document, which a struct decode cannot tell from an
// absent or zero one.
func checkSurfaceMembers(data []byte) error {
	var document struct {
		GlobalFlags []json.RawMessage `json:"globalFlags"`
		Commands    []json.RawMessage `json:"commands"`
	}
	if err := surfaceMembers(data, "document", "protocolVersion", "globalFlags", "commands"); err != nil {
		return err
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("command surface: %w", err)
	}
	for _, flag := range document.GlobalFlags {
		if err := checkFlagMembers(flag); err != nil {
			return err
		}
	}
	for _, raw := range document.Commands {
		if err := surfaceMembers(raw, "command", "path", "flags", "positionals"); err != nil {
			return err
		}
		var command struct {
			Flags       []json.RawMessage `json:"flags"`
			Positionals []json.RawMessage `json:"positionals"`
		}
		if err := json.Unmarshal(raw, &command); err != nil {
			return fmt.Errorf("command surface: %w", err)
		}
		for _, flag := range command.Flags {
			if err := checkFlagMembers(flag); err != nil {
				return err
			}
		}
		for _, positional := range command.Positionals {
			if err := surfaceMembers(positional, "positional", "name", "required"); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkFlagMembers(data json.RawMessage) error {
	if err := surfaceMembers(data, "flag", "long", "type"); err != nil {
		return err
	}
	var flag struct {
		Short *string `json:"short"`
	}
	if err := json.Unmarshal(data, &flag); err != nil {
		return fmt.Errorf("command surface: %w", err)
	}
	if flag.Short != nil && *flag.Short == "" {
		return errors.New("command surface: an empty short alias is written by omitting short")
	}
	return nil
}

// surfaceMembers refuses a value that is not an object, omits a required
// member, or sets any member to null.
func surfaceMembers(data []byte, kind string, required ...string) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil {
		return fmt.Errorf("command surface: a %s is not a JSON object", kind)
	}
	for name, value := range members {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("command surface: %s member %s is null", kind, name)
		}
	}
	for _, name := range required {
		if _, ok := members[name]; !ok {
			return fmt.Errorf("command surface: %s member %s is missing", kind, name)
		}
	}
	return nil
}

// IncompatibleCommandChanges returns the changes from previous to current that
// can break an invocation that worked against previous, sorted by command,
// flag and message.
//
// Incompatible: a removed command; a removed flag, global or of a command; a
// removed short alias; a flag that starts or stops taking a value; a value
// removed from a flag's closed list, or a closed list given to a flag that
// accepted any value; a removed positional; an optional positional that
// becomes required; and a new required positional. Positionals are compared by
// position, not by name.
//
// Everything added is compatible: a command, a flag, a short alias, a value,
// an optional positional. So are a closed list that is removed, and a
// positional that becomes optional or changes its name.
func IncompatibleCommandChanges(previous, current CommandSurface) []CommandChange {
	var changes []CommandChange
	changes = appendFlagChanges(changes, "", previous.GlobalFlags, current.GlobalFlags)
	commands := make(map[string]CommandSurfaceCommand, len(current.Commands))
	for _, command := range current.Commands {
		commands[command.Path] = command
	}
	for _, before := range previous.Commands {
		after, ok := commands[before.Path]
		if !ok {
			changes = append(changes, CommandChange{Command: before.Path, Message: fmt.Sprintf("command %q removed", before.Path)})
			continue
		}
		changes = appendFlagChanges(changes, before.Path, before.Flags, after.Flags)
		changes = appendPositionalChanges(changes, before.Path, before.Positionals, after.Positionals)
	}
	slices.SortStableFunc(changes, func(a, b CommandChange) int {
		if c := strings.Compare(a.Command, b.Command); c != 0 {
			return c
		}
		if c := strings.Compare(a.Flag, b.Flag); c != 0 {
			return c
		}
		return strings.Compare(a.Message, b.Message)
	})
	return changes
}

func appendFlagChanges(changes []CommandChange, command string, previous, current []CommandSurfaceFlag) []CommandChange {
	flags := make(map[string]CommandSurfaceFlag, len(current))
	for _, flag := range current {
		flags[flag.Long] = flag
	}
	for _, before := range previous {
		subject := "global flag " + before.Long
		if command != "" {
			subject = fmt.Sprintf("command %q: flag %s", command, before.Long)
		}
		change := func(format string, args ...any) {
			changes = append(changes, CommandChange{Command: command, Flag: before.Long, Message: subject + " " + fmt.Sprintf(format, args...)})
		}
		after, ok := flags[before.Long]
		if !ok {
			change("removed")
			continue
		}
		if before.Short != "" && before.Short != after.Short {
			change("lost its short alias %s", before.Short)
		}
		if before.Type != after.Type {
			if after.Type == CommandFlagValue {
				change("now takes a value")
			} else {
				change("no longer takes a value")
			}
			continue
		}
		switch {
		case after.Values == nil:
		case before.Values == nil:
			change("now accepts only %s", strings.Join(after.Values, ", "))
		default:
			for _, value := range before.Values {
				if !slices.Contains(after.Values, value) {
					change("no longer accepts %q", value)
				}
			}
		}
	}
	return changes
}

func appendPositionalChanges(changes []CommandChange, command string, previous, current []CommandSurfacePositional) []CommandChange {
	for i, before := range previous {
		if i >= len(current) {
			changes = append(changes, CommandChange{Command: command,
				Message: fmt.Sprintf("command %q: positional %q (position %d) removed", command, before.Name, i+1)})
			continue
		}
		if !before.Required && current[i].Required {
			changes = append(changes, CommandChange{Command: command,
				Message: fmt.Sprintf("command %q: positional %q (position %d) is now required", command, current[i].Name, i+1)})
		}
	}
	for i := len(previous); i < len(current); i++ {
		if current[i].Required {
			changes = append(changes, CommandChange{Command: command,
				Message: fmt.Sprintf("command %q: new required positional %q (position %d)", command, current[i].Name, i+1)})
		}
	}
	return changes
}
