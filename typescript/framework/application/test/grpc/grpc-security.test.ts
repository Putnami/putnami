import { afterEach, describe, expect, it } from 'bun:test';
import { Stream } from '@putnami/runtime';
import { api, endpoint } from '../../src/api';
import { type Application, application } from '../../src/application';
import { grpc } from '../../src/grpc/grpc.plugin';
import { http, type HttpPlugin } from '../../src/http/http.plugin';
import { proto } from '../../src/proto';

// The Connect bridge serves the same routes as REST, so it owes them the same
// admission decision. These are the two shapes a guard refuses in: a unary call
// and a server stream.

describe('gRPC security', () => {
  let app: Application | undefined;
  let httpPlugin: HttpPlugin;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
  });

  /** An application whose two routes require an authenticated caller. */
  async function securedApp(): Promise<string> {
    httpPlugin = http({ port: 0 });
    const apiPlugin = api({ autoScan: false });

    apiPlugin.register(
      '/secrets',
      {
        GET: endpoint()
          .returns({ value: String })
          .secure()
          .handle(() => ({ value: 'classified' })),
      },
      'GET',
    );
    apiPlugin.register(
      '/secrets/feed',
      {
        GET: endpoint()
          .returns(Stream({ value: String }))
          .secure()
          .handle((ctx) => {
            ctx.send({ value: 'classified' });
          }),
      },
      'GET',
    );

    app = application()
      .use(httpPlugin)
      .use(apiPlugin)
      .use(proto({ packageName: 'test.v1', output: false }))
      .use(grpc());
    await app.start();
    return `http://localhost:${httpPlugin.getServer()?.port}`;
  }

  it('answers a refused unary call with the Connect error, not a 200 carrying it', async () => {
    const baseUrl = await securedApp();

    const response = await fetch(`${baseUrl}/test.v1.SecretsService/ListSecrets`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'connect-protocol-version': '1' },
      body: JSON.stringify({}),
    });

    // A 200 here would be read as a successful call by every conforming client,
    // whatever the body said.
    expect(response.status).toBe(401);
    expect(await response.json()).toMatchObject({ code: 'unauthenticated' });
  });

  it('refuses a server stream before its first message instead of serving it', async () => {
    const baseUrl = await securedApp();

    const payload = new TextEncoder().encode(JSON.stringify({}));
    const frame = new Uint8Array(5 + payload.length);
    new DataView(frame.buffer).setUint32(1, payload.length);
    frame.set(payload, 5);

    const response = await fetch(`${baseUrl}/test.v1.SecretsService/ListSecretsFeed`, {
      method: 'POST',
      headers: { 'content-type': 'application/connect+json', 'connect-protocol-version': '1' },
      body: frame,
    });

    // A streaming RPC states its error in the terminal the protocol reserves,
    // under HTTP 200 — so what proves the refusal is the frame, not the status.
    const bytes = new Uint8Array(await response.arrayBuffer());
    const view = new DataView(bytes.buffer, bytes.byteOffset);
    const frames: Array<{ flags: number; text: string }> = [];
    for (let offset = 0; offset + 5 <= bytes.length; ) {
      const flags = bytes[offset] ?? 0;
      const size = view.getUint32(offset + 1);
      frames.push({ flags, text: new TextDecoder().decode(bytes.slice(offset + 5, offset + 5 + size)) });
      offset += 5 + size;
    }

    expect(frames).toHaveLength(1);
    // The one frame is the terminal, and it carries the refusal: no message of
    // the guarded stream was produced at all.
    expect(frames[0]?.flags).toBe(2);
    expect(JSON.parse(frames[0]?.text ?? '{}')).toMatchObject({ error: { code: 'unauthenticated' } });
  });
});
