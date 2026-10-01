package cli

import (
	"errors"
	"fmt"
)

// Error classes for CLI failures. A producer tags an error with one of these
// so the dispatcher can map it to the matching exit code via ExitCodeForError,
// and Go callers can errors.Is against a stable cause. The sentinels carry no
// exit code themselves — ExitCodeForError owns the class→code policy, keeping
// the failure vocabulary and the exit-code assignment in one place.
var (
	// ErrUsage: the command was invoked incorrectly — an unknown subcommand,
	// a missing required argument, or a flag that does not apply. → ExitUsage.
	ErrUsage = errors.New("usage error")

	// ErrNoWorkspace: a command required a workspace but none was found from
	// the current directory. → ExitUsage.
	ErrNoWorkspace = errors.New("no workspace found")

	// ErrNotFound: a named entity (project, template, session, version, …)
	// does not exist. → ExitUsage.
	ErrNotFound = errors.New("not found")

	// ErrNoMatch: a selector or query matched nothing. → ExitUsage.
	ErrNoMatch = errors.New("nothing matched")

	// ErrInvalidConfig: configuration or manifest validation failed.
	// → ExitUsage.
	ErrInvalidConfig = errors.New("invalid configuration")

	// ErrAuth: authentication or authorization failed. → ExitAuth.
	ErrAuth = errors.New("authentication failed")

	// ErrAPI: a remote or upstream API call failed. → ExitAPI.
	ErrAPI = errors.New("api error")

	// ErrSignal: a signal (Ctrl-C, SIGTERM) stopped the command before it
	// finished. Distinct from an unclassified failure: the work was cut short
	// from outside, so nothing can be concluded about what it was doing.
	// → ExitSignal.
	ErrSignal = errors.New("interrupted by signal")
)

// ExitCodeForError maps a (possibly classified) error to a process exit code.
// A nil error is ExitSuccess; an unclassified error is ExitFailure.
func ExitCodeForError(err error) int {
	switch {
	case err == nil:
		return ExitSuccess
	case errors.Is(err, ErrAuth):
		return ExitAuth
	case errors.Is(err, ErrAPI):
		return ExitAPI
	case errors.Is(err, ErrSignal):
		return ExitSignal
	case errors.Is(err, ErrUsage), errors.Is(err, ErrNoWorkspace),
		errors.Is(err, ErrNotFound), errors.Is(err, ErrNoMatch),
		errors.Is(err, ErrInvalidConfig):
		return ExitUsage
	default:
		return ExitFailure
	}
}

// ErrorCode returns the short, stable string code for the class carried by
// err, for the result envelope's error.code field. A nil error yields "".
func ErrorCode(err error) string {
	switch ExitCodeForError(err) {
	case ExitSuccess:
		return ""
	case ExitUsage:
		return "usage"
	case ExitAuth:
		return "auth"
	case ExitAPI:
		return "api"
	case ExitSignal:
		return "signal"
	default:
		return "failure"
	}
}

// Classify tags err with one or more sentinel classes so callers can
// errors.Is against them, without changing the message err already presents to
// the user. Returns nil when err is nil, so it is safe to wrap a call result
// directly: return cli.Classify(doThing(), cli.ErrNotFound).
func Classify(err error, classes ...error) error {
	if err == nil {
		return nil
	}
	return &classified{err: err, classes: classes}
}

// Usagef builds an ErrUsage-classified error with a formatted message.
func Usagef(format string, a ...any) error {
	return Classify(fmt.Errorf(format, a...), ErrUsage)
}

// NotFoundf builds an ErrNotFound-classified error with a formatted message.
func NotFoundf(format string, a ...any) error {
	return Classify(fmt.Errorf(format, a...), ErrNotFound)
}

// InvalidConfigf builds an ErrInvalidConfig-classified error with a formatted
// message.
func InvalidConfigf(format string, a ...any) error {
	return Classify(fmt.Errorf(format, a...), ErrInvalidConfig)
}

// Authf builds an ErrAuth-classified error with a formatted message.
func Authf(format string, a ...any) error {
	return Classify(fmt.Errorf(format, a...), ErrAuth)
}

// APIf builds an ErrAPI-classified error with a formatted message.
func APIf(format string, a ...any) error {
	return Classify(fmt.Errorf(format, a...), ErrAPI)
}

type classified struct {
	err     error
	classes []error
}

// Error returns the wrapped error's message unchanged so classification is
// invisible to anything that prints or compares the message.
func (c *classified) Error() string { return c.err.Error() }

// Unwrap exposes both the original cause and the sentinel classes to
// errors.Is / errors.As via the Go 1.20+ multi-error unwrap form.
func (c *classified) Unwrap() []error {
	out := make([]error, 0, len(c.classes)+1)
	out = append(out, c.err)
	return append(out, c.classes...)
}

// WithNext wraps err so it carries a suggested next command or doc link the
// user can run to recover, which SuggestedNext later reads back. Like Classify,
// it is invisible to anything that prints or compares the error: the message is
// unchanged and Unwrap exposes err, so it composes with Classify in either
// order (WithNext(Usagef(...), "putnami workspace init") and Classify(WithNext(
// err, ...), ErrAuth) both work) and errors.Is / errors.As still see the
// underlying class. Returns nil when err is nil.
//
// An empty next is a no-op: err is returned unwrapped. This keeps the invariant
// that every carrier holds a real suggestion, so an empty outer WithNext never
// shadows a suggestion set deeper in the chain (see SuggestedNext).
func WithNext(err error, next string) error {
	if err == nil {
		return nil
	}
	if next == "" {
		return err
	}
	return &withNext{err: err, next: next}
}

// SuggestedNext returns the suggested next command carried by err, walking the
// whole error tree (including multi-error Unwrap chains from Classify) and
// returning the first suggestion found, or "" when none is present. Because
// WithNext never wraps an empty suggestion, that first match is always the
// first non-empty suggestion in the tree. Nil-safe.
func SuggestedNext(err error) string {
	var w *withNext
	if errors.As(err, &w) {
		return w.next
	}
	return ""
}

type withNext struct {
	err  error
	next string
}

// Error returns the wrapped error's message unchanged so the suggestion is
// invisible to anything that prints or compares the message.
func (w *withNext) Error() string { return w.err.Error() }

// Unwrap exposes the wrapped cause so errors.Is / errors.As continue to see the
// underlying class through the suggestion carrier.
func (w *withNext) Unwrap() error { return w.err }
