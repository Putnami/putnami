const { add, sign } = require('./calc');

test('add sums two numbers', () => {
  expect(add(2, 3)).toBe(5);
});

describe('sign', () => {
  describe('non-negative input', () => {
    test('returns positive', () => {
      expect(sign(4)).toBe('positive');
    });
  });
});

test('add is wrong on purpose', () => {
  expect(add(2, 2)).toBe(5);
});

test.skip('skipped test', () => {
  expect(add(0, 0)).toBe(0);
});
