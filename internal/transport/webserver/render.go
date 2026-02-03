package webserver

import (
	"fmt"
	"html/template"
	"net/http"
	"sync"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/ui"
)

var (
	globalTemplates     *template.Template
	globalTemplatesOnce sync.Once
	globalTemplatesErr  error
)

// renderer handles template rendering with development/production modes
type renderer struct {
	tmpl   *template.Template
	logger zerolog.Logger

	devMode bool
}

func newRenderer(logger zerolog.Logger, devMode bool) (*renderer, error) {
	if devMode {
		logger.Info().Msg("renderer: development mode enabled (templates will hot-reload)")
		return &renderer{
			logger:  logger,
			devMode: true,
		}, nil
	}
	globalTemplatesOnce.Do(func() {
		logger.Info().Msg("renderer: parsing templates (one-time initialization)")
		globalTemplates, globalTemplatesErr = template.ParseFS(
			ui.Templates,
			"templates/**/*.html",
		)
		if globalTemplatesErr == nil {
			logger.Info().
				Int("templates", len(globalTemplates.Templates())).
				Msg("renderer: templates parsed successfully")
		}
	})

	if globalTemplatesErr != nil {
		return nil, fmt.Errorf("failed to parse templates: %w", globalTemplatesErr)
	}
	return &renderer{
		tmpl:    globalTemplates,
		logger:  logger,
		devMode: false,
	}, nil
}

// Render writes the output of a parsed template to the provided http.ResponseWriter.
func (r *renderer) Render(w http.ResponseWriter, name string, data any) error {
	var tmpl *template.Template

	if r.devMode {
		var err error
		tmpl, err = template.ParseFS(ui.Templates, "templates/**/*.html")
		if err != nil {
			r.logger.Error().Err(err).Msg("renderer: failed to hot-reload templates")
			http.Error(w, "Template parsing error", http.StatusInternalServerError)
			return err
		}
		r.logger.Debug().Msg("renderer: templates hot-reloaded")
	} else {
		tmpl = r.tmpl
	}

	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		r.logger.Error().Err(err).
			Str("template", name).
			Msg("renderer: template execution failed")
		return err
	}
	return nil
}
