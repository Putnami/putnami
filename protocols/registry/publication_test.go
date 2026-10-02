package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

func readFixture(t *testing.T, validity, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "credential-provider", validity, name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(data, []byte("\n"))
}

// fixturePayload returns the payload of a valid request fixture.
func fixturePayload(t *testing.T, name string) json.RawMessage {
	t.Helper()
	request, err := ParseNegotiatedCredentialRequest(readFixture(t, "valid", name), everyCapability)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return request.Payload
}

func fixtureOpen(t *testing.T) *OpenParams {
	t.Helper()
	params, err := ParseOpenParams(fixturePayload(t, "request-open.json"))
	if err != nil {
		t.Fatal(err)
	}
	return params
}

func fixtureRelease(t *testing.T) *ReleaseParams {
	t.Helper()
	params, err := ParseReleaseParams(fixturePayload(t, "request-release.json"))
	if err != nil {
		t.Fatal(err)
	}
	return params
}

func TestPublicationOpsAreRefusedWithoutTheCapability(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"request-resolve.json", "request-open.json", "request-open-empty-plan.json", "request-release.json", "request-release-without-evidence.json"} {
		line := readFixture(t, "valid", name)
		if _, err := ParseNegotiatedCredentialRequest(line, everyCapability); err != nil {
			t.Fatalf("%s: refused in a session that negotiated publication-v1: %v", name, err)
		}
		if _, err := ParseCredentialRequest(line); err == nil {
			t.Errorf("%s: accepted by the credential-v1 parser", name)
		}
		for _, negotiated := range [][]string{nil, {CapabilityCredentialV1}, {CapabilityCredentialV1, "publication-v2"}} {
			if _, err := ParseNegotiatedCredentialRequest(line, negotiated); err == nil {
				t.Errorf("%s: accepted in a session that negotiated %v", name, negotiated)
			}
		}
	}
	if _, err := ParseCredentialRequest(readFixture(t, "valid", "request-resolve.json")); err == nil || !strings.Contains(err.Error(), "needs the publication-v1 capability") {
		t.Errorf("a resolve request outside publication-v1: %v", err)
	}
	for name, op := range map[string]CredentialOp{
		"response-resolve.json":         CredentialOpResolve,
		"response-open.json":            CredentialOpOpen,
		"response-open-refusal.json":    CredentialOpOpen,
		"response-release.json":         CredentialOpRelease,
		"response-release-refusal.json": CredentialOpRelease,
	} {
		line := readFixture(t, "valid", name)
		if _, err := ParseNegotiatedCredentialResponse(line, op, everyCapability); err != nil {
			t.Fatalf("%s: refused in a session that negotiated publication-v1: %v", name, err)
		}
		if _, err := ParseCredentialResponse(line, op); err == nil {
			t.Errorf("%s: accepted by the credential-v1 parser", name)
		}
		if _, err := ParseNegotiatedCredentialResponse(line, op, []string{CapabilityCredentialV1}); err == nil {
			t.Errorf("%s: accepted in a session that negotiated credential-v1 only", name)
		}
	}
	for _, op := range []CredentialOp{CredentialOpInitialize, CredentialOpCredential, CredentialOpShutdown} {
		if !op.Allowed(nil) || !op.Allowed(everyCapability) {
			t.Errorf("%s is not allowed in every session", op)
		}
	}
	for _, test := range []struct{ offered, echoed, want []string }{
		{everyCapability, everyCapability, everyCapability},
		{everyCapability, []string{CapabilityCredentialV1}, []string{CapabilityCredentialV1}},
		{[]string{CapabilityCredentialV1}, everyCapability, []string{CapabilityCredentialV1}},
		{[]string{CapabilityCredentialV1, CapabilityPublicationV1, CapabilityPublicationV1}, []string{CapabilityPublicationV1, CapabilityCredentialV1}, everyCapability},
		{nil, everyCapability, nil},
	} {
		negotiated := NegotiatedCapabilities(test.offered, test.echoed)
		if !slices.Equal(negotiated, test.want) {
			t.Errorf("NegotiatedCapabilities(%v, %v) = %v, want %v", test.offered, test.echoed, negotiated, test.want)
		}
		if CredentialOpRelease.Allowed(negotiated) != slices.Contains(test.want, CapabilityPublicationV1) {
			t.Errorf("release allowed = %v after offering %v and echoing %v", !slices.Contains(test.want, CapabilityPublicationV1), test.offered, test.echoed)
		}
	}
}

