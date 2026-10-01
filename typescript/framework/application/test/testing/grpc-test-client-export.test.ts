import { describe, expect, it } from 'bun:test';
import * as grpcBarrel from '../../src/grpc';
import * as testingBarrel from '../../src/testing';

describe('gRPC test client export surface', () => {
  it('is exported from the testing entry point', () => {
    expect(typeof testingBarrel.createGrpcTestClient).toBe('function');
  });

  it('is not exported from the production grpc entry point', () => {
    // Test-only machinery must stay out of the runtime "." surface (via ./grpc).
    expect('createGrpcTestClient' in grpcBarrel).toBe(false);
  });
});
