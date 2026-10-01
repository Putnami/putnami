import { encodeProto, type ProtoFieldMeta } from '@putnami/application';
import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { ConnectTransport } from '../../src/runtime/connect-transport';
import { ClientError } from '../../src/runtime/errors';
import { ClientResponseValidationError, validateResponseShape } from '../../src/runtime/response-validator';

// ---------------------------------------------------------------------------
// Response shape metadata — mirrors what the generator embeds for a service
// whose GetUser RPC returns a `User { id: string (required), name: string
// (required), age: int32 (optional), tags: repeated string, address: Address }`.
// ---------------------------------------------------------------------------
const MESSAGE_META: Record<string, ProtoFieldMeta[]> = {
  GetUserResponse: [
    { name: 'id', number: 1, type: 'string', optional: false, repeated: false },
    { name: 'name', number: 2, type: 'string', optional: false, repeated: false },
    { name: 'age', number: 3, type: 'int32', optional: true, repeated: false },
    { name: 'active', number: 4, type: 'bool', optional: true, repeated: false },
    { name: 'tags', number: 5, type: 'string', optional: false, repeated: true },
    { name: 'address', number: 6, type: 'Address', optional: false, repeated: false },
    {
      name: 'labels',
      number: 7,
      type: 'map',
      optional: false,
      repeated: false,
      mapKeyType: 'string',
      mapValueType: 'string',
    },
  ],
  Address: [
    { name: 'street', number: 1, type: 'string', optional: false, repeated: false },
    { name: 'zip', number: 2, type: 'string', optional: true, repeated: false },
  ],
};

const SERVICE = 'myapp.v1';
const METHOD = '/myapp.v1.UsersService/GetUser';

/** A fully valid response that satisfies every required field. */
function validUser(): Record<string, unknown> {
  return {
    id: 'u-1',
    name: 'Alice',
    tags: ['admin'],
    address: { street: 'Main St' },
    labels: { tier: 'gold' },
  };
}

describe('validateResponseShape', () => {
  test('(a) missing required field throws ClientResponseValidationError with method', () => {
    // Server returns {} for a User — the headline failure mode.
    let caught: unknown;
    try {
      validateResponseShape({}, MESSAGE_META.GetUserResponse, MESSAGE_META, SERVICE, METHOD);
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(ClientResponseValidationError);
    expect(caught).toBeInstanceOf(ClientError);
    const err = caught as ClientResponseValidationError;
    expect(err.method).toBe(METHOD);
    expect(err.detail).toContain('missing required field');
    expect(err.detail).toContain('id');
    expect(err.message).toContain(METHOD);
  });

  test('(a) wrong-typed required field throws ClientResponseValidationError', () => {
    const body = { ...validUser(), name: 123 }; // name should be string
    expect(() => validateResponseShape(body, MESSAGE_META.GetUserResponse, MESSAGE_META, SERVICE, METHOD)).toThrow(
      ClientResponseValidationError,
    );
    try {
      validateResponseShape(body, MESSAGE_META.GetUserResponse, MESSAGE_META, SERVICE, METHOD);
    } catch (e) {
      expect((e as ClientResponseValidationError).detail).toContain('name');
      expect((e as ClientResponseValidationError).detail).toContain('string');
    }
  });

  test('(a) wrong-typed field inside a required nested message throws', () => {
    const body = { ...validUser(), address: {} }; // address.street missing
    try {
      validateResponseShape(body, MESSAGE_META.GetUserResponse, MESSAGE_META, SERVICE, METHOD);
      throw new Error('expected throw');
    } catch (e) {
      expect(e).toBeInstanceOf(ClientResponseValidationError);
      expect((e as ClientResponseValidationError).detail).toContain('address.street');
    }
  });

  test('(a) repeated field returned as a non-array throws', () => {
    const body = { ...validUser(), tags: 'not-an-array' };
    expect(() => validateResponseShape(body, MESSAGE_META.GetUserResponse, MESSAGE_META, SERVICE, METHOD)).toThrow(
      /should be an array/,
    );
  });

  test('(a) a primitive/null response where a message is expected throws', () => {
    expect(() => validateResponseShape(null, MESSAGE_META.GetUserResponse, MESSAGE_META, SERVICE, METHOD)).toThrow(
      ClientResponseValidationError,
    );
    expect(() => validateResponseShape('oops', MESSAGE_META.GetUserResponse, MESSAGE_META, SERVICE, METHOD)).toThrow(
      /expected response to be an object/,
    );
  });

  test('(b) a correct response passes and is not mutated', () => {
    const body = validUser();
    expect(() =>
      validateResponseShape(body, MESSAGE_META.GetUserResponse, MESSAGE_META, SERVICE, METHOD),
    ).not.toThrow();
    // Optional fields legitimately absent (age, active) do not trigger a throw.
    expect(body.age).toBeUndefined();
  });

  test('(b) camelCase JSON keys are accepted for snake_case proto fields', () => {
    // The Connect JSON path forwards the handler object, which may be camelCase.
    const META: Record<string, ProtoFieldMeta[]> = {
      Resp: [{ name: 'user_id', number: 1, type: 'string', optional: false, repeated: false }],
    };
    expect(() => validateResponseShape({ userId: 'u-9' }, META.Resp, META, SERVICE, METHOD)).not.toThrow();
    // And snake_case still works.
    expect(() => validateResponseShape({ user_id: 'u-9' }, META.Resp, META, SERVICE, METHOD)).not.toThrow();
  });

  test('(c) extra/unknown fields do NOT cause rejection', () => {
    const body = { ...validUser(), futureField: 'whatever', another: { nested: true } };
    expect(() =>
      validateResponseShape(body, MESSAGE_META.GetUserResponse, MESSAGE_META, SERVICE, METHOD),
    ).not.toThrow();
  });

  test('optional fields with wrong types are still rejected when present', () => {
    const body = { ...validUser(), age: 'thirty' }; // age is int32 → number
    expect(() => validateResponseShape(body, MESSAGE_META.GetUserResponse, MESSAGE_META, SERVICE, METHOD)).toThrow(
      /should be a number/,
    );
  });

  test('accepts bigint values emitted by the binary decoder for large 64-bit integers', () => {
    const META: Record<string, ProtoFieldMeta[]> = {
      CounterResponse: [{ name: 'count', number: 1, type: 'uint64', optional: false, repeated: false }],
    };

    expect(() =>
      validateResponseShape({ count: BigInt('18446744073709551615') }, META.CounterResponse, META, SERVICE, METHOD),
    ).not.toThrow();
    expect(() => validateResponseShape({ count: 42 }, META.CounterResponse, META, SERVICE, METHOD)).not.toThrow();
    expect(() => validateResponseShape({ count: '42' }, META.CounterResponse, META, SERVICE, METHOD)).toThrow(
      /number or bigint/,
    );
  });
});

// ---------------------------------------------------------------------------
// Transport-level wiring: validation runs before a response is returned.
// ---------------------------------------------------------------------------

let server: ReturnType<typeof Bun.serve>;
let baseUrl: string;

const ROUTES: Record<string, () => Response> = {
  // Valid User
  '/myapp.v1.UsersService/GetUser': () =>
    Response.json({ id: 'u-1', name: 'Alice', tags: [], address: { street: 'Main' }, labels: {} }),
  // Malformed: server returns {} where a User is expected
  '/myapp.v1.UsersService/GetBadUser': () => Response.json({}),
  // Method with NO embedded response metadata → must pass through unchanged
  '/myapp.v1.UsersService/GetThing': () => Response.json({ anything: 'goes', n: 1 }),
  // Valid binary proto response with a uint64 value that decodes as bigint.
  '/myapp.v1.UsersService/GetCounter': () => {
    const fields = TRANSPORT_META.messageMeta.GetCounterResponse;
    const encoded = encodeProto({ count: BigInt('18446744073709551615') }, fields);
    const body = encoded.buffer.slice(encoded.byteOffset, encoded.byteOffset + encoded.byteLength) as ArrayBuffer;
    return new Response(body, { headers: { 'Content-Type': 'application/proto' } });
  },
};

// Reuse GetUserResponse metadata for GetBadUser by aliasing the message name.
const TRANSPORT_META = {
  messageMeta: {
    ...MESSAGE_META,
    GetBadUserResponse: MESSAGE_META.GetUserResponse,
    GetCounterResponse: [{ name: 'count', number: 1, type: 'uint64', optional: false, repeated: false }],
    // Note: no `GetThingResponse` key on purpose.
  },
  enumTypes: [] as string[],
};

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    fetch(req) {
      const url = new URL(req.url);
      const handler = ROUTES[url.pathname];
      return handler ? handler() : new Response('Not found', { status: 404 });
    },
  });
  baseUrl = `http://localhost:${server.port}`;
});

