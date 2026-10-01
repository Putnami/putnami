import { afterEach, describe, expect, it } from 'bun:test';
import { Bucket } from '../src/bucket/bucket.builders';
import { BucketHelper, bucketHelper, parseFileSize } from '../src/bucket/bucket.helper';
import { bucketRegistry } from '../src/bucket/bucket.registry';
import { BUCKET_MARKER, isBucketDefinition } from '../src/bucket/bucket.types';

afterEach(() => {
  bucketRegistry.clear();
});

describe('Bucket', () => {
  describe('Bucket()', () => {
    it('should create a bucket definition with the correct marker', () => {
      const bucket = Bucket('test-bucket');
      expect(bucket.__bucket).toBe(BUCKET_MARKER);
      expect(bucket.bucketName).toBe('test-bucket');
    });

    it('should register the bucket in the global registry', () => {
      Bucket('registered-bucket');
      expect(bucketRegistry.getByName('registered-bucket')).toBeDefined();
    });

    it('should store options', () => {
      const bucket = Bucket('opts-bucket', {
        maxFileSize: '10mb',
        allowedMimeTypes: ['image/png', 'image/jpeg'],
        public: true,
        storage: 'archive',
      });
      expect(bucket.options.maxFileSize).toBe('10mb');
      expect(bucket.options.allowedMimeTypes).toEqual(['image/png', 'image/jpeg']);
      expect(bucket.options.public).toBe(true);
      expect(bucket.options.storage).toBe('archive');
    });

    it('should default options to empty object', () => {
      const bucket = Bucket('empty-opts');
      expect(bucket.options).toEqual({});
    });

    it('should silently skip duplicate registration', () => {
      Bucket('dup-bucket');
      // Second call with the same name should not throw
      expect(() => Bucket('dup-bucket')).not.toThrow();
    });

    it('should re-throw non-duplicate registration errors', () => {
      const originalRegister = bucketRegistry.register.bind(bucketRegistry);
      bucketRegistry.register = () => {
        throw new Error('unexpected internal error');
      };
      try {
        expect(() => Bucket('error-bucket')).toThrow('unexpected internal error');
      } finally {
        bucketRegistry.register = originalRegister;
      }
    });
  });

  describe('isBucketDefinition', () => {
    it('should return true for bucket definitions', () => {
      const bucket = Bucket('type-guard-test');
      expect(isBucketDefinition(bucket)).toBe(true);
    });

    it('should return false for non-bucket objects', () => {
      expect(isBucketDefinition({})).toBe(false);
      expect(isBucketDefinition(null)).toBe(false);
      expect(isBucketDefinition(undefined)).toBe(false);
      expect(isBucketDefinition('string')).toBe(false);
      expect(isBucketDefinition(42)).toBe(false);
    });
  });
});

describe('BucketRegistry', () => {
  it('should return all registered buckets', () => {
    Bucket('reg-a');
    Bucket('reg-b');
    const all = bucketRegistry.getAll();
    expect(all).toHaveLength(2);
    expect(all.map((b) => b.bucketName)).toEqual(['reg-a', 'reg-b']);
  });

  it('should return names', () => {
    Bucket('name-a');
    Bucket('name-b');
    expect(bucketRegistry.getNames()).toEqual(['name-a', 'name-b']);
  });

  it('should return undefined for unknown buckets', () => {
    expect(bucketRegistry.getByName('nonexistent')).toBeUndefined();
  });

  it('should throw on direct duplicate registration', () => {
    const def = { __bucket: BUCKET_MARKER as typeof BUCKET_MARKER, bucketName: 'direct-dup', options: {} };
    bucketRegistry.register(def);
    expect(() => bucketRegistry.register(def)).toThrow('already registered');
  });

  it('should clear all registrations', () => {
    Bucket('clear-me');
    expect(bucketRegistry.getAll()).toHaveLength(1);
    bucketRegistry.clear();
    expect(bucketRegistry.getAll()).toHaveLength(0);
  });
});

