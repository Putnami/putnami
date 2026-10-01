package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
)

// TestCredentialFixtures runs every fixture through the Go validator: valid
// lines parse and re-encode byte for byte, invalid lines are refused. A
// response fixture names the op it answers: response-<op>-<case>.json.
func TestCredentialFixtures(t *testing.T) {
	t.Parallel()
	for _, validity := range []string{"valid", "invalid"} {
		paths, err := filepath.Glob(filepath.Join("fixtures", "credential-provider", validity, "*.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("fixture corpus: %v, %v", paths, err)
		}
		for _, path := range paths {
			name := filepath.Base(path)
			t.Run(validity+"/"+name, func(t *testing.T) {
				t.Parallel()
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				line := bytes.TrimSuffix(data, []byte("\n"))
				var decoded any
				switch {
				case strings.HasPrefix(name, "request-"):
					decoded, err = ParseCredentialRequest(line)
				case strings.HasPrefix(name, "response-"):
					op := CredentialOp(strings.SplitN(strings.TrimPrefix(name, "response-"), "-", 2)[0])
					op = CredentialOp(strings.TrimSuffix(string(op), ".json"))
					if !op.Valid() {
						t.Fatalf("fixture %s names no op", name)
					}
					decoded, err = ParseCredentialResponse(line, op)
				default:
					t.Fatalf("fixture %s is neither a request nor a response", name)
				}
				if validity == "invalid" {
					if err == nil {
						t.Fatalf("invalid fixture accepted: %s", line)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(decoded)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(encoded, line) {
					t.Fatalf("fixture is not the canonical encoding:\n%s\n%s", line, encoded)
				}
			})
		}
	}
}

// TestCredentialSchemaTracksWireShape keeps the JSON Schema and the Go
// contract in step: members, required lists, closed objects, enums, bounds
// and grammars.
func TestCredentialSchemaTracksWireShape(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("schemas", "credential-provider-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	type property struct {
		Const     *int     `json:"const"`
		Enum      []string `json:"enum"`
		Pattern   string   `json:"pattern"`
		MinLength int      `json:"minLength"`
		MaxLength int      `json:"maxLength"`
		MaxItems  int      `json:"maxItems"`
		Items     struct {
			Pattern   string `json:"pattern"`
			MaxLength int    `json:"maxLength"`
		} `json:"items"`
	}
	type definition struct {
		AdditionalProperties *bool               `json:"additionalProperties"`
		Required             []string            `json:"required"`
		Properties           map[string]property `json:"properties"`
		Pattern              string              `json:"pattern"`
		MaxItems             int                 `json:"maxItems"`
		Items                struct {
			Pattern string `json:"pattern"`
		} `json:"items"`
	}
	var schema struct {
		Definitions map[string]definition `json:"definitions"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	objects := map[string]struct {
		value    any
		required string
	}{
		"request":          {CredentialRequest{}, "protocolVersion,id,op"},
		"response":         {CredentialResponse{}, "protocolVersion,id,ok"},
		"initializeParams": {CredentialInitializeParams{}, "protocolVersion,capabilities"},
		"initializeResult": {CredentialInitializeResult{}, "protocolVersion,capabilities"},
		"credentialParams": {CredentialParams{}, "purpose"},
		"credentialResult": {CredentialResult{}, ""},
		"credential":       {Credential{}, "bearer,expiresAt,hosts"},
		"refusal":          {CredentialRefusal{}, "code"},
	}
	for name, object := range objects {
		def, ok := schema.Definitions[name]
		if !ok {
			t.Fatalf("schema lacks definition %s", name)
		}
		if def.AdditionalProperties == nil || *def.AdditionalProperties {
			t.Errorf("%s: schema object is not closed", name)
		}
		if got := strings.Join(def.Required, ","); got != object.required {
			t.Errorf("%s: required = %s, want %s", name, got, object.required)
		}
		if got, want := sortedKeys(def.Properties), jsonMembers(object.value); got != want {
			t.Errorf("%s: schema members %s, Go members %s", name, got, want)
		}
	}
	for _, name := range []string{"request", "response"} {
		if version := schema.Definitions[name].Properties["protocolVersion"].Const; version == nil || *version != CredentialProtocolVersion {
			t.Errorf("%s: protocolVersion const diverged", name)
		}
	}
	ops := []string{string(CredentialOpInitialize), string(CredentialOpCredential), string(CredentialOpShutdown)}
	if got := strings.Join(schema.Definitions["request"].Properties["op"].Enum, ","); got != strings.Join(ops, ",") {
		t.Errorf("op enum = %s", got)
	}
	if got := strings.Join(schema.Definitions["credentialParams"].Properties["purpose"].Enum, ","); got != PurposeRead+","+PurposePublish {
		t.Errorf("purpose enum = %s", got)
	}
	credential := schema.Definitions["credential"].Properties
	if credential["hosts"].Items.Pattern != CredentialHostPattern || credential["hosts"].Items.MaxLength != MaxCredentialHostBytes || credential["hosts"].MaxItems != MaxCredentialHosts {
		t.Error("hosts grammar or bounds diverged")
	}
	if credential["bearer"].Pattern != "^[!-~]+$" || credential["bearer"].MaxLength != MaxBearerBytes {
		t.Error("bearer grammar or bound diverged")
	}
	refusal := schema.Definitions["refusal"].Properties
	if refusal["code"].Pattern != RefusalCodePattern || refusal["message"].MaxLength != MaxRefusalMessageBytes {
		t.Error("refusal grammar or bound diverged")
	}
	const noControlCharacters = `^[^\x00-\x1f\x7f]*$`
	if refusal["message"].Pattern != noControlCharacters || schema.Definitions["initializeResult"].Properties["providerName"].Pattern != noControlCharacters {
		t.Error("the message and providerName text rule diverged from boundedText")
	}
	capabilities := schema.Definitions["capabilities"]
	if capabilities.Items.Pattern != CapabilityPattern || capabilities.MaxItems != MaxCredentialCapabilities {
		t.Error("capability grammar or bound diverged")
	}
	if schema.Definitions["initializeResult"].Properties["providerName"].MaxLength != MaxProviderNameBytes {
		t.Error("providerName bound diverged")
	}
	runCredential := schema.Definitions["initializeParams"].Properties["runCredential"]
	if runCredential.Pattern != RunCredentialPattern || runCredential.MinLength != 1 || runCredential.MaxLength != MaxRunCredentialBytes {
		t.Error("runCredential grammar or bounds diverged")
	}
}

// TestRunCredentialPatternIsValidBearer proves the schema grammar and the Go
// rule refuse the same characters: every rune unicode.IsSpace reports, and no
// other. unicode.IsSpace reports nothing above U+3000, so the sweep of the
// Basic Multilingual Plane and samples above it cover the whole range.
func TestRunCredentialPatternIsValidBearer(t *testing.T) {
	t.Parallel()
	pattern := regexp.MustCompile(RunCredentialPattern)
	for r := rune(0); r <= 0xffff; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		value := "prc" + string(r) + "x"
		if got, want := pattern.MatchString(value), ValidBearer(value); got != want {
			t.Errorf("U+%04X: pattern matches %v, ValidBearer %v", r, got, want)
		}
	}
	for _, r := range []rune{0x10000, 0x1f600, 0x10ffff} {
		if !pattern.MatchString(string(r)) || unicode.IsSpace(r) {
			t.Errorf("U+%04X: pattern and ValidBearer disagree", r)
		}
	}
}

func TestValidRunCredential(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"prc_x": true,
		"eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ4In0.sig": true,
		"prc_\u00e9": true,
		strings.Repeat("a", MaxRunCredentialBytes): true,
		"":           false,
		"prc x":      false,
		"prc\tx":     false,
		"prc_x\n":    false,
		"prc\u00a0x": false,
		"prc\u3000x": false,
		"prc_\xff":   false,
		strings.Repeat("a", MaxRunCredentialBytes+1):        false,
		strings.Repeat("\u00e9", MaxRunCredentialBytes/2+1): false,
	} {
		if got := ValidRunCredential(value); got != want {
			t.Errorf("ValidRunCredential(%.24q) = %v, want %v", value, got, want)
		}
	}
}

// TestInitializeParamsNeverFormatTheRunCredential keeps the run credential out
// of every formatted value an engine may log, while the wire still carries it.
func TestInitializeParamsNeverFormatTheRunCredential(t *testing.T) {
	t.Parallel()
	const secret = "prc_secret_run_credential"
	params := CredentialInitializeParams{ProtocolVersion: 1, Capabilities: []string{CapabilityCredentialV1}, RunCredential: secret}
	payload, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"runCredential":"`+secret+`"`) {
		t.Fatalf("the wire encoding lost the run credential: %s", payload)
	}
	request := CredentialRequest{ProtocolVersion: 1, ID: 1, Op: CredentialOpInitialize, Payload: payload}
	for _, value := range []any{params, &params, request, &request} {
		for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
			if out := fmt.Sprintf(format, value); strings.Contains(out, secret) {
				t.Errorf("%s of %T printed the run credential: %s", format, value, out)
			}
		}
	}
	if out := fmt.Sprintf("%v", params); !strings.Contains(out, "runCredential:<redacted>") {
		t.Errorf("formatted params do not say a run credential is present: %s", out)
	}
	if out := fmt.Sprintf("%v", CredentialInitializeParams{ProtocolVersion: 1}); strings.Contains(out, "runCredential") {
		t.Errorf("formatted params without a run credential name one: %s", out)
	}
	if _, err := ParseCredentialInitializeParams(json.RawMessage(`{"protocolVersion":1,"capabilities":[],"runCredential":"prc secret"}`)); err == nil || strings.Contains(err.Error(), "prc secret") {
		t.Errorf("an invalid run credential: %v", err)
	}
}

func sortedKeys[V any](values map[string]V) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return strings.Join(sortStrings(keys), ",")
}

// jsonMembers lists the wire member names of a struct from its json tags.
func jsonMembers(value any) string {
	kind := reflect.TypeOf(value)
	names := make([]string, 0, kind.NumField())
	for index := 0; index < kind.NumField(); index++ {
		name, _, _ := strings.Cut(kind.Field(index).Tag.Get("json"), ",")
		names = append(names, name)
	}
	return strings.Join(sortStrings(names), ",")
}

func sortStrings(values []string) []string {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
	return values
}

func TestCredentialServesOnlyItsHosts(t *testing.T) {
	t.Parallel()
	credential := Credential{Bearer: "pat_x", ExpiresAt: "2026-09-28T12:00:00Z", Hosts: []string{"127.0.0.1:8080", "localhost", "put.putnami.dev", "registry.example.com:8443"}}
	cases := map[string]bool{
		"https://put.putnami.dev/npm/pkg.tgz":        true,
		"https://PUT.putnami.dev/npm/pkg.tgz":        true,
		"https://put.putnami.dev:443/npm/pkg.tgz":    true,
		"https://put.putnami.dev:8443/npm/pkg.tgz":   false,
		"http://put.putnami.dev/npm/pkg.tgz":         false,
		"https://evil.put.putnami.dev/x":             false,
		"https://put.putnami.dev.evil.com/x":         false,
		"https://user:pw@put.putnami.dev/x":          false,
		"ftp://put.putnami.dev/x":                    false,
		"https://registry.example.com:8443/x":        true,
		"https://registry.example.com/x":             false,
		"http://127.0.0.1:8080/x":                    true,
		"http://127.0.0.1/x":                         false,
		"http://localhost/x":                         true,
		"http://localhost:80/x":                      true,
		"https://localhost/x":                        true,
		"http://localhost:8080/x":                    false,
		"https://put.putnami.dev:99999/x":            false,
		"https://[::1]/x":                            false,
		"https://other.example.com/put.putnami.dev/": false,
	}
	for raw, want := range cases {
		target, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := credential.Serves(target); got != want {
			t.Errorf("Serves(%s) = %v, want %v", raw, got, want)
		}
	}
	if credential.Serves(nil) {
		t.Error("Serves(nil) = true")
	}
	invalidEntry := Credential{Hosts: []string{"Put.Putnami.dev"}}
	if invalidEntry.Serves(&url.URL{Scheme: "https", Host: "put.putnami.dev"}) {
		t.Error("an invalid hosts entry served a request")
	}
}

func TestCredentialNeverFormatsItsBearer(t *testing.T) {
	t.Parallel()
	credential := Credential{Bearer: "pat_secret_value", ExpiresAt: "2026-09-28T12:00:00Z", Hosts: []string{"put.putnami.dev"}}
	for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
		if out := fmt.Sprintf(format, credential); strings.Contains(out, "pat_secret_value") {
			t.Errorf("%s printed the bearer: %s", format, out)
		}
	}
	if out := fmt.Sprintf("%v", &credential); strings.Contains(out, "pat_secret_value") {
		t.Errorf("pointer format printed the bearer: %s", out)
	}
}

