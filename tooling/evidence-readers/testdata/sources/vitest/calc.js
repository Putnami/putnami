export function add(a, b) {
  return a + b;
}

export function sign(n) {
  if (n >= 0) {
    return 'positive';
  } else {
    return 'negative';
  }
}

export function neverCalled(x) {
  const doubled = x * 2;
  return doubled + 1;
}
