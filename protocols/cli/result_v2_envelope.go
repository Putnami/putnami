package cli

import (
	"encoding/json"
	"fmt"
	"io"
)

// The v2 envelope CONSTRUCTOR and WRITER, for the commands that are not job
// runs.
//
// A job command's envelope is built by the CLI's machine projection, which
// derives status, exit code and the typed run summary from ONE reduction of the
// run. A non-job command has no run to reduce: it reports a payload and an
// error, exactly as the v1 NewResult/WriteResult pair did. These two functions
// are that pair on the v2 document, so every structured command emits
// `protocolVersion: 2` without each command growing its own mapping — the
// versionless v1 envelope B1a left on those surfaces has no spelling left.
//
// The verdict mapping is the contract's, not a convenience:
//
//   - a signal-classified error is status "aborted" with exit code 130, since
//     v2 reports an interrupted command distinctly instead of folding it into
//     "failure" (doc/02-result-v2.md, decision 2);
//   - every other error is "failure", carrying the class ErrorCode derives and
//     the exit code ExitCodeForError derives, which is what keeps the document
//     and the process agreeing;
//   - success carries no error member at all.
//
// ValidateDocument(DocumentResultEnvelope, …) accepts exactly these
// combinations, so a document built here cannot violate the contract it claims.

// NewResultV2 builds the version-2 result envelope from a command path, a
// payload and an error. It is the v2 spelling of NewResult: nil err is a
// success, a signal-classified err is an abort, anything else is a failure, and
// Data is attached either way.
func NewResultV2(command string, data any, err error) ResultV2 {
	r := ResultV2{
		ProtocolVersion: ResultProtocolVersion,
		Command:         command,
		Data:            data,
		ExitCode:        ExitCodeForError(err),
	}
	if err == nil {
		r.Status = StatusSuccess
		return r
	}
	code := ErrorCode(err)
	if code == errorCodeSignal {
		r.Status = StatusAborted
	} else {
		r.Status = StatusFailure
	}
	r.Error = &ResultError{Code: code, Message: err.Error(), Next: SuggestedNext(err)}
	return r
}

// errorCodeSignal is the error class an interrupted command carries. It is the
// one class whose envelope status is not "failure", which is why it is named
// here rather than compared as a literal at the branch.
const errorCodeSignal = "signal"

// WriteResultV2 renders the v2 envelope to w in the given mode and returns the
// exit code the caller should use. Structured modes write the document (indented
// for OutputJSON, one compact line for OutputJSONL); non-structured modes write
// nothing — the human renderers own that output — and only return the code.
func WriteResultV2(w io.Writer, mode OutputMode, r ResultV2) (int, error) {
	if !mode.IsStructured() {
		return r.ExitCode, nil
	}
	var (
		data []byte
		err  error
	)
	if mode == OutputJSON {
		data, err = json.MarshalIndent(r, "", "  ")
	} else {
		data, err = json.Marshal(r)
	}
	if err != nil {
		return r.ExitCode, err
	}
	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		return r.ExitCode, err
	}
	return r.ExitCode, nil
}
