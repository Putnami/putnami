/**
 * First-party WebSocket wire, v1 — the TypeScript half of the contract that
 * `protocols/clientcontract/websocket.go` publishes.
 *
 * Three things live here, and nothing else: the closed frame vocabulary, the
 * strict parser that refuses anything outside it, and the published transition
 * table plus the session facts a single frame cannot carry. A runtime — the
 * provider dispatcher or the client transport — drives a
 * {@link WebSocketConversationV1}; it never restates a phase rule of its own.
 *
 * The diagnostic codes are the Go reader's codes. A frame the Go package
 * refuses is refused here with the same `client_contract.*` code, which is what
 * makes `protocols/clientcontract/fixtures/websocket` one corpus rather than two.
 */

/** Public subprotocol for first-party service streams. It never carries credentials. */
export const SERVICE_WEBSOCKET_SUBPROTOCOL = 'putnami.service.v1' as const;

/** Protocol version carried by every first-party frame. */
export const SERVICE_WEBSOCKET_PROTOCOL_VERSION = 1 as const;

/**
 * Contract diagnostic codes the wire can emit, in the Go package's order.
 * `protocols/clientcontract/strict.go` owns the full closed set; this is the
 * subset a frame or a conversation can produce.
 */
export const WEBSOCKET_DIAGNOSTIC_CODES_V1 = [
  'client_contract.parse_error',
  'client_contract.invalid_protocol_version',
  'client_contract.required',
  'client_contract.invalid_enum',
  'client_contract.unknown_profile',
  'client_contract.duplicate',
  'client_contract.invalid_transport',
  'client_contract.invalid_security',
  'client_contract.invalid_error',
  'client_contract.invalid_resilience',
] as const;

export type WebSocketDiagnosticCode = (typeof WEBSOCKET_DIAGNOSTIC_CODES_V1)[number];

/** One refusal: the contract code, the frame member it names, and why. */
export interface WebSocketDiagnostic {
  readonly code: WebSocketDiagnosticCode;
  readonly field: string;
  readonly message: string;
}

/** Closed frame vocabulary, in the order the published schema enumerates it. */
export const WEBSOCKET_FRAME_TYPES_V1 = [
  'init',
  'ready',
  'message',
  'half-close',
  'result',
  'error',
  'cancel',
  'ping',
  'pong',
] as const;

export type WebSocketFrameType = (typeof WEBSOCKET_FRAME_TYPES_V1)[number];

/** Closed cancel-code vocabulary, in the framework's single-l spelling. */
export const WEBSOCKET_CANCEL_CODES_V1 = ['canceled', 'deadline_exceeded'] as const;

export type WebSocketCancelCode = (typeof WEBSOCKET_CANCEL_CODES_V1)[number];

/** Closed payload encoding vocabulary. Proto stays declarable even where a runtime cannot decode it. */
export const WEBSOCKET_PAYLOAD_ENCODINGS_V1 = ['json', 'proto'] as const;

export type WebSocketPayloadEncoding = (typeof WEBSOCKET_PAYLOAD_ENCODINGS_V1)[number];

export type WebSocketEncodedPayload =
  | { readonly encoding: 'json'; readonly value: unknown }
  | { readonly encoding: 'proto'; readonly base64: string };

export interface WebSocketInitFrame {
  readonly v: 1;
  readonly type: 'init';
  readonly operationId: string;
  readonly clientId: string;
  readonly deadlineUnixMs: string;
  readonly budgetMs: string;
  readonly credentials: readonly { readonly profile: string; readonly value: string }[];
  readonly headers: readonly { readonly name: string; readonly values: readonly string[] }[];
  readonly context?: {
    readonly traceparent?: string;
    readonly tracestate?: string;
    readonly baggage?: string;
    readonly requestId?: string;
  };
  readonly resume?: { readonly token: string; readonly afterSequence: string };
  readonly request?: WebSocketEncodedPayload;
}

export interface WebSocketReadyFrame {
  readonly v: 1;
  readonly type: 'ready';
  readonly resumed: boolean;
  readonly resumeToken?: string;
}

export interface WebSocketMessageFrame {
  readonly v: 1;
  readonly type: 'message';
  readonly sequence: string;
  readonly payload: WebSocketEncodedPayload;
}

export interface WebSocketHalfCloseFrame {
  readonly v: 1;
  readonly type: 'half-close';
}

