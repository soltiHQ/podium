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
//	1 → gRPC, 2 → HTTP. 0 (UNSPECIFIED) is rejected.
//
// Values match solti.discover.v1.EndpointType:
//
//	ENDPOINT_TYPE_UNSPECIFIED = 0
//	ENDPOINT_TYPE_GRPC        = 1
//	ENDPOINT_TYPE_HTTP        = 2
func EndpointTypeFromInt(v int) (EndpointType, error) {
	switch v {
	case 1:
		return EndpointGRPC, nil
	case 2:
		return EndpointHTTP, nil
	default:
		return "", domain.ErrUnknownEndpointType
	}
}
