import { type InferConfig, useConfig } from '@putnami/runtime';
import type { DocumentAdapter } from './adapter/document.adapter';
import { FirestoreAdapter } from './adapter/firestore.adapter';
import { MemoryAdapter } from './adapter/memory.adapter';
import { DocumentConfig } from './config';
import { recordBackendClosed, recordBackendCount, recordBackendCreated } from './observability';

interface ManagedBackendEntry {
  backend: DocumentAdapter;
  refCount: number;
  storeName: string;
}

const backendRegistry = new Map<string, ManagedBackendEntry>();

function resolvedPath(storeName?: string): string | undefined {
  return storeName && storeName !== 'default' ? `document.${storeName}` : undefined;
}

function resolvedStoreName(storeName?: string): string {
  return storeName ?? 'default';
}

function buildBackend(storeName: string, conf: InferConfig<typeof DocumentConfig>): DocumentAdapter {
  switch (conf.backend) {
    case 'memory':
      return new MemoryAdapter(storeName);
    case 'firestore':
      return new FirestoreAdapter(storeName, {
        projectId: conf.projectId,
        databaseId: conf.databaseId,
        emulatorHost: conf.emulatorHost,
        credentials: conf.credentials,
      });
    default:
      throw new Error(`Unknown document backend: "${conf.backend}". Expected "memory" or "firestore".`);
  }
}

function backendCacheKey(storeName: string, conf: InferConfig<typeof DocumentConfig>): string {
  return JSON.stringify({
    storeName,
    backend: conf.backend,
    projectId: conf.projectId ?? null,
    databaseId: conf.databaseId,
    emulatorHost: conf.emulatorHost ?? null,
    credentials: conf.credentials ?? null,
  });
}

function acquireBackend(
  storeName: string,
  conf: InferConfig<typeof DocumentConfig>,
): ManagedBackendEntry & { key: string } {
  const key = backendCacheKey(storeName, conf);
  const cached = backendRegistry.get(key);
  if (cached) {
    cached.refCount++;
    return { ...cached, key };
  }

  const entry = {
    backend: buildBackend(storeName, conf),
    refCount: 1,
    storeName,
  };
  backendRegistry.set(key, entry);
  recordBackendCreated(storeName);
  recordBackendCount(backendRegistry.size);
  return { ...entry, key };
}

async function releaseBackend(key: string, storeName: string): Promise<void> {
  const cached = backendRegistry.get(key);
  if (!cached) return;

  cached.refCount--;
  if (cached.refCount > 0) return;

  backendRegistry.delete(key);
  await cached.backend.close();
  recordBackendClosed(storeName);
  recordBackendCount(backendRegistry.size);
}

export async function useBackend(
  storeName?: string,
  config?: InferConfig<typeof DocumentConfig>,
): Promise<DocumentAdapter> {
  const resolvedStore = resolvedStoreName(storeName);
  const conf = useConfig(DocumentConfig, {
    path: resolvedPath(storeName),
    confInit: config,
  });

  if (config) {
    return buildBackend(resolvedStore, conf);
  }

  return acquireBackend(resolvedStore, conf).backend;
}

export async function closeBackend(storeName?: string): Promise<void> {
  const resolvedStore = resolvedStoreName(storeName);
  const conf = useConfig(DocumentConfig, { path: resolvedPath(storeName) });
  const key = backendCacheKey(resolvedStore, conf);
  await releaseBackend(key, resolvedStore);
}

export async function closeAllBackends(): Promise<void> {
  const entries = Array.from(backendRegistry.entries());
  backendRegistry.clear();
  await Promise.all(
    entries.map(async ([, entry]) => {
      await entry.backend.close();
      recordBackendClosed(entry.storeName);
    }),
  );
  recordBackendCount(0);
}
