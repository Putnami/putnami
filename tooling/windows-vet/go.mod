// The Windows compile gate is its own module and project so that its cache key
// holds the Go sources it vets and nothing else. It requires no workspace
// module: it reads the others through the go command, not through imports.
// Nothing publishes this module.
module go.putnami.dev/tooling/windowsvet

go 1.25.7
