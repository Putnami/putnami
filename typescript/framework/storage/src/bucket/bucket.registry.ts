import type { BucketDefinition } from './bucket.types';

/**
 * Central registry for bucket definitions.
 * Buckets are registered synchronously when Bucket() is called at module evaluation time.
 */
class BucketRegistry {
  private buckets: BucketDefinition[] = [];

  /**
   * Register a bucket definition.
   */
  register(bucket: BucketDefinition): void {
    if (this.buckets.some((b) => b.bucketName === bucket.bucketName)) {
      throw new Error(`Bucket with name "${bucket.bucketName}" is already registered`);
    }
    this.buckets.push(bucket);
  }

  /**
   * Get all registered bucket definitions.
   */
  getAll(): BucketDefinition[] {
    return [...this.buckets];
  }

  /**
   * Find a registered bucket by name.
   */
  getByName(name: string): BucketDefinition | undefined {
    return this.buckets.find((b) => b.bucketName === name);
  }

  /**
   * Get all registered bucket names.
   */
  getNames(): string[] {
    return this.buckets.map((b) => b.bucketName);
  }

  /**
   * Clear all buckets (useful for testing).
   */
  clear(): void {
    this.buckets = [];
  }
}

/**
 * Global bucket registry instance.
 */
export const bucketRegistry = new BucketRegistry();