export interface WebSocketResultFrame {
  readonly v: 1;
  readonly type: 'result';
  readonly payload?: WebSocketEncodedPayload;
}

/** Terminal typed error. The envelope is D0.1: stable `code` beside the status. */
export interface WebSocketErrorFrame {
  readonly v: 1;
  readonly type: 'error';
  readonly error: {
    readonly status: number;
    readonly code: string;
    readonly grpcCode?: number;
    readonly retryable?: boolean;
    readonly message?: string;
    readonly details?: unknown;
  };
}

export interface WebSocketCancelFrame {
  readonly v: 1;
  readonly type: 'cancel';
  readonly code: WebSocketCancelCode;
}

export interface WebSocketHeartbeatFrame {
  readonly v: 1;
  readonly type: 'ping' | 'pong';
  readonly nonce: string;
}

export type WebSocketServiceFrame =
  | WebSocketInitFrame
  | WebSocketReadyFrame
  | WebSocketMessageFrame
  | WebSocketHalfCloseFrame
  | WebSocketResultFrame
  | WebSocketErrorFrame
  | WebSocketCancelFrame
  | WebSocketHeartbeatFrame;

/**
 * A wire refusal carrying the contract's own diagnostic code.
 *
 * The offending frame is intentionally absent from the message: a refused init
 * frame carries credentials, so quoting it would move a secret into a log, an
 * error string or a close reason.
 */
export class WebSocketProtocolError extends Error {
  readonly code: WebSocketDiagnosticCode;
  readonly field: string;

  constructor(diagnostic: WebSocketDiagnostic) {
    super(diagnostic.message);
    this.name = 'WebSocketProtocolError';
    this.code = diagnostic.code;
    this.field = diagnostic.field;
  }

  /** The refusal as a diagnostic, so a caller can forward it without re-deriving it. */
  get diagnostic(): WebSocketDiagnostic {
    return { code: this.code, field: this.field, message: this.message };
  }
}

/** Parse result: exactly one of `frame` or a non-empty `diagnostics`. */
export interface WebSocketParseResult {
  readonly frame?: WebSocketServiceFrame;
  readonly diagnostics: readonly WebSocketDiagnostic[];
}

/**
 * Parse one frame under the closed v1 vocabulary.
 *
 * `maxFrameBytes` bounds the reassembled message, never a TCP frame; a peer
 * that exceeds it is refused before its bytes are interpreted.
 */
export function parseWebSocketServiceFrameV1(input: string | Uint8Array, maxFrameBytes: number): WebSocketParseResult {
  let text: string;
  try {
    text = typeof input === 'string' ? input : new TextDecoder('utf-8', { fatal: true }).decode(input);
  } catch {
    return { diagnostics: [parseDiagnostic()] };
  }
  if (new TextEncoder().encode(text).byteLength > maxFrameBytes) {
    return {
      diagnostics: [
        diagnostic('client_contract.invalid_transport', 'frame', 'websocket frame exceeds the declared maximum'),
      ],
    };
  }
  let value: unknown;
  try {
    value = decodeStrictJson(text);
  } catch {
    return { diagnostics: [parseDiagnostic()] };
  }
  if (!isRecord(value)) return { diagnostics: [parseDiagnostic()] };
  const type = value['type'];
  if (typeof type !== 'string' || !containsFrameType(type)) {
    return {
      diagnostics: [diagnostic('client_contract.invalid_transport', 'type', 'websocket frame type is unsupported')],
    };
  }
  if (!hasExactFields(value, FRAME_FIELDS[type])) return { diagnostics: [parseDiagnostic()] };
  const diagnostics = validateFrame(type, value);
  if (diagnostics.length > 0) return { diagnostics };
  return { frame: value as unknown as WebSocketServiceFrame, diagnostics: [] };
}

/**
 * Parse one frame and throw the first refusal.
 *
 * The throwing form is what a runtime uses on its own outgoing frames: a frame
 * this peer composed that the wire would refuse is a local fault, and it must
 * carry the contract's diagnostic code rather than a code the runtime invented.
 */
export function parseWebSocketServiceFrame(input: string | Uint8Array, maxFrameBytes: number): WebSocketServiceFrame {
  const result = parseWebSocketServiceFrameV1(input, maxFrameBytes);
  if (result.frame === undefined) {
    throw new WebSocketProtocolError(result.diagnostics[0] ?? parseDiagnostic());
  }
  return result.frame;
}

