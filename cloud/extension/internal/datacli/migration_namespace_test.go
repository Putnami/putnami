package datacli

import "testing"

func TestMigrationNamespaceFromOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options map[string]map[string]any
		want    string
		invalid bool
	}{
		{name: "absent"},
		{name: "precedence", options: map[string]map[string]any{"publish": {"namespace": "base"}, "@putnami/cloud:publish": {"namespace": "cloud"}, "@putnami/cloud:publish-migration": {"namespace": "migration"}}, want: "migration"},
		{name: "wrong type", options: map[string]map[string]any{"publish": {"namespace": 1}}, invalid: true},
		{name: "whitespace", options: map[string]map[string]any{"publish": {"namespace": " bad"}}, invalid: true},
		{name: "path", options: map[string]map[string]any{"publish": {"namespace": "a/b"}}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, declared, err := MigrationNamespaceFromOptions(tc.options)
			if (err != nil) != tc.invalid || got != tc.want || declared != (tc.want != "") {
				t.Fatalf("namespace=(%q,%v,%v), want (%q,%v)", got, declared, err, tc.want, tc.invalid)
			}
		})
	}
}
