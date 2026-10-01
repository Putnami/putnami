import type { BucketDefinition } from './bucket.types';

/**
 * Helper class for reading bucket metadata.
 * Provides a convenient API over a BucketDefinition (no reflect-metadata).
 */
export class BucketHelper<T extends BucketDefinition> {
  constructor(private bucketDef: T) {}

  /** The bucket name */
  get bucketName(): string {
    return this.bucketDef.bucketName;
  }

  /** Named storage backend (undefined for default) */
  get storage(): string | undefined {
    return this.bucketDef.options.storage;
  }

  /** Whether objects are publicly readable */
  get isPublic(): boolean {
    return this.bucketDef.options.public === true;
  }

  /** Maximum file size constraint (e.g., '10mb') */
  get maxFileSize(): string | undefined {
    return this.bucketDef.options.maxFileSize;
  }

  /** Maximum file size in bytes, parsed from the human-readable string */
  get maxFileSizeBytes(): number | undefined {
    const raw = this.bucketDef.options.maxFileSize;
    if (!raw) return undefined;
    return parseFileSize(raw);
  }

  /** Allowed MIME types */
  get allowedMimeTypes(): readonly string[] | undefined {
    return this.bucketDef.options.allowedMimeTypes;
  }

  /**
   * Validate that a file meets this bucket's constraints.
   * Returns an array of validation error messages (empty if valid).
   */
  validate(file: { size: number; mimeType?: string }): string[] {
    const errors: string[] = [];

    const maxBytes = this.maxFileSizeBytes;
    if (maxBytes !== undefined && file.size > maxBytes) {
      errors.push(`File size ${file.size} bytes exceeds maximum ${this.maxFileSize}`);
    }

    const allowed = this.allowedMimeTypes;
    if (allowed && allowed.length > 0) {
      if (!file.mimeType) {
        // A missing content type must not bypass the allowlist — otherwise an
        // upload can defeat the MIME restriction simply by omitting Content-Type.
        errors.push(`A content type is required; allowed types: ${allowed.join(', ')}`);
      } else if (!allowed.includes(file.mimeType)) {
        errors.push(`MIME type "${file.mimeType}" is not allowed. Allowed: ${allowed.join(', ')}`);
      }
    }

    return errors;
  }
}

/** Create a bucket helper for a given bucket definition */
export const bucketHelper = <T extends BucketDefinition>(bucketDef: T): BucketHelper<T> => new BucketHelper(bucketDef);

// ---- Utilities ----

const SIZE_UNITS: Record<string, number> = {
  b: 1,
  kb: 1024,
  mb: 1024 * 1024,
  gb: 1024 * 1024 * 1024,
  tb: 1024 * 1024 * 1024 * 1024,
};

/**
 * Parse a human-readable file size (e.g., '10mb', '1.5gb') into bytes.
 */
export function parseFileSize(size: string): number {
  const match = size.toLowerCase().match(/^(\d+(?:\.\d+)?)\s*(b|kb|mb|gb|tb)$/);
  if (!match) {
    throw new Error(`Invalid file size format: "${size}". Expected format like "10mb", "1.5gb".`);
  }
  const value = Number.parseFloat(match[1]);
  const unit = match[2];
  return Math.floor(value * SIZE_UNITS[unit]);
}