// ---------------------------------------------------------------------------
// Published transition table
// ---------------------------------------------------------------------------

/**
 * Closed admission-and-delivery state of one conversation, as observed by
 * either peer.
 */
export type WebSocketState = 'await-init' | 'await-ready' | 'open' | 'half-closed' | 'terminal';

/** The conversation vocabulary, in transition order. */
export const WEBSOCKET_STATES_V1: readonly WebSocketState[] = [
  'await-init',
  'await-ready',
  'open',
  'half-closed',
  'terminal',
];

export type WebSocketDirection = 'client-to-server' | 'server-to-client';

/** Declared stream shape of the operation the conversation carries. */
export type WebSocketStreamMode = 'server' | 'client' | 'bidirectional';

/** One transition: the state that follows the frame, and why it was refused. */
export interface WebSocketTransition {
  readonly state: WebSocketState;
  readonly diagnostics: readonly WebSocketDiagnostic[];
}

/**
 * Apply the published transition table to one already-parsed frame.
 *
 * An illegal frame leaves the state unchanged and returns the refusal. The
 * table is total over the closed vocabulary and takes no session memory beyond
 * state, direction and the declared stream mode; sequence continuity, encoding
 * agreement and resume agreement are session facts carried by
 * {@link WebSocketConversationV1}.
 */
export function nextWebSocketStateV1(
  state: WebSocketState,
  frame: WebSocketServiceFrame,
  direction: WebSocketDirection,
  stream: WebSocketStreamMode,
): WebSocketTransition {
  const refuse = (message: string): WebSocketTransition => ({
    state,
    diagnostics: [diagnostic('client_contract.invalid_transport', 'type', message)],
  });
  const fromClient = direction === 'client-to-server';
  const fromServer = direction === 'server-to-client';
  const clientSends = stream === 'client' || stream === 'bidirectional';
  const serverSends = stream === 'server' || stream === 'bidirectional';
  const admitted = state === 'open' || state === 'half-closed';
  const pending = state === 'await-ready' || admitted;

  switch (frame.type) {
    case 'init':
      if (state !== 'await-init' || !fromClient) return refuse('init must be the first client frame');
      return { state: 'await-ready', diagnostics: [] };
    case 'ready':
      if (state !== 'await-ready' || !fromServer)
        return refuse('ready is valid only once, from the provider, after init');
      return { state: 'open', diagnostics: [] };
    case 'message':
      if (!admitted) return refuse('application message is valid only after ready');
      if (fromClient && (state !== 'open' || !clientSends))
        return refuse('client message direction is invalid for this state or stream');
      if (fromServer && !serverSends) return refuse('server message direction is invalid for this stream');
      return { state, diagnostics: [] };
    case 'half-close':
      if (state !== 'open' || !fromClient || !clientSends)
        return refuse('half-close is valid only from the client, from open, on a client or bidirectional stream');
      return { state: 'half-closed', diagnostics: [] };
    case 'result':
      if (!admitted || !fromServer) return refuse('result is valid only from the provider, after ready');
      if (stream === 'server' && frame.payload !== undefined)
        return refuse('a server stream delivers values in message frames, so result carries no payload');
      return { state: 'terminal', diagnostics: [] };
    case 'error':
      if (state === 'terminal' || !fromServer)
        return refuse('error is valid only from the provider, before a terminal frame');
      return { state: 'terminal', diagnostics: [] };
    case 'cancel':
      if (!pending || !fromClient)
        return refuse('cancel is valid only from the client, from init onwards, before a terminal frame');
      return { state: 'terminal', diagnostics: [] };
    default:
      if (!pending) return refuse('heartbeat is valid in both directions from await-ready onwards');
      return { state, diagnostics: [] };
  }
}

/** What a conversation needs to know about the transport it was opened on. */
export interface WebSocketConversationOptions {
  readonly stream: WebSocketStreamMode;
  readonly encoding: WebSocketPayloadEncoding;
  /** Mirrors the transport's `x-putnami-client` `websocket.resume` flag. */
  readonly resumeDeclared: boolean;
}

