package enum

// JitterStrategy controls how random jitter is applied to backoff delays.
type JitterStrategy string

const (
	JitterNone         JitterStrategy = "none"
	JitterFull         JitterStrategy = "full"
	JitterEqual        JitterStrategy = "equal"
	JitterDecorrelated JitterStrategy = "decorrelated"
)
