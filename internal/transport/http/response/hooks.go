package response

import "github.com/a-h/templ"

// ErrorPageRenderer produces a full-page HTML error view for the given status code, title, and detail message.
var ErrorPageRenderer func(code int, title, message string) templ.Component

func renderErrorPage(code int, title, message string) templ.Component {
	if ErrorPageRenderer == nil {
		return nil
	}
	return ErrorPageRenderer(code, title, message)
}
