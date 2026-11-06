package extensions

import (
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
)

// ActionRecords collects the outcome records of a structured install or
// update, in the order the command reports them. A nil collector records
// nothing.
type ActionRecords struct {
	entries []artifactActionEntry
}

func (r *ActionRecords) add(entry artifactActionEntry) {
	if r == nil {
		return
	}
	r.entries = append(r.entries, entry)
}

// WithActionRecords runs run with a collector under a structured outputFormat
// (--output=json|jsonl) and, once run succeeds, writes the collected records as
// the result of command (shared.RecordsData). A failed run writes nothing, so
// the dispatcher's failure result is the only document on stdout. Outside a
// structured format run receives a nil collector and nothing is written.
func WithActionRecords(outputFormat, command string, run func(records *ActionRecords) error) error {
	if !protocolcli.OutputMode(outputFormat).IsStructured() {
		return run(nil)
	}
	records := &ActionRecords{}
	if err := run(records); err != nil {
		return err
	}
	return shared.WriteResult(outputFormat, command, shared.RecordsData(records.entries), nil)
}
