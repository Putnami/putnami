import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { Constrained, Desc, Int, MapOf, OneOf, Optional } from '@putnami/runtime';
import { IntWidth } from '../../src/api/route/schema-wire';
import { decodeProto, encodeProto, type ProtoEnumRegistry } from '../../src/grpc/proto-codec';
import { generateProto, type ProtoFieldMeta, protoJsonName, toSnakeCase } from '../../src/proto';
import type { DiscoveredRoute } from '../../src/api';

/**
 * Descriptor semantics: what a field's declared shape means on the wire.
 *
 * The wire facts here are protobuf's, not this framework's, and each one is
 * stated as a byte-level assertion rather than as a round-trip through this
 * codec — a round-trip agrees with itself whatever it writes.
 */

function fields(...entries: ProtoFieldMeta[]): ProtoFieldMeta[] {
  return entries;
}

function hex(bytes: Uint8Array): string {
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
}

describe('integer width is declared, never inferred (D0.2)', () => {
  const route: DiscoveredRoute = {
    method: 'POST',
    path: '/items',
    schemas: { body: { count: Int, tally: MapOf(Int, Int) } },
  } as DiscoveredRoute;

  specTest(
    'the contract type int is 64 bits on the Connect wire too',
    {
      feature: 'typescript/api-contracts',
      requirement: 'connect-descriptors',
      check: 'a-contract-int-projects-to-int64-in-the-descriptor',
    },
    () => {
      const proto = generateProto([route], { packageName: 'test.v1' });
      const message = proto.messageMeta['CreateItemsRequestBody'];
      expect(message.find((field) => field.name === 'count')?.type).toBe('int64');
      // A map narrows nothing either: a key or value that survives REST intact
      // must survive Connect intact.
      const tally = message.find((field) => field.name === 'tally');
      expect(tally?.mapKeyType).toBe('int64');
      expect(tally?.mapValueType).toBe('int64');
    },
  );

  it('narrows to the width the field declared, and only to that', () => {
    const declared: DiscoveredRoute = {
      method: 'POST',
      path: '/items',
      schemas: { body: { small: Constrained(Int, IntWidth('int32')), wide: Int } },
    } as DiscoveredRoute;
    const message = generateProto([declared], { packageName: 'test.v1' }).messageMeta['CreateItemsRequestBody'];
    // A 32-bit field is one the author declared; it is never a side effect of
    // choosing protobuf for the transport.
    expect(message.find((field) => field.name === 'small')?.type).toBe('int32');
    expect(message.find((field) => field.name === 'wide')?.type).toBe('int64');
  });

  it('writes a 64-bit varint for a value past 2^31', () => {
    const meta = fields({
      name: 'count',
      jsonName: 'count',
      number: 1,
      type: 'int64',
      optional: false,
      repeated: false,
    });
    // 2^53 + 1 — the first integer JavaScript's Number cannot represent.
    const encoded = encodeProto({ count: 9007199254740993n }, meta);
    expect(hex(encoded)).toBe('088180808080808010');
    expect(decodeProto(encoded, meta)['count']).toBe(9007199254740993n);
  });

  it('sign-extends a negative int32 to ten bytes, as protobuf specifies', () => {
    const meta = fields({
      name: 'delta',
      jsonName: 'delta',
      number: 1,
      type: 'int32',
      optional: false,
      repeated: false,
    });
    const encoded = encodeProto({ delta: -1 }, meta);
    // tag 0x08, then ten 0xff/0x01 bytes: the low 32 bits of -1 sign-extended
    // to 64. A five-byte varint here reads as 4294967295 on a conforming peer.
    expect(hex(encoded)).toBe('08ffffffffffffffffff01');
    expect(decodeProto(encoded, meta)['delta']).toBe(-1);
  });

  it('reads a peer ten-byte negative int32 back as a negative number', () => {
    const meta = fields({
      name: 'delta',
      jsonName: 'delta',
      number: 1,
      type: 'int32',
      optional: false,
      repeated: false,
    });
    const fromPeer = new Uint8Array([0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01]);
    expect(decodeProto(fromPeer, meta)['delta']).toBe(-1);
  });

  it('keeps a uint64 past the safe integer exact', () => {
    const meta = fields({ name: 'seq', jsonName: 'seq', number: 1, type: 'uint64', optional: false, repeated: false });
    const encoded = encodeProto({ seq: 18446744073709551615n }, meta);
    expect(decodeProto(encoded, meta)['seq']).toBe(18446744073709551615n);
  });
});

