package modal

import (
	"encoding/hex"

	"github.com/a-h/templ"
)

// xOn returns a dynamic Alpine x-on attribute that listens for
// the encoded custom DOM event for name and sets show = true.
//
// Usage in templ:  { xOn(name)... }
func xOn(name string) templ.Attributes {
	return templ.Attributes{
		"x-on:" + openEventName(name) + ".window": "show = true",
	}
}

// OpenEvent returns the Alpine $dispatch expression to open a modal
// with the given name.  Use this as the value of @click on a trigger button.
//
//	Example:  x-on:click={ modal.OpenEvent("delete-user") }
func OpenEvent(name string) string {
	return "$dispatch('" + openEventName(name) + "')"
}

// HTML lowercases attribute names. Encoding the complete UTF-8 identifier as
// lowercase hex gives listener and dispatch the same event without merging
// distinct case-sensitive task UIDs or inserting quotes into Alpine code.
func openEventName(name string) string {
	return "modal:open:" + hex.EncodeToString([]byte(name))
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

// xForOpts returns the Alpine x-for attribute for iterating async select options.
//
//	<template { xForOpts("permissions")... }>
//	  → x-for="opt in permissions_opts"
func xForOpts(id string) templ.Attributes {
	return templ.Attributes{"x-for": "opt in " + id + "_opts"}
}

// xForSelected returns the Alpine x-for attribute for iterating selected values.
//
//	<template { xForSelected("permissions")... }>
//	  → x-for="tag in permissions"
func xForSelected(id string) templ.Attributes {
	return templ.Attributes{"x-for": "tag in " + id}
}

// xShowDropdown returns Alpine x-show bound to the dropdown open state.
//
//	<div { xShowDropdown("permissions")... }>
//	  → x-show="permissions_open"
func xShowDropdown(id string) templ.Attributes {
	return templ.Attributes{"x-show": id + "_open"}
}

// toggleExpr returns the Alpine expression to toggle a select item.
func toggleExpr(id string) string {
	return id + ".includes(opt) ? " + id + " = " + id + ".filter(x => x !== opt) : " + id + ".push(opt)"
}

// removeTagExpr returns the Alpine expression to remove a tag from the selection.
func removeTagExpr(id string) string {
	return id + " = " + id + ".filter(x => x !== tag)"
}

// isCheckedExpr returns the Alpine x-bind:checked expression.
func isCheckedExpr(id string) string {
	return id + ".includes(opt)"
}

// toggleDropdownExpr returns the expression to toggle the dropdown open/close.
func toggleDropdownExpr(id string) string {
	return id + "_open = !" + id + "_open"
}

// closeDropdownExpr returns the expression to close the dropdown.
func closeDropdownExpr(id string) string {
	return id + "_open = false"
}

// tagTextExpr returns the Alpine x-text expression for a selected tag.
// For string selects: "tag", for object selects: "role_ids_labels[tag] || tag".
func tagTextExpr(s AsyncSelect) string {
	if s.ValueKey != "" {
		return s.ID + "_labels[tag] || tag"
	}
	return "tag"
}

// optTextExpr returns the Alpine x-text expression for a dropdown option.
// For string selects: "opt", for object selects: "role_ids_labels[opt] || opt".
func optTextExpr(s AsyncSelect) string {
	if s.ValueKey != "" {
		return s.ID + "_labels[opt] || opt"
	}
	return "opt"
}
