import { describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  nextWebSocketStateV1,
  parseWebSocketServiceFrame,
  parseWebSocketServiceFrameV1,
  reservedWebSocketInitHeader,
  SERVICE_WEBSOCKET_SUBPROTOCOL,
  WEBSOCKET_CANCEL_CODES_V1,
  WEBSOCKET_FRAME_TYPES_V1,
  WEBSOCKET_PAYLOAD_ENCODINGS_V1,
  WEBSOCKET_STATES_V1,
  WebSocketConversationV1,
  type WebSocketDirection,
  WebSocketProtocolError,
  type WebSocketServiceFrame,
  type WebSocketState,
  type WebSocketStreamMode,
} from '../../../src/api/stream/websocket-protocol';

const CONTRACT_ROOT = join(import.meta.dir, '../../../../../../protocols/clientcontract');
const CORPUS = join(CONTRACT_ROOT, 'fixtures/websocket');
const WIRE_SCHEMA = join(CONTRACT_ROOT, 'schemas/client-websocket-wire-v1.json');
const LARGE = 1024 * 1024;

interface CorpusScene {
  readonly operationId: string;
  readonly stream: WebSocketStreamMode;
  readonly encoding: 'json' | 'proto';
  readonly resume: boolean;
  readonly maxFrameBytes: number;
  readonly steps: readonly { readonly direction: WebSocketDirection; readonly frame: unknown }[];
}

function scene(kind: 'valid' | 'invalid', name: string): CorpusScene {
  return JSON.parse(readFileSync(join(CORPUS, kind, name), 'utf8')) as CorpusScene;
}

function expectations(): { valid: string[]; invalid: Record<string, string> } {
  return JSON.parse(readFileSync(join(CORPUS, 'expectations.json'), 'utf8')) as {
    valid: string[];
    invalid: Record<string, string>;
  };
}

/** Replay one corpus scene through a conversation and report the first refusal. */
function replay(source: CorpusScene): { refusedAt: number; code?: string } {
  const conversation = new WebSocketConversationV1({
    stream: source.stream,
    encoding: source.encoding,
    resumeDeclared: source.resume,
  });
  for (const [index, step] of source.steps.entries()) {
    const parsed = parseWebSocketServiceFrameV1(JSON.stringify(step.frame), source.maxFrameBytes);
    if (!parsed.frame) return { refusedAt: index, code: parsed.diagnostics[0]?.code };
    const refusal = conversation.accept(parsed.frame, step.direction);
    if (refusal.length > 0) return { refusedAt: index, code: refusal[0]?.code };
  }
  return { refusedAt: -1 };
}

describe('published websocket wire, TypeScript half', () => {
  specTest(
    'accepts every valid scene of the shared corpus',
    {
      feature: 'typescript/api-contracts',
      requirement: 'websocket-wire-conformance',
      check: 'the-corpus-valid-scenes-are-accepted',
    },
    () => {
      const { valid } = expectations();
      expect(valid.length).toBeGreaterThan(0);
      for (const name of valid) {
        expect({ name, ...replay(scene('valid', name)) }).toEqual({ name, refusedAt: -1 });
      }
    },
  );

  specTest(
    'refuses every invalid scene with exactly the diagnostic code the corpus declares',
    {
      feature: 'typescript/api-contracts',
      requirement: 'websocket-wire-conformance',
      check: 'the-corpus-invalid-scenes-are-refused-with-the-declared-code',
    },
    () => {
      const { invalid } = expectations();
      for (const [name, code] of Object.entries(invalid)) {
        const outcome = replay(scene('invalid', name));
        expect({ name, code: outcome.code }).toEqual({ name, code });
        expect(outcome.refusedAt).toBeGreaterThanOrEqual(0);
      }
    },
  );

  test('covers the corpus exhaustively — every fixture on disk is an expectation', () => {
    const declared = expectations();
    expect(readdirSync(join(CORPUS, 'valid')).sort()).toEqual([...declared.valid].sort());
    expect(readdirSync(join(CORPUS, 'invalid')).sort()).toEqual(Object.keys(declared.invalid).sort());
  });

  test('a refused init frame never quotes the frame that carried the credentials', () => {
    const source = scene('invalid', 'secret-shaped-unknown-field.json');
    const raw = JSON.stringify(source.steps[0]?.frame);
    expect(raw).toContain('must-not-appear-in-diagnostics');
    const parsed = parseWebSocketServiceFrameV1(raw, source.maxFrameBytes);
    expect(parsed.frame).toBeUndefined();
    expect(JSON.stringify(parsed.diagnostics)).not.toContain('must-not-appear-in-diagnostics');
  });
});

