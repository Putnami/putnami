package workspace

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestGeneratedFrom_AValidBindingDecodes(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-client-names-its-provider", "a-valid-generated-from-decodes")
	for name, body := range map[string]string{
		"go": `{"name":"c","generatedFrom":{"service":"items","language":"go","packageName":"itemsclient","clientName":"ItemsClient","omitOperations":["deleteItem"]}}`,
		"ts": `{"name":"c","generatedFrom":{"service":"items","language":"ts"}}`,
	} {
		cfg, diags := ParseAndValidateProjectConfig([]byte(body))
		if len(diags) != 0 {
			t.Fatalf("%s: diagnostics = %v", name, diags)
		}
		if cfg.GeneratedFrom == nil || cfg.GeneratedFrom.Service != "items" || cfg.GeneratedFrom.Language != name {
			t.Fatalf("%s: generatedFrom = %+v", name, cfg.GeneratedFrom)
		}
	}
}

func TestGeneratedFrom_AnInvalidBindingIsRefusedWithItsField(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-client-names-its-provider", "an-invalid-generated-from-is-refused-with-its-field")
	cases := map[string]struct {
		body   string
		fields []string
	}{
		"missing service":  {`{"name":"c","generatedFrom":{"language":"go"}}`, []string{"generatedFrom.service"}},
		"unknown language": {`{"name":"c","generatedFrom":{"service":"items","language":"rust"}}`, []string{"generatedFrom.language"}},
		"go options on ts": {`{"name":"c","generatedFrom":{"service":"items","language":"ts","packageName":"p","clientName":"C","omitOperations":["x"]}}`, []string{"generatedFrom.clientName", "generatedFrom.omitOperations", "generatedFrom.packageName"}},
		"bad go names":     {`{"name":"c","generatedFrom":{"service":"items","language":"go","packageName":"items-client","clientName":"1Client"}}`, []string{"generatedFrom.packageName", "generatedFrom.clientName"}},
		"blank omission":   {`{"name":"c","generatedFrom":{"service":"items","language":"go","omitOperations":[" "]}}`, []string{"generatedFrom.omitOperations[0]"}},
		"unknown member":   {`{"name":"c","generatedFrom":{"service":"items","language":"go","module":"x"}}`, []string{""}},
	}
	for name, c := range cases {
		_, diags := ParseAndValidateProjectConfig([]byte(c.body))
		if len(diags) != len(c.fields) {
			t.Fatalf("%s: diagnostics = %v, want fields %v", name, diags, c.fields)
		}
		for i, field := range c.fields {
			if diags[i].Field != field {
				t.Fatalf("%s: diagnostic %d field = %q, want %q (%v)", name, i, diags[i].Field, field, diags)
			}
		}
	}
}
