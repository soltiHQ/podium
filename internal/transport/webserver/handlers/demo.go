package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/ui/pages"
)

type Agent struct {
	ID        string    `json:"id"`
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	Platform  string    `json:"platform"`
	Endpoint  string    `json:"endpoint"`
	Uptime    int64     `json:"uptime"`
	UpdatedAt time.Time `json:"updated_at"`
}

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

	// Запрос к собственному API
	resp, err := http.Get("http://localhost:8082/v1/agents")
	if err != nil {
		d.logger.Error().Err(err).Msg("failed to fetch agents")
		fmt.Fprint(w, `<div class="text-red-600">Failed to load agents</div>`)
		return
	}
	defer resp.Body.Close()

	var agents []Agent
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
		d.logger.Error().Err(err).Msg("failed to decode agents")
		fmt.Fprint(w, `<div class="text-red-600">Failed to parse agents</div>`)
		return
	}

	// Рендерим каждую карточку
	for _, agent := range agents {
		_ = pages.AgentCard(
			agent.ID,
			agent.OS,
			agent.Arch,
			agent.Platform,
			agent.Endpoint,
			agent.Uptime,
		).Render(r.Context(), w)
	}
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