/**
 * The published session view of the wire: the transition table plus the facts
 * a single frame cannot carry — per-direction sequence continuity, the
 * transport's declared encoding, and whether resume was both requested and
 * declared.
 *
 * Every first-party runtime drives one of these. A runtime that keeps a phase
 * rule of its own would drift from the other three the first time the table
 * changes.
 */
export class WebSocketConversationV1 {
  private readonly options: WebSocketConversationOptions;
  private currentState: WebSocketState = 'await-init';
  private clientSequence = 0n;
  private serverSequence = 0n;
  private resumeRequested = false;

  constructor(options: WebSocketConversationOptions) {
    this.options = options;
  }

  /** The state reached by the frames accepted so far. */
  get state(): WebSocketState {
    return this.currentState;
  }

  /** The declared stream shape this conversation carries. */
  get stream(): WebSocketStreamMode {
    return this.options.stream;
  }

  /** True once an accepted `ready` reported a resumed stream. */
  get resumed(): boolean {
    return this.resumeRequested;
  }

  /** The next sequence a direction may send. Sequences are per-direction and gap-free. */
  nextSequence(direction: WebSocketDirection): bigint {
    return (direction === 'client-to-server' ? this.clientSequence : this.serverSequence) + 1n;
  }

  /**
   * Validate one already-parsed frame against the session facts and the
   * transition table, then advance. Returns the refusal and leaves the state
   * unchanged when the frame is illegal.
   */
  accept(frame: WebSocketServiceFrame, direction: WebSocketDirection): readonly WebSocketDiagnostic[] {
    const facts = this.checkSessionFacts(frame, direction);
    if (facts.length > 0) return facts;
    const transition = nextWebSocketStateV1(this.currentState, frame, direction, this.options.stream);
    if (transition.diagnostics.length > 0) return transition.diagnostics;
    this.recordSessionFacts(frame, direction);
    this.currentState = transition.state;
    return [];
  }

  private checkSessionFacts(
    frame: WebSocketServiceFrame,
    direction: WebSocketDirection,
  ): readonly WebSocketDiagnostic[] {
    switch (frame.type) {
      case 'init':
        if (frame.request !== undefined && frame.request.encoding !== this.options.encoding) {
          return [
            diagnostic(
              'client_contract.invalid_transport',
              'request.encoding',
              'request encoding differs from the selected transport',
            ),
          ];
        }
        if (frame.resume !== undefined && !this.options.resumeDeclared) {
          return [
            diagnostic(
              'client_contract.invalid_resilience',
              'resume',
              'selected websocket transport does not support resume',
            ),
          ];
        }
        return [];
      case 'ready':
        if (frame.resumed && (!this.resumeRequested || !this.options.resumeDeclared)) {
          return [
            diagnostic(
              'client_contract.invalid_resilience',
              'resumed',
              'ready reports a resumed stream that the init frame did not request on a resume-capable transport',
            ),
          ];
        }
        return [];
      case 'message': {
        if (frame.payload.encoding !== this.options.encoding) {
          return [
            diagnostic(
              'client_contract.invalid_transport',
              'payload.encoding',
              'message encoding differs from the selected transport',
            ),
          ];
        }
        if (BigInt(frame.sequence) !== this.nextSequence(direction)) {
          return [
            diagnostic(
              'client_contract.invalid_transport',
              'sequence',
              'message sequence is not the next one for its direction',
            ),
          ];
        }
        return [];
      }
      case 'result':
        if (frame.payload !== undefined && frame.payload.encoding !== this.options.encoding) {
          return [
            diagnostic(
              'client_contract.invalid_transport',
              'payload.encoding',
              'result encoding differs from the selected transport',
            ),
          ];
        }
        return [];
      default:
        return [];
    }
  }

  private recordSessionFacts(frame: WebSocketServiceFrame, direction: WebSocketDirection): void {
    if (frame.type === 'init') {
      if (frame.resume !== undefined) {
        this.resumeRequested = true;
        this.serverSequence = BigInt(frame.resume.afterSequence);
      }
      return;
    }
    if (frame.type !== 'message') return;
    const sequence = BigInt(frame.sequence);
    if (direction === 'client-to-server') this.clientSequence = sequence;
    else this.serverSequence = sequence;
  }
}

// ---------------------------------------------------------------------------
// Frame validation
// ---------------------------------------------------------------------------

