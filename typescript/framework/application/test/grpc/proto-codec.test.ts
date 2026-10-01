import { describe, expect, it } from 'bun:test';
import { createEnvelope, decodeProto, encodeProto, encodeVarint, readEnvelope } from '../../src/grpc/proto-codec';
import type { ProtoFieldMeta } from '../../src/proto/proto';

describe('proto-codec', () => {
  describe('encodeVarint / decodeVarint basics', () => {
    it('should encode small values in a single byte', () => {
      const buf = encodeVarint(1);
      expect(buf.length).toBe(1);
      expect(buf[0]).toBe(1);
    });

    it('should encode zero', () => {
      const buf = encodeVarint(0);
      expect(buf.length).toBe(1);
      expect(buf[0]).toBe(0);
    });

    it('should encode 127 in a single byte', () => {
      const buf = encodeVarint(127);
      expect(buf.length).toBe(1);
      expect(buf[0]).toBe(127);
    });

    it('should encode 128 in two bytes', () => {
      const buf = encodeVarint(128);
      expect(buf.length).toBe(2);
    });

    it('should encode 300 in two bytes', () => {
      const buf = encodeVarint(300);
      expect(buf.length).toBe(2);
      expect(buf[0]).toBe(0xac);
      expect(buf[1]).toBe(0x02);
    });
  });

  describe('encodeProto / decodeProto', () => {
    const simpleFields: ProtoFieldMeta[] = [
      { name: 'name', number: 1, type: 'string', optional: false, repeated: false },
      { name: 'age', number: 2, type: 'double', optional: false, repeated: false },
      { name: 'active', number: 3, type: 'bool', optional: false, repeated: false },
    ];

    it('should round-trip a simple message with string, double, and bool', () => {
      const data = { name: 'Alice', age: 30, active: true };
      const encoded = encodeProto(data, simpleFields);
      expect(encoded.length).toBeGreaterThan(0);

      const decoded = decodeProto(encoded, simpleFields);
      expect(decoded.name).toBe('Alice');
      expect(decoded.age).toBe(30);
      expect(decoded.active).toBe(true);
    });

    it('writes nothing for a message whose fields all hold their zero value', () => {
      // Proto3 implicit presence: an omitted field and a zero-valued field are
      // the same message, so a conforming encoder writes neither.
      expect(encodeProto({}, simpleFields).length).toBe(0);
      expect(encodeProto({ name: '', age: 0, active: false }, simpleFields).length).toBe(0);
    });

    it('reads an absent implicit-presence field back as its zero value', () => {
      const decoded = decodeProto(encodeProto({}, simpleFields), simpleFields);
      expect(decoded).toEqual({ name: '', age: 0, active: false });
    });

    it('should skip undefined fields', () => {
      const data = { name: 'Bob' };
      const encoded = encodeProto(data, simpleFields);
      const decoded = decodeProto(encoded, simpleFields);
      expect(decoded.name).toBe('Bob');
      expect(decoded.age).toBe(0);
      expect(decoded.active).toBe(false);
    });

    it('should handle camelCase input keys', () => {
      const fields: ProtoFieldMeta[] = [
        { name: 'first_name', number: 1, type: 'string', optional: false, repeated: false },
      ];
      const data = { firstName: 'Charlie' };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.first_name).toBe('Charlie');
    });

    it('should handle repeated string fields', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'tags', number: 1, type: 'string', optional: false, repeated: true }];
      const data = { tags: ['a', 'b', 'c'] };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.tags).toEqual(['a', 'b', 'c']);
    });

    it('should handle bool false correctly', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'flag', number: 1, type: 'bool', optional: false, repeated: false }];
      const data = { flag: false };
      const encoded = encodeProto(data, fields);
      // Proto3 skips default values; false is the default for bool
      // But we encode all present values
      const decoded = decodeProto(encoded, fields);
      expect(decoded.flag).toBe(false);
    });

    it('should handle nested messages', () => {
      const addressFields: ProtoFieldMeta[] = [
        { name: 'street', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'city', number: 2, type: 'string', optional: false, repeated: false },
      ];
      const userFields: ProtoFieldMeta[] = [
        { name: 'name', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'address', number: 2, type: 'Address', optional: false, repeated: false },
      ];
      const allMessages = { Address: addressFields };

      const data = { name: 'Dave', address: { street: '123 Main St', city: 'Springfield' } };
      const encoded = encodeProto(data, userFields, allMessages);
      const decoded = decodeProto(encoded, userFields, allMessages);

      expect(decoded.name).toBe('Dave');
      expect(decoded.address).toEqual({ street: '123 Main St', city: 'Springfield' });
    });

    it('should handle double with decimal precision', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'price', number: 1, type: 'double', optional: false, repeated: false }];
      const data = { price: 19.99 };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.price).toBeCloseTo(19.99, 10);
    });
  });

  describe('int32 encoding / decoding', () => {
    const int32Fields: ProtoFieldMeta[] = [
      { name: 'count', number: 1, type: 'int32', optional: false, repeated: false },
      { name: 'offset', number: 2, type: 'int32', optional: false, repeated: false },
    ];

    it('should round-trip int32 fields as varint', () => {
      const data = { count: 42, offset: 100 };
      const encoded = encodeProto(data, int32Fields);
      expect(encoded.length).toBeGreaterThan(0);

      const decoded = decodeProto(encoded, int32Fields);
      expect(decoded.count).toBe(42);
      expect(decoded.offset).toBe(100);
    });

    it('should encode int32 zero', () => {
      const data = { count: 0 };
      const encoded = encodeProto(data, int32Fields);
      const decoded = decodeProto(encoded, int32Fields);
      expect(decoded.count).toBe(0);
    });

    it('should handle large int32 values', () => {
      const data = { count: 2_147_483_647 }; // INT32_MAX
      const encoded = encodeProto(data, int32Fields);
      const decoded = decodeProto(encoded, int32Fields);
      expect(decoded.count).toBe(2_147_483_647);
    });
  });

  describe('sint32 / sint64 ZigZag encoding', () => {
    const sint32Fields: ProtoFieldMeta[] = [
      { name: 'value', number: 1, type: 'sint32', optional: false, repeated: false },
    ];

    it('should round-trip positive sint32', () => {
      const data = { value: 42 };
      const encoded = encodeProto(data, sint32Fields);
      const decoded = decodeProto(encoded, sint32Fields);
      expect(decoded.value).toBe(42);
    });

    it('should round-trip negative sint32', () => {
      const data = { value: -1 };
      const encoded = encodeProto(data, sint32Fields);
      const decoded = decodeProto(encoded, sint32Fields);
      expect(decoded.value).toBe(-1);
    });

    it('should round-trip large negative sint32', () => {
      const data = { value: -1000 };
      const encoded = encodeProto(data, sint32Fields);
      const decoded = decodeProto(encoded, sint32Fields);
      expect(decoded.value).toBe(-1000);
    });

    it('should round-trip zero', () => {
      const data = { value: 0 };
      const encoded = encodeProto(data, sint32Fields);
      const decoded = decodeProto(encoded, sint32Fields);
      expect(decoded.value).toBe(0);
    });

    it('should use ZigZag encoding (negative values are small varints)', () => {
      // ZigZag: -1 encodes to 1, -2 encodes to 3, 1 encodes to 2, 2 encodes to 4
      // So -1 should produce a 1-byte varint payload (value 1), very compact
      const dataNeg = { value: -1 };
      const dataPos = { value: 1 };
      const encodedNeg = encodeProto(dataNeg, sint32Fields);
      const encodedPos = encodeProto(dataPos, sint32Fields);
      // Both should be small (2 bytes: tag + varint value)
      expect(encodedNeg.length).toBeLessThanOrEqual(3);
      expect(encodedPos.length).toBeLessThanOrEqual(3);
    });

    it('should round-trip sint64', () => {
      const sint64Fields: ProtoFieldMeta[] = [
        { name: 'value', number: 1, type: 'sint64', optional: false, repeated: false },
      ];
      const data = { value: -999 };
      const encoded = encodeProto(data, sint64Fields);
      const decoded = decodeProto(encoded, sint64Fields);
      expect(decoded.value).toBe(-999);
    });
  });

  describe('64-bit integer encoding', () => {
    it('should round-trip int64 with large positive value', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'big', number: 1, type: 'int64', optional: false, repeated: false }];
      const data = { big: 4_294_967_296 }; // 2^32 — exceeds 32-bit range
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.big).toBe(4_294_967_296);
    });

    it('should round-trip uint64 with large value', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'val', number: 1, type: 'uint64', optional: false, repeated: false }];
      const data = { val: 9_007_199_254_740_991 }; // Number.MAX_SAFE_INTEGER
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.val).toBe(9_007_199_254_740_991);
    });

    it('should round-trip sint64 with large negative value', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'val', number: 1, type: 'sint64', optional: false, repeated: false }];
      const data = { val: -4_294_967_296 }; // -2^32
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.val).toBe(-4_294_967_296);
    });

    it('should round-trip int64 with BigInt input', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'val', number: 1, type: 'int64', optional: false, repeated: false }];
      const data = { val: BigInt('1099511627776') }; // 2^40
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.val).toBe(1_099_511_627_776);
    });

    it('should return BigInt for values exceeding safe integer range', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'val', number: 1, type: 'uint64', optional: false, repeated: false }];
      const bigValue = BigInt('18446744073709551615'); // UINT64_MAX
      const data = { val: bigValue };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(typeof decoded.val).toBe('bigint');
      expect(decoded.val).toBe(bigValue);
    });

    it('should skip unknown 64-bit varint fields without corrupting subsequent fields', () => {
      // Encode with int64 field that produces >5 byte varint
      const fullFields: ProtoFieldMeta[] = [
        { name: 'name', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'big_id', number: 2, type: 'int64', optional: false, repeated: false },
        { name: 'label', number: 3, type: 'string', optional: false, repeated: false },
      ];
      const encoded = encodeProto({ name: 'test', big_id: 4_294_967_296, label: 'ok' }, fullFields);

      // Decode with only fields 1 and 3 — field 2 (64-bit varint) should be skipped
      const partialFields: ProtoFieldMeta[] = [
        { name: 'name', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'label', number: 3, type: 'string', optional: false, repeated: false },
      ];
      const decoded = decodeProto(encoded, partialFields);
      expect(decoded.name).toBe('test');
      expect(decoded.label).toBe('ok');
    });
  });

  describe('fixed64 / sfixed64 encoding', () => {
    it('should round-trip fixed64 with value > 2^32', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'val', number: 1, type: 'fixed64', optional: false, repeated: false }];
      const data = { val: 4_294_967_296 }; // 2^32
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.val).toBe(4_294_967_296);
    });

    it('should round-trip sfixed64 with large negative value', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'val', number: 1, type: 'sfixed64', optional: false, repeated: false }];
      const data = { val: -4_294_967_296 };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.val).toBe(-4_294_967_296);
    });

    it('should encode fixed64 as exactly 8 bytes (not float64)', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'val', number: 1, type: 'fixed64', optional: false, repeated: false }];
      // Encode the integer 1 — as a proper uint64 LE, byte 0 = 0x01, rest 0x00
      // As a float64 LE, byte representation would be completely different
      const encoded = encodeProto({ val: 1 }, fields);
      // tag (1 byte) + 8 bytes = 9 bytes
      expect(encoded.length).toBe(9);
      // Byte after tag should be 0x01 (LE uint64 = 1)
      expect(encoded[1]).toBe(0x01);
      expect(encoded[2]).toBe(0x00);
    });

    it('should round-trip packed repeated int64', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'ids', number: 1, type: 'int64', optional: false, repeated: true }];
      const data = { ids: [1, 4_294_967_296, 9_007_199_254_740_991] };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.ids).toEqual([1, 4_294_967_296, 9_007_199_254_740_991]);
    });
  });

  describe('fixed32 / sfixed32 encoding', () => {
    it('should round-trip fixed32 (unsigned)', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'val', number: 1, type: 'fixed32', optional: false, repeated: false }];
      const data = { val: 12_345 };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.val).toBe(12_345);
    });

    it('should round-trip fixed32 max value', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'val', number: 1, type: 'fixed32', optional: false, repeated: false }];
      const data = { val: 4_294_967_295 }; // UINT32_MAX
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.val).toBe(4_294_967_295);
    });

    it('should round-trip sfixed32 (signed negative)', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'val', number: 1, type: 'sfixed32', optional: false, repeated: false }];
      const data = { val: -42 };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.val).toBe(-42);
    });

    it('should round-trip sfixed32 INT32_MIN', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'val', number: 1, type: 'sfixed32', optional: false, repeated: false }];
      const data = { val: -2_147_483_648 }; // INT32_MIN
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.val).toBe(-2_147_483_648);
    });
  });

  describe('float encoding / decoding', () => {
    const floatFields: ProtoFieldMeta[] = [
      { name: 'value', number: 1, type: 'float', optional: false, repeated: false },
    ];

    it('should round-trip float fields as 32-bit', () => {
      const data = { value: 3.14 };
      const encoded = encodeProto(data, floatFields);
      const decoded = decodeProto(encoded, floatFields);
      expect(decoded.value).toBeCloseTo(3.14, 5);
    });

    it('should be exactly 4 bytes for the value (not 8 like double)', () => {
      const data = { value: 1.0 };
      const encoded = encodeProto(data, floatFields);
      // tag (1 byte) + 4 bytes float = 5 bytes
      expect(encoded.length).toBe(5);
    });
  });

  describe('bytes encoding / decoding', () => {
    it('should round-trip bytes field', () => {
      const fields: ProtoFieldMeta[] = [
        { name: 'payload', number: 1, type: 'bytes', optional: false, repeated: false },
      ];
      const original = new Uint8Array([0xde, 0xad, 0xbe, 0xef]);
      const encoded = encodeProto({ payload: original }, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.payload).toEqual(new Uint8Array([0xde, 0xad, 0xbe, 0xef]));
    });
  });

  describe('enum encoding / decoding', () => {
    const enumTypes = new Set(['MyEnum']);
    const enumFields: ProtoFieldMeta[] = [
      { name: 'status', number: 1, type: 'MyEnum', optional: false, repeated: false },
    ];

    it('should round-trip enum fields as varint', () => {
      const data = { status: 2 };
      const encoded = encodeProto(data, enumFields, undefined, enumTypes);
      const decoded = decodeProto(encoded, enumFields);
      expect(decoded.status).toBe(2);
    });

    it('omits an enum holding its zero value and reads it back as that zero', () => {
      // Proto3 implicit presence: the `_UNSPECIFIED` member is the default and
      // is never written. A descriptor that declares no member names keeps the
      // numeric zero; one that declares them reads it back as absent, because
      // the declared union has no name for it.
      const data = { status: 0 };
      const encoded = encodeProto(data, enumFields, undefined, enumTypes);
      expect(encoded.length).toBe(0);
      const decoded = decodeProto(encoded, enumFields, undefined, enumTypes);
      expect(decoded.status).toBe(0);
    });
  });

  describe('packed repeated encoding', () => {
    it('should round-trip packed repeated int32', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'ids', number: 1, type: 'int32', optional: false, repeated: true }];
      const data = { ids: [1, 2, 3, 100, 200] };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.ids).toEqual([1, 2, 3, 100, 200]);
    });

    it('should produce smaller output than non-packed for many values', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'ids', number: 1, type: 'int32', optional: false, repeated: true }];
      const ids = Array.from({ length: 20 }, (_, i) => i);
      const encoded = encodeProto({ ids }, fields);
      // Packed: 1 tag + 1 length + 20 varints ≈ 22 bytes
      // Non-packed would be: 20 * (1 tag + 1 varint) = 40 bytes
      expect(encoded.length).toBeLessThan(30);
    });

    it('should round-trip packed repeated float', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'scores', number: 1, type: 'float', optional: false, repeated: true }];
      const data = { scores: [1.5, 2.5, 3.5] };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.scores).toHaveLength(3);
      const scores = decoded.scores as number[];
      expect(scores[0]).toBeCloseTo(1.5, 5);
      expect(scores[1]).toBeCloseTo(2.5, 5);
      expect(scores[2]).toBeCloseTo(3.5, 5);
    });

    it('should round-trip packed repeated double', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'values', number: 1, type: 'double', optional: false, repeated: true }];
      const data = { values: [1.1, 2.2, 3.3] };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.values).toEqual([1.1, 2.2, 3.3]);
    });

    it('should round-trip packed repeated bool', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'flags', number: 1, type: 'bool', optional: false, repeated: true }];
      const data = { flags: [true, false, true, true] };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.flags).toEqual([true, false, true, true]);
    });

    it('should round-trip packed repeated sint32 with negatives', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'deltas', number: 1, type: 'sint32', optional: false, repeated: true }];
      const data = { deltas: [-5, 0, 5, -100, 100] };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.deltas).toEqual([-5, 0, 5, -100, 100]);
    });

    it('should NOT pack repeated strings (still use individual tags)', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'names', number: 1, type: 'string', optional: false, repeated: true }];
      const data = { names: ['a', 'b'] };
      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.names).toEqual(['a', 'b']);
    });

    it('should handle empty packed array', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'ids', number: 1, type: 'int32', optional: false, repeated: true }];
      const data = { ids: [] };
      const encoded = encodeProto(data, fields);
      // Empty array should produce zero bytes
      expect(encoded.length).toBe(0);
    });
  });

  describe('envelope framing', () => {
    it('should create a valid envelope with data flag', () => {
      const payload = new TextEncoder().encode('{"hello":"world"}');
      const envelope = createEnvelope(0x00, payload);

      expect(envelope[0]).toBe(0x00); // flags
      const length = new DataView(envelope.buffer, 1, 4).getUint32(0, false);
      expect(length).toBe(payload.length);
      expect(new TextDecoder().decode(envelope.subarray(5))).toBe('{"hello":"world"}');
    });

    it('should create a trailer envelope', () => {
      const payload = new TextEncoder().encode('{}');
      const envelope = createEnvelope(0x02, payload);
      expect(envelope[0]).toBe(0x02);
    });

    it('should round-trip with readEnvelope', () => {
      const payload = new TextEncoder().encode('test data');
      const envelope = createEnvelope(0x00, payload);
      const result = readEnvelope(envelope);

      expect(result).toBeDefined();
      expect(result?.flags).toBe(0x00);
      expect(new TextDecoder().decode(result?.payload)).toBe('test data');
      expect(result?.consumed).toBe(envelope.length);
    });

    it('should return undefined for incomplete envelope', () => {
      const result = readEnvelope(new Uint8Array([0x00, 0x00]));
      expect(result).toBeUndefined();
    });
  });

  describe('map field encoding/decoding', () => {
    it('should round-trip map<string, string>', () => {
      const fields: ProtoFieldMeta[] = [
        {
          name: 'labels',
          number: 1,
          type: 'map',
          optional: false,
          repeated: false,
          mapKeyType: 'string',
          mapValueType: 'string',
        },
      ];
      const data = { labels: { env: 'prod', region: 'us-east' } };

      const encoded = encodeProto(data, fields);
      expect(encoded.length).toBeGreaterThan(0);

      const decoded = decodeProto(encoded, fields);
      expect(decoded.labels).toEqual({ env: 'prod', region: 'us-east' });
    });

    it('does not let a __proto__ wire key poison the decoded map prototype', () => {
      const fields: ProtoFieldMeta[] = [
        {
          name: 'labels',
          number: 1,
          type: 'map',
          optional: false,
          repeated: false,
          mapKeyType: 'string',
          mapValueType: 'string',
        },
      ];
      // Build an input map with an *own* "__proto__" key (a literal would set the
      // prototype instead). The decoder must screen it / use a null prototype.
      const labels: Record<string, string> = { env: 'prod' };
      Object.defineProperty(labels, '__proto__', {
        value: 'evil',
        enumerable: true,
        writable: true,
        configurable: true,
      });

      const encoded = encodeProto({ labels }, fields);
      const decoded = decodeProto(encoded, fields) as { labels: Record<string, string> };

      expect(decoded.labels.env).toBe('prod');
      // Prototype must not be reassigned by the attacker-controlled key.
      expect(Object.getPrototypeOf(decoded.labels)).toBeNull();
      expect(Object.hasOwn(decoded.labels, '__proto__')).toBe(false);
    });

    it('should round-trip map<string, double>', () => {
      const fields: ProtoFieldMeta[] = [
        {
          name: 'prices',
          number: 1,
          type: 'map',
          optional: false,
          repeated: false,
          mapKeyType: 'string',
          mapValueType: 'double',
        },
      ];
      const data = { prices: { apple: 1.5, banana: 0.75 } };

      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.prices).toEqual({ apple: 1.5, banana: 0.75 });
    });

    it('should round-trip map<string, int32>', () => {
      const fields: ProtoFieldMeta[] = [
        {
          name: 'counts',
          number: 1,
          type: 'map',
          optional: false,
          repeated: false,
          mapKeyType: 'string',
          mapValueType: 'int32',
        },
      ];
      const data = { counts: { a: 10, b: 20, c: 0 } };

      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.counts).toEqual({ a: 10, b: 20, c: 0 });
    });

    it('should round-trip map<string, bool>', () => {
      const fields: ProtoFieldMeta[] = [
        {
          name: 'flags',
          number: 1,
          type: 'map',
          optional: false,
          repeated: false,
          mapKeyType: 'string',
          mapValueType: 'bool',
        },
      ];
      const data = { flags: { enabled: true, debug: false } };

      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.flags).toEqual({ enabled: true, debug: false });
    });

    it('should handle empty map', () => {
      const fields: ProtoFieldMeta[] = [
        {
          name: 'labels',
          number: 1,
          type: 'map',
          optional: false,
          repeated: false,
          mapKeyType: 'string',
          mapValueType: 'string',
        },
      ];
      const data = { labels: {} };

      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      // Empty map produces no bytes — decoding gives undefined for the field
      expect(decoded.labels ?? {}).toEqual({});
    });

    it('should work alongside other fields', () => {
      const fields: ProtoFieldMeta[] = [
        { name: 'name', number: 1, type: 'string', optional: false, repeated: false },
        {
          name: 'labels',
          number: 2,
          type: 'map',
          optional: false,
          repeated: false,
          mapKeyType: 'string',
          mapValueType: 'string',
        },
        { name: 'active', number: 3, type: 'bool', optional: false, repeated: false },
      ];
      const data = { name: 'item-1', labels: { color: 'red' }, active: true };

      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.name).toBe('item-1');
      expect(decoded.labels).toEqual({ color: 'red' });
      expect(decoded.active).toBe(true);
    });

    it('should round-trip map<int32, string>', () => {
      const fields: ProtoFieldMeta[] = [
        {
          name: 'lookup',
          number: 1,
          type: 'map',
          optional: false,
          repeated: false,
          mapKeyType: 'int32',
          mapValueType: 'string',
        },
      ];
      const data = { lookup: { 1: 'one', 2: 'two', 100: 'hundred' } };

      const encoded = encodeProto(data, fields);
      const decoded = decodeProto(encoded, fields);
      expect(decoded.lookup).toEqual({ 1: 'one', 2: 'two', 100: 'hundred' });
    });
  });

  describe('truncated / malformed input', () => {
    it('does not throw RangeError on a truncated 64-bit (double) field', () => {
      const fields: ProtoFieldMeta[] = [{ name: 'age', number: 2, type: 'double', optional: false, repeated: false }];
      // tag = (field 2 << 3) | wire 64BIT(1) = 0x11, then only 3 of 8 needed bytes.
      const truncated = new Uint8Array([0x11, 0x00, 0x00, 0x00]);
      expect(() => decodeProto(truncated, fields)).not.toThrow();
      // The field never arrived, so implicit presence reads it back as 0.
      expect(decodeProto(truncated, fields).age).toBe(0);
    });

    it('does not throw RangeError on a truncated 32-bit (fixed32) field', () => {
      const fields: ProtoFieldMeta[] = [
        { name: 'count', number: 1, type: 'fixed32', optional: false, repeated: false },
      ];
      // tag = (field 1 << 3) | wire 32BIT(5) = 0x0d, then only 2 of 4 needed bytes.
      const truncated = new Uint8Array([0x0d, 0x00, 0x00]);
      expect(() => decodeProto(truncated, fields)).not.toThrow();
    });
  });

  describe('unknown field skipping', () => {
    it('should skip unknown varint fields and decode remaining fields', () => {
      // Encode a message with fields 1 (name) and 2 (age)
      const fullFields: ProtoFieldMeta[] = [
        { name: 'name', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'age', number: 2, type: 'int32', optional: false, repeated: false },
      ];
      const encoded = encodeProto({ name: 'Alice', age: 30 }, fullFields);

      // Decode with only field 1 — field 2 (varint) should be skipped
      const partialFields: ProtoFieldMeta[] = [
        { name: 'name', number: 1, type: 'string', optional: false, repeated: false },
      ];
      const decoded = decodeProto(encoded, partialFields);
      expect(decoded.name).toBe('Alice');
      expect(decoded.age).toBeUndefined();
    });

    it('should skip unknown length-delimited fields and decode remaining fields', () => {
      // Encode: field 1 (string), field 2 (string), field 3 (int32)
      const fullFields: ProtoFieldMeta[] = [
        { name: 'id', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'extra', number: 2, type: 'string', optional: false, repeated: false },
        { name: 'count', number: 3, type: 'int32', optional: false, repeated: false },
      ];
      const encoded = encodeProto({ id: 'abc', extra: 'unknown-data', count: 42 }, fullFields);

      // Decode with only fields 1 and 3 — field 2 should be skipped
      const partialFields: ProtoFieldMeta[] = [
        { name: 'id', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'count', number: 3, type: 'int32', optional: false, repeated: false },
      ];
      const decoded = decodeProto(encoded, partialFields);
      expect(decoded.id).toBe('abc');
      expect(decoded.count).toBe(42);
      expect(decoded.extra).toBeUndefined();
    });

    it('should skip unknown 64-bit fields and decode remaining fields', () => {
      const fullFields: ProtoFieldMeta[] = [
        { name: 'name', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'value', number: 2, type: 'double', optional: false, repeated: false },
        { name: 'active', number: 3, type: 'bool', optional: false, repeated: false },
      ];
      const encoded = encodeProto({ name: 'test', value: 3.14, active: true }, fullFields);

      // Decode with only fields 1 and 3 — field 2 (64-bit double) should be skipped
      const partialFields: ProtoFieldMeta[] = [
        { name: 'name', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'active', number: 3, type: 'bool', optional: false, repeated: false },
      ];
      const decoded = decodeProto(encoded, partialFields);
      expect(decoded.name).toBe('test');
      expect(decoded.active).toBe(true);
      expect(decoded.value).toBeUndefined();
    });

    it('should skip unknown 32-bit fields and decode remaining fields', () => {
      const fullFields: ProtoFieldMeta[] = [
        { name: 'name', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'score', number: 2, type: 'float', optional: false, repeated: false },
        { name: 'label', number: 3, type: 'string', optional: false, repeated: false },
      ];
      const encoded = encodeProto({ name: 'test', score: 1.5, label: 'ok' }, fullFields);

      // Decode with only fields 1 and 3 — field 2 (32-bit float) should be skipped
      const partialFields: ProtoFieldMeta[] = [
        { name: 'name', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'label', number: 3, type: 'string', optional: false, repeated: false },
      ];
      const decoded = decodeProto(encoded, partialFields);
      expect(decoded.name).toBe('test');
      expect(decoded.label).toBe('ok');
      expect(decoded.score).toBeUndefined();
    });

    it('should skip multiple unknown fields of mixed types', () => {
      const fullFields: ProtoFieldMeta[] = [
        { name: 'id', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'unknown_str', number: 2, type: 'string', optional: false, repeated: false },
        { name: 'unknown_int', number: 3, type: 'int32', optional: false, repeated: false },
        { name: 'unknown_dbl', number: 4, type: 'double', optional: false, repeated: false },
        { name: 'result', number: 5, type: 'string', optional: false, repeated: false },
      ];
      const encoded = encodeProto(
        { id: 'start', unknown_str: 'skip-me', unknown_int: 999, unknown_dbl: Math.E, result: 'end' },
        fullFields,
      );

      // Decode with only fields 1 and 5 — all middle fields skipped
      const partialFields: ProtoFieldMeta[] = [
        { name: 'id', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'result', number: 5, type: 'string', optional: false, repeated: false },
      ];
      const decoded = decodeProto(encoded, partialFields);
      expect(decoded.id).toBe('start');
      expect(decoded.result).toBe('end');
    });

    it('should handle empty schema (all fields unknown)', () => {
      const fullFields: ProtoFieldMeta[] = [
        { name: 'a', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'b', number: 2, type: 'int32', optional: false, repeated: false },
      ];
      const encoded = encodeProto({ a: 'hello', b: 42 }, fullFields);

      const decoded = decodeProto(encoded, []);
      expect(Object.keys(decoded)).toHaveLength(0);
    });
  });
});