describe('closed vocabulary drift against the published schema', () => {
  const schema = JSON.parse(readFileSync(WIRE_SCHEMA, 'utf8')) as Record<string, unknown>;
  const defs = schema['$defs'] as Record<string, Record<string, unknown>>;

  specTest(
    'the frame types this parser accepts are exactly the ones the schema enumerates',
    {
      feature: 'typescript/api-contracts',
      requirement: 'websocket-wire-conformance',
      check: 'the-frame-vocabulary-matches-the-published-schema',
    },
    () => {
      const published = new Set<string>();
      for (const branch of schema['oneOf'] as { $ref: string }[]) {
        const name = branch.$ref.replace('#/$defs/', '');
        published.add(frameTypeConst(defs, name));
      }
      expect([...published].sort()).toEqual([...WEBSOCKET_FRAME_TYPES_V1].sort());
    },
  );

  test('the cancel codes are the framework single-l spelling on both sides', () => {
    const cancel = defs['cancel'] as { properties: { code: { enum: string[] } } };
    expect(cancel.properties.code.enum.sort()).toEqual([...WEBSOCKET_CANCEL_CODES_V1].sort());
    expect(WEBSOCKET_CANCEL_CODES_V1).toContain('canceled');
    expect(WEBSOCKET_CANCEL_CODES_V1 as readonly string[]).not.toContain('cancelled');
  });

  test('the payload encodings are the ones the schema declares, proto included', () => {
    const encodings = ['jsonPayload', 'protoPayload'].map(
      (name) => (defs[name] as { properties: { encoding: { const: string } } }).properties.encoding.const,
    );
    expect(encodings.sort()).toEqual([...WEBSOCKET_PAYLOAD_ENCODINGS_V1].sort());
  });

  test('the reserved init header list refuses every credential and propagation channel', () => {
    for (const name of [
      'authorization',
      'cookie',
      'set-cookie',
      'traceparent',
      'tracestate',
      'baggage',
      'x-client-id',
      'x-request-id',
      'sec-websocket-key',
      'proxy-authorization',
    ]) {
      expect({ name, reserved: reservedWebSocketInitHeader(name) }).toEqual({ name, reserved: true });
    }
    expect(reservedWebSocketInitHeader('x-correlation-key')).toBe(false);
  });

  test('the subprotocol is the published token and carries no credential material', () => {
    expect(SERVICE_WEBSOCKET_SUBPROTOCOL).toBe('putnami.service.v1');
  });
});

function frameTypeConst(defs: Record<string, Record<string, unknown>>, name: string): string {
  const def = defs[name] as Record<string, unknown>;
  const direct = (def['properties'] as { type?: { const?: string } } | undefined)?.type?.const;
  if (direct) return direct;
  for (const branch of (def['allOf'] ?? []) as { properties?: { type?: { const?: string } } }[]) {
    if (branch.properties?.type?.const) return branch.properties.type.const;
  }
  throw new Error(`schema $defs.${name} declares no frame type`);
}

