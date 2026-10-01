import { getExternalCaller, getProjectRoot } from '@putnami/utils';
import { bucketRegistry } from './bucket.registry';
import { BUCKET_MARKER, type BucketDefinition, type BucketOptions } from './bucket.types';

/**
 * Define a storage bucket.
 *
 * Buckets are registered globally at definition time (like Table() in @putnami/database).
 * The storage plugin discovers them during `generate()` and configures them during `warmup()`.
 *
 * @param bucketName - Unique bucket identifier (e.g., 'avatars', 'invoices')
 * @param options - Bucket-level constraints and settings
 *
 * @example
 * ```typescript
 * export const AvatarsBucket = Bucket('avatars', {
 *   maxFileSize: '10mb',
 *   allowedMimeTypes: ['image/png', 'image/jpeg', 'image/webp'],
 *   public: true,
 * });
 *
 * export const InvoicesBucket = Bucket('invoices', {
 *   maxFileSize: '50mb',
 *   allowedMimeTypes: ['application/pdf'],
 *   public: false,
 * });
 * ```
 */
export function Bucket<O extends BucketOptions>(bucketName: string, options?: O): BucketDefinition<O> {
  const bucketOptions = options ?? ({} as O);
  const caller = getExternalCaller(getProjectRoot());

  const definition: BucketDefinition<O> = {
    __bucket: BUCKET_MARKER,
    bucketName,
    options: bucketOptions,
    ...(caller
      ? {
          __source: {
            path: caller.filePath,
            line: caller.lineNumber,
            ...(caller.functionName ? { symbol: caller.functionName } : {}),
          },
        }
      : {}),
  };

  try {
    bucketRegistry.register(definition);
  } catch (error) {
    // Silently skip already-registered buckets (e.g., in tests or re-imports)
    if (error instanceof Error && !error.message.includes('already registered')) {
      throw error;
    }
  }

  return definition;
}
