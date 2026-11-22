package docexamples

import (
	"reflect"
	"testing"
)

func TestBlocks(t *testing.T) {
	markdown := "intro\n" +
		"```go nocheck\n" +
		"a()\n" +
		"```\n" +
		"- item\n" +
		"  ````ts\n" +
		"  const a = '```';\n" +
		"  ````\n" +
		"~~~\n" +
		"plain\n"
	got := Blocks(markdown)
	want := []Block{
		{Lang: "go", Attributes: []string{"nocheck"}, Line: 2, Code: "a()"},
		{Lang: "ts", Attributes: []string{}, Line: 6, Code: "const a = '```';"},
		{Lang: "", Line: 9, Code: "plain\n"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Blocks() = %#v, want %#v", got, want)
	}
	if got[0].Checked() || !got[1].Checked() {
		t.Fatal("the nocheck word opts a block out, and only that word")
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"go/framework/**/AI.md", "go/framework/http/AI.md", true},
		{"go/framework/**/AI.md", "go/framework/migration/migratecli/AI.md", true},
		{"go/doc/**/*.md", "go/doc/x.md", true},
		{"**/adr/**", "go/framework/grpc/doc/adr/0001-x.md", true},
		{"go/framework/*/AI.md", "go/framework/migration/migratecli/AI.md", false},
		{"go/doc/**/*.md", "go/doc/x.txt", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}
