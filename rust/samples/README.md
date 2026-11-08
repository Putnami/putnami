# @putnami/rust sample

Two crates: `application` depends on `library` through a path dependency. Each
is its own Cargo build, with its own `Cargo.lock` and `target/`.

These projects carry the `e2e` tag, so the default gate skips them. To run
them, you need no Rust: the first task installs the Rust the extension pins,
and the `test` task installs cargo-nextest and cargo-llvm-cov. rustc still
needs a C linker on the machine. A Rust on `PATH` with no llvm-tools runs,
and `test` warns and writes no coverage report.

```bash
putnami projects sync
putnami lint,test,build --tag e2e --projects /rust/samples/library,/rust/samples/application
```