func TestCredentialValidation(t *testing.T) {
	t.Parallel()
	valid := Credential{Bearer: "pat_x", ExpiresAt: "2026-09-28T12:00:00Z", Hosts: []string{"put.putnami.dev"}}
	if err := ValidateCredential(valid); err != nil {
		t.Fatal(err)
	}
	expiry, err := valid.Expiry()
	if err != nil || !expiry.Equal(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("Expiry = %v, %v", expiry, err)
	}
	tooMany := make([]string, MaxCredentialHosts+1)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("h%03d.example.com", index)
	}
	for name, credential := range map[string]Credential{
		"control byte bearer": {Bearer: "pat\x01x", ExpiresAt: valid.ExpiresAt, Hosts: valid.Hosts},
		"non-ascii bearer":    {Bearer: "pat_é", ExpiresAt: valid.ExpiresAt, Hosts: valid.Hosts},
		"long bearer":         {Bearer: strings.Repeat("a", MaxBearerBytes+1), ExpiresAt: valid.ExpiresAt, Hosts: valid.Hosts},
		"lowercase z":         {Bearer: "pat_x", ExpiresAt: "2026-09-28T12:00:00z", Hosts: valid.Hosts},
		"zero offset":         {Bearer: "pat_x", ExpiresAt: "2026-09-28T12:00:00+00:00", Hosts: valid.Hosts},
		"too many hosts":      {Bearer: "pat_x", ExpiresAt: valid.ExpiresAt, Hosts: tooMany},
		"nil hosts":           {Bearer: "pat_x", ExpiresAt: valid.ExpiresAt},
		"long host":           {Bearer: "pat_x", ExpiresAt: valid.ExpiresAt, Hosts: []string{strings.Repeat("a.", 127) + "com"}},
		"port zero":           {Bearer: "pat_x", ExpiresAt: valid.ExpiresAt, Hosts: []string{"put.putnami.dev:0"}},
		"trailing dot":        {Bearer: "pat_x", ExpiresAt: valid.ExpiresAt, Hosts: []string{"put.putnami.dev."}},
		"ipv6":                {Bearer: "pat_x", ExpiresAt: valid.ExpiresAt, Hosts: []string{"[::1]:8443"}},
	} {
		if err := ValidateCredential(credential); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidateRefusal(CredentialRefusal{Code: "denied", Message: "bad \xff utf-8"}); err == nil {
		t.Error("invalid UTF-8 refusal message accepted")
	}
}

