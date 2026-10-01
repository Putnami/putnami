package workspace

import (
	"encoding/json"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// --- bin: parsing both wire forms -------------------------------------------

func TestProjectConfig_BinString(t *testing.T) {
	c, diags := ParseProjectConfig([]byte(`{"name":"p","bin":"./cli.js"}`))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if c.Bin == nil || c.Bin.String != "./cli.js" || c.Bin.Map != nil {
		t.Fatalf("bin = %+v, want string form ./cli.js", c.Bin)
	}
}

func TestProjectConfig_BinObject(t *testing.T) {
	c, diags := ParseProjectConfig([]byte(`{"name":"p","bin":{"cli":"./cli.js","tool":"./tool.js"}}`))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if c.Bin == nil || c.Bin.Map == nil {
		t.Fatalf("bin = %+v, want object form", c.Bin)
	}
	if c.Bin.Map["cli"] != "./cli.js" || c.Bin.Map["tool"] != "./tool.js" {
		t.Errorf("bin map = %v", c.Bin.Map)
	}
}

// --- bin: strict decoding must bite -----------------------------------------

func TestProjectConfig_BinWrongType_Rejected(t *testing.T) {
	// A number is neither a string nor an object of strings.
	_, diags := ParseProjectConfig([]byte(`{"name":"p","bin":42}`))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for numeric bin (previously silently accepted as any)")
	}
}

func TestProjectConfig_BinNonStringValue_Rejected(t *testing.T) {
	// Object with a non-string value is invalid per the schema.
	_, diags := ParseProjectConfig([]byte(`{"name":"p","bin":{"cli":{"nested":"x"}}}`))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for object bin with non-string value")
	}
}

// --- exports: parsing both wire forms ---------------------------------------

func TestProjectConfig_ExportsString(t *testing.T) {
	c, diags := ParseProjectConfig([]byte(`{"name":"p","exports":"./index.js"}`))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if c.Exports == nil || c.Exports.String != "./index.js" || c.Exports.Map != nil {
		t.Fatalf("exports = %+v, want string form", c.Exports)
	}
}

func TestProjectConfig_ExportsObjectStringEntry(t *testing.T) {
	c, diags := ParseProjectConfig([]byte(`{"name":"p","exports":{".":"./index.js"}}`))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	entry := c.Exports.Map["."]
	if entry.String != "./index.js" || entry.Conditions != nil {
		t.Fatalf("entry = %+v, want string form", entry)
	}
}

func TestProjectConfig_ExportsConditionalEntry(t *testing.T) {
	input := `{"name":"p","exports":{".":{"types":"./index.d.ts","import":"./index.js","require":"./index.cjs","default":"./index.js"}}}`
	c, diags := ParseProjectConfig([]byte(input))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	entry := c.Exports.Map["."]
	if entry.Conditions == nil {
		t.Fatalf("entry = %+v, want conditional form", entry)
	}
	got := entry.Conditions
	if got.Types != "./index.d.ts" || got.Import != "./index.js" || got.Require != "./index.cjs" || got.Default != "./index.js" {
		t.Errorf("conditions = %+v", got)
	}
}

// --- exports: strict decoding must bite -------------------------------------

func TestProjectConfig_ExportsUnknownCondition_Rejected(t *testing.T) {
	// "browser" is not one of the closed condition keys — additionalProperties:false.
	// Previously this whole subtree escaped strict decoding as `any`.
	input := `{"name":"p","exports":{".":{"import":"./index.js","browser":"./browser.js"}}}`
	_, diags := ParseProjectConfig([]byte(input))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for unknown condition key 'browser'")
	}
}

func TestProjectConfig_ExportsWrongType_Rejected(t *testing.T) {
	// A number is neither a string nor an object.
	_, diags := ParseProjectConfig([]byte(`{"name":"p","exports":7}`))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for numeric exports")
	}
}

func TestProjectConfig_ExportsEntryWrongType_Rejected(t *testing.T) {
	// An array entry is neither a string nor a conditional object.
	_, diags := ParseProjectConfig([]byte(`{"name":"p","exports":{".":[1,2,3]}}`))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for array export entry")
	}
}

// --- round-trip determinism -------------------------------------------------

func TestProjectConfig_BinExportsRoundtrip(t *testing.T) {
	cases := map[string]string{
		"bin string":            `{"bin":"./cli.js","name":"p"}`,
		"bin object":            `{"bin":{"a":"./a.js","b":"./b.js"},"name":"p"}`,
		"exports string":        `{"exports":"./index.js","name":"p"}`,
		"exports object string": `{"exports":{".":"./index.js"},"name":"p"}`,
		"exports conditional":   `{"exports":{".":{"import":"./index.js","types":"./index.d.ts"}},"name":"p"}`,
		"exports mixed":         `{"exports":{".":{"default":"./index.js"},"./sub":"./sub.js"},"name":"p"}`,
	}

	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			var c ProjectConfig
			if err := json.Unmarshal([]byte(input), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out, err := json.Marshal(&c)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Canonicalize the input the same way encoding/json canonicalizes
			// (sorted map keys, no whitespace) so we compare canonical against
			// canonical. Re-parse and re-marshal a second time; the two
			// serializations must be byte-identical (stable round-trip).
			var c2 ProjectConfig
			if err := json.Unmarshal(out, &c2); err != nil {
				t.Fatalf("re-unmarshal: %v", err)
			}
			out2, err := json.Marshal(&c2)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if string(out) != string(out2) {
				t.Fatalf("round-trip not stable:\nfirst:  %s\nsecond: %s", out, out2)
			}
		})
	}
}

// TestProjectConfig_BinExportsMarshalDeterministic verifies re-serialization is
// byte-identical across many iterations (map-key ordering is stable).
func TestProjectConfig_BinExportsMarshalDeterministic(t *testing.T) {
	input := `{"bin":{"z":"./z.js","a":"./a.js","m":"./m.js"},"exports":{"./z":"./z.js","./a":{"import":"./a.js","types":"./a.d.ts"}},"name":"p"}`
	var c ProjectConfig
	if err := json.Unmarshal([]byte(input), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	canonical, err := json.Marshal(&c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := range 100 {
		got, err := json.Marshal(&c)
		if err != nil {
			t.Fatalf("iter %d marshal: %v", i, err)
		}
		if string(got) != string(canonical) {
			t.Fatalf("iter %d: non-deterministic marshal\ncanonical: %s\ngot:       %s", i, canonical, got)
		}
	}
}

// TestProjectConfig_BinExportsFormPreserved verifies the string form stays a
// string and the object form stays an object after a round-trip (no coercion).
func TestProjectConfig_BinExportsFormPreserved(t *testing.T) {
	var c ProjectConfig
	if err := json.Unmarshal([]byte(`{"name":"p","bin":"./cli.js","exports":"./index.js"}`), &c); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(&c)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["bin"]) != `"./cli.js"` {
		t.Errorf("bin re-serialized as %s, want string", raw["bin"])
	}
	if string(raw["exports"]) != `"./index.js"` {
		t.Errorf("exports re-serialized as %s, want string", raw["exports"])
	}
}
