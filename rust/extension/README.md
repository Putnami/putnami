# @putnami/rust

`@putnami/rust` runs the crates of a Cargo workspace as Putnami projects:
`putnami lint`, `putnami test` and `putnami build` run each crate's cargo
steps with Putnami's runner and cache, without changing the build.

It is the reference stack extension: a manifest, the Rust stack profile
(`profile.json`) and the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md). The crates and their
edges come from the Cargo detector of `putnami observe`.

See [the reference](doc/01-rust.md).
