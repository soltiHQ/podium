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

// Record appends an event to the package-level hub ring buffer.
func Record(kind string, payload EventPayload) {
	if defaultHub != nil {
		defaultHub.Record(kind, payload)
	}
}

// RecentEvents returns the last n events from the package-level hub.
func RecentEvents(n int) []EventRecord {
	if defaultHub != nil {
		return defaultHub.RecentEvents(n)
	}
	return nil
}

// RecentEventsOfKind returns the last n events matching any of the given kinds.
func RecentEventsOfKind(n int, kinds ...string) []EventRecord {
	if defaultHub != nil {
		return defaultHub.RecentEventsOfKind(n, kinds...)
	}
	return nil
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
