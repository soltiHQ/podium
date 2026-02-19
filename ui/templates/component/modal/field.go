package modal

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/a-h/templ"
)

// Field describes a single form field inside an Edit modal.
type Field struct {
	ID          string // input name and x-model key (matches JSON field name)
	Label       string // human-readable label
	Value       string // initial value
	Placeholder string
	Required    bool
	Disabled    bool // rendered as read-only, excluded from submission
}

// AsyncSelect describes a multi-select field whose options are loaded from an API.
//
// For flat string arrays (e.g. permissions):
//
//	Endpoint returns { "items": ["users:get", "users:edit"] }
//	ValueKey and LabelKey are empty.
//
// For object arrays (e.g. roles):
//
//	Endpoint returns { "items": [{"id": "role-admin", "name": "admin"}, ...] }
//	ValueKey = "id", LabelKey = "name".
type AsyncSelect struct {
	ID       string // JSON field name (e.g. "permissions", "role_ids")
	Label    string
	Endpoint string   // GET URL that returns { "items": [...] }
	Selected []string // currently selected values
	ValueKey string   // object field for value (empty = items are strings)
	LabelKey string   // object field for display label (empty = same as value)
}

// disabledAttrs returns templ.Attributes for a disabled input.
func disabledAttrs() templ.Attributes {
	return templ.Attributes{"disabled": "true"}
}

// modelAttrs returns templ.Attributes with x-model bound to the field ID.
func modelAttrs(id string) templ.Attributes {
	return templ.Attributes{"x-model": id}
}

// editFormData builds the Alpine x-data expression including async select state.
//
//	{ subject: "admin", ..., permissions: ["users:get"], permissions_opts: [], loading: true, submitting: false }
func editFormData(fields []Field, selects []AsyncSelect) string {
	parts := make([]string, 0, len(fields)+len(selects)*2+2)

	for _, f := range fields {
		if f.Disabled {
			continue
		}
		v, _ := json.Marshal(f.Value)
		parts = append(parts, fmt.Sprintf("%s: %s", f.ID, string(v)))
	}

	for _, s := range selects {
		v, _ := json.Marshal(s.Selected)
		parts = append(parts, fmt.Sprintf("%s: %s", s.ID, string(v)))
		parts = append(parts, fmt.Sprintf("%s_opts: []", s.ID))
		parts = append(parts, fmt.Sprintf("%s_open: false", s.ID))
		if s.ValueKey != "" {
			parts = append(parts, fmt.Sprintf("%s_labels: {}", s.ID))
		}
	}

	parts = append(parts, "loading: true")
	parts = append(parts, "submitting: false")
	return "{ " + strings.Join(parts, ", ") + " }"
}

// initExpr builds the Alpine init expression that fetches async select options.
func initExpr(selects []AsyncSelect) string {
	if len(selects) == 0 {
		return "loading = false"
	}

	fetches := make([]string, 0, len(selects))
	for _, s := range selects {
		if s.ValueKey != "" {
			// Object items: extract values and build label map.
			fetches = append(fetches, fmt.Sprintf(
				`fetch('%s').then(r => r.json()).then(d => {`+
					` const items = d.items || [];`+
					` %s_opts = items.map(x => x.%s);`+
					` %s_labels = Object.fromEntries(items.map(x => [x.%s, x.%s]));`+
					` })`,
				s.Endpoint, s.ID, s.ValueKey, s.ID, s.ValueKey, s.LabelKey,
			))
		} else {
			// Flat string items.
			fetches = append(fetches, fmt.Sprintf(
				`fetch('%s').then(r => r.json()).then(d => { %s_opts = d.items || [] })`,
				s.Endpoint, s.ID,
			))
		}
	}

	return fmt.Sprintf(
		`Promise.all([%s]).finally(() => loading = false)`,
		strings.Join(fetches, ", "),
	)
}

// editableFieldIDs returns the IDs of editable (non-disabled) fields.
func editableFieldIDs(fields []Field) []string {
	ids := make([]string, 0, len(fields))
	for _, f := range fields {
		if !f.Disabled {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// passwordFormData returns the Alpine x-data expression for the password modal.
func passwordFormData() string {
	return `{ password: "", confirm: "", error: "", submitting: false }`
}

// passwordSubmitExpr returns the Alpine submit expression for the password modal.
// It validates that both fields match, then sends a POST with the password.
func passwordSubmitExpr(action string) string {
	return fmt.Sprintf(
		`if (password !== confirm) { error = "Passwords do not match"; return; }
		if (password.length === 0) { error = "Password cannot be empty"; return; }
		error = "";
		submitting = true;
		fetch('%s', {
			method: 'POST',
			headers: {
				'Content-Type': 'application/json',
				'HX-Request': 'true'
			},
			body: JSON.stringify({ password: password })
		}).then(r => {
			if (r.ok) {
				password = ""; confirm = ""; error = "";
				show = false;
				htmx.trigger(document.body, 'user_update');
			} else {
				error = "Failed to set password";
			}
		}).catch(() => {
			error = "Network error";
		}).finally(() => submitting = false)`,
		action,
	)
}

// submitExpr builds the Alpine x-on:submit.prevent expression
// that collects editable fields and async selects into a JSON body and sends a PUT.
func submitExpr(action string, fields []Field, selects []AsyncSelect) string {
	return formSubmitExpr("PUT", action, fields, selects)
}

// createSubmitExpr builds the same expression but sends a POST.
func createSubmitExpr(action string, fields []Field, selects []AsyncSelect) string {
	return formSubmitExpr("POST", action, fields, selects)
}

// formSubmitExpr builds the Alpine submit expression with a configurable HTTP method.
func formSubmitExpr(method, action string, fields []Field, selects []AsyncSelect) string {
	ids := editableFieldIDs(fields)
	pairs := make([]string, 0, len(ids)+len(selects))
	for _, id := range ids {
		pairs = append(pairs, fmt.Sprintf("%s: %s", id, id))
	}
	for _, s := range selects {
		pairs = append(pairs, fmt.Sprintf("%s: %s", s.ID, s.ID))
	}
	jsonObj := "{ " + strings.Join(pairs, ", ") + " }"

	return fmt.Sprintf(
		`submitting = true;
		fetch('%s', {
			method: '%s',
			headers: {
				'Content-Type': 'application/json',
				'HX-Request': 'true'
			},
			body: JSON.stringify(%s)
		}).then(r => {
			if (r.ok) {
				const redirect = r.headers.get('HX-Redirect');
				if (redirect) { window.location.href = redirect; return; }
				show = false;
				htmx.trigger(document.body, 'user_update');
			}
		}).finally(() => submitting = false)`,
		action, method, jsonObj,
	)
}
