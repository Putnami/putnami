/**
 * The harness the Go→TS cells share: the real Go provider as a subprocess this
 * suite owns, started from the binary `putnami build` already produced — never
 * `go run` on sources. It binds `PORT=0` and the suite learns the bound port
 * from the reserved `putnami.ready` log marker (protocols/runtime/ready_marker.go),
 * so there is no fixed port, no port file, no scan and no sleep. Stopping it
 * sends SIGTERM, the graceful stop that drains its streams, and awaits the exit,
 * so no child survives the task.
 */
import type { Application } from '@putnami/application';
import { resetConfigLoader } from '@putnami/runtime';
import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { app as createConsumer } from '../src/consumer';

export const PROVIDER_PROJECT = 'go/samples/service-to-service';
const PROVIDER_BINARY = 'go.putnami.dev-examples-service-to-service';
const READY_KEY = 'putnami.ready';
const READY_TIMEOUT_MS = 60_000;
export const SERVICE_ID = 'items';
export const CATALOG_KEY_HEADER = 'X-Catalog-Key';
export const CATALOG_API_KEY = 'sample-catalog-key';
export const SAMPLE_TENANT = 'tenant-a';
// The Go provider's sample user identity (service/service.go): a token a
// consumer forwards, and the subject the provider names it with.
export const USER_TOKEN = 'user-token-alice';
export const USER_SUBJECT = 'alice';
/** Every credential profile the Go provider declares, all bound. */
export const ALL_CREDENTIALS = {
  'catalog-key': { source: 'static', value: CATALOG_API_KEY },
  tenant: { source: 'static', value: SAMPLE_TENANT },
  user: { source: 'forwarded-user' },
};

export function workspaceRoot(): string {
  let dir = import.meta.dir;
  while (!existsSync(join(dir, 'putnami.workspace.json'))) {
    const parent = dirname(dir);
    if (parent === dir) throw new Error(`no putnami.workspace.json above ${import.meta.dir}`);
    dir = parent;
  }
  return dir;
}

/**
 * The provider binary lives in the per-command output directory `build-compile`
 * declares: `<workspace>/.putnami/out/<project>/build/bin/<binary>`. This
 * project declares the provider project as a dependency, so the scheduler has
 * already produced it when this test runs.
 */
function providerBinary(): string {
  const binary = join(workspaceRoot(), '.putnami', 'out', PROVIDER_PROJECT, 'build', 'bin', PROVIDER_BINARY);
  if (!existsSync(binary)) {
    throw new Error(
      `provider binary ${binary} is missing; run \`putnami build --projects go.putnami.dev/examples/service-to-service\``,
    );
  }
  return binary;
}

/** Extract the bound port from one structured log line. */
export function readyPort(line: string): number | undefined {
  let record: unknown;
  try {
    record = JSON.parse(line);
  } catch {
    return undefined;
  }
  if (typeof record !== 'object' || record === null) return undefined;
  const marker = (record as Record<string, unknown>)[READY_KEY];
  if (typeof marker !== 'object' || marker === null) return undefined;
  const port = (marker as { endpoints?: Array<{ port?: number }> }).endpoints?.[0]?.port;
  return typeof port === 'number' && port > 0 ? port : undefined;
}

export interface RunningProvider {
  readonly baseUrl: string;
  readonly stop: () => Promise<void>;
}

export async function startForeignProvider(): Promise<RunningProvider> {
  const child = Bun.spawn([providerBinary()], {
    cwd: join(workspaceRoot(), PROVIDER_PROJECT),
    env: { ...process.env, PORT: '0', APP_ENV: 'test' },
    stdout: 'pipe',
    stderr: 'pipe',
  });
  const stop = async () => {
    child.kill();
    await child.exited;
  };

  const reader = (child.stdout as ReadableStream<Uint8Array>).getReader();
  const decoder = new TextDecoder();
  const deadline = Date.now() + READY_TIMEOUT_MS;
  let transcript = '';
  let pending = '';
  try {
    while (Date.now() < deadline) {
      const { done, value } = await reader.read();
      if (done) {
        const code = await child.exited;
        throw new Error(`provider exited (${code}) before announcing readiness; output:\n${transcript}`);
      }
      const chunk = decoder.decode(value, { stream: true });
      transcript += chunk;
      pending += chunk;
      const lines = pending.split('\n');
      pending = lines.pop() ?? '';
      for (const line of lines) {
        const port = readyPort(line);
        if (port !== undefined) return { baseUrl: `http://127.0.0.1:${port}`, stop };
      }
    }
  } finally {
    reader.releaseLock();
  }
  await stop();
  throw new Error(`provider never announced ${READY_KEY} within ${READY_TIMEOUT_MS}ms; output:\n${transcript}`);
}

export function bindingConfig(baseUrl: string, credentials: Record<string, unknown>): string {
  return JSON.stringify({
    clients: {
      clientId: 'catalog.consumer',
      services: { [SERVICE_ID]: { url: baseUrl, allowInsecure: true, credentials } },
    },
  });
}

export async function startConsumer(configData: string): Promise<Application> {
  process.env.CONFIG_DATA = configData;
  resetConfigLoader();
  const consumer = createConsumer();
  await consumer.start();
  return consumer;
}
