import { describe, expect, it } from 'bun:test';
import { hello } from '../src';

describe('hello', () => {
  it('should return a greeting', () => {
    expect(hello('World')).toBe('Hello, World!');
  });
});
