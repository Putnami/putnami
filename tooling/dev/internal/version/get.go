package version

import (
	"context"
	"fmt"

	protocolcli "go.putnami.dev/protocol/cli"
)

// getCommand is the command path the result envelope of `version get` names.
const getCommand = "version get"

// getRecord is one version line in the result envelope of `version get`. Its
// fields are in the order of their JSON names.
type getRecord struct {
	Action  string `json:"action"` // "get"
	Line    string `json:"line"`   // the line's scope path; "" is the root line
	Tag     string `json:"tag,omitempty"`
	Tagged  bool   `json:"tagged,omitempty"`
	Version string `json:"version"`
}

// Get runs `version get [--scope <line>]` and returns the exit code. It
// prints one line per version line, "<line> <version>", or under json and
// jsonl one result envelope whose data is the one record, or the array of
// records when there are several. A failure under json and jsonl is a failure
// envelope on stdout and the message on stderr.
func (c Command) Get(ctx context.Context, args []string) int {
	records, err := c.get(ctx, args)
	if c.Output.IsStructured() {
		var data any
		if err == nil {
			data = records
			if len(records) == 1 {
				data = records[0]
			}
		}
		code, writeErr := protocolcli.WriteResultV2(c.Stdout, c.Output, protocolcli.NewResultV2(getCommand, data, err))
		if err != nil {
			fmt.Fprintln(c.Stderr, err)
		}
		if writeErr != nil {
			return c.report(writeErr)
		}
		return code
	}
	if err != nil {
		return c.report(err)
	}
	for _, record := range records {
		fmt.Fprintf(c.Stdout, "  %s %s\n", printableLine(record.Line), record.Version)
	}
	return protocolcli.ExitSuccess
}

// get reads the lines args select. A host that answers from a recorded
// dependency graph is said on stderr, and its answer is printed: reading a
// version must work offline and with a broken provider.
func (c Command) get(ctx context.Context, args []string) ([]getRecord, error) {
	parsed, rest, err := parseFlags(args)
	if err != nil {
		return nil, err
	}
	if len(rest) > 0 {
		return nil, protocolcli.Usagef("usage: putnami version get [--scope <line>]")
	}
	result, err := c.versionLines(ctx, parsed.Scope)
	if err != nil {
		return nil, err
	}
	if recorded := result.RecordedGraph; recorded != nil {
		fmt.Fprintf(c.Stderr, "version: the dependency graph could not be refreshed (%s); "+
			"answering from the recorded view observed at %s\n", recorded.RefreshError, recorded.ObservedAt)
	}
	records := make([]getRecord, 0, len(result.Lines))
	for _, line := range result.Lines {
		records = append(records, getRecord{Action: "get", Line: line.Line, Tag: line.Tag, Tagged: line.Tagged, Version: line.Version})
	}
	return records, nil
}
