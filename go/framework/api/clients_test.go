package api

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/app"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// fakeSpecSource is a minimal SpecSource registered in the module so the client
// declaration can discover it via app.Collect, standing in for the openapi plugin.
type fakeSpecSource struct {
	name string
	spec []byte
	err  error
}

func (f *fakeSpecSource) Name() string                     { return f.name }
func (f *fakeSpecSource) OpenAPISpecJSON() ([]byte, error) { return f.spec, f.err }

// The declaration names no client: its options decide only the contract, and
// describe writes no client-generation contract and stages no client.
func TestClients_NamesNoTargetAndWritesNothing(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "contract-carries-attribution", "code-declarations-name-no-target")
	options := reflect.TypeOf(ClientsOptions{})
	fields := make([]string, 0, options.NumField())
	for i := range options.NumField() {
		fields = append(fields, options.Field(i).Name)
	}
	if !reflect.DeepEqual(fields, []string{"ThirdParty"}) {
		t.Fatalf("ClientsOptions fields = %v, want only ThirdParty", fields)
	}

	tmp := t.TempDir()
	apiPlugin := New(&fakeServer{})
	apiPlugin.Register(Endpoint("GET", "/items/{id}").Document())
	fake := &fakeSpecSource{name: "openapi", spec: []byte(strictItemsContract)}
	application := app.New("items-service")
	application.Use(app.NewModule("items").Feature(app.Feature{
		ID: "items/read", Name: "Item reads", Outcome: "Consumers read items", Owner: "samples",
	}).Use(apiPlugin).Use(fake).Use(Clients(ClientsOptions{}).From(apiPlugin)))
	if err := application.Describe(tmp, []string{"clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "clientgen" {
			t.Fatalf("describe wrote %s/clientgen", tmp)
		}
	}
}

func TestClients_FirstPartyDefaultRejectsUnmarkedProviderSpec(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generated-from-contract", "the-provider-document-is-read-in-memory-from-the-owning-plugin")
	fake := &fakeSpecSource{name: "openapi", spec: []byte(unmarkedContract)}
	module := app.New("svc").Use(fake)
	clients := Clients(ClientsOptions{})
	if err := clients.Configure(context.Background(), module); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	err := clients.Describe(&app.DescribeContext{OutputDir: t.TempDir(), Targets: []string{"all"}})
	if err == nil || !strings.Contains(err.Error(), "requires x-putnami-client") {
		t.Fatalf("Describe error = %v, want strict first-party marker failure", err)
	}

	thirdParty := Clients(ClientsOptions{ThirdParty: true})
	if err := thirdParty.Configure(context.Background(), module); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := thirdParty.Describe(&app.DescribeContext{OutputDir: t.TempDir(), Targets: []string{"all"}}); err != nil {
		t.Fatalf("Describe with ThirdParty: %v", err)
	}
}

// Every route carries the producer of the module that owns its api plugin:
// two feature modules keep their own features, and a module with no feature
// leaves its routes unattributed instead of borrowing the declaration's.
func TestClients_AttributesEachRouteToItsOwningModule(t *testing.T) {
	operatorAPI := New(&fakeServer{})
	operatorAPI.Register(Endpoint("GET", "/v1/operator/cli-usage").Document())
	billingAPI := New(&fakeServer{})
	billingAPI.Register(Endpoint("GET", "/v1/billing/invoices").Document())
	coreAPI := New(&fakeServer{})
	coreAPI.Register(Endpoint("GET", "/v1/health").Document())

	application := app.New("acme-platform")
	application.Use(&fakeSpecSource{name: "openapi", spec: []byte(strictEmptyContract)})
	application.Use(Clients(ClientsOptions{}))
	application.Use(app.NewModule("operator").Feature(app.Feature{
		ID: "platform/operator-cli-usage", Name: "Operator CLI usage",
		Outcome: "Operators can inspect CLI usage", Owner: "platform",
	}).Use(operatorAPI))
	application.Use(app.NewModule("billing").Feature(app.Feature{
		ID: "platform/billing", Name: "Billing", Outcome: "Customers are billed", Owner: "platform",
	}).Use(billingAPI))
	application.Use(app.NewModule("core").Use(coreAPI))

	if err := application.Describe(t.TempDir(), []string{"clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	producerOf := func(plugin *Plugin) *clientcontract.OperationProducer {
		t.Helper()
		routes := plugin.DiscoveredRoutes()
		if len(routes) != 1 {
			t.Fatalf("routes = %d, want 1", len(routes))
		}
		return routes[0].Producer
	}
	if got := producerOf(operatorAPI); !reflect.DeepEqual(got, &clientcontract.OperationProducer{Project: "acme-platform", Feature: "platform/operator-cli-usage"}) {
		t.Errorf("operator producer = %+v", got)
	}
	if got := producerOf(billingAPI); !reflect.DeepEqual(got, &clientcontract.OperationProducer{Project: "acme-platform", Feature: "platform/billing"}) {
		t.Errorf("billing producer = %+v", got)
	}
	if got := producerOf(coreAPI); got != nil {
		t.Errorf("core producer = %+v, want none", got)
	}
}

// Without the declaration, no route carries a producer.
func TestClients_RoutesCarryNoProducerWithoutTheDeclaration(t *testing.T) {
	itemsAPI := New(&fakeServer{})
	itemsAPI.Register(Endpoint("GET", "/items").Document())
	application := app.New("items-service")
	application.Use(app.NewModule("items").Feature(app.Feature{
		ID: "items/read", Name: "Item reads", Outcome: "Consumers read items", Owner: "samples",
	}).Use(itemsAPI))
	if err := application.Describe(t.TempDir(), []string{"clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for _, route := range itemsAPI.DiscoveredRoutes() {
		if route.Producer != nil {
			t.Fatalf("route %s %s carries producer %+v", route.Method, route.Path, route.Producer)
		}
	}
}

func TestClients_NoRoutesSkipsTheContractCheck(t *testing.T) {
	apiPlugin := New(&fakeServer{})
	if err := apiPlugin.Configure(context.Background(), nil); err != nil { // no endpoints registered
		t.Fatalf("api Configure: %v", err)
	}
	clients := Clients(ClientsOptions{}).From(apiPlugin)
	_ = clients.Configure(context.Background(), app.New("svc").Module)
	if err := clients.Describe(&app.DescribeContext{OutputDir: t.TempDir(), Targets: []string{"all"}}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
}

func TestClients_NoSpecSource_Errors(t *testing.T) {
	// apiPlugin omitted (nil) so the route gate is skipped; module has no SpecSource.
	clients := Clients(ClientsOptions{})
	_ = clients.Configure(context.Background(), app.New("svc").Module)
	err := clients.Describe(&app.DescribeContext{OutputDir: t.TempDir(), Targets: []string{"all"}})
	if err == nil {
		t.Fatal("expected an error when no SpecSource is registered")
	}
	if !strings.Contains(err.Error(), "openapi") {
		t.Errorf("error should point at openapi registration: %v", err)
	}
}

func TestClients_NotWanted_Skips(t *testing.T) {
	clients := Clients(ClientsOptions{})
	_ = clients.Configure(context.Background(), app.New("svc").Module)
	// Targets that don't include "clients" or "all" → describer is a no-op.
	if err := clients.Describe(&app.DescribeContext{OutputDir: t.TempDir(), Targets: []string{"openapi"}}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
}
