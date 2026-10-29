package agent

import (
	"encoding/json"
	"fmt"
	"sort"
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
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rows = append(rows, kvRow{Key: k, Value: labels[k]})
	}

	b, _ := json.Marshal(rows)
	return fmt.Sprintf("{ rows: %s, submitting: false, error: '' }", string(b))
}

// kvSubmitExpr builds the Alpine submit expression that collects rows into
// a flat { key: value } object, sends PUT, then triggers the given event.
func kvSubmitExpr(action string, triggerEvent string) string {
	return fmt.Sprintf(
		`if(submitting) return;
 error='';
 const body = Object.create(null);
		for (const r of rows) {
			const k = r.key.trim();
			if (!k && r.value) {error='A label needs a key.';return;}
 if (Object.hasOwn(body,k)) {error='Duplicate label: '+k;return;}
 if (k) body[k] = r.value;
		}
		submitting=true;
 fetch('%s', {
			method: 'PUT',
			headers: {
				'Content-Type': 'application/json', 'Accept':'application/json',
				'HX-Request': 'true'
			},
			body: JSON.stringify(body)
		}).then(async r => {
			if (r.status === 401) { window.location.href='/login'; return; }
 if (r.ok) {
				show = false;
				htmx.trigger(document.body, '%s');
 } else { try {const d=await r.json();error=d.message || 'Labels could not be saved.';} catch {error='Labels could not be saved. Retry when the server is available.';} }
 }).catch(() => {error='Connection failed. Your changes are retained.';}).finally(() => submitting = false)`,
		action,
		strings.ReplaceAll(triggerEvent, "'", "\\'"),
	)
}
