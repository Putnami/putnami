package database

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func firstErrorCode(diags []diag.Diagnostic) string {
	for _, d := range diags {
		if d.Severity == diag.Error {
			return d.Code
		}
	}
	return ""
}

func boolPtr(v bool) *bool { return &v }

func TestEnumsValid(t *testing.T) {
	t.Run("engine", func(t *testing.T) {
		if !EnginePostgres.Valid() {
			t.Error("postgres should be valid")
		}
		for _, e := range []Engine{"", "mysql", "sqlite"} {
			if e.Valid() {
				t.Errorf("engine %q should be invalid", e)
			}
		}
	})
	t.Run("mode", func(t *testing.T) {
		for _, m := range []TestMode{TestModeSkip, TestModeRequire, TestModeAuto} {
			if !m.Valid() {
				t.Errorf("mode %q should be valid", m)
			}
		}
		for _, m := range []TestMode{"", "always"} {
			if m.Valid() {
				t.Errorf("mode %q should be invalid", m)
			}
		}
	})
	t.Run("isolation", func(t *testing.T) {
		for _, i := range []Isolation{IsolationDatabase, IsolationSchema} {
			if !i.Valid() {
				t.Errorf("isolation %q should be valid", i)
			}
		}
		for _, i := range []Isolation{"", "table"} {
			if i.Valid() {
				t.Errorf("isolation %q should be invalid", i)
			}
		}
	})
	t.Run("reuse", func(t *testing.T) {
		for _, r := range []Reuse{ReuseNone, ReuseBundleTemplate} {
			if !r.Valid() {
				t.Errorf("reuse %q should be valid", r)
			}
		}
		for _, r := range []Reuse{"", "cache"} {
			if r.Valid() {
				t.Errorf("reuse %q should be invalid", r)
			}
		}
	})
}

func TestValidErrorCodesPrefixed(t *testing.T) {
	if len(ValidErrorCodes) == 0 {
		t.Fatal("ValidErrorCodes is empty")
	}
	for code := range ValidErrorCodes {
		if !strings.HasPrefix(code, "database.") {
			t.Errorf("error code %q is not namespaced under database.", code)
		}
	}
}

func TestParse_MalformedJSON(t *testing.T) {
	bad := []byte("{")
	if _, diags := ParseAndValidateRequirementManifest(bad); firstErrorCode(diags) != ErrorCodeParseError {
		t.Errorf("requirement: want %s, got %v", ErrorCodeParseError, diags)
	}
	if _, diags := ParseAndValidateBinding(bad); firstErrorCode(diags) != ErrorCodeParseError {
		t.Errorf("binding: want %s, got %v", ErrorCodeParseError, diags)
	}
	if _, diags := ParseAndValidateTestBinding(bad); firstErrorCode(diags) != ErrorCodeParseError {
		t.Errorf("test binding: want %s, got %v", ErrorCodeParseError, diags)
	}
}

func TestValidate_NilGuards(t *testing.T) {
	if !diag.HasErrors(ValidateRequirementManifest(nil)) {
		t.Error("nil requirement manifest should error")
	}
	if !diag.HasErrors(ValidateBinding(nil)) {
		t.Error("nil binding should error")
	}
	if !diag.HasErrors(ValidateTestBinding(nil)) {
		t.Error("nil test binding should error")
	}
}

func TestValidateConnection(t *testing.T) {
	cases := []struct {
		name string
		conn *Connection
		code string // "" means valid
	}{
		{"nil", nil, ErrorCodeMissingConnection},
		{"empty", &Connection{}, ErrorCodeInvalidConnection},
		{"dsn only", &Connection{DSN: "postgres://localhost/db"}, ""},
		{"host only", &Connection{Host: "localhost"}, ""},
		{"instance only", &Connection{Instance: "p:r:i"}, ""},
		{"host full", &Connection{Host: "localhost", Port: 5432, Database: "db", User: "u", Password: "p", SSL: boolPtr(true), Params: map[string]string{"sslmode": "require"}}, ""},
		{"instance with creds", &Connection{Instance: "p:r:i", Database: "db", User: "u", Password: "p"}, ""},
		{"dsn and host", &Connection{DSN: "postgres://localhost/db", Host: "localhost"}, ErrorCodeInvalidConnection},
		{"host and instance", &Connection{Host: "localhost", Instance: "p:r:i"}, ErrorCodeInvalidConnection},
		{"dsn with user", &Connection{DSN: "postgres://localhost/db", User: "u"}, ErrorCodeInvalidConnection},
		{"dsn with port", &Connection{DSN: "postgres://localhost/db", Port: 5432}, ErrorCodeInvalidConnection},
		{"dsn with ssl false", &Connection{DSN: "postgres://localhost/db", SSL: boolPtr(false)}, ErrorCodeInvalidConnection},
		{"dsn with ssl true", &Connection{DSN: "postgres://localhost/db", SSL: boolPtr(true)}, ErrorCodeInvalidConnection},
		{"dsn with params", &Connection{DSN: "postgres://localhost/db", Params: map[string]string{"x": "y"}}, ErrorCodeInvalidConnection},
		{"instance with port", &Connection{Instance: "p:r:i", Port: 5432}, ErrorCodeInvalidConnection},
		{"instance with ssl", &Connection{Instance: "p:r:i", SSL: boolPtr(false)}, ErrorCodeInvalidConnection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := firstErrorCode(validateConnection("connection", tc.conn))
			if got != tc.code {
				t.Errorf("got code %q, want %q", got, tc.code)
			}
		})
	}
}

