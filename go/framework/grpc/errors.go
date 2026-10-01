package grpc

import "go.putnami.dev/errors"

// Error codes for the grpc package.
const (
	CodeGrpcListen errors.Code = "grpc.listen"
	CodeGrpcScope  errors.Code = "grpc.scope"
	CodeGrpcPanic  errors.Code = "grpc.panic"
)
