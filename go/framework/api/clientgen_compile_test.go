package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// compileFixtureSpec is one first-party contract that exercises every emission
// path a REST client has today: path/query/header parameters, a request body, a
// $ref'd model, a closed enum, a nullable map, typed errors with and without a
// payload, a void response, and a declared server stream.
func compileFixtureSpec() SpecIR {
	nullable := true
	notRetryable := false
	user := strictSchemaObject(map[string]clientcontract.Schema{
		"id":       {Type: "integer", Format: "uint64"},
		"nickname": {Type: "string", Nullable: &nullable},
		"tags":     {Type: "array", Items: &clientcontract.Schema{Type: "string"}},
		"metadata": {Type: "object", AdditionalProperties: &clientcontract.AdditionalProperties{Schema: &clientcontract.Schema{Type: "string", Nullable: &nullable}}},
		"state":    {Ref: "#/components/schemas/State"},
	}, "id", "tags", "metadata", "state")
	errorBody := strictSchemaObject(map[string]clientcontract.Schema{
		"code":    {Type: "string"},
		"message": {Type: "string"},
	}, "code", "message")

	streamOperation := &clientcontract.OperationV1{
		Stream:   clientcontract.StreamServer,
		Messages: &clientcontract.MessageShapes{Output: &clientcontract.Schema{Ref: "#/components/schemas/User"}},
		Transports: []clientcontract.Transport{{
			Protocol: clientcontract.TransportSSE, Path: "/users/watch", Encoding: clientcontract.EncodingJSON,
		}},
		Security:    clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
	}
	deleteOperation := &clientcontract.OperationV1{
		Stream:      clientcontract.StreamUnary,
		Transports:  []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/users/{id}", Encoding: clientcontract.EncodingJSON}},
		Security:    clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencyIdempotent},
	}

	return SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "users", Audience: "https://users.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Schemas: map[string]clientcontract.Schema{
			"User":      user,
			"ErrorBody": errorBody,
			"State":     {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"active"`), json.RawMessage(`"disabled"`)}},
		},
		Services: []ServiceIR{{Methods: []MethodIR{
			{
				Name: "replaceUser", OperationID: "replaceUser", HTTPMethod: "PUT", Path: "/users/{id}",
				Parameters: []ParameterIR{
					{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "integer", Format: "uint64"}},
					{Name: "tag", Location: "query", Schema: clientcontract.Schema{Type: "array", Items: &clientcontract.Schema{Type: "string"}}},
					{Name: "X-Region", Location: "header", Required: true, Schema: clientcontract.Schema{Type: "string"}},
				},
				Request:   &RequestIR{Required: true, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/User"}}}},
				Successes: []SuccessIR{{Status: 200, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/User"}}}}},
				Client: strictUnaryOperation("/users/{id}", []clientcontract.DeclaredError{
					{Status: 404, Code: "not_found", Schema: &clientcontract.Schema{Ref: "#/components/schemas/ErrorBody"}, Retryable: &notRetryable},
					{Status: 409, Code: "conflict"},
				}),
			},
			{
				Name: "removeUser", OperationID: "removeUser", HTTPMethod: "DELETE", Path: "/users/{id}",
				Parameters: []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"}}},
				Successes:  []SuccessIR{{Status: 204}},
				Client:     deleteOperation,
			},
			{
				Name: "watchUsers", OperationID: "watchUsers", HTTPMethod: "GET", Path: "/users/watch",
				Client: streamOperation,
			},
			{
				// A declared response cache: the emitted package pins the
				// response-cache runtime capability, which only type-checks
				// against a runtime that implements it.
				Name: "lookupUser", OperationID: "lookupUser", HTTPMethod: "GET", Path: "/users/{id}/profile",
				Parameters: []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"}}},
				Successes:  []SuccessIR{{Status: 200, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/User"}}}}},
				Client:     cachedUnaryOperation("/users/{id}/profile"),
			},
			{
				// Raw octets on both sides at once: the emitted reader input, the
				// bounded read, and the payload output all have to type-check
				// against the real runtime, not only parse.
				Name: "putAvatar", OperationID: "putAvatar", HTTPMethod: "PUT", Path: "/users/{id}/avatar",
				Parameters: []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"}}},
				Request: &RequestIR{Required: true, Content: []ContentIR{
					{MediaType: "image/png", Schema: &clientcontract.Schema{Type: "string", Format: "binary"}, MaxBytes: 65536},
				}},
				Successes: []SuccessIR{{Status: 200, Content: []ContentIR{
					{MediaType: "image/png", Schema: &clientcontract.Schema{Type: "string", Format: "binary"}, MaxBytes: 65536},
				}}},
				Client: strictUnaryOperation("/users/{id}/avatar", []clientcontract.DeclaredError{{Status: 404, Code: "not_found"}}),
			},
			{
				// Several success statuses with different bodies: the result
				// type, the call that keeps the answered status and each
				// per-status decode have to type-check against the runtime.
				Name: "upsertUser", OperationID: "upsertUser", HTTPMethod: "PUT", Path: "/users/{id}/upsert",
				Parameters: []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"}}},
				Request:    &RequestIR{Required: true, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/User"}}}},
				Successes: []SuccessIR{
					{Status: 200, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/User"}}}},
					{Status: 202, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Type: "string"}}}},
				},
				Client: strictUnaryOperation("/users/{id}/upsert", []clientcontract.DeclaredError{{Status: 409, Code: "conflict"}}),
			},
			{
				// A nullable body or nothing: one shared field decoded by pointer.
				Name: "touchUser", OperationID: "touchUser", HTTPMethod: "POST", Path: "/users/{id}/touch",
				Parameters: []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"}}},
				Successes: []SuccessIR{
					{Status: 200, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/User", Nullable: &nullable}}}},
					{Status: 204},
				},
				Client: strictUnaryOperation("/users/{id}/touch", nil),
			},
		}}},
	}
}

// TestGeneratedGoClientCompilesAgainstTheRealRuntime writes the emitted client
// into a throwaway Go module that replaces go.putnami.dev/client, app and inject
// with this repository's sources, and compiles it. gofmt-clean output and a
// parsed AST only prove syntax; this proves the emitted code type-checks against
// the runtime signatures it calls, which is what a consumer actually needs.
func TestGeneratedGoClientCompilesAgainstTheRealRuntime(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generated-from-contract", "the-emitted-go-client-compiles-against-the-real-client-runtime")

	source, err := GenerateClientFromIR(compileFixtureSpec(), ClientGenOptions{
		PackageName: "usersclient", ClientName: "UsersClient",
	})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}

	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, source)

	command := exec.Command("go", "build", "./...")
	command.Dir = moduleDir
	// GOPROXY=off proves the module resolves entirely from the replaced
	// workspace sources and the local cache, so the test never depends on a
	// network fetch to decide whether generated code compiles.
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off")
	if output, buildErr := command.CombinedOutput(); buildErr != nil {
		t.Fatalf("generated client does not compile: %v\n%s\n--- source:\n%s", buildErr, output, source)
	}
}

var goModModuleLine = regexp.MustCompile(`(?m)^module\s+\S+`)
var goModRelativeReplace = regexp.MustCompile(`(?m)^replace\s+(\S+)\s+=>\s+(\.\S+)`)

// writeGeneratedClientModule materializes a module whose dependency closure is
// exactly the client runtime's own: it starts from go/framework/client's go.mod,
// rewrites every relative replace to this checkout's absolute path, and adds the
// replace for the client module itself.
func writeGeneratedClientModule(t *testing.T, moduleDir, source string) {
	t.Helper()
	clientDir := filepath.Join(repoRoot(t), "go", "framework", "client")
	body, err := os.ReadFile(filepath.Join(clientDir, "go.mod"))
	if err != nil {
		t.Fatalf("read client go.mod: %v", err)
	}
	rewritten := goModModuleLine.ReplaceAllString(string(body), "module generated.example/usersclient")
	rewritten = goModRelativeReplace.ReplaceAllStringFunc(rewritten, func(match string) string {
		parts := goModRelativeReplace.FindStringSubmatch(match)
		return "replace " + parts[1] + " => " + filepath.Clean(filepath.Join(clientDir, parts[2]))
	})
	rewritten = strings.TrimRight(rewritten, "\n") +
		"\n\nrequire go.putnami.dev/client v0.0.0\n\nreplace go.putnami.dev/client => " + clientDir + "\n"
	if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte(rewritten), 0o600); err != nil {
		t.Fatalf("write module go.mod: %v", err)
	}
	sum, err := os.ReadFile(filepath.Join(clientDir, "go.sum"))
	if err != nil {
		t.Fatalf("read client go.sum: %v", err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "go.sum"), sum, 0o600); err != nil {
		t.Fatalf("write module go.sum: %v", err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "client.gen.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write generated client: %v", err)
	}
}

// generatedCacheE2E runs inside the throwaway module, in the generated
// package: a real provider, the emitted client bound through the real runtime,
// and the declared cache observed from the provider's side.
const generatedCacheE2E = `package usersclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"go.putnami.dev/client"
)

