package api

import (
	"errors"
	"net/http"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/recipes"
)

func toRecipeRuntime(rt recipes.Runtime) apigen.RecipeRuntime {
	images := rt.Images("http-service")
	for _, img := range rt.RunImages {
		images = append(images, img)
	}
	out := apigen.RecipeRuntime{Version: rt.Version, Images: images, Source: apigen.RecipeRuntimeSource(rt.Source), Status: apigen.RecipeRuntimeStatusApproved}
	if rt.ID != "" {
		out.Id = ptr(rt.ID)
	}
	return out
}

func toOrgRuntime(o recipes.OrgRuntime) apigen.OrganizationRuntime {
	out := apigen.OrganizationRuntime{Id: o.ID, RecipeId: o.RecipeID, RecipeVersion: o.RecipeVersion, Label: o.Label, BuildImage: o.BuildImage, RunImage: o.RunImage,
		Status: apigen.OrganizationRuntimeStatus(o.Status), CreatedBy: o.CreatedBy, CreatedAt: o.CreatedAt, ApprovedAt: o.ApprovedAt, WithdrawnAt: o.WithdrawnAt, Superseded: o.Superseded}
	if o.Note != "" {
		out.Note = ptr(o.Note)
	}
	if o.Reason != "" {
		out.Reason = ptr(o.Reason)
	}
	if o.ApprovedBy != "" {
		out.ApprovedBy = ptr(o.ApprovedBy)
	}
	if o.WithdrawnBy != "" {
		out.WithdrawnBy = ptr(o.WithdrawnBy)
	}
	return out
}

func (a *API) recipeRuntimeError(w http.ResponseWriter, r *http.Request, err error) {
	var inv *recipes.Invalid
	switch {
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_runtime", inv.Reason)
	case errors.Is(err, recipes.ErrRuntimeExists):
		apigen.WriteProblem(w, http.StatusConflict, "duplicate_runtime", err.Error())
	case errors.Is(err, recipes.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, recipes.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators manage recipe runtimes")
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) ListRecipeRuntimes(w http.ResponseWriter, r *http.Request, id, version string) {
	shipped, org, err := a.recipes.ListRuntimes(r.Context(), principal(r), id, version)
	if err != nil {
		a.recipeRuntimeError(w, r, err)
		return
	}
	out := apigen.RecipeRuntimes{Shipped: []apigen.RecipeRuntime{}, Organization: []apigen.OrganizationRuntime{}}
	for _, rt := range shipped {
		out.Shipped = append(out.Shipped, toRecipeRuntime(rt))
	}
	for _, o := range org {
		out.Organization = append(out.Organization, toOrgRuntime(o))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateRecipeRuntime(w http.ResponseWriter, r *http.Request, id, version string, _ apigen.CreateRecipeRuntimeParams) {
	var in apigen.OrganizationRuntimeWrite
	if !decodeBody(w, r, &in) {
		return
	}
	ri := recipes.RuntimeInput{Label: in.Label, BuildImage: in.BuildImage, RunImage: in.RunImage}
	if in.Note != nil {
		ri.Note = *in.Note
	}
	o, err := a.recipes.CreateRuntime(r.Context(), principal(r), id, version, ri, requestID(w))
	if err != nil {
		a.recipeRuntimeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toOrgRuntime(o))
}

func (a *API) DecideRecipeRuntime(w http.ResponseWriter, r *http.Request, id, version, runtimeID string, _ apigen.DecideRecipeRuntimeParams) {
	var in apigen.OrganizationRuntimeAction
	if !decodeBody(w, r, &in) {
		return
	}
	o, err := a.recipes.DecideRuntime(r.Context(), principal(r), id, version, runtimeID, string(in.Action), in.Reason, requestID(w))
	if err != nil {
		a.recipeRuntimeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toOrgRuntime(o))
}
