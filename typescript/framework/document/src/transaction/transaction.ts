import { runInContext, tryContext, useContext, useLogger } from '@putnami/runtime';
import type { AdapterTx } from '../adapter/document.adapter';
import { useBackend } from '../factory';
import { DocumentErrorCode, TransactionNotSupported } from '../errors';
import { recordTransactionCommitted, recordTransactionRolledBack, recordTransactionStarted } from '../observability';

const TX_KEY = Symbol.for('__putnami_document_tx__');

type TransactionState = {
  active: boolean;
  storeName: string;
  tx?: AdapterTx;
  timedOut?: boolean;
  timeoutId?: Timer;
};

function getTxState(): TransactionState | undefined {
  const context = tryContext<Record<symbol, unknown>>();
  return context?.[TX_KEY] as TransactionState | undefined;
}

export interface TransactionOptions {
  storeName?: string;
  timeoutMs?: number;
}

export function useDocumentTransaction(storeName?: string): AdapterTx | undefined {
  const tx = getTxState();
  if (!tx?.active) return undefined;

  const resolvedStore = storeName ?? 'default';
  if (tx.storeName !== resolvedStore) {
    throw new TransactionNotSupported(
      `Cross-store document transaction is not supported: active="${tx.storeName}", requested="${resolvedStore}"`,
      `active=${tx.storeName}, requested=${resolvedStore}`,
      undefined,
      DocumentErrorCode.CrossStoreTransaction,
    );
  }

  if (tx.timedOut) {
    throw new TransactionNotSupported('Document transaction timed out', 'timeout');
  }

  return tx.tx;
}

export async function runInTransaction<R>(fn: () => Promise<R> | R, options: TransactionOptions = {}): Promise<R> {
  const existingContext = tryContext<Record<symbol, unknown>>();
  if (!existingContext) {
    return runInContext({}, () => runInTransaction(fn, options));
  }

  const resolvedStore = options.storeName ?? 'default';
  const existing = getTxState();
  if (existing?.active) {
    if (existing.storeName !== resolvedStore) {
      throw new TransactionNotSupported(
        `Cross-store document transaction is not supported: active="${existing.storeName}", requested="${resolvedStore}"`,
        `active=${existing.storeName}, requested=${resolvedStore}`,
        undefined,
        DocumentErrorCode.CrossStoreTransaction,
      );
    }
    return fn();
  }

  const adapter = await useBackend(options.storeName);
  if (!adapter.capabilities.transactions) {
    throw new TransactionNotSupported(`Backend does not support transactions`, 'capability-missing');
  }

  const context = useContext<Record<symbol, unknown>>();
  const state: TransactionState = {
    active: true,
    storeName: resolvedStore,
  };
  context[TX_KEY] = state;

  if (getTxState() !== state) {
    delete context[TX_KEY];
    throw new Error('runInTransaction() requires an active context. Wrap execution in runInContext().');
  }

  const timeoutMs = options.timeoutMs ?? 0;
  if (timeoutMs > 0) {
    state.timeoutId = setTimeout(() => {
      if (!state.active) return;
      state.timedOut = true;
      useLogger('document').warn(`Document transaction timed out after ${timeoutMs}ms`, { store: resolvedStore });
    }, timeoutMs);
  }

  const startedAt = Date.now();
  recordTransactionStarted(resolvedStore);

  try {
    const result = await adapter.runInTransaction(async (tx) => {
      state.tx = tx;
      if (state.timedOut) {
        throw new TransactionNotSupported('Document transaction timed out', 'timeout');
      }
      const value = await fn();
      if (state.timedOut) {
        throw new TransactionNotSupported('Document transaction timed out', 'timeout');
      }
      return value;
    });
    recordTransactionCommitted(resolvedStore, Date.now() - startedAt);
    return result;
  } catch (error) {
    recordTransactionRolledBack(resolvedStore, Date.now() - startedAt, error);
    throw error;
  } finally {
    state.active = false;
    state.tx = undefined;
    if (state.timeoutId) {
      clearTimeout(state.timeoutId);
    }
    delete context[TX_KEY];
  }
}
