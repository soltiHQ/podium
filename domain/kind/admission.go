package kind

// AdmissionStrategy defines how the controller admits a new task into a slot.
type AdmissionStrategy string

// Admission strategies.
// https://github.com/soltiHQ/taskvisor/blob/main/src/controller/admission.rs
const (
	AdmissionDropIfRunning AdmissionStrategy = "dropIfRunning"
	AdmissionReplace       AdmissionStrategy = "replace"
	AdmissionQueue         AdmissionStrategy = "queue"
)
