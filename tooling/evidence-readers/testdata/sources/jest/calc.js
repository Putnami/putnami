function add(a, b) {
  return a + b;
}

function sign(n) {
  if (n >= 0) {
    return 'positive';
  } else {
    return 'negative';
  }
}

function neverCalled(x) {
  const doubled = x * 2;
  return doubled + 1;
}

module.exports = { add, sign, neverCalled };
