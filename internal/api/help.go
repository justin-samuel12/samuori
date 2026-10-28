package api

import (
	"net/http"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/help"
)

// The guides are embedded and checked by tests, so a load failure is a build defect reported on every call.
var guides, guidesErr = help.Load()

func (a *API) ListHelpTopics(w http.ResponseWriter, r *http.Request) {
	if guidesErr != nil {
		a.internalError(w, r, guidesErr)
		return
	}
	out := apigen.HelpTopicList{Items: []apigen.HelpTopicSummary{}}
	for _, t := range guides.For(principal(r)) {
		out.Items = append(out.Items, apigen.HelpTopicSummary{Id: t.ID, Title: t.Title, Section: t.Section, Summary: t.Summary, Roles: t.Roles})
	}
	writeJSON(w, http.StatusOK, out)
}

// GetHelpTopic reports a guide the caller may not read exactly like one that does not exist.
func (a *API) GetHelpTopic(w http.ResponseWriter, r *http.Request, topicID string) {
	if guidesErr != nil {
		a.internalError(w, r, guidesErr)
		return
	}
	t, ok := guides.Get(principal(r), topicID)
	if !ok {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	writeJSON(w, http.StatusOK, apigen.HelpTopic{Id: t.ID, Title: t.Title, Section: t.Section, Summary: t.Summary, Roles: t.Roles, Body: t.Body})
}
