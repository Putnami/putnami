# @example/go-items-consumer

A TypeScript consumer of the Go Items provider: the Go-to-TypeScript cell of the cross-language REST JSON matrix.

It calls the provider only through the generated TypeScript client in `../clients/ts`, and its integration tests run against a live provider.

## Commands

```bash
putnami test @example/go-items-consumer  # start the provider and run the integration tests
```

## Documentation

The [service-to-service sample README](../README.md) explains the provider, the generated clients and the consumers.
