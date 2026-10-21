package grpcservices

import (
	"bytes"
	"os"
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	goldenDigest = "sha256:08f7c229ba094c96052a66266d8cd0cda7d13cc76851c09189c2b488a00f426c"
	emptyDigest  = "sha256:a07e1eebaa2eea13b757f45a072b33e146de5550cc5914c913069f77a8c01f9a"
)

func TestCanonicalJSON_MatchesGolden(t *testing.T) {
	data, err := os.ReadFile("fixtures/valid/full.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("parse valid fixture: %v", diags)
	}

	got, diags := CanonicalJSON(manifest)
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize: %v", diags)
	}
	want, err := os.ReadFile("fixtures/equivalence/grpc-services.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical bytes differ from golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if manifest.Digest != goldenDigest {
		t.Fatalf("digest = %q, want %q", manifest.Digest, goldenDigest)
	}
	if golden, diags := ParseAndValidateManifest(want); diag.HasErrors(diags) || golden.Digest != goldenDigest {
		t.Fatalf("golden does not validate to its own digest: %v", diags)
	}
}

func TestCanonicalize_EmptyInventory(t *testing.T) {
	for _, services := range [][]Service{nil, {}} {
		canonical, diags := Canonicalize(services)
		if diag.HasErrors(diags) {
			t.Fatalf("canonicalize empty: %v", diags)
		}
		if canonical.Digest != emptyDigest {
			t.Fatalf("empty digest = %q, want %q", canonical.Digest, emptyDigest)
		}
		got, diags := CanonicalJSON(canonical)
		if diag.HasErrors(diags) {
			t.Fatal(diags)
		}
		want, err := os.ReadFile("fixtures/valid/minimal.json")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("empty inventory bytes\n--- got ---\n%s\n--- want ---\n%s", got, want)
		}
	}
}

func TestCanonicalize_OrderIndependentAndPure(t *testing.T) {
	data, err := os.ReadFile("fixtures/equivalence/grpc-services.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, parseDiags := ParseManifest(data)
	if diag.HasErrors(parseDiags) {
		t.Fatalf("parse: %v", parseDiags)
	}

	permuted := cloneServices(manifest.Services)
	for left, right := 0, len(permuted)-1; left < right; left, right = left+1, right-1 {
		permuted[left], permuted[right] = permuted[right], permuted[left]
	}
	for i := range permuted {
		methods := permuted[i].Methods
		for left, right := 0, len(methods)-1; left < right; left, right = left+1, right-1 {
			methods[left], methods[right] = methods[right], methods[left]
		}
	}
	before := make([]Service, len(permuted))
	for i, service := range permuted {
		before[i] = service
		before[i].Methods = append([]Method{}, service.Methods...)
	}

	canonical, diags := Canonicalize(permuted)
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize permutation: %v", diags)
	}
	if canonical.Digest != goldenDigest {
		t.Fatalf("permuted digest = %q, want %q", canonical.Digest, goldenDigest)
	}
	if !reflect.DeepEqual(permuted, before) {
		t.Fatal("Canonicalize mutated its input")
	}
	got, diags := CanonicalJSON(canonical)
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("permuted inventory did not produce golden bytes\n%s", got)
	}

	for i := range 100 {
		again, againDiags := Canonicalize(permuted)
		if diag.HasErrors(againDiags) || again.Digest != canonical.Digest {
			t.Fatalf("iteration %d was not deterministic: %v, %q", i, againDiags, again.Digest)
		}
	}
}

func TestCanonicalize_MethodListAlwaysSerialized(t *testing.T) {
	canonical, diags := Canonicalize([]Service{{
		Name: "pkg.v1.Echo", Transport: TransportConnect, BasePath: "/pkg.v1.Echo/",
		Provenance: Provenance{Project: "p", Package: "go.putnami.dev/grpc", SourceKind: SourceConnectGateway},
	}})
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	data, diags := CanonicalJSON(canonical)
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	if !bytes.Contains(data, []byte(`"methods": []`)) {
		t.Fatalf("nil methods must serialize as []:\n%s", data)
	}
	if _, diags := ParseAndValidateManifest(data); diag.HasErrors(diags) {
		t.Fatalf("canonical output does not round-trip: %v", diags)
	}
}

func TestCanonicalizeManifest_RejectsForeignProtocol(t *testing.T) {
	if _, diags := CanonicalizeManifest(nil); !hasCode(diags, ErrorCodeParseError) {
		t.Fatalf("nil manifest: %v", diags)
	}
	_, diags := CanonicalizeManifest(&Manifest{Protocol: "putnami.http-routes.v1"})
	if !hasCode(diags, ErrorCodeInvalidProtocol) {
		t.Fatalf("foreign protocol: %v", diags)
	}
}