describe('BucketHelper', () => {
  it('should expose bucket name', () => {
    const bucket = Bucket('helper-test');
    const helper = new BucketHelper(bucket);
    expect(helper.bucketName).toBe('helper-test');
  });

  it('should expose public flag', () => {
    const pub = Bucket('pub', { public: true });
    const priv = Bucket('priv', { public: false });
    const def = Bucket('def');
    expect(new BucketHelper(pub).isPublic).toBe(true);
    expect(new BucketHelper(priv).isPublic).toBe(false);
    expect(new BucketHelper(def).isPublic).toBe(false);
  });

  it('should expose storage name', () => {
    const bucket = Bucket('storage-test', { storage: 'archive' });
    expect(new BucketHelper(bucket).storage).toBe('archive');
  });

  it('should expose maxFileSize', () => {
    const bucket = Bucket('size-test', { maxFileSize: '50mb' });
    const helper = new BucketHelper(bucket);
    expect(helper.maxFileSize).toBe('50mb');
    expect(helper.maxFileSizeBytes).toBe(50 * 1024 * 1024);
  });

  it('should return undefined for unset maxFileSizeBytes', () => {
    const bucket = Bucket('no-size');
    expect(new BucketHelper(bucket).maxFileSizeBytes).toBeUndefined();
  });

  it('should expose allowedMimeTypes', () => {
    const bucket = Bucket('mime-test', { allowedMimeTypes: ['image/png'] });
    expect(new BucketHelper(bucket).allowedMimeTypes).toEqual(['image/png']);
  });

  describe('validate', () => {
    it('should pass when file meets all constraints', () => {
      const bucket = Bucket('valid-test', {
        maxFileSize: '10mb',
        allowedMimeTypes: ['image/png', 'image/jpeg'],
      });
      const helper = bucketHelper(bucket);
      expect(helper.validate({ size: 1024, mimeType: 'image/png' })).toEqual([]);
    });

    it('should reject files exceeding max size', () => {
      const bucket = Bucket('size-reject', { maxFileSize: '1kb' });
      const helper = bucketHelper(bucket);
      const errors = helper.validate({ size: 2048 });
      expect(errors).toHaveLength(1);
      expect(errors[0]).toContain('exceeds maximum');
    });

    it('should reject disallowed MIME types', () => {
      const bucket = Bucket('mime-reject', { allowedMimeTypes: ['image/png'] });
      const helper = bucketHelper(bucket);
      const errors = helper.validate({ size: 100, mimeType: 'image/gif' });
      expect(errors).toHaveLength(1);
      expect(errors[0]).toContain('not allowed');
    });

    it('should return multiple errors at once', () => {
      const bucket = Bucket('multi-error', {
        maxFileSize: '1kb',
        allowedMimeTypes: ['image/png'],
      });
      const helper = bucketHelper(bucket);
      const errors = helper.validate({ size: 2048, mimeType: 'image/gif' });
      expect(errors).toHaveLength(2);
    });

    it('should reject a missing mimeType when an allowlist is set', () => {
      // A missing content type must not bypass the allowlist — otherwise an
      // upload defeats the MIME restriction by omitting Content-Type.
      const bucket = Bucket('no-mime', { allowedMimeTypes: ['image/png'] });
      const helper = bucketHelper(bucket);
      const errors = helper.validate({ size: 100 });
      expect(errors).toHaveLength(1);
      expect(errors[0]).toContain('content type is required');
    });

    it('should pass when no constraints are set', () => {
      const bucket = Bucket('no-constraints');
      const helper = bucketHelper(bucket);
      expect(helper.validate({ size: 999_999_999, mimeType: 'anything/goes' })).toEqual([]);
    });
  });
});

describe('parseFileSize', () => {
  it('should parse bytes', () => {
    expect(parseFileSize('100b')).toBe(100);
  });

  it('should parse kilobytes', () => {
    expect(parseFileSize('1kb')).toBe(1024);
  });

  it('should parse megabytes', () => {
    expect(parseFileSize('10mb')).toBe(10 * 1024 * 1024);
  });

  it('should parse gigabytes', () => {
    expect(parseFileSize('2gb')).toBe(2 * 1024 * 1024 * 1024);
  });

  it('should parse terabytes', () => {
    expect(parseFileSize('1tb')).toBe(1024 * 1024 * 1024 * 1024);
  });

  it('should parse decimal values', () => {
    expect(parseFileSize('1.5gb')).toBe(Math.floor(1.5 * 1024 * 1024 * 1024));
  });

  it('should be case-insensitive', () => {
    expect(parseFileSize('10MB')).toBe(10 * 1024 * 1024);
    expect(parseFileSize('1GB')).toBe(1024 * 1024 * 1024);
  });

  it('should throw on invalid format', () => {
    expect(() => parseFileSize('abc')).toThrow('Invalid file size format');
    expect(() => parseFileSize('10')).toThrow('Invalid file size format');
    expect(() => parseFileSize('')).toThrow('Invalid file size format');
  });
});
