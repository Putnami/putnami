package workspace

import (
	"os"
	"sync/atomic"
)

// ConfigReader returns the bytes of the configuration file at path, as
// os.ReadFile does. It may answer bytes it recorded for the same unchanged
// file rather than read it; an error it returns is the error a read returns.
type ConfigReader func(path string) ([]byte, error)

// configReader is the reader the configuration loaders go through; nil reads
// every file.
var configReader atomic.Pointer[ConfigReader]

// UseConfigReader makes read the reader through which the workspace
// configuration, scope files and project configurations are read, and returns
// a function that restores the previous one. A nil read reads every file.
// The bytes it answers are decoded as read bytes are, so a configuration
// loads the same whichever reader answered them.
func UseConfigReader(read ConfigReader) (restore func()) {
	var next *ConfigReader
	if read != nil {
		next = &read
	}
	previous := configReader.Swap(next)
	return func() { configReader.CompareAndSwap(next, previous) }
}

// readConfigFile reads the configuration file at path through the current
// ConfigReader.
func readConfigFile(path string) ([]byte, error) {
	if read := configReader.Load(); read != nil {
		return (*read)(path)
	}
	return os.ReadFile(path) //nolint:gosec // a configuration file of the workspace
}