describe('presence', () => {
  const implicit = fields(
    { name: 'name', jsonName: 'name', number: 1, type: 'string', optional: false, repeated: false },
    { name: 'count', jsonName: 'count', number: 2, type: 'int64', optional: false, repeated: false },
    { name: 'active', jsonName: 'active', number: 3, type: 'bool', optional: false, repeated: false },
  );
  const explicit = implicit.map((field) => ({ ...field, optional: true }));

  specTest(
    'an implicit-presence zero is omitted and read back as that zero',
    {
      feature: 'typescript/api-contracts',
      requirement: 'connect-descriptors',
      check: 'implicit-presence-omits-the-zero-value-and-restores-it',
    },
    () => {
      expect(encodeProto({ name: '', count: 0, active: false }, implicit).length).toBe(0);
      expect(decodeProto(new Uint8Array(0), implicit)).toEqual({ name: '', count: 0, active: false });
    },
  );

  specTest(
    'an explicit-presence zero is written, and its absence stays observable',
    {
      feature: 'typescript/api-contracts',
      requirement: 'connect-descriptors',
      check: 'explicit-presence-keeps-a-zero-on-the-wire-and-an-absence-absent',
    },
    () => {
      // The whole point of `optional`: `0` and "not set" are different messages.
      const written = encodeProto({ name: '', count: 0, active: false }, explicit);
      expect(written.length).toBeGreaterThan(0);
      expect(decodeProto(written, explicit)).toEqual({ name: '', count: 0, active: false });
      expect(decodeProto(new Uint8Array(0), explicit)).toEqual({});
    },
  );

  it('gives a repeated field no presence of its own', () => {
    const repeated = fields({
      name: 'ids',
      jsonName: 'ids',
      number: 1,
      type: 'int64',
      optional: false,
      repeated: true,
    });
    expect(encodeProto({ ids: [] }, repeated).length).toBe(0);
    expect(decodeProto(new Uint8Array(0), repeated)).toEqual({ ids: [] });
  });

  it('gives a map field no presence of its own', () => {
    const map = fields({
      name: 'labels',
      jsonName: 'labels',
      number: 1,
      type: 'map',
      optional: false,
      repeated: false,
      mapKeyType: 'string',
      mapValueType: 'string',
    });
    expect(encodeProto({ labels: {} }, map).length).toBe(0);
    expect(decodeProto(new Uint8Array(0), map)['labels']).toEqual({});
  });

  it('leaves a nested message absent rather than empty when the peer omitted it', () => {
    const meta = fields({
      name: 'inner',
      jsonName: 'inner',
      number: 1,
      type: 'Inner',
      optional: false,
      repeated: false,
    });
    const messages = {
      Inner: fields({ name: 'v', jsonName: 'v', number: 1, type: 'string', optional: false, repeated: false }),
    };
    // Proto3 gives message fields explicit presence: an absent message is not
    // an empty message.
    expect(decodeProto(new Uint8Array(0), meta, messages)['inner']).toBeUndefined();
  });

  it('keeps a zero map key and a zero map value', () => {
    const map = fields({
      name: 'counts',
      jsonName: 'counts',
      number: 1,
      type: 'map',
      optional: false,
      repeated: false,
      mapKeyType: 'int64',
      mapValueType: 'int64',
    });
    // Both members of a map entry carry explicit presence: an entry keyed on 0
    // with value 0 must survive, not arrive as an entry with neither member.
    const encoded = encodeProto({ counts: { 0: 0, 7: 3 } }, map);
    expect(decodeProto(encoded, map)['counts']).toEqual({ 0: 0, 7: 3 });
  });
});

describe('oneof', () => {
  const meta = fields(
    { name: 'text', jsonName: 'text', number: 1, type: 'string', optional: false, repeated: false, oneof: 'payload' },
    { name: 'count', jsonName: 'count', number: 2, type: 'int64', optional: false, repeated: false, oneof: 'payload' },
  );

  specTest(
    'a oneof carries at most one member, in both directions',
    {
      feature: 'typescript/api-contracts',
      requirement: 'connect-descriptors',
      check: 'a-oneof-refuses-two-members-and-resolves-a-malformed-message-to-the-last',
    },
    () => {
      // Writing two members would hand the peer a message whose meaning depends
      // on field order.
      expect(() => encodeProto({ text: 'a', count: 1n }, meta)).toThrow(/oneof payload carries more than one member/);

      // A member holding its zero value is still the set member: a `oneof`
      // has explicit presence even for a scalar.
      expect(encodeProto({ text: '' }, meta).length).toBeGreaterThan(0);

      // A malformed message carrying two members resolves last-one-wins, and
      // the loser is cleared rather than left set alongside the winner.
      const both = new Uint8Array([...encodeProto({ text: 'a' }, meta), ...encodeProto({ count: 5n }, meta)]);
      expect(decodeProto(both, meta)).toEqual({ count: 5 });
    },
  );

  it('leaves every member absent when the peer set none', () => {
    expect(decodeProto(new Uint8Array(0), meta)).toEqual({});
  });
});

describe('bytes', () => {
  const meta = fields({ name: 'blob', jsonName: 'blob', number: 1, type: 'bytes', optional: false, repeated: false });

  it('writes bytes length-delimited and reads them back byte for byte', () => {
    const value = new Uint8Array([0x00, 0xff, 0x7f, 0x80]);
    const encoded = encodeProto({ blob: value }, meta);
    expect(hex(encoded)).toBe('0a0400ff7f80');
    expect(decodeProto(encoded, meta)['blob']).toEqual(value);
  });

  it('treats empty bytes as the implicit-presence zero', () => {
    expect(encodeProto({ blob: new Uint8Array(0) }, meta).length).toBe(0);
    expect(decodeProto(new Uint8Array(0), meta)['blob']).toEqual(new Uint8Array(0));
  });
});

