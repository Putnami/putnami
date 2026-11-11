package project

import "go.putnami.dev/sdk/extension/tscscript"

// TsConfigValue is one setting a tsconfig extends chain resolves, with the
// directory of the config file that declares it (tscscript.Value).
type TsConfigValue = tscscript.Value

// TsConfigChain holds the settings a tsconfig file resolves once its extends
// chain is applied (tscscript.Chain). The reader lives in the extension SDK,
// so the Nx import reads the outDir and rootDir the tsc build compiles with.
type TsConfigChain = tscscript.Chain

// ReadTsConfigChain reads the tsconfig at path and every config it extends
// (tscscript.ReadChain).
func ReadTsConfigChain(path string) TsConfigChain { return tscscript.ReadChain(path) }

// StripJSONC removes the comments and trailing commas of a JSONC document
// (tscscript.StripJSONC).
func StripJSONC(data []byte) []byte { return tscscript.StripJSONC(data) }
