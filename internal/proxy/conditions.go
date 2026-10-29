package proxy

import taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"

func conditionStatusString(status taskv1.ConditionStatus) string {
	switch status {
	case taskv1.ConditionStatus_CONDITION_STATUS_UNKNOWN:
		return "Unknown"
	case taskv1.ConditionStatus_CONDITION_STATUS_TRUE:
		return "True"
	case taskv1.ConditionStatus_CONDITION_STATUS_FALSE:
		return "False"
	case taskv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED:
		return ""
	default:
		return status.String() // Preserve an unrecognized numeric wire value.
	}
}
