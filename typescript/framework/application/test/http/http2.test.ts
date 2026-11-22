import { afterEach, describe, expect } from 'bun:test';
import http2 from 'node:http2';
import { Stream } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { api, endpoint } from '../../src/api';
import { type Application, application } from '../../src/application';
import { http, type HttpPlugin } from '../../src/http/http.plugin';
import { arrivedOverHttp2 } from '../../src/http/http-version';

describe('cleartext HTTP/2', () => {
  let app: Application | undefined;
  let httpPlugin: HttpPlugin;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
  });

  async function start(): Promise<number> {
    httpPlugin = http({ port: 0 });
    const apiPlugin = api({ autoScan: false });
    apiPlugin.register(
      '/hello',
      {
        GET: endpoint()
          .returns({ greeting: String })
          .handle(() => ({ greeting: 'hello' })),
      },
      'GET',
    );
    apiPlugin.register(
      '/echo',
      {
        GET: endpoint()
          .body(Stream({ text: String }))
          .returns(Stream({ text: String }))
          .handle(async (ctx) => {
            let last = '';
            for await (const message of ctx.messages()) {
              last = message.text;
              ctx.send({ text: last });
            }
            return { text: last };
          }),
      },
      'GET',
    );
    app = application().use(httpPlugin).use(apiPlugin);
    await app.start();
    return httpPlugin.getServer()?.port as number;
  }

  specTest(
    'serves HTTP/2 with prior knowledge beside HTTP/1.1 and WebSocket on one port',
    {
      feature: 'typescript/http-services',
      requirement: 'cleartext-http2',
      check: 'a-prior-knowledge-http2-request-is-served-beside-http1-and-websocket-on-one-port',
    },
    async () => {
      const port = await start();

      const overHttp2 = await new Promise<{ status: number; body: string }>((resolve, reject) => {
        const session = http2.connect(`http://localhost:${port}`);
        const stream = session.request({ ':method': 'GET', ':path': '/hello', accept: 'application/json' });
        let status = 0;
        let body = '';
        stream.on('response', (headers) => {
          status = Number(headers[':status']);
        });
        stream.on('data', (chunk: Buffer) => {
          body += chunk.toString();
        });
        stream.on('end', () => {
          session.close();
          resolve({ status, body });
        });
        stream.on('error', reject);
        stream.end();
      });
      expect(overHttp2.status).toBe(200);
      expect(JSON.parse(overHttp2.body)).toEqual({ greeting: 'hello' });

      const overHttp1 = await fetch(`http://localhost:${port}/hello`);
      expect(overHttp1.status).toBe(200);
      expect(await overHttp1.json()).toEqual({ greeting: 'hello' });

      const echoed = await new Promise<string>((resolve, reject) => {
        const socket = new WebSocket(`ws://localhost:${port}/echo`);
        socket.onopen = () => socket.send(JSON.stringify({ text: 'over websocket' }));
        socket.onmessage = (event) => {
          resolve(String(event.data));
          socket.close();
        };
        socket.onerror = () => reject(new Error('websocket failed'));
      });
      expect(JSON.parse(echoed)).toEqual({ text: 'over websocket' });
    },
  );

  specTest(
    'reads a request that carries an HTTP/1.1 framing header as HTTP/1.1',
    {
      feature: 'typescript/http-services',
      requirement: 'cleartext-http2',
      check: 'a-prior-knowledge-http2-request-is-served-beside-http1-and-websocket-on-one-port',
    },
    () => {
      const request = (headers: Record<string, string>) => new Request('http://localhost/', { headers });
      expect(arrivedOverHttp2(request({ host: 'localhost', 'content-type': 'application/connect+json' }))).toBe(true);
      for (const name of ['connection', 'keep-alive', 'transfer-encoding', 'upgrade', 'content-length']) {
        expect(arrivedOverHttp2(request({ [name]: name === 'content-length' ? '3' : 'x' }))).toBe(false);
      }
    },
  );
});
