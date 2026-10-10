package authoredmember

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func request() BuildAuthoredMemberRequest {
	return BuildAuthoredMemberRequest{WorkspaceID: "workspace-1", Project: "storefront", Environment: "production", SourceProvenance: SourceProvenance{Repository: "github.com/acme/storefront", Revision: "a1b2c3"}, Schema: NormalizedSchema{Fields: []SchemaField{{Path: "server.port", Type: "integer", Required: true}, {Path: "database.url", Type: "string", Required: true, Sensitive: true}}}, AuthoredLayers: []AuthoredLayer{{Name: "application", Pin: "a1b2c3", Values: map[string]any{"server.port": 8080}}}, SecretReferences: []SecretReference{{Path: "database.url", Reference: "config-secret://database/url"}}}
}

func TestBuildCanonicalSecretFreeBytesAndDescriptor(t *testing.T) {
	p := Publisher{}
	r := request()
	first, err := p.Build(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	r.Schema.Fields[0], r.Schema.Fields[1] = r.Schema.Fields[1], r.Schema.Fields[0]
	second, err := p.Build(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Bytes) != string(second.Bytes) || first.Descriptor != second.Descriptor {
		t.Fatal("equivalent input was not canonical")
	}
	if !strings.Contains(string(first.Bytes), "config-secret://database/url") || strings.Contains(string(first.Bytes), "plaintext") {
		t.Fatalf("member bytes are not secret-free references: %s", first.Bytes)
	}
	if first.Descriptor.MediaType != MediaType || first.Descriptor.Size != int64(len(first.Bytes)) || first.Descriptor.ContentDigest == "" || first.Descriptor.SelectionFingerprint == "" {
		t.Fatalf("incomplete descriptor: %+v", first.Descriptor)
	}
}

func TestValidateDetectsNoncanonicalBytesAndDescriptorMismatch(t *testing.T) {
	p := Publisher{}
	member, err := p.Build(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Validate(context.Background(), member); err != nil {
		t.Fatal(err)
	}
	member.Bytes = append(member.Bytes, ' ')
	if _, err := p.Validate(context.Background(), member); !errors.Is(err, ErrInvalidMember) {
		t.Fatalf("noncanonical bytes err=%v", err)
	}
	member, _ = p.Build(context.Background(), request())
	member.Descriptor.ContentDigest = "sha256:wrong"
	if _, err := p.Validate(context.Background(), member); !errors.Is(err, ErrInvalidMember) {
		t.Fatalf("bad descriptor err=%v", err)
	}
}

func TestRejectsSchemaAuthoredAndReferenceViolations(t *testing.T) {
	p := Publisher{}
	cases := []struct {
		name   string
		mutate func(*BuildAuthoredMemberRequest)
	}{
		{"missing required", func(r *BuildAuthoredMemberRequest) { r.AuthoredLayers[0].Values = map[string]any{} }},
		{"placeholder", func(r *BuildAuthoredMemberRequest) { r.AuthoredLayers[0].Values["server.port"] = "${PORT}" }},
		{"sensitive authored", func(r *BuildAuthoredMemberRequest) { r.AuthoredLayers[0].Values["database.url"] = "never-a-secret" }},
		{"invalid secret ref", func(r *BuildAuthoredMemberRequest) { r.SecretReferences[0].Reference = "https://secret.example/value" }},
		{"duplicate authored path", func(r *BuildAuthoredMemberRequest) {
			r.AuthoredLayers = append(r.AuthoredLayers, AuthoredLayer{Name: "override", Pin: "d4e5f6", Values: map[string]any{"server.port": 8081}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := request()
			tc.mutate(&r)
			if _, err := p.Build(context.Background(), r); !errors.Is(err, ErrInvalidMember) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestRejectsSensitiveDescendantsAndOverlappingAuthoredPaths(t *testing.T) {
	p := Publisher{}
	cases := []struct {
		name   string
		schema []SchemaField
		values map[string]any
		refs   []SecretReference
	}{
		{
			name: "sensitive child through authored object with separate reference",
			schema: []SchemaField{
				{Path: "database", Type: "object"},
				{Path: "database.password", Type: "string", Sensitive: true, Required: true},
			},
			values: map[string]any{"database": map[string]any{"password": "PLAINTEXT_SENTINEL"}},
			refs:   []SecretReference{{Path: "database.password", Reference: "config-secret://database/password"}},
		},
		{
			name: "deeply nested sensitive child",
			schema: []SchemaField{
				{Path: "service", Type: "object"},
				{Path: "service.database", Type: "object"},
				{Path: "service.database.password", Type: "string", Sensitive: true},
			},
			values: map[string]any{"service": map[string]any{"database": map[string]any{"password": "not-inspected-by-keyword"}}},
		},
		{
			name: "sensitive child in array object",
			schema: []SchemaField{
				{Path: "databases", Type: "array"},
				{Path: "databases.name", Type: "string"},
				{Path: "databases.password", Type: "string", Sensitive: true},
			},
			values: map[string]any{"databases": []any{map[string]any{"name": "primary", "password": "not-inspected-by-keyword"}}},
		},
		{
			name: "authored parent and child collision",
			schema: []SchemaField{
				{Path: "database", Type: "object"},
				{Path: "database.name", Type: "string"},
			},
			values: map[string]any{"database": map[string]any{"name": "orders"}, "database.name": "orders"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := BuildAuthoredMemberRequest{
				WorkspaceID: "workspace-1", Project: "storefront", Environment: "production",
				Schema:           NormalizedSchema{Fields: tc.schema},
				AuthoredLayers:   []AuthoredLayer{{Name: "application", Pin: "a1b2c3", Values: tc.values}},
				SecretReferences: tc.refs,
				SourceProvenance: SourceProvenance{Repository: "github.com/acme/storefront", Revision: "a1b2c3"},
			}
			if _, err := p.Build(t.Context(), request); !errors.Is(err, ErrInvalidMember) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateRejectsSensitiveDescendantInReceivedMemberBytes(t *testing.T) {
	wire := memberWire{
		FormatVersion: FormatVersion, MediaType: MediaType,
		WorkspaceID: "workspace-1", Project: "storefront", Environment: "production",
		Schema: NormalizedSchema{Fields: []SchemaField{
			{Path: "database", Type: "object"},
			{Path: "database.password", Type: "string", Sensitive: true, Required: true},
		}},
		AuthoredLayers: []AuthoredLayer{{Name: "application", Pin: "a1b2c3", Values: map[string]any{
			"database": map[string]any{"password": "PLAINTEXT_SENTINEL"},
		}}},
		SecretReferences: []SecretReference{{Path: "database.password", Reference: "config-secret://database/password"}},
		SourceProvenance: SourceProvenance{Repository: "github.com/acme/storefront", Revision: "a1b2c3"},
	}
	bytes, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Publisher{}).Validate(t.Context(), AuthoredMember{Bytes: bytes}); !errors.Is(err, ErrInvalidMember) {
		t.Fatalf("received sensitive descendant err=%v", err)
	}
}

func TestAcceptsSchemaBoundedNonSensitiveObjectDescendants(t *testing.T) {
	p := Publisher{}
	request := BuildAuthoredMemberRequest{
		WorkspaceID: "workspace-1", Project: "storefront", Environment: "production",
		Schema: NormalizedSchema{Fields: []SchemaField{
			{Path: "databases", Type: "array", Required: true},
			{Path: "databases.name", Type: "string", Required: true},
			{Path: "databases.options", Type: "object"},
			{Path: "databases.options.poolSize", Type: "integer"},
		}},
		AuthoredLayers: []AuthoredLayer{{Name: "application", Pin: "a1b2c3", Values: map[string]any{
			"databases": []any{map[string]any{"name": "primary", "options": map[string]any{"poolSize": int64(12)}}},
		}}},
		SourceProvenance: SourceProvenance{Repository: "github.com/acme/storefront", Revision: "a1b2c3"},
	}
	member, err := p.Build(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Validate(t.Context(), member); err != nil {
		t.Fatal(err)
	}
}

func TestSecretReferencePathRequiresCanonicalExactURI(t *testing.T) {
	path, err := SecretReferencePath("config-secret://database/other")
	if err != nil || path != "database.other" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	for _, reference := range []string{
		"config-secret://database//other",
		"config-secret://database/other/",
		"config-secret://database/%6fther",
		"config-secret://database/other?fallback=url",
		"https://database/other",
	} {
		if _, err := SecretReferencePath(reference); !errors.Is(err, ErrInvalidMember) {
			t.Fatalf("reference %q err=%v", reference, err)
		}
	}
}

func TestIdentityChangesWithAuthoredSelection(t *testing.T) {
	p := Publisher{}
	base, _ := p.Build(context.Background(), request())
	for _, mutate := range []func(*BuildAuthoredMemberRequest){func(r *BuildAuthoredMemberRequest) { r.AuthoredLayers[0].Values["server.port"] = 9090 }, func(r *BuildAuthoredMemberRequest) { r.AuthoredLayers[0].Pin = "other-pin" }, func(r *BuildAuthoredMemberRequest) {
		r.SecretReferences[0].Reference = "config-secret://database/other"
	}, func(r *BuildAuthoredMemberRequest) { r.Schema.Fields[0].Required = false }} {
		r := request()
		mutate(&r)
		got, err := p.Build(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		if got.Descriptor.SelectionFingerprint == base.Descriptor.SelectionFingerprint {
			t.Fatal("selection fingerprint did not change")
		}
	}
}

func TestNoSecretObservability(t *testing.T) {
	p := Publisher{}
	member, err := p.Build(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.Build(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if member.Descriptor.ContentDigest != again.Descriptor.ContentDigest {
		t.Fatal("unchanged authored input was not an idempotent member")
	}
	for _, text := range []string{string(member.Bytes), member.Descriptor.ContentDigest, member.Descriptor.SelectionFingerprint} {
		if strings.Contains(text, "never-a-secret") || strings.Contains(text, "ciphertext") || strings.Contains(text, "custody") {
			t.Fatalf("observability leaked protected material: %q", text)
		}
	}
}

func TestEmptySchemaMemberRemainsCanonicalAndRejectsUndeclaredContent(t *testing.T) {
	r := request()
	r.Schema.Fields = nil
	r.AuthoredLayers[0].Values = map[string]any{}
	r.SecretReferences = nil
	p := Publisher{}
	first, err := p.Build(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Validate(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	r.Schema.Fields = []SchemaField{}
	second, err := p.Build(t.Context(), r)
	if err != nil || string(first.Bytes) != string(second.Bytes) || first.Descriptor != second.Descriptor {
		t.Fatalf("empty schema is not canonical: %v", err)
	}
	r.AuthoredLayers[0].Values["unexpected"] = "value"
	if _, err := p.Build(t.Context(), r); !errors.Is(err, ErrInvalidMember) {
		t.Fatalf("undeclared value accepted: %v", err)
	}
	r.AuthoredLayers[0].Values = map[string]any{}
	r.SecretReferences = []SecretReference{{Path: "unexpected", Reference: "config-secret://unexpected"}}
	if _, err := p.Build(t.Context(), r); !errors.Is(err, ErrInvalidMember) {
		t.Fatalf("undeclared secret reference accepted: %v", err)
	}
}

// An optional secret is a reference to a sensitive field that is not
// required. It uses the same two-field shape as a required one: Validate
// re-encodes and compares bytes, so any other field would make a reader that
// predates it refuse the member. A member with only required references keeps
// the same bytes.
func TestOptionalSecretReferenceKeepsTheReferenceShape(t *testing.T) {
	p := Publisher{}
	required, err := p.Build(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	const requiredOnly = `"secretReferences":[{"path":"database.url","reference":"config-secret://database/url"}]`
	if !strings.Contains(string(required.Bytes), requiredOnly) {
		t.Fatalf("member bytes = %s, want the reference encoded as %s", required.Bytes, requiredOnly)
	}

	r := request()
	r.Schema.Fields = append(r.Schema.Fields, SchemaField{Path: "oauth.clientSecret", Type: "string", Sensitive: true})
	r.SecretReferences = append(r.SecretReferences, SecretReference{Path: "oauth.clientSecret", Reference: "config-secret://oauth/clientSecret"})
	member, err := p.Build(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Validate(t.Context(), member); err != nil {
		t.Fatal(err)
	}
	const both = `"secretReferences":[{"path":"database.url","reference":"config-secret://database/url"},{"path":"oauth.clientSecret","reference":"config-secret://oauth/clientSecret"}]`
	if !strings.Contains(string(member.Bytes), both) {
		t.Fatalf("member bytes = %s, want both references encoded as %s", member.Bytes, both)
	}
	if !strings.Contains(string(member.Bytes), `{"path":"oauth.clientSecret","type":"string","sensitive":true}`) {
		t.Fatalf("member bytes = %s, want the optional secret's field not required", member.Bytes)
	}
}

// The v1 encoding is fixed: the same request yields these exact bytes and
// this content digest. A change to either is a new FormatVersion.
func TestCanonicalBytesArePinned(t *testing.T) {
	const want = `{"formatVersion":"config.authored-member.v1","mediaType":"application/vnd.putnami.config.authored-member+json","workspaceID":"workspace-1","project":"storefront","environment":"production","schema":{"fields":[{"path":"database.url","type":"string","required":true,"sensitive":true},{"path":"server.port","type":"integer","required":true}]},"authoredLayers":[{"name":"application","pin":"a1b2c3","values":{"server.port":8080}}],"secretReferences":[{"path":"database.url","reference":"config-secret://database/url"}],"sourceProvenance":{"repository":"github.com/acme/storefront","revision":"a1b2c3"}}`
	const wantDigest = "sha256:cd168b0c5485b0d618332071bfb89f46dea86779694c70f040d6cd4090bf7f7f"
	member, err := (Publisher{}).Build(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	if string(member.Bytes) != want {
		t.Fatalf("member bytes = %s, want %s", member.Bytes, want)
	}
	if member.Descriptor.ContentDigest != wantDigest || member.Descriptor.Size != int64(len(want)) {
		t.Fatalf("descriptor = %+v, want content digest %s and size %d", member.Descriptor, wantDigest, len(want))
	}
}
