package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// Demo provides HTMX demo endpoints.
type Demo struct {
	logger zerolog.Logger
}

// NewDemo creates a new Demo handler.
func NewDemo(logger zerolog.Logger) *Demo {
	return &Demo{
		logger: logger.With().Str("handler", "demo").Logger(),
	}
}

// Status returns a status HTML fragment for HTMX.
func (d *Demo) Status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	html := `
		<div class="space-y-2">
			<div class="flex items-center gap-2">
				<div class="w-3 h-3 bg-green-500 rounded-full animate-pulse"></div>
				<span class="font-semibold text-green-700 dark:text-green-400">System Online</span>
			</div>
			<div class="text-sm text-gray-600 dark:text-gray-400">
				<div>CPU: 23%</div>
				<div>Memory: 1.2 GB / 8 GB</div>
				<div>Agents: 42 active</div>
			</div>
		</div>
	`

	fmt.Fprint(w, html)
}

// Time returns current server time as HTML fragment.
func (d *Demo) Time(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	now := time.Now().Format("2006-01-02 15:04:05 MST")
	html := fmt.Sprintf(`
		<div class="text-gray-900 dark:text-white">
			<strong>Server Time:</strong> %s
		</div>
	`, now)

	fmt.Fprint(w, html)
}