afterAll(() => {
  server.stop(true);
});

describe('ConnectTransport response validation wiring', () => {
  test('(a) JSON mode: malformed response throws ClientResponseValidationError with the RPC path', async () => {
    const transport = new ConnectTransport(baseUrl, SERVICE, 'json', TRANSPORT_META);
    let caught: unknown;
    try {
      await transport.execute({ method: 'POST', path: '/myapp.v1.UsersService/GetBadUser', headers: new Headers() });
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(ClientResponseValidationError);
    expect((caught as ClientResponseValidationError).method).toBe('/myapp.v1.UsersService/GetBadUser');
  });

  test('(b) JSON mode: a valid response is returned unchanged', async () => {
    const transport = new ConnectTransport(baseUrl, SERVICE, 'json', TRANSPORT_META);
    const res = await transport.execute<{ id: string; name: string }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/GetUser',
      headers: new Headers(),
    });
    expect(res.status).toBe(200);
    expect(res.data.id).toBe('u-1');
    expect(res.data.name).toBe('Alice');
  });

  test('(b) proto mode: a valid 64-bit integer response decoded as bigint is returned unchanged', async () => {
    const transport = new ConnectTransport(baseUrl, SERVICE, 'proto', TRANSPORT_META);
    const res = await transport.execute<{ count: bigint }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/GetCounter',
      headers: new Headers(),
    });
    expect(res.status).toBe(200);
    expect(res.data.count).toBe(BigInt('18446744073709551615'));
  });

  test('(d) JSON mode: method without embedded shape metadata returns as-is (no throw)', async () => {
    const transport = new ConnectTransport(baseUrl, SERVICE, 'json', TRANSPORT_META);
    const res = await transport.execute<{ anything: string; n: number }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/GetThing',
      headers: new Headers(),
    });
    expect(res.data.anything).toBe('goes');
    expect(res.data.n).toBe(1);
  });

  test('(d) JSON mode without any protoMeta returns as-is (current behavior preserved)', async () => {
    const transport = new ConnectTransport(baseUrl, SERVICE); // no protoMeta at all
    const res = await transport.execute<Record<string, unknown>>({
      method: 'POST',
      path: '/myapp.v1.UsersService/GetBadUser', // would be invalid IF metadata existed
      headers: new Headers(),
    });
    // No metadata → no validation → empty object returned untouched.
    expect(res.data).toEqual({});
  });
});
