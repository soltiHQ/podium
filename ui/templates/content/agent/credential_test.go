package agent

import (
	"bytes"
	"context"
	"html"
	"strings"
	"testing"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
	"github.com/soltiHQ/control-plane/ui/templates/component/modal"
)

// renderDetail renders the agent detail card for policy p, as the markup the
// browser parses: with its HTML entities still in place.
func renderDetail(t *testing.T, id string, p policy.AgentDetail) string {
	t.Helper()
	var buf bytes.Buffer
	a := restv1.Agent{ID: id, Name: "agent", Status: "active"}
	if err := Detail(a, p).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// listener is the attribute with which a modal listens for the event that opens
// it.
func listener(name string) string {
	open := modal.OpenEvent(name) // $dispatch("modal:open:<hex>")
	event := strings.TrimSuffix(strings.TrimPrefix(open, `$dispatch("`), `")`)
	return "x-on:" + event + `.window="show = true"`
}

// Each operator action shows on the agent page with its own permission, and
// with it its dialogs: the reset a confirmation that posts to the credential
// route and the dialog that shows the new token, the delete a confirmation that
// sends DELETE to the agent. Without the permission nothing of the action is
// in the page, not hidden.
func TestDetailOffersEachOperatorActionOnlyWithItsPermission(t *testing.T) {
	const (
		resetMarks  = `title="Reset credential"`
		deleteMarks = `title="Delete agent"`
	)
	for _, tc := range []struct {
		name          string
		p             policy.AgentDetail
		reset, remove bool
	}{
		{"neither", policy.AgentDetail{CanEditLabels: true, CanOperateTasks: true, CanViewHistory: true}, false, false},
		{"reset", policy.AgentDetail{CanResetCredential: true}, true, false},
		{"delete", policy.AgentDetail{CanDelete: true}, false, true},
		{"both", policy.AgentDetail{CanResetCredential: true, CanDelete: true}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			markup := html.UnescapeString(renderDetail(t, "agent-1", tc.p))
			for _, want := range []struct {
				present bool
				needle  string
			}{
				{tc.reset, resetMarks},
				{tc.reset, `hx-post="` + routepath.ApiAgentCredential("agent-1") + `"`},
				{tc.reset, modal.OpenEvent(resetCredentialModal)},
				{tc.reset, listener(resetCredentialModal)},
				{tc.reset, listener(credentialIssuedModal)},
				{tc.reset, "New credential"},
				{tc.remove, deleteMarks},
				{tc.remove, `hx-delete="` + routepath.ApiAgentByID("agent-1") + `"`},
				{tc.remove, modal.OpenEvent(deleteAgentModal)},
				{tc.remove, listener(deleteAgentModal)},
			} {
				if got := strings.Contains(markup, want.needle); got != want.present {
					t.Errorf("page holds %q: %v, want %v", want.needle, got, want.present)
				}
			}
			if !tc.reset && !tc.remove {
				for _, absent := range []string{"credential", "Delete agent", "hx-post", "hx-delete"} {
					if strings.Contains(markup, absent) {
						t.Errorf("a page without the permissions holds %q", absent)
					}
				}
			}
		})
	}
}

// The agent ID reaches the dialogs as data: the URLs the forms send to are the
// escaped path builders', the ID in the text of a confirmation cannot start
// markup, and the code of the dialogs holds no ID.
func TestOperatorActionsEscapeTheAgentID(t *testing.T) {
	for _, id := range []string{
		`x'+alert(1)+'`,
		`</script><img src=x onerror=alert(1)>`,
		"x\n\u2028alert(1)",
		`a/b c?d#e`,
	} {
		markup := renderDetail(t, id, policy.AgentDetail{CanResetCredential: true, CanDelete: true})
		if got, want := attrValue(t, markup, "hx-post"), routepath.ApiAgentCredential(id); got != want {
			t.Errorf("reset posts to %q, want %q", got, want)
		}
		if got, want := attrValue(t, markup, "hx-delete"), routepath.ApiAgentByID(id); got != want {
			t.Errorf("delete sends to %q, want %q", got, want)
		}
		if strings.Contains(markup, "<img src=x") {
			t.Errorf("the agent ID %q started markup", id)
		}
		if expr := attrValue(t, markup, "x-on:htmx:after-on-load"); strings.Contains(expr, "alert") {
			t.Errorf("the agent ID is in the code that reads the answer: %s", expr)
		}
	}
}

// The confirmation of a reset hands the answer to the dialog that shows it: it
// reads the token from the JSON the reset answers, keeps it in the state both
// dialogs share, and opens the dialog by the event that dialog listens for. The
// dialog shows the token with a copy button, the command that derives it again
// only for a derived credential (the answer has a generation), and forgets the
// token when it closes, however it closes.
func TestCredentialIssuedDialogShowsTheTokenOnce(t *testing.T) {
	markup := html.UnescapeString(renderDetail(t, "agent-1", policy.AgentDetail{CanResetCredential: true}))

	expr := attrValue(t, renderDetail(t, "agent-1", policy.AgentDetail{CanResetCredential: true}), "x-on:htmx:after-on-load")
	for _, want := range []string{
		"$event.detail.successful",
		"JSON.parse($event.detail.xhr.responseText)",
		"issued = ",
		modal.OpenEvent(credentialIssuedModal),
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("the code that reads the answer lacks %q:\n%s", want, expr)
		}
	}
	for _, want := range []string{
		`x-data="{ issued: null }"`,
		`x-text="issued.token"`,
		`x-bind:data-copy-text="issued.token"`,
		`<template x-if="issued.generation">`,
		`x-text="` + derivationLine + `"`,
		`x-effect="show || (issued = null)"`,
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("the dialog lacks %q", want)
		}
	}
	if !strings.Contains(markup, "Reset credential") {
		t.Error("the confirmation does not say what it does")
	}
}

// The confirmation of a delete lists what goes and what stays, and says that an
// agent that still runs registers again, and how to keep it out.
func TestDeleteConfirmationListsWhatGoesAndStays(t *testing.T) {
	markup := html.UnescapeString(renderDetail(t, "agent-1", policy.AgentDetail{CanDelete: true}))
	for _, want := range []string{
		"<li>", "rollout", "credential", "execution history", "registers again", "reset its credential first",
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("the confirmation lacks %q", want)
		}
	}
	if got := strings.Count(markup, "<li>"); got != 4 {
		t.Errorf("the confirmation lists %d items, want 4 (two that go, two that stay)", got)
	}
}
