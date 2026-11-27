package job

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// The canonical decoder answers exactly what encoding/json answers for the
// bytes EncodeCandidateFiles writes, and declines everything else.
func TestDecodeCanonicalCandidateFilesMatchesEncodingJSON(t *testing.T) {
	cuts := [][]CandidateFile{
		{},
		{{Path: "a", Mode: CandidateModeUntracked}},
		{{Path: "README.md", Mode: "100644"}, {Path: "dir/é ü.txt", Mode: "100755"}, {Path: "link", Mode: "120000"}},
		{{Path: `quote"inside`, Mode: "100644"}},
		{{Path: `back\slash`, Mode: "100644"}},
		{{Path: "tab\there", Mode: "100644"}},
		{{Path: "<html>&", Mode: "100644"}},
		{{Path: "doc/03-jobs-&-caching.md", Mode: "100644"}, {Path: "a\u2028b\u2029c", Mode: "100644"}},
	}
	for _, cut := range cuts {
		data, _, err := EncodeCandidateFiles(cut)
		if err != nil {
			t.Fatal(err)
		}
		assertSameAsEncodingJSON(t, data)
	}
	for _, raw := range []string{
		``, `[`, `]`, `[]`, ` []`, `[] `, `[]x`, `null`, `{}`,
		`[{"path":"a","mode":"100644"}]`,
		`[{"path":"a","mode":"100644"},]`,
		`[{"path":"a","mode":"100644"}]trailing`,
		`[{"path":"a","mode":"100644"} ]`,
		`[{"mode":"100644","path":"a"}]`,
		`[{"path":"a","mode":"100644","extra":1}]`,
		`[{"Path":"a","mode":"100644"}]`,
		`[{"path":"ab","mode":"100644"}]`,
		"[{\"path\":\"\xff\",\"mode\":\"100644\"}]",
		`[{"path":"a","mode":"100644"},{"path":"b","mode":"160000"}]`,
		`[{"path":"a&b","mode":"100644"}]`,
		`[{"path":"a\u0026b","mode":"100644"}]`,
		`[{"path":"a\u0026","mode":"100644"}]`,
		`[{"path":"a\u0026\u003c\u003e","mode":"100644"}]`,
		`[{"path":"a\u003C","mode":"100644"}]`,
		`[{"path":"a\u0041","mode":"100644"}]`,
		`[{"path":"a\u002","mode":"100644"}]`,
		`[{"path":"a\","mode":"100644"}]`,
		`[{"path":"a\"b","mode":"100644"}]`,
		`[{"path":"a\\b","mode":"100644"}]`,
		"[{\"path\":\"a\u2028\",\"mode\":\"100644\"}]",
		`[{"path":"a\u2028","mode":"100644"}]`,
	} {
		assertSameAsEncodingJSON(t, []byte(raw))
	}
}

func assertSameAsEncodingJSON(t *testing.T, data []byte) {
	t.Helper()
	fast, ok := decodeCanonicalCandidateFiles(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var want []CandidateFile
	err := decoder.Decode(&want)
	if !ok {
		return
	}
	if err != nil {
		t.Fatalf("%q: the canonical decoder accepted what encoding/json refuses: %v", data, err)
	}
	if !reflect.DeepEqual(fast, want) {
		t.Fatalf("%q: canonical %#v, encoding/json %#v", data, fast, want)
	}
	// The canonical decoder accepts only what EncodeCandidateFiles writes.
	again, _, err := EncodeCandidateFiles(fast)
	if err != nil || !bytes.Equal(again, data) {
		t.Fatalf("%q: accepted bytes EncodeCandidateFiles does not write (%q)", data, again)
	}
}

// Every cut whose strings need no escape but the HTML-safe ones json.Marshal
// writes takes the canonical path.
func TestDecodeCanonicalCandidateFilesTakesPlainCuts(t *testing.T) {
	cut := []CandidateFile{
		{Path: "a/b.go", Mode: "100644"},
		{Path: "c", Mode: CandidateModeUntracked},
		{Path: "doc/03-jobs-&-caching.md", Mode: "100644"},
		{Path: "<a>\u2028\u2029", Mode: "100644"},
	}
	data, _, err := EncodeCandidateFiles(cut)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := decodeCanonicalCandidateFiles(data)
	if !ok || !reflect.DeepEqual(got, cut) {
		t.Fatalf("got %#v, %v", got, ok)
	}
}
