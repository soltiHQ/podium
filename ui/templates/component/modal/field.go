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

// editFormData builds the Alpine x-data expression.
//
//	{ subject: "admin", name: "Admin", email: "", submitting: false }
func editFormData(fields []Field) string {
	parts := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		if f.Disabled {
			continue
		}
		v, _ := json.Marshal(f.Value)
		parts = append(parts, fmt.Sprintf("%s: %s", f.ID, string(v)))
	}
	parts = append(parts, "submitting: false")
	return "{ " + strings.Join(parts, ", ") + " }"
}

// disabledAttrs returns templ.Attributes for a disabled input.
func disabledAttrs() templ.Attributes {
	return templ.Attributes{"disabled": "true"}
}

// modelAttrs returns templ.Attributes with x-model bound to the field ID.
func modelAttrs(id string) templ.Attributes {
	return templ.Attributes{"x-model": id}
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

// submitExpr builds the Alpine x-on:submit.prevent expression
// that collects editable fields into a JSON body and sends a PUT.
func submitExpr(action string, fields []Field) string {
	// Build explicit JSON object: { subject: subject, name: name, email: email }
	ids := editableFieldIDs(fields)
	pairs := make([]string, 0, len(ids))
	for _, id := range ids {
		pairs = append(pairs, fmt.Sprintf("%s: %s", id, id))
	}
	jsonObj := "{ " + strings.Join(pairs, ", ") + " }"

	return fmt.Sprintf(
		`submitting = true;
		fetch('%s', {
			method: 'PUT',
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
		action, jsonObj,
	)
}
