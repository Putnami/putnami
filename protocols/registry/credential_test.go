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

	distribution "go.putnami.dev/protocol/distribution"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// everyCapability is the negotiation of a session that offered and echoed
// every capability this package defines.
var everyCapability = []string{CapabilityCredentialV1, CapabilityPublicationV1}

// TestCredentialFixtures runs every fixture through the Go validator, in a
// session that negotiated every capability: valid lines parse and re-encode
// byte for byte, a publication-v1 payload included, and invalid lines are
// refused. A response fixture names the op it answers:
// response-<op>-<case>.json.
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
				var op CredentialOp
				var payload json.RawMessage
				answer := false
				switch {
				case strings.HasPrefix(name, "request-"):
					var request *CredentialRequest
					request, err = ParseNegotiatedCredentialRequest(line, everyCapability)
					if request != nil {
						decoded, op, payload = request, request.Op, request.Payload
					}
				case strings.HasPrefix(name, "response-"):
					op = CredentialOp(strings.SplitN(strings.TrimPrefix(name, "response-"), "-", 2)[0])
					op = CredentialOp(strings.TrimSuffix(string(op), ".json"))
					if !op.Valid() {
						t.Fatalf("fixture %s names no op", name)
					}
					var response *CredentialResponse
					response, err = ParseNegotiatedCredentialResponse(line, op, everyCapability)
					if response != nil {
						decoded, payload, answer = response, response.Payload, true
					}
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
				if op.Capability() != CapabilityPublicationV1 || len(payload) == 0 {
					return
				}
				typed, err := parsePublicationPayload(op, answer, payload)
				if err != nil {
					t.Fatal(err)
				}
				if encoded, err = json.Marshal(typed); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(encoded, payload) {
					t.Fatalf("payload is not the canonical encoding of its type:\n%s\n%s", payload, encoded)
				}
			})
		}
	}
}

// parsePublicationPayload decodes the payload of a publication-v1 request, or
// of its answer, into its type.
func parsePublicationPayload(op CredentialOp, answer bool, payload json.RawMessage) (any, error) {
	switch {
	case op == CredentialOpResolve && answer:
		return ParseResolveResult(payload)
	case op == CredentialOpResolve:
		return ParseResolveParams(payload)
	case op == CredentialOpOpen && answer:
		return ParseOpenResult(payload)
	case op == CredentialOpOpen:
		return ParseOpenParams(payload)
	case op == CredentialOpRelease && answer:
		return ParseReleaseResult(payload)
	case op == CredentialOpRelease:
		return ParseReleaseParams(payload)
	}
	return nil, fmt.Errorf("%s is not a publication-v1 op", op)
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
		MinItems  int      `json:"minItems"`
		MaxItems  int      `json:"maxItems"`
		Minimum   int      `json:"minimum"`
		Maximum   int      `json:"maximum"`
		Ref       string   `json:"$ref"`
		Items     struct {
			Pattern   string `json:"pattern"`
			MaxLength int    `json:"maxLength"`
			Ref       string `json:"$ref"`
		} `json:"items"`
	}
	type definition struct {
		AdditionalProperties *bool               `json:"additionalProperties"`
		Required             []string            `json:"required"`
		Properties           map[string]property `json:"properties"`
		Pattern              string              `json:"pattern"`
		MinLength            int                 `json:"minLength"`
		MaxLength            int                 `json:"maxLength"`
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
		"resolveParams":    {ResolveParams{}, "request"},
		"resolveResult":    {ResolveResult{}, "response"},
		"openParams":       {OpenParams{}, "plan,ancestry"},
		"openResult":       {OpenResult{}, "planDigest"},
		"releaseParams":    {ReleaseParams{}, "planDigest,request,ancestry,evidence"},
		"releaseResult":    {ReleaseResult{}, "response"},
		"plan":             {PublicationPlan{}, "protocolVersion,namespace,sourceRevision,channels,members,planDigest"},
		"planMember":       {PublicationPlanMember{}, "ecosystem,coordinate,version,sourceRevision,selectionFingerprint"},
		"ancestry":         {PublicationAncestry{}, "sourceRevision,snapshotCommits,channels"},
		"channelAncestry":  {PublicationChannelAncestry{}, "name,ancestor"},
		"evidence":         {PublicationEvidence{}, "images,members"},
		"imageEvidence":    {runtimeproto.ReleaseSetPublishedImage{}, "project,digest"},
		"memberEvidence":   {PublicationMemberEvidence{}, "project,ecosystem,coordinate,version,digest,publisher,command,step"},
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
	ops := []string{string(CredentialOpInitialize), string(CredentialOpCredential), string(CredentialOpShutdown), string(CredentialOpResolve), string(CredentialOpOpen), string(CredentialOpRelease)}
	if got := strings.Join(schema.Definitions["request"].Properties["op"].Enum, ","); got != strings.Join(ops, ",") {
		t.Errorf("op enum = %s", got)
	}
	for _, op := range ops {
		if !CredentialOp(op).Valid() {
			t.Errorf("schema op %s is not a Go op", op)
		}
	}
	grammars := map[string]string{
		"digest":         PlanDigestPattern,
		"sourceRevision": SourceRevisionPattern,
		"channel":        distribution.ChannelPattern,
		"ecosystem":      distribution.EcosystemPattern,
	}
	for name, pattern := range grammars {
		if schema.Definitions[name].Pattern != pattern {
			t.Errorf("%s grammar = %s, want %s", name, schema.Definitions[name].Pattern, pattern)
		}
	}
	for name, limit := range map[string]int{"coordinate": distribution.MaxCoordinateBytes, "version": distribution.MaxVersionBytes, "routeName": MaxEvidenceTextBytes} {
		if def := schema.Definitions[name]; def.MinLength != 1 || def.MaxLength != limit || def.Pattern != noControlCharactersPattern {
			t.Errorf("%s bounds or text rule diverged", name)
		}
	}
	plan := schema.Definitions["plan"].Properties
	if *plan["protocolVersion"].Const != PublicationPlanProtocolVersion || plan["members"].MaxItems != distribution.MaxMembers || plan["channels"].MinItems != 1 || plan["channels"].MaxItems != distribution.MaxChannelsPerRelease {
		t.Error("plan version or bounds diverged")
	}
	if plan["namespace"].MaxLength != distribution.MaxNamespaceBytes {
		t.Error("plan namespace bound diverged")
	}
	ancestry := schema.Definitions["ancestry"].Properties
	if ancestry["snapshotCommits"].Minimum != 1 || ancestry["snapshotCommits"].Maximum != MaxAncestrySnapshotCommits || ancestry["channels"].MaxItems != distribution.MaxChannelsPerRelease {
		t.Error("ancestry bounds diverged")
	}
	if schema.Definitions["evidence"].Properties["members"].MaxItems != distribution.MaxMembers {
		t.Error("evidence member bound diverged")
	}
	for _, name := range []string{"imageEvidence", "memberEvidence"} {
		if project := schema.Definitions[name].Properties["project"]; project.MinLength != 1 || project.MaxLength != MaxEvidenceProjectBytes {
			t.Errorf("%s project bound diverged", name)
		}
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
	if refusal["message"].Pattern != noControlCharactersPattern || schema.Definitions["initializeResult"].Properties["providerName"].Pattern != noControlCharactersPattern {
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

// noControlCharactersPattern is the schema spelling of the boundedText rule.
const noControlCharactersPattern = `^[^\x00-\x1f\x7f]*$`

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
