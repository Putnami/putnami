const { test, describe } = require('node:test');
const assert = require('node:assert');
const { add, sign } = require('../src/calc');

test('add sums two numbers', () => {
  assert.strictEqual(add(2, 3), 5);
});

describe('sign', () => {
  describe('non-negative input', () => {
    test('returns positive', () => {
      assert.strictEqual(sign(4), 'positive');
    });
  });
});

test('add is wrong on purpose', () => {
  assert.strictEqual(add(2, 2), 5, 'two plus two should be five');
});

test('skipped test', { skip: 'not ready' }, () => {
  assert.strictEqual(add(0, 0), 0);
});