// padLine inserts white space before the closing brace of line until it is
// size bytes long.
func padLine(line []byte, size int) []byte {
	padded := make([]byte, 0, size)
	padded = append(padded, line[:len(line)-1]...)
	padded = append(padded, bytes.Repeat([]byte(" "), size-len(line))...)
	return append(padded, '}')
}

func TestPublicationLineBoundIsPerOp(t *testing.T) {
	t.Parallel()
	credential := []byte(`{"protocolVersion":1,"id":4,"op":"credential","payload":{"purpose":"publish"}}`)
	for _, negotiated := range [][]string{nil, everyCapability} {
		if _, err := ParseNegotiatedCredentialRequest(padLine(credential, MaxCredentialLineBytes), negotiated); err != nil {
			t.Errorf("a credential line of %d bytes with %v: %v", MaxCredentialLineBytes, negotiated, err)
		}
		if _, err := ParseNegotiatedCredentialRequest(padLine(credential, MaxCredentialLineBytes+1), negotiated); err == nil || !strings.Contains(err.Error(), fmt.Sprint(MaxCredentialLineBytes)) {
			t.Errorf("a credential line over %d bytes with %v: %v", MaxCredentialLineBytes, negotiated, err)
		}
	}
	params := fixtureRelease(t)
	params.Evidence.Members = evidenceMembers(2000, "")
	payload, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(CredentialRequest{ProtocolVersion: 1, ID: 5, Op: CredentialOpRelease, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if len(line) <= MaxCredentialLineBytes {
		t.Fatalf("the release line is %d bytes, within the credential-v1 bound", len(line))
	}
	if _, err := ParseNegotiatedCredentialRequest(line, everyCapability); err != nil {
		t.Fatalf("a %d-byte release line: %v", len(line), err)
	}
	if _, err := ParseNegotiatedCredentialRequest(padLine(line, MaxPublicationLineBytes), everyCapability); err != nil {
		t.Errorf("a release line of %d bytes: %v", MaxPublicationLineBytes, err)
	}
	if _, err := ParseNegotiatedCredentialRequest(padLine(line, MaxPublicationLineBytes+1), everyCapability); err == nil || !strings.Contains(err.Error(), fmt.Sprint(MaxPublicationLineBytes)) {
		t.Errorf("a release line over %d bytes: %v", MaxPublicationLineBytes, err)
	}

	answer := readFixture(t, "valid", "response-release.json")
	if _, err := ParseNegotiatedCredentialResponse(padLine(answer, MaxCredentialLineBytes+1), CredentialOpRelease, everyCapability); err != nil {
		t.Errorf("a release answer over %d bytes: %v", MaxCredentialLineBytes, err)
	}
	if _, err := ParseNegotiatedCredentialResponse(padLine(answer, MaxPublicationLineBytes+1), CredentialOpRelease, everyCapability); err == nil {
		t.Errorf("a release answer over %d bytes was accepted", MaxPublicationLineBytes)
	}
	credentialAnswer := []byte(`{"protocolVersion":1,"id":4,"ok":true,"payload":{}}`)
	if _, err := ParseNegotiatedCredentialResponse(padLine(credentialAnswer, MaxCredentialLineBytes+1), CredentialOpCredential, everyCapability); err == nil {
		t.Errorf("a credential answer over %d bytes was accepted", MaxCredentialLineBytes)
	}

	deepInitialize := []byte(`{"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"capabilities":[[[[["x"]]]]]}}`)
	if _, err := ParseNegotiatedCredentialRequest(deepInitialize, everyCapability); err == nil || !strings.Contains(err.Error(), "nesting exceeds 5 levels") {
		t.Errorf("an initialize nested deeper than credential-v1 allows: %v", err)
	}
	resolveAnswer := readFixture(t, "valid", "response-resolve.json")
	if _, err := ParseNegotiatedCredentialResponse(resolveAnswer, CredentialOpCredential, everyCapability); err == nil || !strings.Contains(err.Error(), "nesting exceeds 5 levels") {
		t.Errorf("a resolve answer read as a credential answer: %v", err)
	}
	tooDeep := []byte(`{"protocolVersion":1,"id":5,"op":"release","payload":{"request":` + strings.Repeat("[", 12) + strings.Repeat("]", 12) + `}}`)
	if _, err := ParseNegotiatedCredentialRequest(tooDeep, everyCapability); err == nil || !strings.Contains(err.Error(), "nesting exceeds 12 levels") {
		t.Errorf("a release nested deeper than publication-v1 allows: %v", err)
	}
	if _, err := ParseReleaseParams(json.RawMessage(`{"request":` + strings.Repeat("[", 12) + strings.Repeat("]", 12) + `}`)); err == nil || !strings.Contains(err.Error(), "nesting exceeds 11 levels") {
		t.Errorf("a release payload nested deeper than its line allows: %v", err)
	}
}

// evidenceMembers returns count member evidence entries in (ecosystem,
// coordinate) order, each coordinate carrying suffix.
func evidenceMembers(count int, suffix string) []PublicationMemberEvidence {
	members := make([]PublicationMemberEvidence, count)
	for index := range members {
		members[index] = PublicationMemberEvidence{
			Project:    fmt.Sprintf("typescript/pkg-%05d", index),
			Ecosystem:  "npm",
			Coordinate: fmt.Sprintf("@putnami/pkg-%05d%s", index, suffix),
			Version:    "0.3.0",
			Digest:     "sha256:" + strings.Repeat("e", 64),
			Publisher:  "@putnami/typescript",
			Command:    "publish",
			Step:       "upload",
		}
	}
	return members
}

func TestReleasePayloadReusesTheDistributionRequest(t *testing.T) {
	t.Parallel()
	payload := fixturePayload(t, "request-release.json")
	params, err := ParseReleaseParams(payload)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(payload, &members); err != nil {
		t.Fatal(err)
	}
	direct, diagnostics := distribution.ParseAndValidateReleaseRequest(members["request"])
	if direct == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("distribution refuses the request member: %v", diagnostics)
	}
	if !reflect.DeepEqual(*direct, params.Request) {
		t.Error("the parsed request differs from the distribution parse of the same member")
	}
	if encoded, err := json.Marshal(direct); err != nil || !bytes.Equal(encoded, members["request"]) {
		t.Errorf("the request member is not the distribution document byte for byte: %v", err)
	}
	if params.Request.Channels[1].Expected != nil || params.Request.Visibility.Set != nil {
		t.Error("the null members distribution admits did not decode to absent values")
	}

	answer, err := ParseNegotiatedCredentialResponse(readFixture(t, "valid", "response-release.json"), CredentialOpRelease, everyCapability)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ParseReleaseResult(answer.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics := distribution.ValidateReleaseExchange(&params.Request, &result.Response); diag.HasErrors(diagnostics) {
		t.Errorf("the release fixtures do not form a distribution exchange: %v", diagnostics)
	}

	refused := readFixture(t, "invalid", "request-release-null-in-request.json")
	if _, err := ParseNegotiatedCredentialRequest(refused, everyCapability); err == nil || !strings.Contains(err.Error(), "invalid release request") {
		t.Errorf("a null distribution refuses: %v", err)
	}

	reordered := *params
	reordered.Request.ReleaseSet.Members = slices.Clone(params.Request.ReleaseSet.Members)
	slices.Reverse(reordered.Request.ReleaseSet.Members)
	encoded, err := json.Marshal(reordered)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := ParseReleaseParams(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized.Request.ReleaseSet, params.Request.ReleaseSet) {
		t.Error("the release set is not normalized as distribution normalizes it")
	}

	resolve, err := ParseResolveParams(fixturePayload(t, "request-resolve.json"))
	if err != nil {
		t.Fatal(err)
	}
	resolveAnswer, err := ParseNegotiatedCredentialResponse(readFixture(t, "valid", "response-resolve.json"), CredentialOpResolve, everyCapability)
	if err != nil {
		t.Fatal(err)
	}
	heads, err := ParseResolveResult(resolveAnswer.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics := distribution.ValidateResolveExchange(&resolve.Request, &heads.Response); diag.HasErrors(diagnostics) {
		t.Errorf("the resolve fixtures do not form a distribution exchange: %v", diagnostics)
	}
}

// releasePlanContractMember and releasePlanContract are the plan tuple the
// engine hashes, field for field and tag for tag. PublicationPlan must encode
// to the same bytes.
type releasePlanContractMember struct {
	Ecosystem            distribution.Ecosystem `json:"ecosystem"`
	Coordinate           string                 `json:"coordinate"`
	Version              string                 `json:"version"`
	SourceRevision       string                 `json:"sourceRevision"`
	SelectionFingerprint string                 `json:"selectionFingerprint"`
}

type releasePlanContract struct {
	ProtocolVersion  int                         `json:"protocolVersion"`
	Namespace        string                      `json:"namespace"`
	SourceRevision   string                      `json:"sourceRevision"`
	Channels         []string                    `json:"channels"`
	ImmutableChannel string                      `json:"immutableChannel,omitempty"`
	Members          []releasePlanContractMember `json:"members"`
	PlanDigest       string                      `json:"planDigest,omitempty"`
}

func TestOpenPlanDigestMatchesTheReleasePlanContract(t *testing.T) {
	t.Parallel()
	source := strings.Repeat("a", 40)
	digestFor := func(char string) string { return "sha256:" + strings.Repeat(char, 64) }
	plan := PublicationPlan{ProtocolVersion: 1, Namespace: "putnami", SourceRevision: source, Channels: []string{"canary", "v1.2.3"}, ImmutableChannel: "v1.2.3", Members: []PublicationPlanMember{
		{Ecosystem: "npm", Coordinate: "@scope/pkg", Version: "1.2.3", SourceRevision: source, SelectionFingerprint: digestFor("1")},
		{Ecosystem: "go", Coordinate: "go.putnami.dev/protocol/cli", Version: "v1.2.3", SourceRevision: source, SelectionFingerprint: digestFor("2")},
		{Ecosystem: "oci", Coordinate: "putnami/cloud", Version: "1.2.3", SourceRevision: source, SelectionFingerprint: digestFor("3")},
		{Ecosystem: "put", Coordinate: "putnami/app", Version: "1.2.3", SourceRevision: source, SelectionFingerprint: digestFor("4")},
	}}
	const vector = "sha256:e75cd832da7476399dd2aaccf24c97583fa4ae75d3c29399774525f70b6549ce"
	if digest, err := PlanDigest(plan); err != nil || digest != vector {
		t.Fatalf("PlanDigest = %s, %v; want %s", digest, err, vector)
	}
	contract := releasePlanContract{ProtocolVersion: plan.ProtocolVersion, Namespace: plan.Namespace, SourceRevision: plan.SourceRevision, Channels: plan.Channels, ImmutableChannel: plan.ImmutableChannel}
	for _, member := range plan.Members {
		contract.Members = append(contract.Members, releasePlanContractMember(member))
	}
	for _, planDigest := range []string{"", vector} {
		plan.PlanDigest, contract.PlanDigest = planDigest, planDigest
		got, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(contract)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("the plan encodes differently from the contract:\n%s\n%s", got, want)
		}
		if digest, err := PlanDigest(plan); err != nil || digest != vector {
			t.Errorf("with planDigest %q, PlanDigest = %s, %v", planDigest, digest, err)
		}
	}
	if err := ValidatePublicationPlan(plan); err == nil || !strings.Contains(err.Error(), "order") {
		t.Errorf("a plan whose members are out of order: %v", err)
	}
	slices.SortFunc(plan.Members, func(a, b PublicationPlanMember) int {
		return strings.Compare(string(a.Ecosystem)+"\x00"+a.Coordinate, string(b.Ecosystem)+"\x00"+b.Coordinate)
	})
	sorted, err := PlanDigest(plan)
	if err != nil || sorted == vector {
		t.Fatalf("the digest ignores member order: %s, %v", sorted, err)
	}
	plan.PlanDigest = sorted
	if err := ValidatePublicationPlan(plan); err != nil {
		t.Errorf("the sorted plan: %v", err)
	}

	open := fixtureOpen(t)
	if digest, err := PlanDigest(open.Plan); err != nil || digest != open.Plan.PlanDigest || digest != "sha256:e51fdc56cded66adf23c53d447acac9b6cc77d54c48ccb3fc49f2a4c7018b7a4" {
		t.Errorf("the open fixture's digest = %s, %v", digest, err)
	}

	huge := PublicationPlan{ProtocolVersion: 1, Namespace: "putnami", SourceRevision: source, Channels: []string{"canary"}}
	for index := range distribution.MaxMembers {
		huge.Members = append(huge.Members, PublicationPlanMember{Ecosystem: "npm", Coordinate: fmt.Sprintf("%05d%s", index, strings.Repeat("c", 400)), Version: "1.2.3", SourceRevision: source, SelectionFingerprint: digestFor("1")})
	}
	if _, err := PlanDigest(huge); err == nil || !strings.Contains(err.Error(), fmt.Sprint(MaxPublicationPlanBytes)) {
		t.Errorf("a plan over %d bytes: %v", MaxPublicationPlanBytes, err)
	}
}

// A plan may select Put registry members: a release archive and a config,
// migration or doc member. The engine uploads them like any other member, so
// open names them in the plan and release names the digest of each in its
// evidence.
func TestPublicationCarriesPutRegistryMembers(t *testing.T) {
	t.Parallel()
	open, err := ParseOpenParams(fixturePayload(t, "request-open-put-members.json"))
	if err != nil {
		t.Fatal(err)
	}
	if digest, err := PlanDigest(open.Plan); err != nil || digest != open.Plan.PlanDigest || digest != "sha256:96d3a32cdede07c0b62db190c50b1f51121e8f856adcf7f082a836223f6f88d5" {
		t.Errorf("the put-members open fixture's digest = %s, %v", digest, err)
	}
	release, err := ParseReleaseParams(fixturePayload(t, "request-release-put-members.json"))
	if err != nil {
		t.Fatal(err)
	}
	if release.PlanDigest != open.Plan.PlanDigest {
		t.Fatalf("the release names plan %s; open named %s", release.PlanDigest, open.Plan.PlanDigest)
	}
	evidence := map[string]string{}
	for _, member := range release.Evidence.Members {
		evidence[member.Ecosystem+"\x00"+member.Coordinate] = member.Digest
	}
	for _, member := range open.Plan.Members {
		if evidence[string(member.Ecosystem)+"\x00"+member.Coordinate] == "" {
			t.Errorf("the release evidence names no digest for %s %s", member.Ecosystem, member.Coordinate)
		}
	}
	for _, ecosystem := range []distribution.Ecosystem{"archive", "put"} {
		if !slices.ContainsFunc(open.Plan.Members, func(member PublicationPlanMember) bool { return member.Ecosystem == ecosystem }) {
			t.Errorf("the put-members plan selects no %s member", ecosystem)
		}
	}
}

func TestPublicationLinesNeverFormatTheirPayload(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"request-open.json", "request-release.json"} {
		request, err := ParseNegotiatedCredentialRequest(readFixture(t, "valid", name), everyCapability)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []any{request, *request} {
			for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
				if out := fmt.Sprintf(format, value); strings.Contains(out, "sha256:") || strings.Contains(out, "8d5edb75") {
					t.Errorf("%s of %s printed payload bytes: %s", format, name, out)
				}
			}
		}
	}
	for name, op := range map[string]CredentialOp{"response-resolve.json": CredentialOpResolve, "response-release.json": CredentialOpRelease} {
		response, err := ParseNegotiatedCredentialResponse(readFixture(t, "valid", name), op, everyCapability)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []any{response, *response} {
			for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
				if out := fmt.Sprintf(format, value); strings.Contains(out, "sha256:") || strings.Contains(out, "rs_") {
					t.Errorf("%s of %s printed payload bytes: %s", format, name, out)
				}
			}
		}
	}
}

