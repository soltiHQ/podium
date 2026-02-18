package form

import "github.com/a-h/templ"

// mergeAttrs flattens a variadic list of templ.Attributes into one map.
func mergeAttrs(attrs []templ.Attributes) templ.Attributes {
	if len(attrs) == 0 {
		return nil
	}
	out := templ.Attributes{}
	for _, a := range attrs {
		for k, v := range a {
			out[k] = v
		}
	}
	return out
}
