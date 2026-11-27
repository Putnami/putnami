package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// walkSanitizedJSON is the token walk the recorder ran before
// sanitizeMarshaledJSON: it decodes every member and value. The tests hold the
// scan to its bytes.
func walkSanitizedJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var compact bytes.Buffer
	if err := writeSanitizedJSONValue(decoder, &compact); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

func writeSanitizedJSONValue(decoder *json.Decoder, out *bytes.Buffer) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch typed := token.(type) {
	case json.Delim:
		switch typed {
		case '{':
			out.WriteByte('{')
			first := true
			for decoder.More() {
				nameToken, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := nameToken.(string)
				if !ok {
					return fmt.Errorf("machine output object member is %T", nameToken)
				}
				if !first {
					out.WriteByte(',')
				}
				first = false
				encodedName, _ := json.Marshal(name)
				out.Write(encodedName)
				out.WriteByte(':')
				if replacement, sensitive := protocolcli.MachineOutputMemberRedaction([]byte(name)); sensitive {
					var discarded any
					if err := decoder.Decode(&discarded); err != nil {
						return err
					}
					encoded, _ := json.Marshal(replacement)
					out.Write(encoded)
					continue
				}
				if err := writeSanitizedJSONValue(decoder, out); err != nil {
					return err
				}
			}
			if _, err := decoder.Token(); err != nil {
				return err
			}
			out.WriteByte('}')
		case '[':
			out.WriteByte('[')
			first := true
			for decoder.More() {
				if !first {
					out.WriteByte(',')
				}
				first = false
				if err := writeSanitizedJSONValue(decoder, out); err != nil {
					return err
				}
			}
			if _, err := decoder.Token(); err != nil {
				return err
			}
			out.WriteByte(']')
		default:
			return fmt.Errorf("unexpected machine output delimiter %q", typed)
		}
	case string:
		encoded, _ := json.Marshal(protocolcli.SanitizeMachineOutputString(typed))
		out.Write(encoded)
	case json.Number:
		out.WriteString(typed.String())
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case nil:
		out.WriteString("null")
	default:
		return fmt.Errorf("unexpected machine output token %T", token)
	}
	return nil
}

// machineOutputSamples are strings sanitization v1 rewrites or must look at:
// controls, terminal sequences, C1 controls, invalid UTF-8, a private key
// header and a match of every token pattern.
var machineOutputSamples = []string{
	"",
	"plain/path/to/file.go",
	"2026-11-27T07:14:54.123Z",
	"tab\tline\nreturn\r",
	"bell\x07 nul\x00 del\x7f",
	"\x1b[31mred\x1b[0m",
	"\x1b]0;title\x07after",
	"\x1bPdcs\x1b\\after",
	"\u009b31mC1 CSI",
	"\u009dosc\u009cafter",
	"\u0085next line",
	"invalid \xff\xfe utf8",
	"html <a href=\"x\">&amp;</a>",
	"line\u2028separator\u2029",
	"-----BEGIN " + "PRIVATE KEY-----\nMII",
	"-----BEGIN " + "RSA PRIVATE KEY-----",
	"key AKIA" + "ABCDEFGHIJKLMNOP end",
	"key ASIA" + "ABCDEFGHIJKLMNOP end",
	"ghp_" + strings.Repeat("a", 36),
	"x ghs_" + strings.Repeat("B", 40) + " y",
	"github_pat_" + strings.Repeat("c", 22),
	"glpat-" + strings.Repeat("d", 20) + ".x1",
	"AIza" + strings.Repeat("e", 35),
	"xoxb-" + strings.Repeat("1", 10),
	"sk_live_" + strings.Repeat("f", 16),
	"sk_test_" + strings.Repeat("g", 20),
	"high through enough gh_ ghx_ xox sk_",
	"é unicode ✓",
}

var machineOutputNames = []string{
	"type", "level", "message", "data", "path", "token", "Access-Token", "api_key",
	"client.secret", "Authorization", "SECRET", "credentials", "x\u001bname", "\xffname", "a\"b",
}

// randomMachineOutputJSON writes a random document the way json.Marshal
// writes one: compact, with every string encoded by json.Marshal.
func randomMachineOutputJSON(r *rand.Rand, depth int) []byte {
	encode := func(text string) []byte {
		encoded, _ := json.Marshal(text)
		return encoded
	}
	switch n := r.Intn(9); {
	case n == 0:
		return rawMachineOutputString(r)
	case depth > 3 || n < 3:
		return encode(randomMachineOutputString(r))
	case n == 3:
		return []byte(fmt.Sprint(r.Int63() - r.Int63()))
	case n == 4:
		encoded, _ := json.Marshal(r.Float64() * 1e6)
		return encoded
	case n == 5:
		return []byte(fmt.Sprint(r.Intn(2) == 0))
	case n == 6:
		return []byte("null")
	case n == 7:
		items := make([][]byte, r.Intn(4))
		for i := range items {
			items[i] = randomMachineOutputJSON(r, depth+1)
		}
		return append(append([]byte("["), bytes.Join(items, []byte(","))...), ']')
	default:
		members := make([][]byte, r.Intn(5))
		for i := range members {
			name := machineOutputNames[r.Intn(len(machineOutputNames))]
			if r.Intn(3) == 0 {
				name = randomMachineOutputString(r)
			}
			encodedName := encode(name)
			if r.Intn(6) == 0 {
				encodedName = rawMachineOutputString(r)
			}
			members[i] = append(append(encodedName, ':'), randomMachineOutputJSON(r, depth+1)...)
		}
		return append(append([]byte("{"), bytes.Join(members, []byte(","))...), '}')
	}
}

