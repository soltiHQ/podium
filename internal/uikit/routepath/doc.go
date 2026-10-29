// Package routepath declares all URL constants used by the control-plane UI and API.
//
// Constants are split into two groups:
//   - Page* - browser-facing paths served by the UI handler (HTML pages).
//   - Api*  - JSON/REST endpoints served by the API handler.
//
// Path-builder functions (var block) append an entity ID to a base path, keeping URL construction consistent
// and typo-free across handlers, templates, and Alpine.js fetch calls. Every ID is path-escaped as a single
// segment (url.PathEscape): it cannot add a query or a fragment, nor break out of the HTML attribute or
// JavaScript string the URL is placed in. The server routes on the decoded r.URL.Path, where an escaped "/" is a
// separator again: an ID with a "/" gets 404, and any other ID reaches its handler unchanged.
package routepath
