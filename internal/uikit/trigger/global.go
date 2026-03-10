package trigger

import "context"

var defaultHub *Hub

// InitHub creates the package-level hub.
func InitHub() { defaultHub = NewHub() }

// CloseHub shuts down the package-level hub, disconnecting all SSE clients.
func CloseHub() {
	if defaultHub != nil {
		defaultHub.Close()
	}
}

// Notify broadcasts an event to all SSE clients via the package-level hub.
func Notify(event string) {
	if defaultHub != nil {
		defaultHub.Notify(event)
	}
}

// Record appends an event to the package-level hub.
// Issue-classified events are stored in both activity and issues buffers.
func Record(kind string, payload EventPayload) {
	if defaultHub != nil {
		defaultHub.Record(kind, payload)
	}
}

// RecentEvents returns the last n activity events from the package-level hub.
func RecentEvents(n int) []EventRecord {
	if defaultHub != nil {
		return defaultHub.RecentEvents(n)
	}
	return nil
}

// RecentIssues returns the last n issues from the package-level hub.
func RecentIssues(n int) []EventRecord {
	if defaultHub != nil {
		return defaultHub.RecentIssues(n)
	}
	return nil
}

// DeleteIssues removes all issues matching kind and entity ID from the package-level hub.
func DeleteIssues(kind, id string) int {
	if defaultHub != nil {
		return defaultHub.DeleteIssues(kind, id)
	}
	return 0
}

// Subscribe registers a listener on the package-level hub.
func Subscribe(ctx context.Context) <-chan string {
	if defaultHub != nil {
		return defaultHub.Subscribe(ctx)
	}

	ch := make(chan string)
	close(ch)
	return ch
}
