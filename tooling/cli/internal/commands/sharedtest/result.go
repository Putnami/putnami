package sharedtest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// ResultData decodes out as the one successful version-2 result a structured
// command writes, checks that it names command, and decodes its data into v.
func ResultData(t *testing.T, out, command string, v any) {
	t.Helper()
	data := resultData(t, out, command)
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode the data of %q: %v\n%s", command, err, data)
	}
}

// ResultRecords decodes out as ResultData does and returns the records of a
// list result: the elements of an array data, or the data itself when it is
// one record. An empty command accepts any command name.
func ResultRecords(t *testing.T, out, command string) []json.RawMessage {
	t.Helper()
	data := resultData(t, out, command)
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) {
		return []json.RawMessage{data}
	}
	var records []json.RawMessage
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatalf("decode the records of %q: %v\n%s", command, err, data)
	}
	return records
}

func resultData(t *testing.T, out, command string) json.RawMessage {
	t.Helper()
	var result struct {
		ProtocolVersion int             `json:"protocolVersion"`
		Command         string          `json:"command"`
		Status          string          `json:"status"`
		Data            json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(out))
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("stdout is not a result envelope: %v\n%s", err, out)
	}
	if decoder.More() {
		t.Fatalf("stdout holds more than one document:\n%s", out)
	}
	if result.ProtocolVersion != protocolcli.ResultProtocolVersion || (command != "" && result.Command != command) ||
		result.Status != protocolcli.StatusSuccess {
		t.Fatalf("result = version %d, command %q, status %q; want a successful version-%d %q result\n%s",
			result.ProtocolVersion, result.Command, result.Status, protocolcli.ResultProtocolVersion, command, out)
	}
	return result.Data
}
