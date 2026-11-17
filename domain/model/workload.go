package model

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Workload is the workload a spec runs: the type it names (API version and
// kind), the fingerprint of the schema its body was validated against, and the
// body. The body is JSON-compatible data; the spec service validates it against
// the catalog schema of the fingerprint.
type Workload struct {
	APIVersion        string
	Kind              string
	SchemaFingerprint string
	Body              map[string]any
}

// GVK returns the workload type, with the schema fingerprint. The API version
// must be "group/version", with both parts set, and the kind must be set.
func (w Workload) GVK() (GroupVersionKind, error) {
	group, version, ok := strings.Cut(w.APIVersion, "/")
	if !ok || group == "" || version == "" || strings.Contains(version, "/") {
		return GroupVersionKind{}, fmt.Errorf("workload: the API version %q is not group/version", w.APIVersion)
	}
	if w.Kind == "" {
		return GroupVersionKind{}, fmt.Errorf("workload: the kind is empty")
	}
	return GroupVersionKind{Group: group, Version: version, Kind: w.Kind, SchemaFingerprint: w.SchemaFingerprint}, nil
}

// IsZero reports whether no workload is set.
func (w Workload) IsZero() bool {
	return w.APIVersion == "" && w.Kind == "" && w.SchemaFingerprint == "" && len(w.Body) == 0
}

// Clone returns a deep copy.
func (w Workload) Clone() Workload {
	out := w
	if w.Body != nil {
		out.Body = make(map[string]any, len(w.Body))
		for k, v := range w.Body {
			out.Body[k] = cloneConfigValue(v)
		}
	}
	return out
}

// Equal reports whether two workloads have the same type, schema and body. The
// body is compared deeply; a nil body equals an empty one, and a number equals
// the same number however it is held (json.Number, float64, an integer).
func (w Workload) Equal(o Workload) bool {
	return w.APIVersion == o.APIVersion && w.Kind == o.Kind && w.SchemaFingerprint == o.SchemaFingerprint &&
		valueEqual(w.Body, o.Body)
}

func valueEqual(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			bv, ok := y[k]
			if !ok || !valueEqual(v, bv) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !valueEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	if an, ok := numberString(a); ok {
		bn, ok := numberString(b)
		return ok && an == bn
	}
	return reflect.DeepEqual(a, b)
}

// numberString returns the canonical decimal text of a JSON number held as
// json.Number, a float or an integer.
func numberString(v any) (string, bool) {
	switch n := v.(type) {
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return fmt.Sprintf("%v", f), true
		}
		return string(n), true
	case float64:
		return fmt.Sprintf("%v", n), true
	case float32:
		return fmt.Sprintf("%v", float64(n)), true
	case int:
		return fmt.Sprintf("%v", float64(n)), true
	case int32:
		return fmt.Sprintf("%v", float64(n)), true
	case int64:
		return fmt.Sprintf("%v", float64(n)), true
	}
	return "", false
}