const FRAME_FIELDS: Record<WebSocketFrameType, readonly string[]> = {
  init: [
    'v',
    'type',
    'operationId',
    'clientId',
    'deadlineUnixMs',
    'budgetMs',
    'credentials',
    'headers',
    'context',
    'resume',
    'request',
  ],
  ready: ['v', 'type', 'resumed', 'resumeToken'],
  message: ['v', 'type', 'sequence', 'payload'],
  'half-close': ['v', 'type'],
  result: ['v', 'type', 'payload'],
  error: ['v', 'type', 'error'],
  cancel: ['v', 'type', 'code'],
  ping: ['v', 'type', 'nonce'],
  pong: ['v', 'type', 'nonce'],
};

function validateFrame(type: WebSocketFrameType, frame: Record<string, unknown>): WebSocketDiagnostic[] {
  const diagnostics: WebSocketDiagnostic[] = [];
  if (frame['v'] !== SERVICE_WEBSOCKET_PROTOCOL_VERSION) {
    diagnostics.push(
      diagnostic('client_contract.invalid_protocol_version', 'v', 'websocket frame version is unsupported'),
    );
  }
  switch (type) {
    case 'init':
      validateInit(frame, diagnostics);
      break;
    case 'ready':
      validateReady(frame, diagnostics);
      break;
    case 'message':
      diagnostics.push(...validateSequence('sequence', frame['sequence'], false));
      diagnostics.push(...validatePayload('payload', frame['payload'], true));
      break;
    case 'half-close':
      break;
    case 'result':
      if (frame['payload'] !== undefined) diagnostics.push(...validatePayload('payload', frame['payload'], true));
      break;
    case 'error':
      validateError(frame, diagnostics);
      break;
    case 'cancel':
      if (!containsCancelCode(frame['code'])) {
        diagnostics.push(diagnostic('client_contract.invalid_enum', 'code', 'cancel code is unsupported'));
      }
      break;
    default:
      if (!isToken(frame['nonce'], 64)) {
        diagnostics.push(
          diagnostic('client_contract.invalid_transport', 'nonce', 'heartbeat type or nonce is invalid'),
        );
      }
  }
  return diagnostics;
}

function validateInit(frame: Record<string, unknown>, diagnostics: WebSocketDiagnostic[]): void {
  if (!isNonBlankString(frame['operationId']) || (frame['operationId'] as string).length > 512) {
    diagnostics.push(required('operationId'));
  }
  if (!isToken(frame['clientId'], 128)) {
    diagnostics.push(diagnostic('client_contract.invalid_transport', 'clientId', 'clientId must be a bounded token'));
  }
  diagnostics.push(...validateSequence('deadlineUnixMs', frame['deadlineUnixMs'], true));
  diagnostics.push(...validateSequence('budgetMs', frame['budgetMs'], true));
  validateInitCredentials(frame['credentials'], diagnostics);
  validateInitHeaders(frame['headers'], diagnostics);
  validateInitContext(frame['context'], diagnostics);
  validateInitResume(frame['resume'], diagnostics);
  if (frame['request'] !== undefined) diagnostics.push(...validatePayload('request', frame['request'], true));
}

function validateInitCredentials(value: unknown, diagnostics: WebSocketDiagnostic[]): void {
  if (!Array.isArray(value)) {
    diagnostics.push(required('credentials'));
    return;
  }
  if (value.length > 32) {
    diagnostics.push(
      diagnostic('client_contract.invalid_transport', 'credentials', 'at most 32 credential profiles are allowed'),
    );
  }
  const seen = new Set<string>();
  value.forEach((entry, index) => {
    const field = `credentials[${index}]`;
    if (!isRecord(entry) || !hasExactFields(entry, ['profile', 'value'])) {
      diagnostics.push(required(field));
      return;
    }
    if (!isNonBlankString(entry['profile']) || !isNonBlankString(entry['value'])) diagnostics.push(required(field));
    const credential = entry['value'];
    if (typeof credential === 'string' && (credential.length > 65_536 || hasControl(credential))) {
      diagnostics.push(
        diagnostic(
          'client_contract.invalid_transport',
          `${field}.value`,
          'credential value exceeds its bound or contains a forbidden control character',
        ),
      );
    }
    const profile = entry['profile'];
    if (typeof profile === 'string') {
      if (seen.has(profile)) {
        diagnostics.push(
          diagnostic('client_contract.duplicate', `${field}.profile`, 'credential profile appears more than once'),
        );
      }
      seen.add(profile);
    }
  });
}