// rawMachineOutputString writes a string as a json.Marshaler or a
// json.RawMessage can, and json.Marshal then copies: its bytes as they are,
// invalid UTF-8 and C1 controls included, with no escape.
func rawMachineOutputString(r *rand.Rand) []byte {
	out := []byte{'"'}
	for i := r.Intn(12); i > 0; i-- {
		c := byte(0x20 + r.Intn(0xe0))
		if c == '"' || c == '\\' {
			c = 'q'
		}
		out = append(out, c)
	}
	return append(out, '"')
}

func randomMachineOutputString(r *rand.Rand) string {
	var b strings.Builder
	for i := r.Intn(4); i >= 0; i-- {
		if r.Intn(2) == 0 {
			b.WriteString(machineOutputSamples[r.Intn(len(machineOutputSamples))])
			continue
		}
		for j := r.Intn(8); j > 0; j-- {
			b.WriteByte(byte(r.Intn(256)))
		}
	}
	return b.String()
}

// requireSameSanitizedJSON compares the scan with the walk on document as
// json.Marshal writes it inside a json.RawMessage: compacted, with <, >, &,
// U+2028 and U+2029 escaped, and its other bytes, invalid UTF-8 included,
// copied as they are.
func requireSameSanitizedJSON(t *testing.T, document []byte) {
	t.Helper()
	raw, err := json.Marshal(json.RawMessage(document))
	if err != nil {
		return // not JSON: json.Marshal refuses it before any sanitizing
	}
	want, err := walkSanitizedJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sanitizeMarshaledJSON(raw)
	if err != nil {
		t.Fatalf("scan refused %s: %v", raw, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("scan and walk differ for %s:\nscan %s\nwalk %s", raw, got, want)
	}
	// The protocol's own value sanitizer, on the decoded document, is an
	// oracle that shares no code with either.
	var decoded, scanned any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &scanned); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scanned, protocolcli.SanitizeMachineOutputValue(decoded)) {
		t.Fatalf("scan of %s is not the sanitized document: %s", raw, got)
	}
}

func marshaled(t *testing.T, document any) []byte {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSanitizeMarshaledJSONMatchesTheTokenWalk(t *testing.T) {
	for _, sample := range machineOutputSamples {
		encoded := marshaled(t, sample)
		requireSameSanitizedJSON(t, encoded)
		requireSameSanitizedJSON(t, []byte(`{`+string(encoded)+`:`+string(encoded)+`,"token":{"x":"]}\"[{"}}`))
		requireSameSanitizedJSON(t, []byte(`[{"password":`+string(encoded)+`},`+string(encoded)+`]`))
	}
	for _, document := range []string{
		`{}`, `[]`, `""`, `-1.5e+10`, "\"\x9b31mred\"", "{\"\xfftoken\":\"\xff\",\"a\xc2\x9bb\":[\"\xc2\x85\"]}",
		`{"password":["a",{"b":"}"}],"secret":null,"apiKey":12.5e3,"x":{"Token":{"token":"y"}},"z":"ghp_` + strings.Repeat("a", 36) + `"}`,
	} {
		requireSameSanitizedJSON(t, []byte(document))
	}
	r := rand.New(rand.NewSource(1565))
	for i := 0; i < 5000; i++ {
		requireSameSanitizedJSON(t, randomMachineOutputJSON(r, 0))
	}
}

func FuzzSanitizeMarshaledJSONMatchesTheTokenWalk(f *testing.F) {
	for _, sample := range machineOutputSamples {
		f.Add(sample, "message")
	}
	f.Add("value", "Refresh_Token")
	f.Fuzz(func(t *testing.T, value, name string) {
		encodedValue, _ := json.Marshal(value)
		encodedName, _ := json.Marshal(name)
		v, n := string(encodedValue), string(encodedName)
		requireSameSanitizedJSON(t, []byte(`{`+n+`:`+v+`,"list":[`+v+`,{`+n+`:[`+v+`,{`+n+`:`+v+`}]}]}`))
		if !strings.ContainsAny(value+name, "\"\\") && !strings.ContainsFunc(value+name, func(r rune) bool { return r < 0x20 }) {
			// As a json.RawMessage would carry them: the bytes as they are.
			requireSameSanitizedJSON(t, []byte(`{"`+name+`":["`+value+`"]}`))
		}
	})
}

func TestSanitizedSessionRecordClassifiesLikeTheDecodedLine(t *testing.T) {
	for _, line := range []string{
		`{"protocolVersion":2,"record":"task:start","time":"t"}`,
		`{"protocolVersion":2,"record":"task:end","time":"t"}`,
		`{"protocolVersion":2,"record":"test:case","time":"t"}`,
		`{"protocolVersion":2,"record":"task:event","time":"t","event":{"type":"log","level":"debug"}}`,
		`{"protocolVersion":2,"record":"task:event","time":"t","event":{"type":"diagnostic","severity":"error","token":"x"}}`,
		`{"protocolVersion":2,"record":"task:event","time":"t","event":{"type":"result","data":{"status":"FAILED"}}}`,
	} {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		sanitizedLine, classified, err := sanitizedSessionRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		var decoded protocolcli.SessionStreamRecord
		if err := json.Unmarshal(sanitizedLine, &decoded); err != nil {
			t.Fatal(err)
		}
		if protocolcli.IsMachineOutputLifecycle(classified) != protocolcli.IsMachineOutputLifecycle(decoded) ||
			protocolcli.IsMachineOutputFailurePriority(classified) != protocolcli.IsMachineOutputFailurePriority(decoded) ||
			protocolcli.IsMachineOutputDebugDetail(classified) != protocolcli.IsMachineOutputDebugDetail(decoded) {
			t.Errorf("record %s is classified unlike its decoded line", sanitizedLine)
		}
	}
}
