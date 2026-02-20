package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// kvRow is a single key-value pair for the labels editor.
type kvRow struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// kvEditorData builds the Alpine x-data expression for the KV editor.
//
//	{ rows: [{key:"env",value:"prod"}, ...], submitting: false }
func kvEditorData(labels map[string]string) string {
	rows := make([]kvRow, 0, len(labels))
	for k, v := range labels {
		rows = append(rows, kvRow{Key: k, Value: v})
	}

	b, _ := json.Marshal(rows)
	return fmt.Sprintf("{ rows: %s, submitting: false }", string(b))
}

// kvSubmitExpr builds the Alpine submit expression that collects rows into
// a flat { key: value } object, sends PUT, then triggers the given event.
func kvSubmitExpr(action string, triggerEvent string) string {
	return fmt.Sprintf(
		`submitting = true;
		const body = {};
		for (const r of rows) {
			const k = r.key.trim();
			if (k) body[k] = r.value;
		}
		fetch('%s', {
			method: 'PUT',
			headers: {
				'Content-Type': 'application/json',
				'HX-Request': 'true'
			},
			body: JSON.stringify(body)
		}).then(r => {
			if (r.ok) {
				show = false;
				htmx.trigger(document.body, '%s');
			}
		}).catch(() => {
		}).finally(() => submitting = false)`,
		action,
		strings.ReplaceAll(triggerEvent, "'", "\\'"),
	)
}