function validateInitHeaders(value: unknown, diagnostics: WebSocketDiagnostic[]): void {
  if (!Array.isArray(value)) {
    diagnostics.push(required('headers'));
    return;
  }
  if (value.length > 64) {
    diagnostics.push(
      diagnostic('client_contract.invalid_transport', 'headers', 'at most 64 ordinary headers are allowed'),
    );
  }
  const seen = new Set<string>();
  value.forEach((entry, index) => {
    const field = `headers[${index}]`;
    if (!isRecord(entry) || !hasExactFields(entry, ['name', 'values'])) {
      diagnostics.push(required(field));
      return;
    }
    const name = entry['name'];
    if (typeof name !== 'string' || !HTTP_HEADER.test(name) || reservedWebSocketInitHeader(name.toLowerCase())) {
      diagnostics.push(
        diagnostic('client_contract.invalid_transport', `${field}.name`, 'ordinary header name is forbidden'),
      );
    } else {
      if (seen.has(name.toLowerCase())) {
        diagnostics.push(
          diagnostic('client_contract.duplicate', `${field}.name`, 'ordinary header name appears more than once'),
        );
      }
      seen.add(name.toLowerCase());
    }
    const values = entry['values'];
    if (!Array.isArray(values) || values.length < 1 || values.length > 32) {
      diagnostics.push(
        diagnostic(
          'client_contract.invalid_transport',
          `${field}.values`,
          'ordinary header requires between 1 and 32 values',
        ),
      );
      return;
    }
    for (const item of values) {
      if (typeof item !== 'string' || item.length > 8192 || hasControl(item)) {
        diagnostics.push(
          diagnostic(
            'client_contract.invalid_transport',
            `${field}.values`,
            'ordinary header value exceeds its bound or contains a forbidden control character',
          ),
        );
      }
    }
  });
}

function validateInitContext(value: unknown, diagnostics: WebSocketDiagnostic[]): void {
  if (value === undefined) return;
  if (!isRecord(value) || !hasExactFields(value, ['traceparent', 'tracestate', 'baggage', 'requestId'])) {
    diagnostics.push(parseDiagnostic());
    return;
  }
  for (const [name, item] of Object.entries(value)) {
    if (item === undefined) continue;
    if (typeof item !== 'string' || item.length > 4096 || hasControl(item)) {
      diagnostics.push(
        diagnostic(
          'client_contract.invalid_transport',
          `context.${name}`,
          'propagation value is invalid or exceeds 4096 bytes',
        ),
      );
    }
  }
}

function validateInitResume(value: unknown, diagnostics: WebSocketDiagnostic[]): void {
  if (value === undefined) return;
  if (!isRecord(value) || !hasExactFields(value, ['token', 'afterSequence'])) {
    diagnostics.push(parseDiagnostic());
    return;
  }
  const token = value['token'];
  if (!isNonBlankString(token)) diagnostics.push(required('resume.token'));
  else if ((token as string).length > 4096) {
    diagnostics.push(
      diagnostic('client_contract.invalid_transport', 'resume.token', 'resume token exceeds 4096 bytes'),
    );
  }
  diagnostics.push(...validateSequence('resume.afterSequence', value['afterSequence'], true));
}

function validateReady(frame: Record<string, unknown>, diagnostics: WebSocketDiagnostic[]): void {
  const resumed = frame['resumed'];
  const token = frame['resumeToken'];
  if (typeof resumed !== 'boolean') {
    diagnostics.push(required('resumed'));
  } else if (resumed && !isNonBlankString(token)) {
    diagnostics.push(required('resumeToken'));
  }
  if (typeof token === 'string' && token.length > 4096) {
    diagnostics.push(diagnostic('client_contract.invalid_transport', 'resumeToken', 'resume token exceeds 4096 bytes'));
  } else if (token !== undefined && typeof token !== 'string') {
    diagnostics.push(required('resumeToken'));
  }
}

