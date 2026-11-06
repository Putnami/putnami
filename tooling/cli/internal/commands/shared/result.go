package shared

import (
	"io"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// WriteResult writes the success result of a structured command to stdout: one
// version-2 result envelope that names command and carries data and the
// workspace warnings the command reported, indented under --output=json and on
// one line under --output=jsonl. It is the only document a structured command
// writes on stdout; a failure writes nothing there, and the dispatcher writes
// the failure envelope instead.
func WriteResult(outputFormat, command string, data any, warnings []protocolcli.WorkspaceWarning) error {
	return WriteResultTo(iox.Stdout(), outputFormat, command, data, warnings)
}

// WriteResultTo is WriteResult on the stream w.
func WriteResultTo(w io.Writer, outputFormat, command string, data any, warnings []protocolcli.WorkspaceWarning) error {
	result := protocolcli.NewResultV2(command, data, nil)
	result.WorkspaceWarnings = warnings
	_, err := protocolcli.WriteResultV2(w, protocolcli.OutputMode(outputFormat), result)
	return err
}

// RecordsData is the data of a structured command that reports a list of
// records: the record itself when there is exactly one, every record in order
// when there are more, and an empty array when there is none.
func RecordsData[T any](records []T) any {
	switch len(records) {
	case 0:
		return []T{}
	case 1:
		return records[0]
	default:
		return records
	}
}
