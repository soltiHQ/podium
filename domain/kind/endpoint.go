package kind

import (
	"github.com/soltiHQ/control-plane/domain"
)

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
		return "", domain.ErrUnknownEndpointType
	}
}