function validateError(frame: Record<string, unknown>, diagnostics: WebSocketDiagnostic[]): void {
  const error = frame['error'];
  if (!isRecord(error) || !hasExactFields(error, ['status', 'code', 'grpcCode', 'retryable', 'message', 'details'])) {
    diagnostics.push(parseDiagnostic());
    return;
  }
  const status = error['status'];
  if (
    typeof status !== 'number' ||
    !Number.isInteger(status) ||
    status < 400 ||
    status > 599 ||
    !isNonBlankString(error['code'])
  ) {
    diagnostics.push(diagnostic('client_contract.invalid_error', 'error', 'typed error status and code are required'));
  }
  const grpcCode = error['grpcCode'];
  if (
    grpcCode !== undefined &&
    (!Number.isInteger(grpcCode) || (grpcCode as number) < 1 || (grpcCode as number) > 16)
  ) {
    diagnostics.push(
      diagnostic('client_contract.invalid_error', 'error.grpcCode', 'grpcCode must be between 1 and 16'),
    );
  }
  if (error['retryable'] !== undefined && typeof error['retryable'] !== 'boolean') {
    diagnostics.push(parseDiagnostic());
  }
  const message = error['message'];
  if (message !== undefined && (typeof message !== 'string' || message.length > 4096)) {
    diagnostics.push(
      diagnostic('client_contract.invalid_error', 'error.message', 'typed error message exceeds 4096 bytes'),
    );
  }
}

function validatePayload(field: string, value: unknown, present: boolean): WebSocketDiagnostic[] {
  if (!present || !isRecord(value)) return [parseDiagnostic()];
  const encoding = value['encoding'];
  if (encoding === 'json') {
    if (!hasExactFields(value, ['encoding', 'value']) || !Object.hasOwn(value, 'value')) {
      return [diagnostic('client_contract.invalid_transport', field, 'json payload requires value and forbids base64')];
    }
    return [];
  }
  if (encoding === 'proto') {
    if (!hasExactFields(value, ['encoding', 'base64']) || typeof value['base64'] !== 'string') {
      return [
        diagnostic(
          'client_contract.invalid_transport',
          field,
          'proto payload requires canonical RFC 4648 base64 and forbids value',
        ),
      ];
    }
    if (!isCanonicalBase64(value['base64'])) {
      return [
        diagnostic(
          'client_contract.invalid_transport',
          field,
          'proto payload requires canonical RFC 4648 base64 and forbids value',
        ),
      ];
    }
    return [];
  }
  return [diagnostic('client_contract.invalid_enum', `${field}.encoding`, 'payload encoding is unsupported')];
}

function validateSequence(field: string, value: unknown, allowZero: boolean): WebSocketDiagnostic[] {
  const invalid = [
    diagnostic('client_contract.invalid_transport', field, 'sequence must be a canonical uint64 decimal string'),
  ];
  if (typeof value !== 'string' || !/^(?:0|[1-9][0-9]*)$/.test(value)) return invalid;
  if (!allowZero && value === '0') return invalid;
  if (value.length > UINT64_MAX.length || (value.length === UINT64_MAX.length && value > UINT64_MAX)) return invalid;
  return [];
}

// ---------------------------------------------------------------------------
// Strict JSON
// ---------------------------------------------------------------------------

/**
 * Decode one JSON document under the same rules the Go contract applies:
 * no duplicate object keys, no JSON `null` anywhere, no trailing value.
 *
 * `JSON.parse` keeps the last of two identical keys and accepts `null`, so a
 * frame that a Go peer refuses would otherwise be admitted here — the exact
 * drift the shared corpus exists to prevent.
 */
function decodeStrictJson(text: string): unknown {
  const value = JSON.parse(text) as unknown;
  rejectStrictJsonDefects(text);
  return value;
}

function rejectStrictJsonDefects(text: string): void {
  const scanner = new StrictJsonScanner(text);
  scanner.value();
  scanner.end();
}

/** Minimal scanner: it only looks for the two defects `JSON.parse` hides. */
class StrictJsonScanner {
  private index = 0;

  constructor(private readonly text: string) {}

  end(): void {
    this.skipWhitespace();
    if (this.index !== this.text.length) throw new SyntaxError('trailing JSON value');
  }

  value(): void {
    this.skipWhitespace();
    const char = this.text[this.index];
    if (char === '{') {
      this.object();
      return;
    }
    if (char === '[') {
      this.array();
      return;
    }
    if (char === '"') {
      this.string();
      return;
    }
    if (this.text.startsWith('null', this.index)) throw new SyntaxError('null is not valid for this field');
    this.primitive();
  }

