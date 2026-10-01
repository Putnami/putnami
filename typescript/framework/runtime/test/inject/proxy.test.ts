import { describe, expect, it } from 'bun:test';
import { Container, ScopedContainer } from '../../src/inject/container';
import { ContainerContext } from '../../src/inject/container-context';
import type { TraceSink } from '../../src/inject/inject.type';
import { provide } from '../../src/inject/provider';
import { createScopeProxy, createTracingProxy } from '../../src/inject/proxy';

describe('proxy', () => {
  describe('scope proxy', () => {
    it('should delegate property access to the current scope instance', () => {
      class RequestContext {
        constructor(public requestId: string) {}
        getRequestId() {
          return this.requestId;
        }
      }

      const container = new ScopedContainer('scope', new Container('root'));
      container.register(provide(RequestContext, () => new RequestContext('req-123'), { scope: 'scoped' }));

      const proxy = createScopeProxy(RequestContext, () => container);

      expect(proxy.requestId).toBe('req-123');
      expect(proxy.getRequestId()).toBe('req-123');
    });

    it('should resolve from different scopes on each access', () => {
      class Counter {
        value = 0;
        increment() {
          this.value++;
        }
      }

      const root = new Container('root');

      // Scope A
      const scopeA = new ScopedContainer('scopeA', root);
      scopeA.register(provide(Counter, () => new Counter(), { scope: 'scoped' }));
      const instanceA = scopeA.get(Counter);
      instanceA.value = 10;

      // Scope B
      const scopeB = new ScopedContainer('scopeB', root);
      scopeB.register(provide(Counter, () => new Counter(), { scope: 'scoped' }));
      const instanceB = scopeB.get(Counter);
      instanceB.value = 20;

      // Proxy that switches scope
      let activeScope: Container = scopeA;
      const proxy = createScopeProxy(Counter, () => activeScope);

      expect(proxy.value).toBe(10);

      activeScope = scopeB;
      expect(proxy.value).toBe(20);

      // Switch back
      activeScope = scopeA;
      expect(proxy.value).toBe(10);
    });

    it('should delegate method calls to the current scope instance', () => {
      class Service {
        private data: string[] = [];
        add(item: string) {
          this.data.push(item);
        }
        getAll() {
          return [...this.data];
        }
      }

      const root = new Container('root');
      const scope = new ScopedContainer('scope', root);
      scope.register(provide(Service, () => new Service(), { scope: 'scoped' }));

      const proxy = createScopeProxy(Service, () => scope);

      proxy.add('a');
      proxy.add('b');
      expect(proxy.getAll()).toEqual(['a', 'b']);
    });

    it('should support set operations through the proxy', () => {
      class MutableState {
        value = 'initial';
      }

      const root = new Container('root');
      const scope = new ScopedContainer('scope', root);
      scope.register(provide(MutableState, () => new MutableState(), { scope: 'scoped' }));

      const proxy = createScopeProxy(MutableState, () => scope);
      proxy.value = 'updated';

      // The underlying instance should reflect the change
      const instance = scope.get(MutableState);
      expect(instance.value).toBe('updated');
    });

    it('should support has() trap for in operator', () => {
      class Config {
        port = 3000;
      }

      const root = new Container('root');
      const scope = new ScopedContainer('scope', root);
      scope.register(provide(Config, () => new Config(), { scope: 'scoped' }));

      const proxy = createScopeProxy(Config, () => scope);

      expect('port' in proxy).toBe(true);
      expect('missing' in proxy).toBe(false);
    });

    it('should support ownKeys() trap for Object.keys()', () => {
      class Settings {
        host = 'localhost';
        port = 3000;
      }

      const root = new Container('root');
      const scope = new ScopedContainer('scope', root);
      scope.register(provide(Settings, () => new Settings(), { scope: 'scoped' }));

      const proxy = createScopeProxy(Settings, () => scope);
      const keys = Object.keys(proxy);

      expect(keys).toContain('host');
      expect(keys).toContain('port');
    });

    it('should support getPrototypeOf() trap', () => {
      class MyClass {
        myMethod() {
          return 42;
        }
      }

      const root = new Container('root');
      const scope = new ScopedContainer('scope', root);
      scope.register(provide(MyClass, () => new MyClass(), { scope: 'scoped' }));

      const proxy = createScopeProxy(MyClass, () => scope);
      const proto = Object.getPrototypeOf(proxy);

      expect(proto).toBe(MyClass.prototype);
    });

    it('should return the proxy label for Symbol.toStringTag', () => {
      class MyService {}

      const root = new Container('root');
      const scope = new ScopedContainer('scope', root);
      scope.register(provide(MyService, () => new MyService(), { scope: 'scoped' }));

      const proxy = createScopeProxy(MyService, () => scope);

      // Object.prototype.toString reads Symbol.toStringTag for diagnostics.
      expect(Object.prototype.toString.call(proxy)).toBe('[object ScopeProxy<MyService>]');
    });

    it('should delegate Symbol.toPrimitive for numeric coercion of scoped values', () => {
      // Regression: hijacking Symbol.toPrimitive made every coercion
      // return the literal "ScopeProxy<...>" string instead of the real value.
      class Money {
        constructor(private cents: number) {}
        [Symbol.toPrimitive](hint: string) {
          return hint === 'string' ? `$${this.cents / 100}` : this.cents;
        }
      }

      const root = new Container('root');
      const scope = new ScopedContainer('scope', root);
      scope.register(provide(Money, () => new Money(1050), { scope: 'scoped' }));

      const proxy = createScopeProxy(Money, () => scope);

      // Numeric coercion delegates to the instance's toPrimitive.
      expect(+proxy).toBe(1050);
      expect(`${proxy}`).toBe('$10.5');
    });

    it('should not shadow Symbol.toPrimitive with the label string', () => {
      // Reading Symbol.toPrimitive used to return the literal
      // "ScopeProxy<...>" string for every scoped instance. Now it must delegate.
      class Temperature {
        constructor(private celsius: number) {}
        [Symbol.toPrimitive]() {
          return this.celsius;
        }
      }

      const root = new Container('root');
      const scope = new ScopedContainer('scope', root);
      scope.register(provide(Temperature, () => new Temperature(21), { scope: 'scoped' }));

      const proxy = createScopeProxy(Temperature, () => scope);

      // The proxy exposes the instance's toPrimitive function, not a string.
      expect(typeof proxy[Symbol.toPrimitive]).toBe('function');
    });

    it('should delegate Symbol.toPrimitive-free instances to valueOf/toString', () => {
      // Plain objects have no own Symbol.toPrimitive; coercion must fall back to
      // the delegated valueOf/toString rather than the old label string.
      class Box {
        constructor(private n: number) {}
        valueOf() {
          return this.n;
        }
      }

      const root = new Container('root');
      const scope = new ScopedContainer('scope', root);
      scope.register(provide(Box, () => new Box(7), { scope: 'scoped' }));

      const proxy = createScopeProxy(Box, () => scope);

      // No own toPrimitive → coercion uses delegated valueOf().
      expect(proxy[Symbol.toPrimitive as keyof Box]).toBeUndefined();
      expect(proxy + 1).toBe(8);
    });

    it('should throw when scope resolver returns a container without the token', () => {
      class Missing {}

      const root = new Container('root');
      const scope = new ScopedContainer('scope', root);
      // Not registering Missing

      const proxy = createScopeProxy(Missing, () => scope);
      expect(() => proxy.toString()).toThrow();
    });

    it('should work with async methods through the proxy', async () => {
      class AsyncService {
        async fetchData(id: string) {
          return `data-${id}`;
        }
      }

      const root = new Container('root');
      const scope = new ScopedContainer('scope', root);
      scope.register(provide(AsyncService, () => new AsyncService(), { scope: 'scoped' }));

      const proxy = createScopeProxy(AsyncService, () => scope);
      const result = await proxy.fetchData('42');
      expect(result).toBe('data-42');
    });

    it('should work end-to-end with ContainerContext.scope()', async () => {
      class RequestId {
        constructor(public id: string) {}
      }

      class GreetingService {
        greet(name: string) {
          return `Hello, ${name}!`;
        }
      }

      const ctx = new ContainerContext('test');
      ctx.register(provide(GreetingService, () => new GreetingService()));
      ctx.register(provide(RequestId, () => new RequestId(`req-${Date.now()}`), { scope: 'scoped' }));

      await ctx.start();

      // Each scope gets its own RequestId
      const results = await Promise.all([
        ctx.scope((scope) => {
          const rid = scope.get(RequestId);
          const svc = scope.get(GreetingService);
          return { requestId: rid.id, greeting: svc.greet('Alice') };
        }),
        ctx.scope((scope) => {
          const rid = scope.get(RequestId);
          return { requestId: rid.id };
        }),
      ]);

      // Both scopes should have different request IDs (or same if created at same ms)
      expect(results[0].greeting).toBe('Hello, Alice!');
      expect(results[0].requestId).toBeDefined();
      expect(results[1].requestId).toBeDefined();

      await ctx.close();
    });

    it('should allow singleton to use scoped dependency via scope proxy', async () => {
      class RequestScope {
        constructor(public id: string) {}
      }

      class SingletonService {
        constructor(private requestScope: RequestScope) {}
        getCurrentRequestId() {
          return this.requestScope.id;
        }
      }

      const ctx = new ContainerContext('test');
      ctx.register(provide(RequestScope, () => new RequestScope(`req-${Math.random()}`), { scope: 'scoped' }));
      ctx.register(
        provide(SingletonService, (resolve) => new SingletonService(resolve(RequestScope)), {
          deps: [RequestScope],
        }),
      );

      await ctx.start();

      // The singleton gets a scope proxy for RequestScope
      const svc = ctx.get(SingletonService);

      // Inside a scope, the proxy should resolve to that scope's RequestScope
      const id1 = await ctx.scope(() => svc.getCurrentRequestId());
      const id2 = await ctx.scope(() => svc.getCurrentRequestId());

      // Each scope creates a fresh RequestScope, so IDs should differ
      // (extremely unlikely to collide with Math.random())
      expect(id1).toBeDefined();
      expect(id2).toBeDefined();
      expect(id1).not.toBe(id2);

      await ctx.close();
    });

    it('scope proxy is not the same reference as the scoped instance', async () => {
      class ScopedService {
        value = 'hello';
      }

      class SingletonHolder {
        constructor(public scopedRef: ScopedService) {}
      }

      const ctx = new ContainerContext('test');
      ctx.register(provide(ScopedService, () => new ScopedService(), { scope: 'scoped' }));
      ctx.register(
        provide(SingletonHolder, (resolve) => new SingletonHolder(resolve(ScopedService)), {
          deps: [ScopedService],
        }),
      );

      await ctx.start();

      const holder = ctx.get(SingletonHolder);

      await ctx.scope((scope) => {
        const directInstance = scope.get(ScopedService);
        // The holder has a proxy, not the direct instance
        // But both should see the same value
        expect(holder.scopedRef.value).toBe(directInstance.value);
        // They are not the same object reference (one is a proxy)
        expect(holder.scopedRef).not.toBe(directInstance);
      });

      await ctx.close();
    });
  });

  describe('tracing proxy', () => {
    it('should trace sync method calls', () => {
      const traces: Array<{ method: string | symbol; duration: number }> = [];
      const sink: TraceSink = {
        emit(data) {
          traces.push({ method: data.method, duration: data.duration });
        },
      };

      class Calculator {
        add(a: number, b: number) {
          return a + b;
        }
      }

      const calc = new Calculator();
      const traced = createTracingProxy(calc, { token: 'Calculator' }, sink);

      const result = traced.add(2, 3);
      expect(result).toBe(5);
      expect(traces).toHaveLength(1);
      expect(traces[0].method).toBe('add');
      expect(traces[0].duration).toBeGreaterThanOrEqual(0);
    });

    it('should trace async method calls', async () => {
      const traces: Array<{ method: string | symbol; error?: unknown }> = [];
      const sink: TraceSink = {
        emit(data) {
          traces.push({ method: data.method, error: data.error });
        },
      };

      class AsyncService {
        async fetch() {
          return 'data';
        }
      }

      const service = new AsyncService();
      const traced = createTracingProxy(service, { token: 'AsyncService' }, sink);

      const result = await traced.fetch();
      expect(result).toBe('data');
      expect(traces).toHaveLength(1);
      expect(traces[0].method).toBe('fetch');
      expect(traces[0].error).toBeUndefined();
    });

    it('should trace errors', () => {
      const traces: Array<{ error?: unknown }> = [];
      const sink: TraceSink = {
        emit(data) {
          traces.push({ error: data.error });
        },
      };

      class Faulty {
        fail() {
          throw new Error('boom');
        }
      }

      const faulty = new Faulty();
      const traced = createTracingProxy(faulty, { token: 'Faulty' }, sink);

      expect(() => traced.fail()).toThrow('boom');
      expect(traces).toHaveLength(1);
      expect(traces[0].error).toBeDefined();
    });

    it('should pass through non-function properties', () => {
      const sink: TraceSink = { emit() {} };

      class Config {
        port = 3000;
        host = 'localhost';
      }

      const config = new Config();
      const traced = createTracingProxy(config, { token: 'Config' }, sink);

      expect(traced.port).toBe(3000);
      expect(traced.host).toBe('localhost');
    });
  });
});
