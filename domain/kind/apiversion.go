package kind

// APIVersion describes the agent API version the control-plane should use
// when communicating with the agent.
type APIVersion uint8

const (
	APIVersionUnspecified APIVersion = iota
	APIVersionV1
)

// String returns the human-readable version label.
func (v APIVersion) String() string {
	switch v {
	case APIVersionV1:
		return "v1"
	default:
		return "unknown"
	}
}

// APIVersionFromString parses a version string into APIVersion.
func APIVersionFromString(s string) APIVersion {
	switch s {
	case "v1":
		return APIVersionV1
	default:
		return APIVersionUnspecified
	}
}

// APIVersionFromInt maps an integer (e.g. from proto) to APIVersion.
func APIVersionFromInt(v int) APIVersion {
	switch v {
	case 1:
		return APIVersionV1
	default:
		return APIVersionUnspecified
	}
}
