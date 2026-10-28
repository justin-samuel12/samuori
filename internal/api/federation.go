package api

import (
	"errors"
	"net/http"
	"strings"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/federation"
)

// FederationInfo is what the federation identity screen shows about this installation.
type FederationInfo struct {
	CAPin     func() string
	Listening bool
	Hostnames []string
}

func (a *API) federationError(w http.ResponseWriter, r *http.Request, err error) {
	var inv *federation.Invalid
	var inUse *federation.InUse
	switch {
	case errors.As(err, &inUse):
		apigen.WriteProblem(w, http.StatusConflict, "in_use", "This location is still used by "+strings.Join(inUse.Uses, "; ")+". Remove it from those first.")
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_federation", inv.Reason)
	case errors.Is(err, federation.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, federation.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, federation.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", err.Error())
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) GetFederationIdentity(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.IsOrgAdmin() {
		a.federationError(w, r, federation.ErrForbidden)
		return
	}
	out := apigen.FederationIdentity{OrganizationId: p.OrgID, Listening: a.federationInfo.Listening}
	if a.federationInfo.CAPin != nil {
		out.CaPin = a.federationInfo.CAPin()
	}
	if len(a.federationInfo.Hostnames) > 0 {
		h := a.federationInfo.Hostnames
		out.Hostnames = &h
	}
	writeJSON(w, http.StatusOK, out)
}

func toPeer(x federation.Peer) apigen.FederationPeer {
	return apigen.FederationPeer{Id: x.ID, Name: x.Name, Endpoint: x.Endpoint, CaPin: x.CAPin, OrganizationId: x.RemoteOrgID, CreatedAt: x.CreatedAt, Disabled: x.Disabled}
}

func (a *API) ListFederationPeers(w http.ResponseWriter, r *http.Request, params apigen.ListFederationPeersParams) {
	list, err := a.federation.Peers(r.Context(), principal(r))
	if err != nil {
		a.federationError(w, r, err)
		return
	}
	items := []apigen.FederationPeer{}
	for _, x := range list {
		items = append(items, toPeer(x))
	}
	page, next, ok := pageOf(w, items, params.Limit, params.Cursor, func(x apigen.FederationPeer) string { return x.Id })
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": page, "nextCursor": next})
}

func (a *API) CreateFederationPeer(w http.ResponseWriter, r *http.Request, _ apigen.CreateFederationPeerParams) {
	var in apigen.FederationPeerWrite
	if !decodeBody(w, r, &in) {
		return
	}
	x, err := a.federation.CreatePeer(r.Context(), principal(r), in.Name, in.Endpoint, in.CaPin, in.OrganizationId, requestID(w))
	if err != nil {
		a.federationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toPeer(x))
}

func (a *API) DisableFederationPeer(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DisableFederationPeerParams) {
	var in apigen.FederationReason
	if !decodeBody(w, r, &in) {
		return
	}
	if err := a.federation.DisablePeer(r.Context(), principal(r), id, in.Reason, requestID(w)); err != nil {
		a.federationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toGrant(g federation.Grant) apigen.FederationGrant {
	return apigen.FederationGrant{Id: g.ID, PeerId: g.PeerID, ProjectId: g.ProjectID, Locations: g.Locations, MaxActive: g.MaxActive, CreatedAt: g.CreatedAt, Revoked: g.Revoked}
}

func (a *API) ListFederationGrants(w http.ResponseWriter, r *http.Request, params apigen.ListFederationGrantsParams) {
	list, err := a.federation.Grants(r.Context(), principal(r))
	if err != nil {
		a.federationError(w, r, err)
		return
	}
	items := []apigen.FederationGrant{}
	for _, g := range list {
		items = append(items, toGrant(g))
	}
	page, next, ok := pageOf(w, items, params.Limit, params.Cursor, func(x apigen.FederationGrant) string { return x.Id })
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": page, "nextCursor": next})
}

func (a *API) CreateFederationGrant(w http.ResponseWriter, r *http.Request, _ apigen.CreateFederationGrantParams) {
	var in apigen.FederationGrantWrite
	if !decodeBody(w, r, &in) {
		return
	}
	g, err := a.federation.CreateGrant(r.Context(), principal(r), in.PeerId, in.ProjectId, in.Locations, in.MaxActive, requestID(w))
	if err != nil {
		a.federationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toGrant(g))
}

func (a *API) RevokeFederationGrant(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RevokeFederationGrantParams) {
	var in apigen.FederationReason
	if !decodeBody(w, r, &in) {
		return
	}
	if err := a.federation.RevokeGrant(r.Context(), principal(r), id, in.Reason, requestID(w)); err != nil {
		a.federationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toRoute(x federation.Route) apigen.FederationRoute {
	return apigen.FederationRoute{Id: x.ID, PeerId: x.PeerID, Location: x.Location, RemoteLocation: x.RemoteLocation, CreatedAt: x.CreatedAt}
}

func (a *API) ListFederationRoutes(w http.ResponseWriter, r *http.Request, params apigen.ListFederationRoutesParams) {
	list, err := a.federation.Routes(r.Context(), principal(r))
	if err != nil {
		a.federationError(w, r, err)
		return
	}
	items := []apigen.FederationRoute{}
	for _, x := range list {
		items = append(items, toRoute(x))
	}
	page, next, ok := pageOf(w, items, params.Limit, params.Cursor, func(x apigen.FederationRoute) string { return x.Id })
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": page, "nextCursor": next})
}

func (a *API) CreateFederationRoute(w http.ResponseWriter, r *http.Request, _ apigen.CreateFederationRouteParams) {
	var in apigen.FederationRouteWrite
	if !decodeBody(w, r, &in) {
		return
	}
	x, err := a.federation.CreateRoute(r.Context(), principal(r), in.PeerId, in.Location, in.RemoteLocation, requestID(w))
	if err != nil {
		a.federationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRoute(x))
}

func (a *API) DeleteFederationRoute(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DeleteFederationRouteParams) {
	if err := a.federation.DeleteRoute(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.federationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) ListFederationDelegations(w http.ResponseWriter, r *http.Request, params apigen.ListFederationDelegationsParams) {
	list, err := a.federation.Delegations(r.Context(), principal(r), 200)
	if err != nil {
		a.federationError(w, r, err)
		return
	}
	items := []apigen.FederationDelegation{}
	for _, d := range list {
		items = append(items, apigen.FederationDelegation{Id: d.ID, RunId: d.RunID, PeerId: d.PeerID, RemoteLocation: d.RemoteLocation, State: apigen.FederationDelegationState(d.State),
			RemoteRunId: opt(d.RemoteRunID), RemoteState: opt(d.RemoteState), Detail: opt(d.Detail), LastContactAt: d.LastContactAt, UnreachableSince: d.UnreachableSince, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt})
	}
	page, next, ok := pageOf(w, items, params.Limit, params.Cursor, func(x apigen.FederationDelegation) string { return x.Id })
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": page, "nextCursor": next})
}

func (a *API) AbandonFederationDelegation(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.AbandonFederationDelegationParams) {
	var in apigen.FederationReason
	if !decodeBody(w, r, &in) {
		return
	}
	if err := a.federation.Abandon(r.Context(), principal(r), a.builds, id, in.Reason, requestID(w)); err != nil {
		a.federationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