func TestTheEmittedCachedOperationGoesUpstreamOncePerFreshWindow(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(` + "`" + `{"id":7,"tags":[],"metadata":{},"state":"active"}` + "`" + `))
	}))
	defer server.Close()
	transport, err := client.NewServiceClientBinding(client.ServiceBinding{URL: server.URL, ClientID: "consumer", AllowInsecure: true}, serviceDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	users := NewUsersClient(transport)
	var in LookupUserInput
	in.Path.Id = "7"
	var wg sync.WaitGroup
	failures := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := users.LookupUser(context.Background(), in); err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("20 concurrent calls went upstream %d times, want 1", got)
	}
	if dropped := transport.InvalidateResponses("lookupUser?"); dropped != 1 {
		t.Fatalf("invalidation dropped %d entries, want 1", dropped)
	}
	if _, err := users.LookupUser(context.Background(), in); err != nil || calls.Load() != 2 {
		t.Fatalf("after invalidation: err=%v upstream=%d, want 2", err, calls.Load())
	}

	// A bypassed call reads nothing and stores nothing: it goes upstream while
	// a fresh answer is stored, and the next plain call still reads that one.
	if _, err := users.LookupUser(client.WithoutResponseCache(context.Background()), in); err != nil || calls.Load() != 3 {
		t.Fatalf("bypassed call: err=%v upstream=%d, want 3", err, calls.Load())
	}
	var other LookupUserInput
	other.Path.Id = "8"
	if _, err := users.LookupUser(client.WithoutResponseCache(context.Background()), other); err != nil || calls.Load() != 4 {
		t.Fatalf("bypassed call on an empty key: err=%v upstream=%d, want 4", err, calls.Load())
	}
	if _, err := users.LookupUser(context.Background(), other); err != nil || calls.Load() != 5 {
		t.Fatalf("a bypassed answer was stored: err=%v upstream=%d, want 5", err, calls.Load())
	}

	// The provider declared the answer's id as an invalidation field: both
	// keys answered id 7, so one invalidation by that value drops both.
	dropped, err := transport.InvalidateResponsesByField("id", uint64(7))
	if err != nil || dropped != 2 {
		t.Fatalf("invalidation by field dropped %d entries (err=%v), want 2", dropped, err)
	}
	if _, err := users.LookupUser(context.Background(), in); err != nil || calls.Load() != 6 {
		t.Fatalf("after invalidation by field: err=%v upstream=%d, want 6", err, calls.Load())
	}
	if dropped, err := transport.InvalidateResponsesByField("id", "7"); err != nil || dropped != 0 {
		t.Fatalf("the string \"7\" matched the integer 7: dropped=%d err=%v", dropped, err)
	}
}

