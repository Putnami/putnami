import { afterEach, describe, expect } from 'bun:test';
import { api, type Application, application, endpoint, grpc, http, type HttpPlugin, proto } from '@putnami/application';
import { NotFoundException, Stream } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { ConnectTransport } from '../../src/runtime/connect-transport';
import { ClientStreamError } from '../../src/runtime/errors';
import type { DuplexStream } from '../../src/runtime/stream.type';

/**
 * The Connect transport carries client and bidirectional streams over an
 * HTTP/2 session to a real TypeScript provider on a real port.
 */

interface Tally {
  id: string;
  count: number;
  total: number;
}

const SUM_PATH = '/test.v1.TalliesService/ListTalliesByIdSum';
const RUNNING_PATH = '/test.v1.TalliesService/ListTalliesByIdRunning';

describe('ConnectTransport.duplex', () => {
  let app: Application | undefined;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
  });

  async function start(): Promise<string> {
    const httpPlugin: HttpPlugin = http({ port: 0 });
    const apiPlugin = api({ autoScan: false });
    apiPlugin.register(
      '/tallies/[id]/sum',
      {
        GET: endpoint()
          .params({ id: String })
          .body(Stream({ delta: Number }))
          .returns({ id: String, count: Number, total: Number })
          .handle(async (ctx) => {
            if (ctx.params.id === 'missing') throw new NotFoundException('tally not found');
            let count = 0;
            let total = 0;
            for await (const message of ctx.messages()) {
              count++;
              total += message.delta;
            }
            return { id: ctx.params.id, count, total };
          }),
      },
      'GET',
    );
    apiPlugin.register(
      '/tallies/[id]/running',
      {
        GET: endpoint()
          .params({ id: String })
          .body(Stream({ delta: Number }))
          .returns(Stream({ id: String, count: Number, total: Number }))
          .handle(async (ctx) => {
            let count = 0;
            let total = 0;
            for await (const message of ctx.messages()) {
              count++;
              total += message.delta;
              ctx.send({ id: ctx.params.id, count, total });
            }
            return { id: ctx.params.id, count, total };
          }),
      },
      'GET',
    );
    apiPlugin.register(
      '/tallies/[id]/greet',
      {
        GET: endpoint()
          .params({ id: String })
          .body(Stream({ delta: Number }))
          .returns(Stream({ id: String, count: Number, total: Number }))
          .handle(async (ctx) => {
            // The provider speaks first, before the caller sent any value.
            ctx.send({ id: ctx.params.id, count: 0, total: 0 });
            let count = 0;
            let total = 0;
            for await (const message of ctx.messages()) {
              count++;
              total += message.delta;
            }
            return { id: ctx.params.id, count, total };
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

  /** Every message to the terminal, under a controlled deadline. */
  function collect<T>(stream: DuplexStream<unknown, T>, timeoutMs = 5000): Promise<T[]> {
    return new Promise((resolve, reject) => {
      const messages: T[] = [];
      const timer = setTimeout(() => reject(new Error('stream did not terminate')), timeoutMs);
      stream.onMessage((message) => messages.push(message));
      stream.onError((error) => {
        clearTimeout(timer);
        reject(error);
      });
      stream.onComplete(() => {
        clearTimeout(timer);
        resolve(messages);
      });
    });
  }

  specTest(
    'carries a client stream over HTTP/2 and delivers its one result',
    {
      feature: 'typescript/service-clients',
      requirement: 'connect-duplex-streams',
      check: 'a-connect-client-stream-delivers-its-one-result',
    },
    async () => {
      const transport = new ConnectTransport(await start(), 'test.v1');
      const stream = transport.duplex<{ delta: number }, Tally>(SUM_PATH, { params: { id: 'a' } });
      const result = collect(stream);
      stream.send({ delta: 2 });
      stream.send({ delta: 3 });
      stream.send({ delta: 5 });
      stream.end();

      expect(await result).toEqual([{ id: 'a', count: 3, total: 10 }]);
    },
  );

  specTest(
    'delivers each bidirectional answer while the caller still sends',
    {
      feature: 'typescript/service-clients',
      requirement: 'connect-duplex-streams',
      check: 'a-connect-bidirectional-stream-delivers-each-answer-while-the-caller-still-sends',
    },
    async () => {
      const transport = new ConnectTransport(await start(), 'test.v1');
      const stream = transport.duplex<{ delta: number }, Tally>(RUNNING_PATH, { params: { id: 'b' } });
      const received: Tally[] = [];
      let answered: (() => void) | undefined;
      const completed = new Promise<void>((resolve, reject) => {
        stream.onMessage((message) => {
          received.push(message);
          answered?.();
        });
        stream.onError(reject);
        stream.onComplete(resolve);
      });
      const nextAnswer = () =>
        new Promise<void>((resolve) => {
          answered = resolve;
        });

      // Each answer is awaited before the next message is sent: a transport
      // that buffered the request until end() would never answer here.
      let waiting = nextAnswer();
      stream.send({ delta: 1 });
      await waiting;
      expect(received).toEqual([{ id: 'b', count: 1, total: 1 }]);
      waiting = nextAnswer();
      stream.send({ delta: 4 });
      await waiting;
      expect(received).toEqual([
        { id: 'b', count: 1, total: 1 },
        { id: 'b', count: 2, total: 5 },
      ]);
      stream.end();
      await completed;

      // The handler's returned value is the last message before completion.
      expect(received.at(-1)).toEqual({ id: 'b', count: 2, total: 5 });
      expect(received).toHaveLength(3);
    },
  );

  specTest(
    'delivers what a provider says first before the caller sends anything',
    {
      feature: 'typescript/service-clients',
      requirement: 'connect-duplex-streams',
      check: 'a-connect-bidirectional-stream-delivers-each-answer-while-the-caller-still-sends',
    },
    async () => {
      const transport = new ConnectTransport(await start(), 'test.v1');
      const stream = transport.duplex<{ delta: number }, Tally>('/test.v1.TalliesService/ListTalliesByIdGreet', {
        params: { id: 'g' },
      });
      const greeting = await new Promise<Tally>((resolve, reject) => {
        stream.onMessage(resolve);
        stream.onError(reject);
      });
      expect(greeting).toEqual({ id: 'g', count: 0, total: 0 });
      stream.cancel();
    },
  );

  specTest(
    'surfaces a failure in the end-of-stream message as the typed stream error',
    {
      feature: 'typescript/service-clients',
      requirement: 'connect-duplex-streams',
      check: 'a-connect-duplex-stream-failure-is-the-typed-stream-error',
    },
    async () => {
      const transport = new ConnectTransport(await start(), 'test.v1');
      const stream = transport.duplex<{ delta: number }, Tally>(SUM_PATH, { params: { id: 'missing' } });
      const result = collect(stream);
      stream.end();

      const error = await result.then(
        () => undefined,
        (failure: unknown) => failure,
      );
      expect(error).toBeInstanceOf(ClientStreamError);
      expect((error as ClientStreamError).grpcStatus).toBe('NOT_FOUND');
    },
  );

  specTest(
    'stops a cancelled stream without reporting a failure',
    {
      feature: 'typescript/service-clients',
      requirement: 'connect-duplex-streams',
      check: 'a-connect-duplex-stream-failure-is-the-typed-stream-error',
    },
    async () => {
      const transport = new ConnectTransport(await start(), 'test.v1');
      const stream = transport.duplex<{ delta: number }, Tally>(RUNNING_PATH, { params: { id: 'c' } });
      const outcomes: string[] = [];
      const first = new Promise<void>((resolve) => {
        stream.onMessage(() => {
          outcomes.push('message');
          resolve();
        });
      });
      stream.onError(() => outcomes.push('error'));
      stream.onComplete(() => outcomes.push('complete'));
      stream.send({ delta: 1 });
      await first;
      stream.cancel();
      stream.send({ delta: 2 });
      await Bun.sleep(50);

      expect(outcomes).toEqual(['message']);
    },
  );
});
