package agent

import (
	"github.com/a-h/templ"

	"github.com/soltiHQ/control-plane/ui/templates/component/modal"
)

// The dialogs of the operator actions on the agent page, by the name that opens
// them (modal.OpenEvent).
const (
	resetCredentialModal  = "reset-agent-credential"
	credentialIssuedModal = "agent-credential-issued"
	deleteAgentModal      = "delete-agent"
)

// credentialIssuedForm makes the confirmation of a reset show the credential it
// issued. The reset answers the new token once, as JSON; when the request
// succeeds, the form keeps the answer in the issued state of credentialDialogs
// and opens the dialog that shows it. An answer that cannot be read leaves the
// confirmation open with an error, since the credential is reset already and
// the token is in no other place. The expression is a constant: the token is
// read from the answer, never pasted into code.
func credentialIssuedForm() templ.Attributes {
	return templ.Attributes{
		"x-on:htmx:after-on-load": "if (!$event.detail.successful) return; " +
			"try { issued = JSON.parse($event.detail.xhr.responseText); } catch { issued = null; } " +
			"if (issued && issued.token) { " + modal.OpenEvent(credentialIssuedModal) + "; } " +
			"else { issued = null; error = 'The credential was reset, but the answer holds no token. Reset it again.'; show = true; }",
	}
}

// derivationLine is the Alpine expression of the command that prints the token
// of a derived credential again, on a host that has the enrollment secret. An
// agent ID that starts with a dash needs the double dash before it. The values
// are the answer's, read when the dialog shows.
const derivationLine = `'podium agent-token --generation ' + issued.generation + ' ' + (issued.agent_id.startsWith('-') ? '-- ' : '') + issued.agent_id`
