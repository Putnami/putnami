import { describe, expect, it } from 'bun:test';
import { isRegistration, provide } from '../../src/inject/provider';
import { named } from '../../src/inject/token';

describe('provide()', () => {
  describe('class provider', () => {
    it('should create a registration for a class with no deps', () => {
      class AppConfig {
        port = 3000;
      }
      const reg = provide(AppConfig);
      expect(reg.__brand).toBe('Registration');
      expect(reg.provider.token).toBe(AppConfig);
      expect(reg.provider.scope).toBe('singleton');
      expect(reg.provider.visibility).toBe('public');
      expect(reg.provider.deps).toEqual([]);
      expect(reg.provider.async).toBe(false);
    });

    it('should create a registration with deps', () => {
      class Database {}
      class UserService {}
      const reg = provide(UserService, { deps: [Database] });
      expect(reg.provider.deps).toEqual([Database]);
    });

    it('should create a registration with full options', () => {
      class MyService {}
      const onClose = () => {};
      const reg = provide(MyService, {
        scope: 'scoped',
        visibility: 'private',
        tags: ['service'],
        onClose,
      });
      expect(reg.provider.scope).toBe('scoped');
      expect(reg.provider.visibility).toBe('private');
      expect(reg.provider.tags).toEqual(['service']);
      expect(reg.provider.onClose).toBe(onClose);
    });
  });

  describe('factory provider', () => {
    it('should create a sync factory registration', () => {
      class Database {
        constructor(public url: string) {}
      }
      const reg = provide(Database, () => new Database('postgres://localhost'));
      expect(reg.provider.token).toBe(Database);
      expect(reg.provider.async).toBe(false);
    });

    it('should create an async factory registration', () => {
      class Database {
        constructor(public url: string) {}
        async connect() {}
      }
      const reg = provide(
        Database,
        async () => {
          const db = new Database('postgres://localhost');
          await db.connect();
          return db;
        },
        { onClose: (db) => db.connect() },
      );
      expect(reg.provider.token).toBe(Database);
      expect(reg.provider.async).toBe(true);
    });

    it('should support factory with options', () => {
      class Cache {}
      const reg = provide(Cache, () => new Cache(), { scope: 'scoped', tags: ['cache'] });
      expect(reg.provider.scope).toBe('scoped');
      expect(reg.provider.tags).toEqual(['cache']);
    });
  });

  describe('named token provider', () => {
    it('should create a registration for a named token', () => {
      const Version = named<string>('version');
      const reg = provide(Version, () => '2.0.0');
      expect(reg.provider.token).toBe(Version);
      expect(reg.provider.async).toBe(false);
    });
  });

  describe('lazy option', () => {
    it('should create a registration with lazy: true', () => {
      class ExpensiveService {}
      const reg = provide(ExpensiveService, { lazy: true });
      expect(reg.provider.lazy).toBe(true);
    });

    it('should default lazy to false', () => {
      class Service {}
      const reg = provide(Service);
      expect(reg.provider.lazy).toBe(false);
    });

    it('should support lazy with factory', () => {
      class Service {}
      const reg = provide(Service, () => new Service(), { lazy: true });
      expect(reg.provider.lazy).toBe(true);
    });
  });

  describe('validation', () => {
    it('should throw for invalid arguments', () => {
      // @ts-expect-error Testing invalid arguments
      expect(() => provide('not-a-class-or-token')).toThrow('Invalid provide() arguments');
    });
  });
});

describe('isRegistration()', () => {
  it('should return true for a valid registration', () => {
    class Service {}
    const reg = provide(Service);
    expect(isRegistration(reg)).toBe(true);
  });

  it('should return false for null', () => {
    expect(isRegistration(null)).toBe(false);
  });

  it('should return false for undefined', () => {
    expect(isRegistration(undefined)).toBe(false);
  });

  it('should return false for a plain object', () => {
    expect(isRegistration({ __brand: 'Other' })).toBe(false);
  });

  it('should return false for a non-object', () => {
    expect(isRegistration('string')).toBe(false);
    expect(isRegistration(42)).toBe(false);
  });
});
