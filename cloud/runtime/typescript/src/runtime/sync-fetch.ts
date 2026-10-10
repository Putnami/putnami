export interface SyncFetchRequest {
  url: string;
  method?: 'GET' | 'POST';
  headers?: Record<string, string>;
  body?: string;
  timeoutMs?: number;
  throwOnHttpError?: boolean;
}

export interface SyncFetchResponse {
  status: number;
  body: string;
}

type SyncFetchImpl = (request: SyncFetchRequest) => SyncFetchResponse;

let testSyncFetch: SyncFetchImpl | undefined;
let workerState: SyncFetchWorkerState | undefined;

const controlState = {
  pending: 0,
  ok: 1,
  httpError: 2,
  fetchError: 3,
  bodyTooLarge: 4,
} as const;

const headerBytes = 16;
const maxBodyBytes = 8 * 1024 * 1024;

const workerSource = `
self.onmessage = async (event) => {
  const { request, shared } = event.data;
  const control = new Int32Array(shared, 0, 4);
  const output = new Uint8Array(shared, ${headerBytes});
  const encoder = new TextEncoder();

  function finish(state, status, text) {
    const bytes = encoder.encode(text || '');
    if (bytes.length > output.length) {
      const err = encoder.encode('sync fetch response exceeded ${maxBodyBytes} bytes');
      output.set(err.subarray(0, output.length));
      Atomics.store(control, 1, status || 0);
      Atomics.store(control, 2, Math.min(err.length, output.length));
      Atomics.store(control, 0, ${controlState.bodyTooLarge});
      Atomics.notify(control, 0);
      return;
    }
    output.set(bytes);
    Atomics.store(control, 1, status || 0);
    Atomics.store(control, 2, bytes.length);
    Atomics.store(control, 0, state);
    Atomics.notify(control, 0);
  }

  try {
    const timeout = request.timeoutMs || 5000;
    const response = await fetch(request.url, {
      method: request.method || 'GET',
      headers: request.headers || undefined,
      body: request.body,
      signal: AbortSignal.timeout(timeout),
    });
    const text = await response.text();
    finish(response.ok ? ${controlState.ok} : ${controlState.httpError}, response.status, text);
  } catch (err) {
    finish(${controlState.fetchError}, 0, err instanceof Error ? err.message : String(err));
  }
};
`;

export function setSyncFetchForTest(impl: SyncFetchImpl | undefined): void {
  testSyncFetch = impl;
}

export function resetSyncFetchForTest(): void {
  testSyncFetch = undefined;
  disposeSyncFetchWorker();
}

export function syncFetch(request: SyncFetchRequest): SyncFetchResponse {
  if (testSyncFetch) {
    return testSyncFetch(request);
  }
  return workerSyncFetch(request);
}

interface SyncFetchWorkerState {
  shared: SharedArrayBuffer;
  control: Int32Array;
  output: Uint8Array;
  worker: Worker;
  blobURL: string;
}

function workerSyncFetch(request: SyncFetchRequest): SyncFetchResponse {
  const timeoutMs = request.timeoutMs ?? 5000;
  const state = getSyncFetchWorker();
  const { control, output, shared, worker } = state;

  Atomics.store(control, 0, controlState.pending);
  Atomics.store(control, 1, 0);
  Atomics.store(control, 2, 0);

  try {
    worker.postMessage({ request: { ...request, timeoutMs }, shared });
    const waitResult = Atomics.wait(control, 0, controlState.pending, timeoutMs + 1000);
    if (waitResult === 'timed-out') {
      disposeSyncFetchWorker();
      throw new Error(`sync fetch timed out after ${timeoutMs}ms`);
    }

    const responseState = Atomics.load(control, 0);
    const status = Atomics.load(control, 1);
    const length = Atomics.load(control, 2);
    const body = new TextDecoder().decode(output.subarray(0, length));

    if (responseState === controlState.ok) {
      return { status, body };
    }
    if (responseState === controlState.httpError && request.throwOnHttpError === false) {
      return { status, body };
    }
    if (responseState === controlState.httpError) {
      throw new Error(`sync fetch returned HTTP ${status}`);
    }
    throw new Error(body || 'sync fetch failed');
  } catch (cause) {
    if (cause instanceof Error && cause.message.startsWith('Failed to post message')) {
      disposeSyncFetchWorker();
    }
    throw cause;
  }
}

function getSyncFetchWorker(): SyncFetchWorkerState {
  if (workerState) {
    return workerState;
  }

  const shared = new SharedArrayBuffer(headerBytes + maxBodyBytes);
  const blobURL = URL.createObjectURL(new Blob([workerSource], { type: 'application/javascript' }));
  const worker = new Worker(blobURL);
  workerState = {
    shared,
    control: new Int32Array(shared, 0, 4),
    output: new Uint8Array(shared, headerBytes),
    worker,
    blobURL,
  };
  return workerState;
}

function disposeSyncFetchWorker(): void {
  if (!workerState) {
    return;
  }
  workerState.worker.terminate();
  URL.revokeObjectURL(workerState.blobURL);
  workerState = undefined;
}
