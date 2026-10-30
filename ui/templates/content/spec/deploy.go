package spec

import (
	"github.com/a-h/templ"

	"github.com/soltiHQ/control-plane/ui/templates/jsexpr"
)

// PlacementPanelID is the ID of the spec page element that the placement panel
// is loaded into. The element keeps the token of the last plan the panel showed.
const PlacementPanelID = "spec-placement"

// keepPlanTokenExpr is the x-init of a rendered placement panel: it copies the
// panel's plan token onto the panel's container. A failed refresh replaces only
// the container's children (control-plane.js), so the container still holds the
// token of the last plan the panel showed.
func keepPlanTokenExpr() string {
	return "$el.closest(" + jsexpr.String("#"+PlacementPanelID) + ")?.setAttribute('data-plan-token', $el.dataset.planToken)"
}

// deployConfirmForm makes the Deploy confirmation send the plan token as
// If-Match. When the dialog opens it keeps the token the placement container
// holds: that of the plan the panel shows, or showed before a failed refresh. A
// plan that changes while the dialog is open is therefore refused with 412
// instead of deployed unseen, and the error stays in the dialog. Deploy runs
// unchecked only without a token: no AgentsGet (no panel) or a panel that never
// loaded.
//
// The expressions are constants: the token is read from the DOM when the
// dialog opens, never pasted into code.
func deployConfirmForm() templ.Attributes {
	return templ.Attributes{
		"x-data":                   "{ planToken: '' }",
		"x-effect":                 "show && (planToken = document.getElementById(" + jsexpr.String(PlacementPanelID) + ")?.dataset.planToken ?? '')",
		"x-on:htmx:config-request": "planToken && ($event.detail.headers['If-Match'] = JSON.stringify(planToken))",
	}
}