describe('enums with an initial zero', () => {
  const route: DiscoveredRoute = {
    method: 'POST',
    path: '/widgets',
    schemas: { body: { state: Desc('state', OneOf('active', 'retired')) } },
  } as DiscoveredRoute;

  specTest(
    'a declared string enum travels as its wire number, with zero reserved',
    {
      feature: 'typescript/api-contracts',
      requirement: 'connect-descriptors',
      check: 'an-enum-reserves-zero-for-unspecified-and-numbers-members-from-one',
    },
    () => {
      const proto = generateProto([route], { packageName: 'test.v1' });
      expect(proto.content).toContain('CREATE_WIDGETS_REQUEST_BODY_STATE_UNSPECIFIED = 0;');
      expect(proto.content).toContain('CREATE_WIDGETS_REQUEST_BODY_STATE_ACTIVE = 1;');
      expect(proto.content).toContain('CREATE_WIDGETS_REQUEST_BODY_STATE_RETIRED = 2;');

      const enums: ProtoEnumRegistry = {
        types: new Set(proto.enumTypes),
        values: proto.enumMeta,
      };
      const meta = proto.messageMeta['CreateWidgetsRequestBody'];
      const encoded = encodeProto({ state: 'retired' }, meta, proto.messageMeta, enums.types, enums.values);
      // Field 1, varint, value 2 — the declared member's position plus one.
      expect(hex(encoded)).toBe('0802');
      expect(decodeProto(encoded, meta, proto.messageMeta, enums.types, enums.values)['state']).toBe('retired');
    },
  );

  it('refuses a member the contract never declared', () => {
    const proto = generateProto([route], { packageName: 'test.v1' });
    const enums = { types: new Set(proto.enumTypes), values: proto.enumMeta };
    expect(() =>
      encodeProto(
        { state: 'melted' },
        proto.messageMeta['CreateWidgetsRequestBody'],
        proto.messageMeta,
        enums.types,
        enums.values,
      ),
    ).toThrow(/is not a declared member of enum/);
  });

  it('reads the unspecified zero back as absent, not as a declared state', () => {
    const proto = generateProto([route], { packageName: 'test.v1' });
    const enums = { types: new Set(proto.enumTypes), values: proto.enumMeta };
    const meta = proto.messageMeta['CreateWidgetsRequestBody'];
    const decoded = decodeProto(new Uint8Array(0), meta, proto.messageMeta, enums.types, enums.values);
    expect(decoded['state']).toBeUndefined();
  });

  it('keeps a member number the contract does not know yet', () => {
    // Protobuf keeps an unknown enum value; dropping it would hide a provider
    // that has added a member since this client was generated.
    const proto = generateProto([route], { packageName: 'test.v1' });
    const enums = { types: new Set(proto.enumTypes), values: proto.enumMeta };
    const meta = proto.messageMeta['CreateWidgetsRequestBody'];
    const fromFuture = new Uint8Array([0x08, 0x09]);
    expect(decodeProto(fromFuture, meta, proto.messageMeta, enums.types, enums.values)['state']).toBe(9);
  });
});

describe('jsonName comes from the proto emitter', () => {
  const route: DiscoveredRoute = {
    method: 'POST',
    path: '/widgets',
    schemas: { body: { widgetName: Optional(String), retryCount: Int } },
  } as DiscoveredRoute;

  specTest(
    'every emitted field carries the json_name protobuf derives from its wire name',
    {
      feature: 'typescript/api-contracts',
      requirement: 'connect-descriptors',
      check: 'the-descriptor-carries-the-emitter-json-name-for-every-field',
    },
    () => {
      const proto = generateProto([route], { packageName: 'test.v1' });
      for (const message of Object.values(proto.messageMeta)) {
        for (const field of message) {
          expect(field.jsonName).toBe(protoJsonName(field.name));
        }
      }
      const body = proto.messageMeta['CreateWidgetsRequestBody'];
      expect(body.map((field) => [field.name, field.jsonName])).toEqual([
        ['widget_name', 'widgetName'],
        ['retry_count', 'retryCount'],
      ]);
      // The declared property name round-trips: a decoded message uses the same
      // key the declaration used, on either encoding.
      for (const name of ['widgetName', 'retryCount']) {
        expect(protoJsonName(toSnakeCase(name))).toBe(name);
      }
    },
  );

  it('decodes into json_name keys, so both encodings produce the same object', () => {
    const proto = generateProto([route], { packageName: 'test.v1' });
    const meta = proto.messageMeta['CreateWidgetsRequestBody'];
    const encoded = encodeProto({ widgetName: 'gear', retryCount: 3n }, meta, proto.messageMeta);
    expect(decodeProto(encoded, meta, proto.messageMeta)).toEqual({ widgetName: 'gear', retryCount: 3 });
  });
});
