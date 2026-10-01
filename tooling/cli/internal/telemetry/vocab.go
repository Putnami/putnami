package telemetry

import (
	"fmt"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/telemetry/cliusage"
)

// allowedCommands comes from the shared wire contract so producer and receiver
// enforce the exact same aggregate-key vocabulary.
var allowedCommands = func() map[string]struct{} {
	allowed := make(map[string]struct{}, len(cliusage.Commands))
	for _, command := range cliusage.Commands {
		allowed[command] = struct{}{}
	}
	return allowed
}()

// FlagPresence records whether the small, allowlisted telemetry flags were
// present in the invocation. Values are deliberately not retained.
type FlagPresence struct {
	Impacted bool
	Coverage bool
	Output   bool
	NoCache  bool
	Projects bool
	Watch    bool
}

// SessionStart contains the closed-vocabulary dimensions of a session-start
// event. Commands not in the public job-command registry are omitted.
type SessionStart struct {
	Commands    []string
	Projects    int
	Jobs        int
	Flags       FlagPresence
	Interactive bool
}

// SessionEnd contains the closed-vocabulary dimensions of a session-end event.
// ErrorCategory is derived from ExitCode rather than accepting arbitrary text.
type SessionEnd struct {
	ExitCode    int
	DurationMS  int64
	Interactive bool
}

// TrackSessionStart records the fixed session-start telemetry shape.
func (c *Client) TrackSessionStart(session SessionStart) {
	commands := make([]string, 0, len(session.Commands))
	for _, command := range session.Commands {
		if _, allowed := allowedCommands[command]; allowed {
			commands = append(commands, command)
		}
	}

	c.track(Event{
		Name: cliusage.EventSessionStart,
		Data: map[string]any{
			cliusage.AttrCommands:     commands,
			cliusage.AttrProjects:     session.Projects,
			cliusage.AttrJobs:         session.Jobs,
			cliusage.AttrInteractive:  session.Interactive,
			cliusage.AttrFlagImpacted: session.Flags.Impacted,
			cliusage.AttrFlagCoverage: session.Flags.Coverage,
			cliusage.AttrFlagOutput:   session.Flags.Output,
			cliusage.AttrFlagNoCache:  session.Flags.NoCache,
			cliusage.AttrFlagProjects: session.Flags.Projects,
			cliusage.AttrFlagWatch:    session.Flags.Watch,
		},
	})
}

// TrackSessionEnd records the fixed session-end telemetry shape.
func (c *Client) TrackSessionEnd(session SessionEnd) {
	data := map[string]any{
		cliusage.AttrSuccess:     session.ExitCode == protocolcli.ExitSuccess,
		cliusage.AttrDuration:    session.DurationMS,
		cliusage.AttrInteractive: session.Interactive,
	}
	if category, failed := errorCategoryForExitCode(session.ExitCode); failed {
		data[cliusage.AttrErrorCategory] = category
	}

	c.track(Event{Name: cliusage.EventSessionEnd, Data: data})
}

func errorCategoryForExitCode(exitCode int) (string, bool) {
	switch exitCode {
	case protocolcli.ExitSuccess:
		return "", false
	case protocolcli.ExitUsage:
		return cliusage.ErrorCategoryUsage, true
	case protocolcli.ExitAuth:
		return cliusage.ErrorCategoryAuth, true
	case protocolcli.ExitAPI:
		return cliusage.ErrorCategoryAPI, true
	default:
		return cliusage.ErrorCategoryFailure, true
	}
}

// validateEventVocabulary is the data-minimization backstop. Session events are
// constructed only in this package, and this guard keeps additions to their
// attributes and string values inside the compiled-in vocabulary. It sources the
// allow/require sets, command names, and the error-category enum from the shared
// cliusage package.
func validateEventVocabulary(event Event) error {
	allowedAttributes, knownEvent := cliusage.AllowedAttributes[event.Name]
	if !knownEvent {
		return fmt.Errorf("telemetry event %q is not in the vocabulary", event.Name)
	}
	for key, value := range event.Data {
		if _, allowed := allowedAttributes[key]; !allowed {
			return fmt.Errorf("telemetry attribute %q is not in the vocabulary", key)
		}
		switch key {
		case cliusage.AttrCommands:
			commands, ok := value.([]string)
			if !ok {
				return fmt.Errorf("telemetry commands must be []string, got %T", value)
			}
			for _, command := range commands {
				if _, allowed := allowedCommands[command]; !allowed {
					return fmt.Errorf("telemetry command %q is not in the command registry", command)
				}
			}
		case cliusage.AttrErrorCategory:
			category, ok := value.(string)
			if !ok {
				return fmt.Errorf("telemetry error category must be string, got %T", value)
			}
			if !cliusage.IsErrorCategory(category) {
				return fmt.Errorf("telemetry error category %q is not in the vocabulary", category)
			}
		case cliusage.AttrProjects, cliusage.AttrJobs:
			if _, ok := value.(int); !ok {
				return fmt.Errorf("telemetry attribute %q must be int, got %T", key, value)
			}
		case cliusage.AttrDuration:
			if _, ok := value.(int64); !ok {
				return fmt.Errorf("telemetry duration must be int64, got %T", value)
			}
		default:
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("telemetry attribute %q must be bool, got %T", key, value)
			}
		}
	}
	for key := range cliusage.RequiredAttributes[event.Name] {
		if _, present := event.Data[key]; !present {
			return fmt.Errorf("telemetry event %q is missing required attribute %q", event.Name, key)
		}
	}
	if event.Name == cliusage.EventSessionEnd {
		success, ok := event.Data[cliusage.AttrSuccess].(bool)
		if !ok {
			return fmt.Errorf("telemetry success must be bool, got %T", event.Data[cliusage.AttrSuccess])
		}
		_, hasCategory := event.Data[cliusage.AttrErrorCategory]
		if success && hasCategory {
			return fmt.Errorf("successful telemetry session cannot have an error category")
		}
		if !success && !hasCategory {
			return fmt.Errorf("failed telemetry session must have an error category")
		}
	}
	return nil
}