func TestTheEmittedCachedOperationDeliversItsSuccessBodyVerbatim(t *testing.T) {
	const answer = "{ \"state\" : \"active\",\n  \"id\":7, \"tags\":[], \"metadata\":{} }\n"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	defer server.Close()
	transport, err := client.NewServiceClientBinding(client.ServiceBinding{URL: server.URL, ClientID: "consumer", AllowInsecure: true}, serviceDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	users := NewUsersClient(transport)
	var in LookupUserInput
	in.Path.Id = "7"
	var fresh, replay client.SuccessBody
	value, err := users.LookupUser(client.WithSuccessBody(context.Background(), &fresh), in)
	if err != nil || value.Id != 7 {
		t.Fatalf("value=%#v err=%v", value, err)
	}
	if string(fresh.Bytes()) != answer {
		t.Fatalf("success body = %q, want the provider's %q", fresh.Bytes(), answer)
	}
	if encoded, marshalErr := json.Marshal(value); marshalErr != nil || string(encoded) == answer {
		t.Fatalf("marshaling the generated value reproduced the body: %s (%v)", encoded, marshalErr)
	}
	if _, err := users.LookupUser(client.WithSuccessBody(context.Background(), &replay), in); err != nil {
		t.Fatal(err)
	}
	if string(replay.Bytes()) != answer || calls.Load() != 1 {
		t.Fatalf("cached replay = %q after %d upstream calls", replay.Bytes(), calls.Load())
	}
}

func TestTheEmittedClientCallsIndependentEndpoints(t *testing.T) {
	var calls [2]atomic.Int32
	servers := make([]*httptest.Server, 2)
	for i := range servers {
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[i].Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, ` + "`" + `{"id":%d,"tags":[],"metadata":{},"state":"active"}` + "`" + `, i+7)
		}))
		defer servers[i].Close()
	}
	transport, err := client.NewServiceClientBinding(client.ServiceBinding{URL: servers[0].URL, ClientID: "consumer"}, serviceDescriptor)
	if err != nil { t.Fatal(err) }
	users := NewUsersClient(transport)
	var input LookupUserInput
	input.Path.Id = "same-key"
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			target := i%2
			value, err := users.LookupUser(client.WithEndpoint(t.Context(), "users", servers[target].URL), input)
			if err != nil || value.Id != uint64(target+7) { t.Errorf("target %d: value=%#v err=%v", target, value, err) }
		})
	}
	wg.Wait()
	if calls[0].Load() != 1 || calls[1].Load() != 1 { t.Fatalf("upstream calls=(%d,%d)", calls[0].Load(), calls[1].Load()) }
	value, err := users.LookupUser(t.Context(), input)
	if err != nil || value.Id != 7 { t.Fatalf("default binding changed: value=%#v err=%v", value, err) }
}
`

// TestTheEmittedGoClientAnswersADeclaredCachedOperationThroughTheRealRuntime
// is the Go half of provider declaration → generation → compiled client →
// real bound call: the emitted package is compiled and run against this
// repository's runtime, and the provider sees one call per fresh window, one
// call per bypassed call, and one more after an invalidation by the declared
// response field.
func TestTheEmittedGoClientAnswersADeclaredCachedOperationThroughTheRealRuntime(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-response-cache",
		"the-emitted-go-client-answers-a-declared-cached-operation-through-the-real-runtime")
	// Only the cached operation: the package initializes every descriptor it
	// embeds, so the run needs operations that are valid at run time, not only
	// at compile time.
	spec := compileFixtureSpec()
	var cached []MethodIR
	for _, method := range spec.Services[0].Methods {
		if method.OperationID == "lookupUser" {
			cached = append(cached, method)
		}
	}
	spec.Services = []ServiceIR{{Methods: cached}}
	source, err := GenerateClientFromIR(spec, ClientGenOptions{
		PackageName: "usersclient", ClientName: "UsersClient",
	})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, source)
	if err := os.WriteFile(filepath.Join(moduleDir, "cache_e2e_test.go"), []byte(generatedCacheE2E), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off")
	if output, testErr := command.CombinedOutput(); testErr != nil {
		t.Fatalf("the emitted cached client failed against the real runtime: %v\n%s", testErr, output)
	}
}
