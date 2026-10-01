package gocacheprog

import "time"

// Go's GOCACHEPROG wire contract, re-declared.
//
// GOCACHEPROG names a program the `go` command starts as a child and asks to
// back its build cache, one JSON object per line over the child's stdin and
// stdout. The definitions live in cmd/go/internal/cacheprog, which is internal
// to the Go distribution, so a helper outside it cannot import them and must
// restate them. Everything below is copied from that package (Go 1.24+, the
// release where GOCACHEPROG left GOEXPERIMENT), including the JSON shape: the
// fields carry no tags there, so their Go names ARE the wire names and renaming
// one here silently breaks the protocol rather than failing to compile.
//
// The framing, for the same reason:
//
//   - the child writes one response with ID 0 and KnownCommands as its FIRST
//     output, before any request; the go command waits for it and refuses a
//     helper that declares no command;
//   - each request is one JSON line; a "put" with BodySize > 0 is followed by
//     the body on the NEXT line, as a base64-encoded JSON string literal;
//   - each response is one JSON line, and responses may be written in any order
//     as long as each echoes the ID of the request it answers.

// command is a GOCACHEPROG request type.
type command string

const (
	// commandGet asks for the object stored under an action id. A lookup that
	// finds nothing answers Miss rather than an error.
	commandGet command = "get"
	// commandPut stores an object under an action id. The response must carry a
	// DiskPath the go command can read the body back from.
	commandPut command = "put"
	// commandClose asks the helper to finish and exit. The go command waits for
	// the response and then for the helper's stdout to close.
	commandClose command = "close"
)

// knownCommands is the capability set this helper advertises. The go command
// only sends commands that appear here, so this list is the version
// negotiation.
var knownCommands = []command{commandGet, commandPut, commandClose}

// request is one line from the go command.
type request struct {
	// ID is unique per go process and must be echoed in the response.
	ID int64
	// Command is the request type.
	Command command
	// ActionID is the cache key of a "get" or "put".
	ActionID []byte `json:",omitempty"`
	// OutputID identifies the body of a "put". The go command derives it as the
	// SHA-256 of the body, which is what makes the body content-addressed.
	OutputID []byte `json:",omitempty"`
	// BodySize is the byte length of a "put" body. Zero means no body line
	// follows.
	BodySize int64 `json:",omitempty"`
}

// response is one line back to the go command.
type response struct {
	// ID echoes the request being answered; 0 marks the initial capability
	// message.
	ID int64
	// Err reports a failed request. A cache MISS is not an error (see Miss).
	Err string `json:",omitempty"`
	// KnownCommands is set only in the initial ID 0 message.
	KnownCommands []command `json:",omitempty"`

	// Miss reports that a "get" found nothing.
	Miss bool `json:",omitempty"`
	// OutputID is the output id stored with the body, on a "get" hit.
	OutputID []byte `json:",omitempty"`
	// Size is the body's byte length, on a "get" hit.
	Size int64 `json:",omitempty"`
	// Time is when the object entered the cache. A nil time makes the go
	// command assume "now", which would hide every entry's real age from its
	// expiry logic.
	Time *time.Time `json:",omitempty"`
	// DiskPath is the absolute path of a file whose content IS the body, for a
	// "get" hit and for every "put". The go command reads compiled output
	// straight from it, so the file must stay valid until the helper exits.
	DiskPath string `json:",omitempty"`
}
