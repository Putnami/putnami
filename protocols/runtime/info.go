package runtime

// RuntimeABIVersion is the executable-handshake ABI understood by this
// release. It is distinct from the job event protocol: the handshake describes
// the executable before the CLI trusts it to emit any job events.
const RuntimeABIVersion = 1

// Info is the strict document returned by an extension executable for
// `__putnami runtime-info`.
type Info struct {
	// Extension is the extension name the executable answers for
	// ("@putnami/go"). It must match the name the loaded manifest declares.
	Extension string `json:"extension"`
	// Version is the executable's own release version.
	Version string `json:"version"`
	// Platform is the host triple the binary was built for ("darwin/arm64").
	Platform string `json:"platform"`
	// CLIContract is the extension contract generation the executable
	// implements. The CLI refuses to load an executable below the contract it
	// requires, rather than adapting it.
	CLIContract int `json:"cliContract"`
	// RuntimeProtocol is the highest runtime event protocol version the
	// executable can emit. It is the executable's ceiling; what it actually
	// stamps on a given stream is negotiated per invocation (negotiation.go).
	RuntimeProtocol int `json:"runtimeProtocol"`
	// RuntimeABI is the handshake ABI of this document itself
	// (RuntimeABIVersion). It is deliberately separate from RuntimeProtocol: the
	// handshake must be readable before the CLI trusts the executable to emit
	// any job event at all.
	RuntimeABI int `json:"runtimeABI"`
}