func TestCredentialPayloadParsersRefuseMalformedInput(t *testing.T) {
	t.Parallel()
	for name, parse := range map[string]func(json.RawMessage) error{
		"initialize params": func(p json.RawMessage) error { _, err := ParseCredentialInitializeParams(p); return err },
		"initialize result": func(p json.RawMessage) error { _, err := ParseCredentialInitializeResult(p); return err },
		"credential params": func(p json.RawMessage) error { _, err := ParseCredentialParams(p); return err },
		"credential result": func(p json.RawMessage) error { _, err := ParseCredentialResult(p); return err },
	} {
		for _, payload := range []string{"", "[]", `"x"`, "{", `{"protocolVersion":0,"capabilities":[]}`, `{"protocolVersion":1,"capabilities":["Bad Name"]}`, `{"protocolVersion":1,"providerName":"a\u0001b","capabilities":[]}`, `{"purpose":"write"}`, `{"credential":{"bearer":"x","expiresAt":"2026-09-28T12:00:00Z","hosts":["a"],"hosts":["b"]}}`} {
			if err := parse(json.RawMessage(payload)); err == nil {
				t.Errorf("%s accepted %q", name, payload)
			}
		}
	}
	deep := `{"credential":{"hosts":[[[["a"]]]]}}`
	if _, err := ParseCredentialResult(json.RawMessage(deep)); err == nil {
		t.Error("deep nesting accepted")
	}
	if _, err := ParseCredentialRequest(bytes.Repeat([]byte(" "), MaxCredentialLineBytes+1)); err == nil {
		t.Error("oversized line accepted")
	}
	if _, err := ParseCredentialRequest([]byte("{\"protocolVersion\":1,\"id\":1,\"op\":\"shutdown\",\"x\":\"\xff\"}")); err == nil {
		t.Error("invalid UTF-8 accepted")
	}
	if _, err := ParseCredentialResponse([]byte(`{"protocolVersion":1,"id":1,"ok":true}`), CredentialOp("token")); err == nil {
		t.Error("response to an unknown op accepted")
	}
	if _, err := ParseCredentialResponse([]byte(`{"protocolVersion":1,"id":1,"ok":true,"payload":{"a":1}}`), CredentialOpShutdown); err == nil {
		t.Error("shutdown answer with a member accepted")
	}
}

func TestCredentialResponseNeverFormatsTheBearer(t *testing.T) {
	t.Parallel()
	const secret = "pkt_secret_response_bearer"
	payload, err := json.Marshal(CredentialResult{Credential: &Credential{Bearer: secret, ExpiresAt: "2099-01-01T00:00:00Z", Hosts: []string{"registry.example.test"}}})
	if err != nil {
		t.Fatal(err)
	}
	response := CredentialResponse{ProtocolVersion: 1, ID: 2, OK: true, Payload: payload}
	for _, value := range []any{response, &response} {
		for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
			if out := fmt.Sprintf(format, value); strings.Contains(out, secret) {
				t.Errorf("%s of %T printed the bearer: %s", format, value, out)
			}
		}
	}
}
