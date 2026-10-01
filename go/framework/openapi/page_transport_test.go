package openapi

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/security"
)

type SnapshotPage clientcontract.Page[json.RawMessage]

// The provider and transport are real. The external identity authority is the
// only double: a token minted for one audience is admitted by only that owner.
func pageProvider(t *testing.T, path, audience string) (*httptest.Server, string, []byte) {
	t.Helper()
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	httpServer.Use(security.IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
		if ctx.Header("Authorization") == "Bearer "+audience {
			return &phttp.Claims{Subject: "replica", Scopes: []string{"snapshot:read"}}
		}
		return nil
	}))
	provider := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service:     clientcontract.Service{ID: "shared.page", Audience: "https://declared.invalid"},
		Credentials: map[string]clientcontract.CredentialProfile{"owner": {Kind: clientcontract.CredentialServiceToken, Audience: "https://profile.invalid"}},
	}))
	maxBytes := int64(128 << 10)
	provider.Register(api.Endpoint("GET", path).
		Query(api.Type[clientcontract.PageQuery]()).
		Returns(api.Type[SnapshotPage]()).
		Secure(security.Options{Scopes: []string{"snapshot:read"}}).
		Client(api.ClientOperationOptions{Resilience: &clientcontract.ResiliencePolicy{MaxResponseBytes: &maxBytes}}).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			query, err := phttp.QueryAs[clientcontract.PageQuery](ctx)
			if err != nil {
				return phttp.InternalError("query")
			}
			if query.Limit != 7 {
				return phttp.InternalError("limit")
			}
			page := clientcontract.Page[json.RawMessage]{Watermark: 9007199254740993, Relation: query.Relation, Rows: json.RawMessage(`[]`)}
			if query.AfterKey == "" {
				page.OwnerConfirmedAt = time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
				page.NextKey = "opaque/+?=& cursor"
			} else if query.AfterKey != "opaque/+?=& cursor" {
				return phttp.InternalError("cursor")
			}
			return phttp.JSON(page)
		}))
	publication := NewPlugin(PluginOptions{Title: "Pages", Version: "1"}).From(provider)
	emitter := api.Clients(api.ClientsOptions{Go: api.GoClientOptions{PackageName: "pageclient", ClientName: "PageClient"}}).From(provider)
	application := app.New("pages")
	application.Use(provider).Use(publication).Use(emitter)
	out := t.TempDir()
	if err := application.Describe(out, []string{"openapi", "clients"}); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(out, api.ClientStageDir, "go", "client.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := publication.OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpServer.Handler())
	t.Cleanup(server.Close)
	return server, string(source), raw
}

func TestSharedPageGeneratedClientsBindTwoOwnerEndpoints(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "caller-resolved-bindings", "one-emitted-page-client-calls-distinct-owner-paths-and-audiences-in-go-and-typescript")
	first, source, contract := pageProvider(t, "/page", "https://owner-one.example")
	second, _, _ := pageProvider(t, "/domain/two/bootstrap", "https://owner-two.example")
	ir, err := api.ReadOpenAPISpec(contract)
	if err != nil {
		t.Fatal(err)
	}
	method := ir.Services[0].Methods[0]
	query := clientcontract.Schema{Type: "object", Properties: map[string]clientcontract.Schema{}}
	for _, parameter := range method.Parameters {
		query.Properties[parameter.Name] = parameter.Schema
		if parameter.Required {
			query.Required = append(query.Required, parameter.Name)
		}
	}
	envelope := *method.Successes[0].Content[0].Schema
	if envelope.Ref != "" {
		envelope = ir.Schemas[envelope.Ref[len("#/components/schemas/"):]]
	}
	if err := clientcontract.ValidatePageTransportSchemas(query, envelope); err != nil {
		t.Fatalf("owner conformance: %v", err)
	}
	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, source)
	if err := os.WriteFile(filepath.Join(moduleDir, "consumer_test.go"), []byte(pageGoConsumer), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "PAGE_ONE="+first.URL, "PAGE_TWO="+second.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compiled Go page consumer: %v\n%s\n%s", err, output, source)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "openapi.json"), contract, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "consumer.ts"), []byte(pageTSConsumer), 0o600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("bun", filepath.Join(moduleDir, "consumer.ts"))
	command.Dir = workspaceRoot(t)
	command.Env = append(os.Environ(), "PAGE_ROOT="+workspaceRoot(t), "PAGE_DIR="+moduleDir, "PAGE_ONE="+first.URL, "PAGE_TWO="+second.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("loaded TypeScript page consumer: %v\n%s", err, output)
	}
	t.Log("one provider declaration generated Go and TypeScript clients; both called two owner paths/audiences, preserved int64/cursors/freshness, and closed cleanly")
}