describe('strict frame parsing', () => {
  const init = {
    v: 1,
    type: 'init',
    operationId: 'getWidgetsWatch',
    clientId: 'consumer',
    deadlineUnixMs: '0',
    budgetMs: '0',
    credentials: [],
    headers: [],
  };

  test('an unknown member is a parse error, not an unknown-field diagnostic', () => {
    const parsed = parseWebSocketServiceFrameV1(JSON.stringify({ ...init, extra: 1 }), LARGE);
    expect(parsed.diagnostics[0]?.code).toBe('client_contract.parse_error');
  });

  specTest(
    'a duplicate object key is refused even though JSON.parse keeps the last one',
    {
      feature: 'typescript/api-contracts',
      requirement: 'websocket-wire-conformance',
      check: 'a-duplicate-key-or-a-json-null-is-refused-like-the-go-parser-refuses-it',
    },
    () => {
      const raw = '{"v":1,"type":"ping","nonce":"a","nonce":"b"}';
      expect(JSON.parse(raw).nonce).toBe('b');
      expect(parseWebSocketServiceFrameV1(raw, LARGE).diagnostics[0]?.code).toBe('client_contract.parse_error');
    },
  );

  test('a JSON null inside an application payload is refused, exactly as the Go parser refuses it', () => {
    const frame = { v: 1, type: 'message', sequence: '1', payload: { encoding: 'json', value: { id: null } } };
    expect(parseWebSocketServiceFrameV1(JSON.stringify(frame), LARGE).diagnostics[0]?.code).toBe(
      'client_contract.parse_error',
    );
  });

  test('an empty proto payload is a valid empty value, not an absent one', () => {
    const frame = { v: 1, type: 'message', sequence: '1', payload: { encoding: 'proto', base64: '' } };
    const parsed = parseWebSocketServiceFrameV1(JSON.stringify(frame), LARGE);
    expect(parsed.frame).toBeDefined();
    expect((parsed.frame as { payload: { base64: string } }).payload.base64).toBe('');
  });

  test('a sequence past uint64 is refused, and the last representable one is accepted', () => {
    const at = (sequence: string) =>
      parseWebSocketServiceFrameV1(
        JSON.stringify({ v: 1, type: 'message', sequence, payload: { encoding: 'json', value: 1 } }),
        LARGE,
      );
    expect(at('18446744073709551615').frame).toBeDefined();
    expect(at('18446744073709551616').diagnostics[0]?.code).toBe('client_contract.invalid_transport');
    expect(at('01').diagnostics[0]?.code).toBe('client_contract.invalid_transport');
    expect(at('0').diagnostics[0]?.code).toBe('client_contract.invalid_transport');
  });

  test('a frame past the declared bound is refused before its bytes are interpreted', () => {
    const parsed = parseWebSocketServiceFrameV1(JSON.stringify(init), 8);
    expect(parsed.diagnostics[0]).toMatchObject({ code: 'client_contract.invalid_transport', field: 'frame' });
  });

  test('an unsupported frame type names the type member', () => {
    const parsed = parseWebSocketServiceFrameV1(JSON.stringify({ v: 1, type: 'resume' }), LARGE);
    expect(parsed.diagnostics[0]).toMatchObject({ code: 'client_contract.invalid_transport', field: 'type' });
  });

  test('an unsupported protocol version is refused by its own code', () => {
    const parsed = parseWebSocketServiceFrameV1(JSON.stringify({ ...init, v: 2 }), LARGE);
    expect(parsed.diagnostics[0]?.code).toBe('client_contract.invalid_protocol_version');
  });

  test('ready with resumed and no token is a missing required member', () => {
    const parsed = parseWebSocketServiceFrameV1(JSON.stringify({ v: 1, type: 'ready', resumed: true }), LARGE);
    expect(parsed.diagnostics[0]).toMatchObject({ code: 'client_contract.required', field: 'resumeToken' });
  });

  test('a reserved header in init is refused, and a duplicate ordinary header too', () => {
    const reserved = parseWebSocketServiceFrameV1(
      JSON.stringify({ ...init, headers: [{ name: 'Authorization', values: ['Bearer x'] }] }),
      LARGE,
    );
    expect(reserved.diagnostics[0]?.code).toBe('client_contract.invalid_transport');
    const duplicate = parseWebSocketServiceFrameV1(
      JSON.stringify({
        ...init,
        headers: [
          { name: 'X-Trace', values: ['a'] },
          { name: 'x-trace', values: ['b'] },
        ],
      }),
      LARGE,
    );
    expect(duplicate.diagnostics[0]?.code).toBe('client_contract.duplicate');
  });

  test('a duplicate credential profile is refused', () => {
    const parsed = parseWebSocketServiceFrameV1(
      JSON.stringify({
        ...init,
        credentials: [
          { profile: 'service', value: 'a' },
          { profile: 'service', value: 'b' },
        ],
      }),
      LARGE,
    );
    expect(parsed.diagnostics[0]?.code).toBe('client_contract.duplicate');
  });

  test('the throwing form carries the contract code so a local fault is not renamed', () => {
    try {
      parseWebSocketServiceFrame('{', LARGE);
      throw new Error('expected a refusal');
    } catch (error) {
      expect(error).toBeInstanceOf(WebSocketProtocolError);
      expect((error as WebSocketProtocolError).code).toBe('client_contract.parse_error');
      expect((error as WebSocketProtocolError).diagnostic.code).toBe('client_contract.parse_error');
    }
  });
});

