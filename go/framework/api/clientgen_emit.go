package api

import (
	"fmt"
	"os"
	"path/filepath"
)

// GoClientFile is the one file a Go client emission writes in its output
// directory.
const GoClientFile = "client.gen.go"

// EmitGoClientOptions configure [EmitGoClient].
type EmitGoClientOptions struct {
	// PackageName is the Go package of the emitted file. Default "client".
	PackageName string
	// ClientName is the generated client type name. Default "Client".
	ClientName string
	// OmitOperations names operations, by operationId, that the client leaves
	// out. A name the contract does not declare fails the emission.
	OmitOperations []string
}

// EmitGoClientResult reports what [EmitGoClient] did.
type EmitGoClientResult struct {
	// Written is true when the emission wrote client.gen.go. It is false when
	// the contract declares no operation.
	Written bool
	// Omitted lists, in contract order, the operations OmitOperations left out.
	Omitted []string
}

// EmitGoClient generates a Go client from a provider contract and writes it to
// <outputDir>/client.gen.go. It writes nothing else: no manifest, no go.mod
// and no putnami.json.
//
// The contract must declare a document-level x-putnami-client. Each
// operation's x-putnami-client producer becomes that operation's design
// attribution in the generated client. A contract that declares no operation
// removes an earlier client.gen.go, so a provider that drops its last route
// leaves no stale client behind.
func EmitGoClient(contractJSON []byte, outputDir string, opts EmitGoClientOptions) (EmitGoClientResult, error) {
	spec, err := ReadOpenAPISpec(contractJSON)
	if err != nil {
		return EmitGoClientResult{}, fmt.Errorf("read contract: %w", err)
	}
	if spec.Contract == nil {
		return EmitGoClientResult{}, fmt.Errorf("the contract declares no x-putnami-client, so it names no client to generate")
	}
	kept, omitted, err := omitOperations(spec, opts.OmitOperations)
	if err != nil {
		return EmitGoClientResult{}, err
	}
	result := EmitGoClientResult{Omitted: omittedOperationIDs(omitted)}
	target := filepath.Join(outputDir, GoClientFile)
	if !declaresOperations(spec) {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return result, fmt.Errorf("remove %s: %w", target, err)
		}
		return result, nil
	}

	var design *ClientDesignOptions
	if carriesProducer(kept) {
		design = &ClientDesignOptions{}
	}
	// GenerateClientFromIR returns gofmt-clean source.
	source, err := GenerateClientFromIR(spec, ClientGenOptions{
		PackageName:    orDefault(opts.PackageName, "client"),
		ClientName:     orDefault(opts.ClientName, "Client"),
		Design:         design,
		OmitOperations: opts.OmitOperations,
	})
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return result, err
	}
	if err := os.WriteFile(target, []byte(source), 0o600); err != nil {
		return result, err
	}
	result.Written = true
	return result, nil
}

func declaresOperations(spec SpecIR) bool {
	for _, service := range spec.Services {
		if len(service.Methods) > 0 {
			return true
		}
	}
	return false
}

// carriesProducer reports whether any operation the client keeps names its
// producer. A client embeds its design descriptor only then.
func carriesProducer(spec SpecIR) bool {
	for _, service := range spec.Services {
		for _, method := range service.Methods {
			if operationProducer(method) != nil {
				return true
			}
		}
	}
	return false
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