const pageGoConsumer = `package pageclient
import (
 "context"
 "os"
 "sync"
 "testing"
 "time"
 "go.putnami.dev/client"
)
func TestTwoOwners(t *testing.T) {
 var wait sync.WaitGroup
 for _, owner := range []struct{url,path,audience string}{{os.Getenv("PAGE_ONE"),"/page","https://owner-one.example"},{os.Getenv("PAGE_TWO"),"/domain/two/bootstrap","https://owner-two.example"}} {
  wait.Add(1)
  go func(){ defer wait.Done()
   paths:=map[string]string{"get_page":owner.path}
   // Discover the generated canonical identity without inventing a service id.
   for id:=range serviceDescriptor.Operations { delete(paths,"get_page");paths[id]=owner.path }
   bound,err:=BindPageClient(client.ServiceBinding{URL:owner.url,ClientID:"replica",OperationPaths:paths,Credentials:map[string]client.CredentialBinding{"owner":{Source:client.CredentialSourceGCPIDToken,Audience:owner.audience,Provider:client.TokenSourceFunc(func(_ context.Context,r client.CredentialRequest)(client.Credential,error){
    if r.Audience!=owner.audience {t.Errorf("audience=%s",r.Audience)}
    return client.Credential{Value:r.Audience,Expiry:time.Now().Add(time.Hour)},nil
   })}}})
   if err!=nil {t.Error(err);return};defer ClosePageClient(bound)
   for id:=range paths {paths[id]="/mutated"}
   var input ListPageInput;input.Query.Relation="accounts";limit:=int32(7);input.Query.Limit=&limit
   page,err:=bound.ListPage(context.Background(),input)
   if err!=nil {t.Error(err);return}
   if page.Watermark!=9007199254740993 || page.Relation!="accounts" || string(page.Rows)!="[]" || page.NextKey==nil || *page.NextKey!="opaque/+?=& cursor" || page.OwnerConfirmedAt==nil || page.OwnerConfirmedAt.Format(time.RFC3339)!="2026-09-21T00:00:00Z" {t.Errorf("first page: %+v",page);return}
   input.Query.AfterKey=page.NextKey
   page,err=bound.ListPage(context.Background(),input)
   if err!=nil || page.NextKey!=nil || page.OwnerConfirmedAt!=nil {t.Errorf("last page must omit cursor and confirmation: %+v, %v",page,err)}
   if err:=ClosePageClient(bound);err!=nil {t.Error(err)}
   if _,err:=bound.ListPage(context.Background(),input);err==nil {t.Error("closed client dispatched")}
  }()
 }
 wait.Wait()
}
`

const pageTSConsumer = `
import { mkdirSync,readFileSync,writeFileSync } from 'node:fs';
import { dirname,join } from 'node:path';
const root=process.env.PAGE_ROOT!;const directory=process.env.PAGE_DIR!;
const {readOpenApiSource}=await import(join(root,'typescript/framework/client/src/generator/openapi-reader.ts'));
const {generateTypeScriptClient}=await import(join(root,'typescript/framework/client/src/generator/ts/ts-generator.ts'));
const ir=readOpenApiSource(readFileSync(join(directory,'openapi.json'),'utf8'),{mode:'firstParty'});
const method=ir.services[0].methods[0];
const output=join(directory,'ts');
for(const file of generateTypeScriptClient(ir,{packageName:'@test/shared-page'})) {
 const path=join(output,file.path);mkdirSync(dirname(path),{recursive:true});
 writeFileSync(path,file.content.replaceAll("'@putnami/client'",JSON.stringify(join(root,'typescript/framework/client/src/index.ts'))));
}
const generated=await import(join(output,'src/index.ts'));
const bind=generated['bind'+ir.services[0].className];
if(typeof bind!=='function')throw Error('missing generated binder');
await Promise.all([
 {url:process.env.PAGE_ONE!,path:'/page',audience:'https://owner-one.example'},
 {url:process.env.PAGE_TWO!,path:'/domain/two/bootstrap',audience:'https://owner-two.example'},
].map(async owner=>{
 const paths={[method.operationId]:owner.path};
 const client=bind({url:owner.url,clientId:'replica',operationPaths:paths,credentials:{owner:{source:'gcp-id-token',audience:owner.audience,provider:async request=>{
  if(request.audience!==owner.audience)throw Error('audience changed');
  return {value:request.audience,expiresAt:new Date(Date.now()+3600000)};
 }}}});
 paths[method.operationId]='/mutated';
 try {
  let page=await client[method.name]({query:{relation:'accounts',limit:7}});
  if(page.watermark!==9007199254740993n||page.relation!=='accounts'||page.rows.length!==0||page.nextKey!=='opaque/+?=& cursor'||page.ownerConfirmedAt!=='2026-09-21T00:00:00Z')throw Error('first page lost shared semantics');
  page=await client[method.name]({query:{relation:'accounts',afterKey:page.nextKey,limit:7}});
  if(page.nextKey!==undefined||page.ownerConfirmedAt!==undefined||Object.hasOwn(page,'ownerConfirmedAt'))throw Error('terminal page must omit cursor and confirmation');
 } finally {client.dispose();}
 let refused=false;try{await client[method.name]({query:{relation:'accounts',limit:7}})}catch{refused=true}
 if(!refused)throw Error('closed client dispatched');
}));
`
