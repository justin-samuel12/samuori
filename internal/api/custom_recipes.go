package api

import (
	"errors"
	"net/http"

	"go.yaml.in/yaml/v3"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/recipedef"
	"samuori.invalid/samuori/internal/recipes"
)

func toCustomRecipe(v recipes.CustomVersion) apigen.CustomRecipe {
	text, _ := yaml.Marshal(v.Definition)
	out := apigen.CustomRecipe{Id: v.Definition.ID, Version: v.Definition.Version, DisplayName: v.Definition.DisplayName,
		Status: apigen.CustomRecipeStatus(v.Status), Yaml: string(text), CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
	if v.Reason != "" {
		out.Reason = ptr(v.Reason)
	}
	return out
}

func (a *API) customRecipeError(w http.ResponseWriter, r *http.Request, err error) {
	var bad *recipedef.Invalid
	var inv *recipes.Invalid
	switch {
	case errors.As(err, &bad):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_recipe", bad.Error())
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_recipe", inv.Reason)
	case errors.Is(err, recipes.ErrExists), errors.Is(err, recipes.ErrNotDraft):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, recipes.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, recipes.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators manage recipes")
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) ListCustomRecipes(w http.ResponseWriter, r *http.Request, params apigen.ListCustomRecipesParams) {
	list, err := a.recipes.ListCustom(r.Context(), principal(r))
	if err != nil {
		a.customRecipeError(w, r, err)
		return
	}
	out := apigen.CustomRecipeList{Items: []apigen.CustomRecipe{}}
	for _, v := range list {
		out.Items = append(out.Items, toCustomRecipe(v))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x apigen.CustomRecipe) string { return x.Id }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateCustomRecipe(w http.ResponseWriter, r *http.Request, _ apigen.CreateCustomRecipeParams) {
	var in apigen.CustomRecipeWrite
	if !decodeBody(w, r, &in) {
		return
	}
	v, err := a.recipes.CreateCustom(r.Context(), principal(r), []byte(in.Yaml), requestID(w))
	if err != nil {
		a.customRecipeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toCustomRecipe(v))
}

func (a *API) ReplaceCustomRecipeDraft(w http.ResponseWriter, r *http.Request, id, version string, _ apigen.ReplaceCustomRecipeDraftParams) {
	var in apigen.CustomRecipeWrite
	if !decodeBody(w, r, &in) {
		return
	}
	v, err := a.recipes.ReplaceDraft(r.Context(), principal(r), id, version, []byte(in.Yaml), requestID(w))
	if err != nil {
		a.customRecipeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toCustomRecipe(v))
}

func (a *API) DiscardCustomRecipeDraft(w http.ResponseWriter, r *http.Request, id, version string, _ apigen.DiscardCustomRecipeDraftParams) {
	if err := a.recipes.DiscardDraft(r.Context(), principal(r), id, version, requestID(w)); err != nil {
		a.customRecipeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) DecideCustomRecipe(w http.ResponseWriter, r *http.Request, id, version string, _ apigen.DecideCustomRecipeParams) {
	var in apigen.CustomRecipeAction
	if !decodeBody(w, r, &in) {
		return
	}
	v, err := a.recipes.DecideCustom(r.Context(), principal(r), id, version, string(in.Action), in.Reason, requestID(w))
	if err != nil {
		a.customRecipeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toCustomRecipe(v))
}

// PreviewCustomRecipe renders what Samuori would build from a definition and parameter values, saving nothing.
func (a *API) PreviewCustomRecipe(w http.ResponseWriter, r *http.Request, _ apigen.PreviewCustomRecipeParams) {
	var in apigen.CustomRecipePreviewRequest
	if !decodeBody(w, r, &in) {
		return
	}
	out := apigen.CustomRecipePreview{Problems: []struct {
		Message string `json:"message"`
		Path    string `json:"path"`
	}{}}
	problem := func(path, msg string) {
		out.Problems = append(out.Problems, struct {
			Message string `json:"message"`
			Path    string `json:"path"`
		}{msg, path})
	}
	report := func(err error) {
		var bad *recipedef.Invalid
		if errors.As(err, &bad) {
			for _, p := range bad.Problems {
				problem(p.Path, p.Message)
			}
			return
		}
		problem("/", err.Error())
	}
	d, err := recipedef.Parse([]byte(in.Yaml))
	if err != nil {
		report(err)
		writeJSON(w, http.StatusOK, out)
		return
	}
	values := map[string]string{}
	if in.Parameters != nil {
		values = *in.Parameters
	}
	params, err := d.Resolve(values)
	if err != nil {
		report(err)
		writeJSON(w, http.StatusOK, out)
		return
	}
	mounts := map[string]recipedef.Mount{}
	for _, id := range d.Secrets {
		mounts[id] = recipedef.Mount{ID: id, Target: "/run/secrets/" + id, Required: true}
	}
	df, err := d.Render(params, mounts, nil)
	if err != nil {
		report(err)
		writeJSON(w, http.StatusOK, out)
		return
	}
	text := string(df)
	out.Valid, out.Dockerfile = true, &text
	writeJSON(w, http.StatusOK, out)
}
