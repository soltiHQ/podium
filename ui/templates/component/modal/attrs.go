package modal

import "github.com/a-h/templ"

// xOn returns a dynamic Alpine x-on attribute that listens for
// a custom DOM event "modal:open:<name>" and sets show = true.
//
// Usage in templ:  { xOn(name)... }
func xOn(name string) templ.Attributes {
	return templ.Attributes{
		"x-on:modal:open:" + name + ".window": "show = true",
	}
}

// OpenEvent returns the Alpine $dispatch expression to open a modal
// with the given name.  Use this as the value of @click on a trigger button.
//
//	Example:  x-on:click={ modal.OpenEvent("delete-user") }
func OpenEvent(name string) string {
	return "$dispatch('modal:open:" + name + "')"
}

// hxMethod returns a templ.Attributes map with the correct hx-* attribute
// for the given HTTP method.
//
// Usage in templ:  <form { hxMethod(method, url)... }>
func hxMethod(m Method, url string) templ.Attributes {
	key := "hx-post"
	switch m {
	case MethodDelete:
		key = "hx-delete"
	case MethodPut:
		key = "hx-put"
	case MethodPatch:
		key = "hx-patch"
	}
	return templ.Attributes{key: url}
}

// xModel returns a templ.Attributes map with x-model bound to the given field name.
//
// Usage in templ:  <input { xModel(f.ID)... } />
func xModel(field string) templ.Attributes {
	return templ.Attributes{"x-model": field}
}