func TestValidationErrorCodes(t *testing.T) {
	cases := []struct {
		name string
		json string
		kind string // requirement | binding | test
		code string
	}{
		{"bad version", `{"protocolVersion":2}`, "requirement", ErrorCodeInvalidProtocolVersion},
		{"unknown field", `{"protocolVersion":1,"nope":1}`, "requirement", ErrorCodeUnknownField},
		{"missing engine", `{"protocolVersion":1,"databases":{"a":{"schema":"s"}}}`, "requirement", ErrorCodeMissingEngine},
		{"invalid engine", `{"protocolVersion":1,"databases":{"a":{"engine":"mongo","schema":"s"}}}`, "requirement", ErrorCodeInvalidEngine},
		{"missing schema", `{"protocolVersion":1,"databases":{"a":{"engine":"postgres"}}}`, "requirement", ErrorCodeMissingSchema},
		{"bad name", `{"protocolVersion":1,"databases":{"A B":{"engine":"postgres","schema":"s"}}}`, "requirement", ErrorCodeInvalidName},
		{"bad schema", `{"protocolVersion":1,"databases":{"a":{"engine":"postgres","schema":"1x","connection":{"dsn":"d"}}}}`, "binding", ErrorCodeInvalidSchema},
		{"missing connection", `{"protocolVersion":1,"databases":{"a":{"engine":"postgres","schema":"s"}}}`, "binding", ErrorCodeMissingConnection},
		{"invalid mode", `{"protocolVersion":1,"mode":"always","databases":{"a":{"engine":"postgres","schema":"s","connection":{"dsn":"d"}}}}`, "test", ErrorCodeInvalidMode},
		{"invalid isolation", `{"protocolVersion":1,"isolation":"table","databases":{"a":{"engine":"postgres","schema":"s","connection":{"dsn":"d"}}}}`, "test", ErrorCodeInvalidIsolation},
		{"invalid reuse", `{"protocolVersion":1,"reuse":"cache","databases":{"a":{"engine":"postgres","schema":"s","connection":{"dsn":"d"}}}}`, "test", ErrorCodeInvalidReuse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags []diag.Diagnostic
			switch tc.kind {
			case "requirement":
				_, diags = ParseAndValidateRequirementManifest([]byte(tc.json))
			case "binding":
				_, diags = ParseAndValidateBinding([]byte(tc.json))
			case "test":
				_, diags = ParseAndValidateTestBinding([]byte(tc.json))
			}
			if got := firstErrorCode(diags); got != tc.code {
				t.Errorf("got %q, want %q (%v)", got, tc.code, diags)
			}
		})
	}
}

func TestProject(t *testing.T) {
	if got := (*RequirementManifest)(nil).Project(); got != nil {
		t.Errorf("nil manifest: want nil, got %v", got)
	}
	if got := (&RequirementManifest{ProtocolVersion: 1}).Project(); got != nil {
		t.Errorf("empty manifest: want nil, got %v", got)
	}

	m := &RequirementManifest{
		ProtocolVersion: 1,
		Databases: map[string]Requirement{
			"billing": {Engine: EnginePostgres, Schema: "billing"},
			"auth":    {Engine: EnginePostgres, Schema: "iam"},
		},
	}
	want := []ProjectedDatabase{
		{Name: "auth", Engine: EnginePostgres, Schemas: []string{"iam"}},
		{Name: "billing", Engine: EnginePostgres, Schemas: []string{"billing"}},
	}
	if got := m.Project(); !reflect.DeepEqual(got, want) {
		t.Errorf("Project() = %+v, want %+v", got, want)
	}

	// A datasource without a schema projects to nil Schemas (no implicit schema).
	noSchema := &RequirementManifest{
		ProtocolVersion: 1,
		Databases:       map[string]Requirement{"a": {Engine: EnginePostgres}},
	}
	got := noSchema.Project()
	if len(got) != 1 || got[0].Schemas != nil {
		t.Errorf("no-schema projection = %+v, want one entry with nil Schemas", got)
	}
}

func TestBindingRoundTrip(t *testing.T) {
	data, err := os.ReadFile("fixtures/binding/valid/full.json")
	if err != nil {
		t.Fatal(err)
	}
	b, diags := ParseAndValidateBinding(data)
	if diag.HasErrors(diags) {
		t.Fatalf("parse valid binding: %v", diags)
	}
	if b == nil || len(b.Databases) != 3 {
		t.Fatalf("want 3 datasources, got %+v", b)
	}
	if b.Databases["billing"].Connection.Instance != "project:region:billing-db" {
		t.Errorf("billing instance not preserved: %+v", b.Databases["billing"].Connection)
	}

	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, diags := ParseAndValidateBinding(out)
	if diag.HasErrors(diags) {
		t.Fatalf("re-parse marshaled binding: %v", diags)
	}
	if !reflect.DeepEqual(b, b2) {
		t.Errorf("round-trip mismatch:\n%+v\n%+v", b, b2)
	}
}
