import { describe, expect, it } from 'bun:test';
import { declareEvents, registerDeclared, validateProps } from '../src/server/declare';
import { MAX_PROP_STRING_LEN } from '../src/server/sanitize/vocabulary';

describe('declareEvents', () => {
  it('returns the declaration it validated', () => {
    const events = declareEvents({ signup_click: { plan: String }, search: { hits: Number, empty: Boolean } });

    expect(Object.keys(events).sort()).toEqual(['search', 'signup_click']);
  });

  it('names the offending action', () => {
    expect(() => declareEvents({ 'Signup Click': { plan: String } })).toThrow('action name "Signup Click"');
    expect(() => declareEvents({ '1st': { plan: String } })).toThrow('action name "1st"');
  });

  it('names the offending property', () => {
    expect(() => declareEvents({ signup_click: { Plan: String } })).toThrow('property "Plan" of action "signup_click"');
  });

  it('rejects a type outside the three primitives', () => {
    expect(() => declareEvents({ signup_click: { at: Date as unknown as StringConstructor } })).toThrow(
      'must be typed String, Number, or Boolean',
    );
  });

  it('rejects a schema wider than the protocol allows', () => {
    const wide = Object.fromEntries(Array.from({ length: 21 }, (_, index) => [`p${index}`, String]));

    expect(() => declareEvents({ signup_click: wide })).toThrow('the maximum is 20');
  });
});

describe('registerDeclared', () => {
  it('splits a declaration into names and schemas', () => {
    const registered = registerDeclared({ signup_click: { plan: String } });

    expect(registered.names.has('signup_click')).toBe(true);
    expect(registered.schemas['signup_click']).toEqual({ plan: String });
  });

  it('accepts an application that declares nothing', () => {
    expect(registerDeclared({}).names.size).toBe(0);
  });
});

describe('validateProps', () => {
  const schema = { plan: String, seats: Number, trial: Boolean };

  it('keeps every declared key of the declared type', () => {
    expect(validateProps(schema, { plan: 'pro', seats: 12, trial: true })).toEqual({
      plan: 'pro',
      seats: 12,
      trial: true,
    });
  });

  it('drops a key nobody declared', () => {
    expect(validateProps(schema, { plan: 'pro', session_token: 'sk-live-1234' })).toEqual({ plan: 'pro' });
  });

  it('drops a declared key whose value has the wrong type', () => {
    expect(validateProps(schema, { plan: 12, seats: 'many', trial: 'yes' })).toEqual({});
  });

  it('drops a number that is not finite', () => {
    expect(validateProps(schema, { seats: Number.POSITIVE_INFINITY })).toEqual({});
    expect(validateProps(schema, { seats: Number.NaN })).toEqual({});
  });

  it('truncates an over-long declared string instead of dropping the event', () => {
    const value = validateProps(schema, { plan: 'x'.repeat(MAX_PROP_STRING_LEN + 50) })['plan'] as string;

    expect(value).toHaveLength(MAX_PROP_STRING_LEN);
  });

  it('yields nothing for an unknown action or an absent payload', () => {
    expect(validateProps(undefined, { plan: 'pro' })).toEqual({});
    expect(validateProps(schema, undefined)).toEqual({});
  });
});
