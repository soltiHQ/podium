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

// Subscribe registers a listener on the package-level hub.
func Subscribe(ctx context.Context) <-chan string {
	if defaultHub != nil {
		return defaultHub.Subscribe(ctx)
	}
	
	ch := make(chan string)
	close(ch)
	return ch
}
