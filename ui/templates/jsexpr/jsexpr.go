// Package jsexpr renders Go values as JavaScript source for the Alpine and
// HTMX attributes that templ components assemble as strings (x-data, x-on:*,
// x-init, hx-trigger filters).
//
// templ HTML-escapes an attribute value, but the browser decodes it again
// before Alpine or HTMX evaluates it as code. A dynamic value pasted between
// quotes, as in fetch('%s'), can therefore end the string and inject
// statements. Encode every such value with [String] and paste the result
// without surrounding quotes:
//
//	fmt.Sprintf("fetch(%s, {method: 'PUT'})", jsexpr.String(url))
//
// Slices, maps and structs are embedded with encoding/json.Marshal, which
// escapes the same characters.
//
// Names that are emitted as JavaScript identifiers (Alpine data keys, x-model
// targets) cannot be quoted. They must be compile-time constants, never data.
package jsexpr

import "encoding/json"

// String returns s as a double-quoted JavaScript string literal.
//
// The literal cannot end early or reach the surrounding HTML: encoding/json
// escapes quotes, backslashes, control characters and invalid UTF-8, plus
// <, > and & (so "</script>" stays inert) and U+2028 and U+2029, which end a
// line in JavaScript. A single quote is left alone: it is inert inside a
// double-quoted literal.
func String(s string) string {
	// Encoding a string cannot fail.
	b, _ := json.Marshal(s)
	return string(b)
}
