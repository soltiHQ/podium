// Package attrs provides shared helpers for templ.Attributes.
package attrs

import "github.com/a-h/templ"

// Merge flattens a variadic list of templ.Attributes into one map.
// Later values overwrite earlier ones.
func Merge(lists []templ.Attributes) templ.Attributes {
	if len(lists) == 0 {
		return nil
	}
	out := templ.Attributes{}
	for _, a := range lists {
		for k, v := range a {
			out[k] = v
		}
	}
	return out
}
