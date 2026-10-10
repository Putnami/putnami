package runtime

import "os"

// Environment returns the current application environment from the APP_ENV
// environment variable, defaulting to "local". It returns the same value as
// config.Environment.
func Environment() string {
	if env := os.Getenv("APP_ENV"); env != "" {
		return env
	}
	return "local"
}
