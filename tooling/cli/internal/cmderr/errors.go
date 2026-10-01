// Package cmderr re-exports the CLI error taxonomy owned by
// go.putnami.dev/protocol/cli. The failure vocabulary (ErrUsage, ErrNotFound,
// …) and the policy mapping each class to a process exit code live in the
// protocol, so every CLI surface — the framework CLI, the extension SDK, and
// cloud's cli-core — classifies failures the same way.
//
// This package keeps the short, ergonomic names the framework CLI already uses
// throughout; new code may equally import the protocol directly. Because the
// sentinels are the same underlying values, an error classified here matches
// errors.Is against the protocol's sentinels and vice versa.
package cmderr

import protocolcli "go.putnami.dev/protocol/cli"

// Sentinel error classes. See go.putnami.dev/protocol/cli for the exit code
// each maps to via ExitCodeForError (usage/not-found/no-match/invalid-config
// all fold into the Usage code; auth and api have their own).
var (
	// ErrUsage: the command was invoked incorrectly.
	ErrUsage = protocolcli.ErrUsage
	// ErrNoWorkspace: a command required a workspace but none was found.
	ErrNoWorkspace = protocolcli.ErrNoWorkspace
	// ErrNotFound: a named entity does not exist.
	ErrNotFound = protocolcli.ErrNotFound
	// ErrNoMatch: a selector or query matched nothing.
	ErrNoMatch = protocolcli.ErrNoMatch
	// ErrInvalidConfig: configuration or manifest validation failed.
	ErrInvalidConfig = protocolcli.ErrInvalidConfig
	// ErrAuth: authentication or authorization failed.
	ErrAuth = protocolcli.ErrAuth
	// ErrAPI: a remote or upstream API call failed.
	ErrAPI = protocolcli.ErrAPI
)

// Classify tags err with one or more sentinel classes so callers can
// errors.Is against them without changing the message err presents. Returns
// nil when err is nil. See protocolcli.Classify.
func Classify(err error, classes ...error) error {
	return protocolcli.Classify(err, classes...)
}

// Usagef builds an ErrUsage-classified error with a formatted message.
func Usagef(format string, a ...any) error { return protocolcli.Usagef(format, a...) }

// NotFoundf builds an ErrNotFound-classified error with a formatted message.
func NotFoundf(format string, a ...any) error { return protocolcli.NotFoundf(format, a...) }

// InvalidConfigf builds an ErrInvalidConfig-classified error with a formatted
// message.
func InvalidConfigf(format string, a ...any) error { return protocolcli.InvalidConfigf(format, a...) }

// Authf builds an ErrAuth-classified error with a formatted message.
func Authf(format string, a ...any) error { return protocolcli.Authf(format, a...) }

// APIf builds an ErrAPI-classified error with a formatted message.
func APIf(format string, a ...any) error { return protocolcli.APIf(format, a...) }
