import { afterEach, describe, expect, it } from 'bun:test';
import { api, endpoint } from '../../src/api';
import { type Application, application } from '../../src/application';
import { grpc, type GrpcPlugin } from '../../src/grpc/grpc.plugin';
import { http, type HttpPlugin } from '../../src/http/http.plugin';
import { proto } from '../../src/proto';

describe('gRPC Health/Check', () => {
  let app: Application | undefined;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
  });

  async function setup() {
    const httpPlugin: HttpPlugin = http({ port: 0 });
    const apiPlugin = api({ autoScan: false });
    const protoPlugin = proto({ packageName: 'test.v1', output: false });
    const grpcPlugin = grpc();

    apiPlugin.register('/items', { GET: endpoint(() => ({ ok: true })) }, 'GET');

    app = application().use(httpPlugin).use(apiPlugin).use(protoPlugin).use(grpcPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    return { baseUrl, grpcPlugin };
  }

  async function check(baseUrl: string): Promise<number> {
    const res = await fetch(`${baseUrl}/grpc.health.v1.Health/Check`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
    expect(res.status).toBe(200);
    return (await res.json()).status as number;
  }

  it('reports SERVING (1) while the app is running', async () => {
    const { baseUrl } = await setup();
    expect(await check(baseUrl)).toBe(1);
  });

  it('reports NOT_SERVING (2) once the gRPC plugin is draining', async () => {
    const { baseUrl, grpcPlugin } = await setup();
    expect(await check(baseUrl)).toBe(1);

    // Simulate the shutdown drain: the plugin's stop() runs before the HTTP
    // server closes, so health flips to NOT_SERVING while still reachable.
    await (grpcPlugin as GrpcPlugin).stop();
    expect(await check(baseUrl)).toBe(2);
  });
});