describe('published transition table', () => {
  const frame = (value: unknown): WebSocketServiceFrame => value as WebSocketServiceFrame;
  const cancel = frame({ v: 1, type: 'cancel', code: 'canceled' });
  const ping = frame({ v: 1, type: 'ping', nonce: 'n' });
  const errorFrame = frame({ v: 1, type: 'error', error: { status: 503, code: 'http.service_unavailable' } });

  const cases: {
    state: WebSocketState;
    frame: WebSocketServiceFrame;
    direction: WebSocketDirection;
    stream: WebSocketStreamMode;
    next?: WebSocketState;
  }[] = [
    { state: 'await-init', frame: cancel, direction: 'client-to-server', stream: 'server' },
    { state: 'await-ready', frame: cancel, direction: 'client-to-server', stream: 'server', next: 'terminal' },
    { state: 'open', frame: cancel, direction: 'client-to-server', stream: 'server', next: 'terminal' },
    { state: 'await-init', frame: errorFrame, direction: 'server-to-client', stream: 'server', next: 'terminal' },
    { state: 'await-init', frame: ping, direction: 'client-to-server', stream: 'server' },
    { state: 'await-ready', frame: ping, direction: 'client-to-server', stream: 'server', next: 'await-ready' },
    { state: 'await-ready', frame: ping, direction: 'server-to-client', stream: 'server', next: 'await-ready' },
    {
      state: 'open',
      frame: frame({ v: 1, type: 'half-close' }),
      direction: 'server-to-client',
      stream: 'bidirectional',
    },
    {
      state: 'open',
      frame: frame({ v: 1, type: 'result', payload: { encoding: 'json', value: 1 } }),
      direction: 'server-to-client',
      stream: 'server',
    },
    {
      state: 'open',
      frame: frame({ v: 1, type: 'result' }),
      direction: 'server-to-client',
      stream: 'server',
      next: 'terminal',
    },
  ];

  test.each(cases)('$state + $frame.type from $direction on a $stream stream', (item) => {
    const transition = nextWebSocketStateV1(item.state, item.frame, item.direction, item.stream);
    if (item.next === undefined) {
      expect(transition.state).toBe(item.state);
      expect(transition.diagnostics[0]?.code).toBe('client_contract.invalid_transport');
      return;
    }
    expect(transition.state).toBe(item.next);
    expect(transition.diagnostics).toEqual([]);
  });

  test('the state vocabulary is closed and ordered', () => {
    expect(WEBSOCKET_STATES_V1).toEqual(['await-init', 'await-ready', 'open', 'half-closed', 'terminal']);
  });

  test('a bidirectional stream keeps the provider direction live after the client half-closes', () => {
    const conversation = new WebSocketConversationV1({
      stream: 'bidirectional',
      encoding: 'json',
      resumeDeclared: false,
    });
    expect(conversation.accept(frame(initFrame()), 'client-to-server')).toEqual([]);
    expect(conversation.accept(frame({ v: 1, type: 'ready', resumed: false }), 'server-to-client')).toEqual([]);
    expect(conversation.accept(frame({ v: 1, type: 'half-close' }), 'client-to-server')).toEqual([]);
    expect(conversation.state).toBe('half-closed');
    expect(
      conversation.accept(
        frame({ v: 1, type: 'message', sequence: '1', payload: { encoding: 'json', value: 1 } }),
        'server-to-client',
      ),
    ).toEqual([]);
    expect(conversation.accept(frame({ v: 1, type: 'half-close' }), 'client-to-server')[0]?.code).toBe(
      'client_contract.invalid_transport',
    );
  });

  test('per-direction sequences are independent and gap-free', () => {
    const conversation = new WebSocketConversationV1({
      stream: 'bidirectional',
      encoding: 'json',
      resumeDeclared: false,
    });
    conversation.accept(frame(initFrame()), 'client-to-server');
    conversation.accept(frame({ v: 1, type: 'ready', resumed: false }), 'server-to-client');
    const message = (sequence: string) =>
      frame({ v: 1, type: 'message', sequence, payload: { encoding: 'json', value: 1 } });
    expect(conversation.accept(message('1'), 'client-to-server')).toEqual([]);
    expect(conversation.accept(message('1'), 'server-to-client')).toEqual([]);
    expect(conversation.accept(message('3'), 'client-to-server')[0]).toMatchObject({
      code: 'client_contract.invalid_transport',
      field: 'sequence',
    });
    expect(conversation.accept(message('2'), 'client-to-server')).toEqual([]);
  });

  test('a message whose encoding differs from the selected transport is refused', () => {
    const conversation = new WebSocketConversationV1({ stream: 'client', encoding: 'json', resumeDeclared: false });
    conversation.accept(frame(initFrame()), 'client-to-server');
    conversation.accept(frame({ v: 1, type: 'ready', resumed: false }), 'server-to-client');
    expect(
      conversation.accept(
        frame({ v: 1, type: 'message', sequence: '1', payload: { encoding: 'proto', base64: '' } }),
        'client-to-server',
      )[0],
    ).toMatchObject({ code: 'client_contract.invalid_transport', field: 'payload.encoding' });
  });

  test('resume is refused before an encoding limit, because the wire owns that rule', () => {
    const conversation = new WebSocketConversationV1({ stream: 'server', encoding: 'json', resumeDeclared: false });
    const refusal = conversation.accept(
      frame({ ...initFrame(), resume: { token: 'token', afterSequence: '4' } }),
      'client-to-server',
    );
    expect(refusal[0]).toMatchObject({ code: 'client_contract.invalid_resilience', field: 'resume' });
  });

  test('ready resumed=true is accepted when the init requested it on a resume-capable transport', () => {
    const conversation = new WebSocketConversationV1({ stream: 'server', encoding: 'json', resumeDeclared: true });
    expect(
      conversation.accept(frame({ ...initFrame(), resume: { token: 't', afterSequence: '4' } }), 'client-to-server'),
    ).toEqual([]);
    expect(
      conversation.accept(frame({ v: 1, type: 'ready', resumed: true, resumeToken: 'next' }), 'server-to-client'),
    ).toEqual([]);
    expect(conversation.nextSequence('server-to-client')).toBe(5n);
  });

  test('ready resumed=true is refused when the init never requested a resume', () => {
    const conversation = new WebSocketConversationV1({ stream: 'server', encoding: 'json', resumeDeclared: true });
    conversation.accept(frame(initFrame()), 'client-to-server');
    expect(
      conversation.accept(frame({ v: 1, type: 'ready', resumed: true, resumeToken: 'next' }), 'server-to-client')[0],
    ).toMatchObject({ code: 'client_contract.invalid_resilience', field: 'resumed' });
  });
});

function initFrame(): Record<string, unknown> {
  return {
    v: 1,
    type: 'init',
    operationId: 'getWidgetsWatch',
    clientId: 'consumer',
    deadlineUnixMs: '0',
    budgetMs: '0',
    credentials: [],
    headers: [],
  };
}
