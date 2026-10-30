package spec

import "github.com/a-h/templ"

// deployConfirmForm makes the Deploy confirmation send the plan token as
// If-Match. When the dialog opens it keeps the token of the plan the placement
// panel shows (Placement's data-plan-token), so a plan that changes while the
// dialog is open is refused with 412 instead of deployed unseen, and the error
// stays in the dialog. Without a placement panel (no AgentsGet, or the panel
// failed to load) there is no token and no If-Match: Deploy runs unchecked.
//
// The expressions are constants: the token is read from the DOM when the
// dialog opens, never pasted into code.
func deployConfirmForm() templ.Attributes {
	return templ.Attributes{
		"x-data":                   "{ planToken: '' }",
		"x-effect":                 "show && (planToken = document.querySelector('[data-plan-token]')?.dataset.planToken ?? '')",
		"x-on:htmx:config-request": "planToken && ($event.detail.headers['If-Match'] = JSON.stringify(planToken))",
	}
}