func TestPublicationPlanValidation(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*PublicationPlan){
		"version 2":  func(p *PublicationPlan) { p.ProtocolVersion = 2 },
		"no channel": func(p *PublicationPlan) { p.Channels, p.ImmutableChannel = nil, "" },
		"release-set id channel": func(p *PublicationPlan) {
			p.Channels = []string{"rs_" + strings.Repeat("a", 64)}
			p.ImmutableChannel = ""
		},
		"duplicate channel":        func(p *PublicationPlan) { p.Channels = []string{"canary", "canary"}; p.ImmutableChannel = "" },
		"uppercase namespace":      func(p *PublicationPlan) { p.Namespace = "Putnami" },
		"short source revision":    func(p *PublicationPlan) { p.SourceRevision = "8d5edb75" },
		"unlisted immutable":       func(p *PublicationPlan) { p.ImmutableChannel = "v9.9.9" },
		"nil members":              func(p *PublicationPlan) { p.Members = nil },
		"uppercase ecosystem":      func(p *PublicationPlan) { p.Members[0].Ecosystem = "GO" },
		"empty coordinate":         func(p *PublicationPlan) { p.Members[0].Coordinate = "" },
		"control coordinate":       func(p *PublicationPlan) { p.Members[0].Coordinate = "go.putnami.dev/\x01" },
		"long version":             func(p *PublicationPlan) { p.Members[0].Version = strings.Repeat("1", distribution.MaxVersionBytes+1) },
		"bad selection":            func(p *PublicationPlan) { p.Members[0].SelectionFingerprint = "sha256:abc" },
		"foreign source revision":  func(p *PublicationPlan) { p.Members[0].SourceRevision = strings.Repeat("b", 40) },
		"duplicate member":         func(p *PublicationPlan) { p.Members[1] = p.Members[0] },
		"too many members":         func(p *PublicationPlan) { p.Members = make([]PublicationPlanMember, distribution.MaxMembers+1) },
		"digest of another plan":   func(p *PublicationPlan) { p.Namespace = "other" },
		"digest of an absent plan": func(p *PublicationPlan) { p.PlanDigest = "" },
	} {
		plan := fixtureOpen(t).Plan
		mutate(&plan)
		if name != "digest of another plan" && name != "digest of an absent plan" {
			plan.PlanDigest, _ = PlanDigest(plan)
		}
		if err := ValidatePublicationPlan(plan); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPublicationAncestryValidation(t *testing.T) {
	t.Parallel()
	valid := fixtureOpen(t).Ancestry
	if err := ValidatePublicationAncestry(valid); err != nil {
		t.Fatal(err)
	}
	own := valid
	own.SnapshotCommits = MaxAncestrySnapshotCommits
	own.Channels = []PublicationChannelAncestry{{Name: "canary", HeadSourceRevision: valid.SourceRevision, Ancestor: true}}
	if err := ValidatePublicationAncestry(own); err != nil {
		t.Errorf("a head at the source revision itself: %v", err)
	}
	tooMany := make([]PublicationChannelAncestry, distribution.MaxChannelsPerRelease+1)
	for index := range tooMany {
		tooMany[index] = PublicationChannelAncestry{Name: fmt.Sprintf("c%02d", index)}
	}
	for name, mutate := range map[string]func(*PublicationAncestry){
		"uppercase source":    func(a *PublicationAncestry) { a.SourceRevision = strings.ToUpper(a.SourceRevision) },
		"no snapshot":         func(a *PublicationAncestry) { a.SnapshotCommits = 0 },
		"oversized snapshot":  func(a *PublicationAncestry) { a.SnapshotCommits = MaxAncestrySnapshotCommits + 1 },
		"no channel":          func(a *PublicationAncestry) { a.Channels = nil },
		"too many channels":   func(a *PublicationAncestry) { a.Channels = tooMany },
		"duplicate channel":   func(a *PublicationAncestry) { a.Channels[1].Name = a.Channels[0].Name },
		"invalid channel":     func(a *PublicationAncestry) { a.Channels[0].Name = "Canary" },
		"short head":          func(a *PublicationAncestry) { a.Channels[0].HeadSourceRevision = "4b825dc6" },
		"ancestor of nothing": func(a *PublicationAncestry) { a.Channels[1].Ancestor = true },
		"own head not related": func(a *PublicationAncestry) {
			a.Channels[0].HeadSourceRevision, a.Channels[0].Ancestor = a.SourceRevision, false
		},
	} {
		ancestry := fixtureOpen(t).Ancestry
		mutate(&ancestry)
		if err := ValidatePublicationAncestry(ancestry); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPublicationEvidenceValidation(t *testing.T) {
	t.Parallel()
	if err := ValidatePublicationEvidence(PublicationEvidence{Images: []runtimeproto.ReleaseSetPublishedImage{}, Members: []PublicationMemberEvidence{}}); err != nil {
		t.Errorf("empty evidence: %v", err)
	}
	for name, mutate := range map[string]func(*PublicationEvidence){
		"nil images":       func(e *PublicationEvidence) { e.Images = nil },
		"nil members":      func(e *PublicationEvidence) { e.Members = nil },
		"bad image digest": func(e *PublicationEvidence) { e.Images[0].Digest = "sha256:abc" },
		"untrimmed image":  func(e *PublicationEvidence) { e.Images[0].Project = " apps/cloud" },
		"duplicate image":  func(e *PublicationEvidence) { e.Images = append(e.Images, e.Images[0]) },
		"unsorted images": func(e *PublicationEvidence) {
			e.Images = append(e.Images, runtimeproto.ReleaseSetPublishedImage{Project: "apps/a", Digest: e.Images[0].Digest})
		},
		"control project":   func(e *PublicationEvidence) { e.Members[0].Project = "go/\tapp" },
		"untrimmed project": func(e *PublicationEvidence) { e.Members[0].Project = "go/app " },
		"long project":      func(e *PublicationEvidence) { e.Members[0].Project = strings.Repeat("p", MaxEvidenceProjectBytes+1) },
		"bad ecosystem":     func(e *PublicationEvidence) { e.Members[0].Ecosystem = "Go" },
		"empty version":     func(e *PublicationEvidence) { e.Members[0].Version = "" },
		"bad digest":        func(e *PublicationEvidence) { e.Members[0].Digest = "sha256:" + strings.Repeat("C", 64) },
		"empty publisher":   func(e *PublicationEvidence) { e.Members[0].Publisher = "" },
		"long command":      func(e *PublicationEvidence) { e.Members[0].Command = strings.Repeat("c", MaxEvidenceTextBytes+1) },
		"control step":      func(e *PublicationEvidence) { e.Members[0].Step = "upload\n" },
		"unsorted members":  func(e *PublicationEvidence) { e.Members[0], e.Members[1] = e.Members[1], e.Members[0] },
		"duplicate member":  func(e *PublicationEvidence) { e.Members[1] = e.Members[0] },
		"long coordinate": func(e *PublicationEvidence) {
			e.Members = evidenceMembers(1, strings.Repeat("x", distribution.MaxCoordinateBytes))
		},
		"too many evidence":    func(e *PublicationEvidence) { e.Members = evidenceMembers(distribution.MaxMembers+1, "") },
		"replacement in route": func(e *PublicationEvidence) { e.Members[0].Publisher = "@putnami/" + string(utf8.RuneError) },
	} {
		evidence := fixtureRelease(t).Evidence
		mutate(&evidence)
		if err := ValidatePublicationEvidence(evidence); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	oversized := fixtureRelease(t).Evidence
	oversized.Members = evidenceMembers(distribution.MaxMembers, strings.Repeat("x", 480))
	for index := range oversized.Members {
		oversized.Members[index].Command = strings.Repeat("c", MaxEvidenceTextBytes)
		oversized.Members[index].Step = strings.Repeat("s", MaxEvidenceTextBytes)
	}
	if err := ValidatePublicationEvidence(oversized); err == nil || !strings.Contains(err.Error(), fmt.Sprint(MaxPublicationEvidenceBytes)) {
		t.Errorf("evidence members over %d bytes: %v", MaxPublicationEvidenceBytes, err)
	}
	oversized.Members = oversized.Members[:1000]
	if err := ValidatePublicationEvidence(oversized); err != nil {
		t.Errorf("evidence members of maximal entries: %v", err)
	}
}

func TestPublicationPayloadParsersRefuseMalformedInput(t *testing.T) {
	t.Parallel()
	parsers := map[string]func(json.RawMessage) error{
		"resolve params": func(p json.RawMessage) error { _, err := ParseResolveParams(p); return err },
		"resolve result": func(p json.RawMessage) error { _, err := ParseResolveResult(p); return err },
		"open params":    func(p json.RawMessage) error { _, err := ParseOpenParams(p); return err },
		"open result":    func(p json.RawMessage) error { _, err := ParseOpenResult(p); return err },
		"release params": func(p json.RawMessage) error { _, err := ParseReleaseParams(p); return err },
		"release result": func(p json.RawMessage) error { _, err := ParseReleaseResult(p); return err },
	}
	for name, parse := range parsers {
		for _, payload := range []string{"", "null", "[]", `"x"`, "{", "{}", `{"x":1}`, `{"request":{}}`, `{"response":{}}`, `{"planDigest":""}`, `{"request":null,"request":null}`} {
			if err := parse(json.RawMessage(payload)); err == nil {
				t.Errorf("%s accepted %q", name, payload)
			}
		}
	}
	if _, err := ParseResolveParams(json.RawMessage(`{"request":{"protocolVersion":2,"namespace":"putnami","releaseId":"rs_` + strings.Repeat("a", 64) + `"}}`)); err == nil || !strings.Contains(err.Error(), "never a releaseId") {
		t.Errorf("a resolve by releaseId: %v", err)
	}
	if _, err := ParseResolveResult(json.RawMessage(`{"response":{"protocolVersion":2,"heads":{"canary":null}}}`)); err != nil {
		t.Errorf("a resolve answer naming an empty channel: %v", err)
	}
	if _, err := ParseOpenResult(json.RawMessage(`{"planDigest":"sha256:` + strings.Repeat("A", 64) + `"}`)); err == nil {
		t.Error("an open answer with an uppercase digest was accepted")
	}
	release := fixtureRelease(t)
	release.Ancestry.Channels[1].HeadSourceRevision = strings.Repeat("b", 40)
	release.Ancestry.Channels[1].Ancestor = true
	payload, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReleaseParams(payload); err == nil || !strings.Contains(err.Error(), "expected to have no head") {
		t.Errorf("a head revision for a channel expected to have no head: %v", err)
	}
}

func TestStrictScanDefersNullOnlyInsideDelegatedMembers(t *testing.T) {
	t.Parallel()
	delegated := [][]string{{"payload", "request"}}
	for document, want := range map[string]string{
		`{"payload":{"request":{"expected":null}}}`:                      "deferred",
		`{"payload":{"request":null}}`:                                   "deferred",
		`{"payload":{"request":[{"set":null}]}}`:                         "deferred",
		`{"payload":{"request":{"expected":"x"}}}`:                       "clean",
		`{"payload":{"other":null}}`:                                     "refused",
		`{"payload":null}`:                                               "refused",
		`{"request":{"expected":null}}`:                                  "refused",
		`{"payload":[{"request":null}]}`:                                 "refused",
		`{"payload":{"other":{"request":null}}}`:                         "refused",
		`{"payload":{"request":{}},"extra":null}`:                        "refused",
		`{"payload":{"request":{"a":1,"a":null}}}`:                       "refused",
		`{"payload":{"request":{"a":"` + string(utf8.RuneError) + `"}}}`: "refused",
		`{"payload":{"request":{"expected":null}}} x`:                    "refused",
	} {
		scan, err := strictScan([]byte(document), publicationBounds, delegated)
		got := "clean"
		switch {
		case err != nil:
			got = "refused"
		case scan.delegatedNull:
			got = "deferred"
		}
		if got != want {
			t.Errorf("%s: %s, want %s (%v)", document, got, want, err)
		}
	}
	if scan, err := strictScan([]byte(`{"a":[{"b":1}]}`), publicationBounds, nil); err != nil || scan.depth != 3 {
		t.Errorf("depth of a three-level document = %d, %v", scan.depth, err)
	}
	if _, err := strictScan([]byte(`{"payload":{"request":{"expected":null}}}`), credentialBounds, nil); err == nil {
		t.Error("a null was accepted without a delegated member")
	}
}
