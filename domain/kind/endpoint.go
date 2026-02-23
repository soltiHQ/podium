package kind

import "errors"

// ErrUnknownEndpointType indicates an unrecognised endpoint type value.
var ErrUnknownEndpointType = errors.New("unknown endpoint type")

// EndpointType describes the transport protocol an agent exposes.
type EndpointType string

const (
	EndpointGRPC EndpointType = "grpc"
	EndpointHTTP EndpointType = "http"
)

// EndpointTypeFromInt maps the proto/JSON integer enum to EndpointType.
//
//	0 → gRPC, 1 → HTTP.
func EndpointTypeFromInt(v int) (EndpointType, error) {
	switch v {
	case 0:
		return EndpointGRPC, nil
	case 1:
		return EndpointHTTP, nil
	default:
		return "", ErrUnknownEndpointType
	}
}
