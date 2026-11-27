package openapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// redirectTSConsumer generates a TypeScript client from the provider's
// published OpenAPI with the workspace's own emitter, loads it against the
// workspace's @putnami/client runtime, and calls the provider over a real
// socket.
const redirectTSConsumer = `
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
const root = process.env.REDIRECT_ROOT!;
const directory = process.env.REDIRECT_DIR!;
const target = process.env.REDIRECT_TARGET_URL!;
const { readOpenApiSource } = await import(join(root, 'typescript/framework/client/src/generator/openapi-reader.ts'));
const { generateTypeScriptClient } = await import(join(root, 'typescript/framework/client/src/generator/ts/ts-generator.ts'));
const ir = readOpenApiSource(readFileSync(join(directory, 'openapi.json'), 'utf8'), { mode: 'firstParty' });
const methods = ir.services[0].methods;
const open = methods.find((method) => method.path === '/artifacts/{id}');
const resolve = methods.find((method) => method.path === '/artifacts/{id}/resolve');
if (!open || !resolve) throw Error('the reader left a redirect operation out: ' + methods.map((method) => method.path).join(','));
const output = join(directory, 'ts');
for (const file of generateTypeScriptClient(ir, { packageName: '@test/artifacts' })) {
  const path = join(output, file.path);
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, file.content.replaceAll("'@putnami/client'", JSON.stringify(join(root, 'typescript/framework/client/src/index.ts'))));
}
const generated = await import(join(output, 'src/index.ts'));
const bind = generated['bind' + ir.services[0].className];
if (typeof bind !== 'function') throw Error('missing generated binder');
const client = bind({ url: process.env.REDIRECT_PROVIDER_URL!, clientId: 'consumer', allowInsecure: true });
try {
  const opened = await client[open.name]({ path: { id: 'a1' } });
  if (opened.status !== 302 || opened.location !== target + '/blobs/a1' || opened.data !== undefined)
    throw Error('open a redirecting artifact: ' + JSON.stringify(opened));
  const held = await client[resolve.name]({ path: { id: 'here' } });
  if (held.status !== 200 || held.data?.id !== 'here' || held.location !== undefined)
    throw Error('resolve an artifact the provider holds: ' + JSON.stringify(held));
  const moved = await client[resolve.name]({ path: { id: 'far' } });
  if (moved.status !== 307 || moved.location !== target + '/blobs/far' || moved.data !== undefined)
    throw Error('resolve an artifact that lives elsewhere: ' + JSON.stringify(moved));
} finally {
  client.dispose();
}
`

// Provider declaration → generation → loaded client → real bound call, for a
// redirecting Go route and the TypeScript client generated from its contract:
// the client hands back the declared 3xx and its Location, and never follows
// it to the target.
func TestTheEmittedTypeScriptClientAnswersADeclaredRedirectWithoutFollowingIt(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "redirect-success",
		"the-emitted-typescript-client-answers-a-declared-redirect-without-following-it-through-the-real-runtime")
	var followed atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	httpServer, apiPlugin := redirectProvider(target.URL)
	out := describeRedirectProvider(t, apiPlugin)
	contract, err := os.ReadFile(filepath.Join(out, DefaultDescribePath)) //nolint:gosec // G304: test-owned temporary directory
	if err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(httpServer.Handler())
	defer provider.Close()

	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "openapi.json"), contract, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "consumer.ts"), []byte(redirectTSConsumer), 0o600); err != nil {
		t.Fatal(err)
	}
	root := workspaceRoot(t)
	command := exec.Command("bun", filepath.Join(directory, "consumer.ts"))
	command.Dir = root
	command.Env = append(os.Environ(), "REDIRECT_ROOT="+root, "REDIRECT_DIR="+directory,
		"REDIRECT_PROVIDER_URL="+provider.URL, "REDIRECT_TARGET_URL="+target.URL)
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("the emitted TypeScript client failed against the real provider: %v\n%s", runErr, output)
	}
	if hits := followed.Load(); hits != 0 {
		t.Fatalf("the emitted TypeScript client followed the redirect to its target %d time(s)", hits)
	}
}
