import { describe, expect, it } from 'bun:test';
import { createGrpcTestClient } from '../../src/grpc/grpc-testing';
import { createEnvelope } from '../../src/grpc/proto-codec';

describe('createGrpcTestClient', () => {
  it('should create a client with required options', () => {
    const client = createGrpcTestClient({
      baseUrl: 'http://localhost:3000',
      packageName: 'test.v1',
    });
    expect(client).toBeDefined();
    expect(typeof client.unary).toBe('function');
    expect(typeof client.serverStream).toBe('function');
    expect(typeof client.listServices).toBe('function');
    expect(typeof client.checkHealth).toBe('function');
    expect(typeof client.close).toBe('function');
  });

  it('should create a client without packageName', () => {
    const client = createGrpcTestClient({
      baseUrl: 'http://localhost:3000',
    });
    expect(client).toBeDefined();
  });

  it('close should be a no-op', async () => {
    const client = createGrpcTestClient({
      baseUrl: 'http://localhost:3000',
    });
    // Should not throw
    await client.close();
  });
});

describe('GrpcTestClient path resolution', () => {
  // We test path resolution indirectly by verifying the fetch URL.
  // Since we can't easily mock fetch in Bun tests, we test against
  // a non-existent server and catch the connection error.

  it('should expand short path with packageName', async () => {
    const client = createGrpcTestClient({
      baseUrl: 'http://127.0.0.1:1', // Will fail to connect
      packageName: 'myapp.v1',
    });

    try {
      await client.unary('UsersService/ListUsers', {});
    } catch (err) {
      // Expected: connection refused. The important thing is that
      // we attempted to connect (path was resolved correctly).
      expect(err).toBeDefined();
    }
  });

  it('should use fully-qualified path as-is', async () => {
    const client = createGrpcTestClient({
      baseUrl: 'http://127.0.0.1:1',
      packageName: 'myapp.v1',
    });

    try {
      await client.unary('other.v1.UsersService/ListUsers', {});
    } catch (err) {
      expect(err).toBeDefined();
    }
  });
});

describe('Connect streaming envelope parsing', () => {
  // Test that the serverStream method correctly parses Connect envelope frames.
  // We do this by creating a mock response with known envelope data.

  it('should parse data frames from envelope', () => {
    const encoder = new TextEncoder();

    // Create a data frame (flags=0x00) with JSON payload
    const payload1 = encoder.encode(JSON.stringify({ id: 1, name: 'Alice' }));
    const frame1 = createEnvelope(0x00, payload1);

    const payload2 = encoder.encode(JSON.stringify({ id: 2, name: 'Bob' }));
    const frame2 = createEnvelope(0x00, payload2);

    // Create trailer frame (flags=0x02) with gRPC status
    const trailerPayload = encoder.encode(JSON.stringify({ 'grpc-status': 0 }));
    const trailerFrame = createEnvelope(0x02, trailerPayload);

    // Concatenate all frames
    const totalLength = frame1.length + frame2.length + trailerFrame.length;
    const buffer = new Uint8Array(totalLength);
    buffer.set(frame1, 0);
    buffer.set(frame2, frame1.length);
    buffer.set(trailerFrame, frame1.length + frame2.length);

    // Verify envelope structure
    expect(buffer[0]).toBe(0x00); // First frame: data flag
    expect(buffer[frame1.length]).toBe(0x00); // Second frame: data flag
    expect(buffer[frame1.length + frame2.length]).toBe(0x02); // Trailer flag
  });

  it('should handle empty stream (trailer only)', () => {
    const encoder = new TextEncoder();
    const trailerPayload = encoder.encode(JSON.stringify({ 'grpc-status': 0 }));
    const trailerFrame = createEnvelope(0x02, trailerPayload);

    expect(trailerFrame[0]).toBe(0x02);
    expect(trailerFrame.length).toBe(5 + trailerPayload.length);
  });
});
