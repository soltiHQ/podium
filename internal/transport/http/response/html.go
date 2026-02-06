package response

import (
	"context"
	"html"
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

func writeHTMLFragment(ctx context.Context, w http.ResponseWriter, status int, message string) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	msg := html.EscapeString(message)
	if rid, ok := transportctx.RequestID(ctx); ok {
		_, _ = w.Write([]byte(`<div class="error" data-request-id="` + html.EscapeString(rid) + `">` + msg + `</div>`))
		return nil
	}
	_, _ = w.Write([]byte(`<div class="error">` + msg + `</div>`))
	return nil
}

func writeHTMLPage(ctx context.Context, w http.ResponseWriter, status int, message string) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	msg := html.EscapeString(message)

	ridLine := ""
	if rid, ok := transportctx.RequestID(ctx); ok {
		ridLine = `<p style="opacity:.7">request_id: ` + html.EscapeString(rid) + `</p>`
	}

	// Minimal page. Later swap to templates renderer.
	body := `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Error</title></head>
<body>
  <h1>` + msg + `</h1>
  ` + ridLine + `
</body>
</html>`
	_, _ = w.Write([]byte(body))
	return nil
}
