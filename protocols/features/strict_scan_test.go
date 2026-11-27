package features

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// strictScanCases are documents whose version token and nulls the fast paths
// must judge exactly as the encoding/json walks do.
var strictScanCases = []string{
	``, ` `, `null`, `1`, `"x"`, `[]`, `[{"protocolVersion":1}]`, `{`, `{}`, ` { } `,
	`{"protocolVersion":1}`, `{"protocolVersion": 1 }`, "{\n\t\"protocolVersion\" :\r\n2\n}\n",
	`{"protocolVersion":1.0}`, `{"protocolVersion":"1"}`, `{"protocolVersion":01}`, `{"protocolVersion":1e0}`,
	`{"protocolVersion":-1}`, `{"protocolVersion":true}`, `{"protocolVersion":null}`, `{"protocolVersion":{}}`,
	`{"protocolVersion":[1]}`, `{"protocolVersion":1,"protocolVersion":1}`, `{"protocolVersion":1,"protocolVersion":`,
	`{"protocolVersion":1}`, `{"protocolVersion":1,"protocolVersion":1}`, `{"a\"b":1,"protocolVersion":1}`,
	`{"a":"}\"{[","protocolVersion":1}`, `{"a":{"protocolVersion":2},"protocolVersion":1}`,
	`{"a":[1,{"b":[null,"]"]}],"protocolVersion":3}`, `{"protocolVersion":1} {}`, `{"protocolVersion":1}x`,
	`{"protocolVersion":1,}`, `{"ProtocolVersion":1}`, `{"protocolVersion":1,"note":"nullable"}`,
	`{"protocolVersion":1,"a":null}`, `{"protocolVersion":1,"a":[1,null]}`, `{"protocolVersion":1,"b":{"c":null},"a":null}`,
	"{\"protocolVersion\":1,\"a\":\"\xff\"}",
	`1e400`, ` -1e309 `, `1` + strings.Repeat("0", 310), `{"protocolVersion":1,"a":1e400}`, `{"a":-1e309,"protocolVersion":1e400}`,
}

func TestScanProtocolVersionMatchesTokenWalk(t *testing.T) {
	for _, data := range strictScanDocuments(t) {
		for _, supported := range [][]int{{1}, {1, 2, 3}, {2}} {
			want := requireExactProtocolVersionTokens(data, supported...)
			got := requireExactProtocolVersion(data, supported...)
			if !sameStrictError(got, want) {
				t.Fatalf("%q %v: scan answered %v, token walk %v", clip(data), supported, got, want)
			}
		}
	}
}

func TestRejectExplicitNullsMatchesFullDecode(t *testing.T) {
	for _, data := range strictScanDocuments(t) {
		var value any
		if decodeStrictJSON(data, &value) != nil {
			continue // rejectExplicitNulls only ever runs after a strict decode accepted data
		}
		want := rejectExplicitNullsFull(data)
		got := rejectExplicitNulls(data)
		if !sameStrictError(got, want) {
			t.Fatalf("%q: fast path answered %v, full decode %v", clip(data), got, want)
		}
	}
}

// rejectExplicitNullsFull is rejectExplicitNulls without its fast path.
func rejectExplicitNullsFull(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return findExplicitNull(value, "")
}

// strictScanDocuments is the edge cases and every JSON declaration and
// fixture of this project.
func strictScanDocuments(t *testing.T) [][]byte {
	t.Helper()
	documents := make([][]byte, 0, len(strictScanCases))
	for _, raw := range strictScanCases {
		documents = append(documents, []byte(raw))
	}
	root := "."
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		documents = append(documents, data)
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if len(documents) < len(strictScanCases)+10 {
		t.Fatalf("found %d documents, want this project's declarations and fixtures", len(documents)-len(strictScanCases))
	}
	return documents
}

func sameStrictError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	var versionA, versionB protocolVersionError
	if errors.As(a, &versionA) != errors.As(b, &versionB) {
		return false
	}
	var nullA, nullB nullValueError
	if errors.As(a, &nullA) != errors.As(b, &nullB) || nullA.field != nullB.field {
		return false
	}
	return a.Error() == b.Error()
}

func clip(data []byte) string {
	if len(data) > 120 {
		return string(data[:120]) + "…"
	}
	return string(data)
}