func TestValidateServices_Rules(t *testing.T) {
	grpcService := func(name string) Service {
		return Service{
			Name: name, Transport: TransportGRPC, MethodsKnown: true,
			Methods:    []Method{{Name: "Say"}},
			Provenance: Provenance{Project: "p", Package: "go.putnami.dev/grpc", SourceKind: SourceGRPCServer},
		}
	}
	connectService := func(name, basePath string, kind SourceKind) Service {
		return Service{
			Name: name, Transport: TransportConnect, BasePath: basePath, Methods: []Method{},
			Provenance: Provenance{Project: "p", Package: "go.putnami.dev/grpc", SourceKind: kind},
		}
	}
	tests := []struct {
		name     string
		services []Service
		wantCode string
	}{
		{name: "single segment name", services: []Service{grpcService("Health")}},
		{name: "leading underscore in every segment", services: []Service{grpcService("_a._b.C")}},
		{name: "single segment connect base path", services: []Service{connectService("Health", "/Health/", SourceConnectGateway)}},
		{name: "mount prefix", services: []Service{connectService("pkg.v1.Echo", "/api/v2/pkg.v1.Echo/", SourceAPIBridge)}},
		{name: "same name across transports", services: []Service{
			grpcService("pkg.v1.Echo"), connectService("pkg.v1.Echo", "/pkg.v1.Echo/", SourceConnectGateway),
		}},
		{name: "same name at two base paths", services: []Service{
			connectService("pkg.v1.Echo", "/a/pkg.v1.Echo/", SourceConnectGateway),
			connectService("pkg.v1.Echo", "/b/pkg.v1.Echo/", SourceConnectGateway),
		}},
		{name: "empty name", services: []Service{grpcService("")}, wantCode: ErrorCodeInvalidServiceName},
		{name: "leading dot", services: []Service{grpcService(".pkg.Echo")}, wantCode: ErrorCodeInvalidServiceName},
		{name: "base path without name suffix slash", services: []Service{connectService("pkg.v1.Echo", "/pkg.v1.Echo", SourceConnectGateway)}, wantCode: ErrorCodeInvalidBasePath},
		{name: "base path double slash", services: []Service{connectService("pkg.v1.Echo", "/api//pkg.v1.Echo/", SourceConnectGateway)}, wantCode: ErrorCodeInvalidBasePath},
		{name: "base path percent escape", services: []Service{connectService("pkg.v1.Echo", "/a%2Fb/pkg.v1.Echo/", SourceConnectGateway)}, wantCode: ErrorCodeInvalidBasePath},
		{name: "base path dot segment", services: []Service{connectService("pkg.v1.Echo", "/./pkg.v1.Echo/", SourceConnectGateway)}, wantCode: ErrorCodeInvalidBasePath},
		{name: "gateway on grpc transport", services: []Service{func() Service {
			s := grpcService("pkg.v1.Echo")
			s.Provenance.SourceKind = SourceConnectGateway
			return s
		}()}, wantCode: ErrorCodeSourceTransportMismatch},
		{name: "conflicting duplicate", services: []Service{
			grpcService("pkg.v1.Echo"), func() Service {
				s := grpcService("pkg.v1.Echo")
				s.Methods = []Method{{Name: "Say", ServerStreaming: true}}
				return s
			}(),
		}, wantCode: ErrorCodeDuplicateService},
		{name: "byte-identical duplicate is still a duplicate", services: []Service{
			grpcService("pkg.v1.Echo"), grpcService("pkg.v1.Echo"),
		}, wantCode: ErrorCodeDuplicateService},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := ValidateServices(tt.services)
			if tt.wantCode == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("unexpected diagnostics: %v", diags)
				}
				if _, canonicalDiags := Canonicalize(tt.services); diag.HasErrors(canonicalDiags) {
					t.Fatalf("Canonicalize rejected a valid inventory: %v", canonicalDiags)
				}
				return
			}
			if !hasCode(diags, tt.wantCode) {
				t.Fatalf("diagnostics %v do not include %q", diags, tt.wantCode)
			}
			if _, canonicalDiags := Canonicalize(tt.services); !hasCode(canonicalDiags, tt.wantCode) {
				t.Fatalf("Canonicalize diagnostics %v do not include %q", canonicalDiags, tt.wantCode)
			}
		})
	}
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, finding := range diags {
		if finding.Code == code {
			return true
		}
	}
	return false
}