  private object(): void {
    this.index += 1;
    const seen = new Set<string>();
    this.skipWhitespace();
    if (this.text[this.index] === '}') {
      this.index += 1;
      return;
    }
    while (this.index < this.text.length) {
      this.skipWhitespace();
      const key = this.string();
      if (seen.has(key)) throw new SyntaxError('duplicate object key');
      seen.add(key);
      this.skipWhitespace();
      this.index += 1; // ':'
      this.value();
      this.skipWhitespace();
      const char = this.text[this.index];
      this.index += 1;
      if (char === '}') return;
      if (char !== ',') throw new SyntaxError('malformed object');
    }
    throw new SyntaxError('unterminated object');
  }

  private array(): void {
    this.index += 1;
    this.skipWhitespace();
    if (this.text[this.index] === ']') {
      this.index += 1;
      return;
    }
    while (this.index < this.text.length) {
      this.value();
      this.skipWhitespace();
      const char = this.text[this.index];
      this.index += 1;
      if (char === ']') return;
      if (char !== ',') throw new SyntaxError('malformed array');
    }
    throw new SyntaxError('unterminated array');
  }

  private string(): string {
    if (this.text[this.index] !== '"') throw new SyntaxError('expected a string');
    const start = this.index;
    this.index += 1;
    while (this.index < this.text.length) {
      const char = this.text[this.index];
      if (char === '\\') {
        this.index += 2;
        continue;
      }
      this.index += 1;
      if (char === '"') return JSON.parse(this.text.slice(start, this.index)) as string;
    }
    throw new SyntaxError('unterminated string');
  }

  private primitive(): void {
    const start = this.index;
    while (this.index < this.text.length && !',]} \t\n\r'.includes(this.text[this.index] as string)) this.index += 1;
    if (this.index === start) throw new SyntaxError('expected a JSON value');
  }

  private skipWhitespace(): void {
    while (this.index < this.text.length && ' \t\n\r'.includes(this.text[this.index] as string)) this.index += 1;
  }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const TOKEN = /^[A-Za-z0-9._~-]+$/;
const HTTP_HEADER = /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/;
const UINT64_MAX = '18446744073709551615';

/**
 * The closed list of headers an `init` frame may never carry. It is the
 * admission channel's whole security surface: a credential, an identity or a
 * propagation value belongs in its own declared member, never in an ordinary
 * header a consumer chose the name of.
 */
export function reservedWebSocketInitHeader(name: string): boolean {
  if (name.startsWith('sec-websocket-') || name.startsWith('proxy-')) return true;
  return RESERVED_INIT_HEADERS.has(name);
}

const RESERVED_INIT_HEADERS: ReadonlySet<string> = new Set([
  'authorization',
  'baggage',
  'connection',
  'content-length',
  'cookie',
  'host',
  'set-cookie',
  'traceparent',
  'tracestate',
  'upgrade',
  'x-client-id',
  'x-request-id',
]);

function diagnostic(code: WebSocketDiagnosticCode, field: string, message: string): WebSocketDiagnostic {
  return { code, field, message };
}

function parseDiagnostic(): WebSocketDiagnostic {
  return diagnostic('client_contract.parse_error', '', 'websocket frame is malformed or contains unsupported fields');
}

function required(field: string): WebSocketDiagnostic {
  return diagnostic('client_contract.required', field, 'required websocket frame member is missing');
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function hasExactFields(value: Record<string, unknown>, allowed: readonly string[]): boolean {
  const fields = new Set(allowed);
  return Object.keys(value).every((key) => fields.has(key));
}

function containsFrameType(value: string): value is WebSocketFrameType {
  return (WEBSOCKET_FRAME_TYPES_V1 as readonly string[]).includes(value);
}

function containsCancelCode(value: unknown): value is WebSocketCancelCode {
  return typeof value === 'string' && (WEBSOCKET_CANCEL_CODES_V1 as readonly string[]).includes(value);
}

function isNonBlankString(value: unknown): boolean {
  return typeof value === 'string' && value.length > 0;
}

function isToken(value: unknown, max: number): boolean {
  return typeof value === 'string' && value.length > 0 && value.length <= max && TOKEN.test(value);
}

function hasControl(value: string): boolean {
  return /[\r\n\0]/.test(value);
}

function isCanonicalBase64(value: string): boolean {
  if (!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)) return false;
  try {
    return btoa(atob(value)) === value;
  } catch {
    return false;
  }
}
