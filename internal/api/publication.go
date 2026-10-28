package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/publication"
)

func (a *API) publicationError(w http.ResponseWriter, r *http.Request, err error) {
	var inv *publication.Invalid
	var inUse *publication.InUse
	switch {
	case errors.As(err, &inUse):
		apigen.WriteProblem(w, http.StatusConflict, "in_use", "Still used by "+strings.Join(inUse.Uses, "; ")+". Remove or change those first.")
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", inv.Reason)
	case errors.Is(err, publication.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, publication.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "you may not publish this artifact to that destination")
	case errors.Is(err, publication.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", "a record with that name already exists")
	default:
		a.internalError(w, r, err)
	}
}

func toRegistry(r publication.Registry) apigen.Registry {
	out := apigen.Registry{Id: r.ID, Name: r.Name, Kind: apigen.RegistryKind(r.Kind), Endpoint: ptr(r.Endpoint), PlainHttp: ptr(r.PlainHTTP), AllowPrivateNetwork: ptr(r.AllowPrivate)}
	if r.Kind == "ecr" {
		out.AwsAccountId, out.Region = ptr(r.AccountID), ptr(r.Region)
	}
	return out
}

// convert copies between types with the same JSON shape, such as generated anonymous structs.
func convert(from, to any) {
	raw, _ := json.Marshal(from)
	_ = json.Unmarshal(raw, to)
}

func (a *API) ListRegistries(w http.ResponseWriter, r *http.Request, params apigen.ListRegistriesParams) {
	list, err := a.publications.Registries(r.Context(), principal(r))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	out := apigen.RegistryList{Items: []apigen.Registry{}}
	for _, x := range list {
		out.Items = append(out.Items, toRegistry(x))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x apigen.Registry) string { return x.Id }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateRegistry(w http.ResponseWriter, r *http.Request, _ apigen.CreateRegistryParams) {
	var in apigen.RegistryCreate
	if !decodeBody(w, r, &in) {
		return
	}
	val := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	reg := publication.Registry{Name: in.Name, Kind: string(in.Kind), Endpoint: val(in.Endpoint), AccountID: val(in.AwsAccountId), Region: val(in.Region),
		PlainHTTP: in.PlainHttp != nil && *in.PlainHttp, AllowPrivate: in.AllowPrivateNetwork != nil && *in.AllowPrivateNetwork}
	out, err := a.publications.CreateRegistry(r.Context(), principal(r), reg, requestID(w))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRegistry(out))
}

func (a *API) UpdateRegistry(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdateRegistryParams) {
	var in apigen.RegistryUpdate
	if !decodeBody(w, r, &in) {
		return
	}
	val := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	reg := publication.Registry{Name: in.Name, Endpoint: val(in.Endpoint), AccountID: val(in.AwsAccountId), Region: val(in.Region),
		PlainHTTP: in.PlainHttp != nil && *in.PlainHttp, AllowPrivate: in.AllowPrivateNetwork != nil && *in.AllowPrivateNetwork}
	out, err := a.publications.UpdateRegistry(r.Context(), principal(r), id, reg, requestID(w))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRegistry(out))
}

func (a *API) DeleteRegistry(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DeleteRegistryParams) {
	if err := a.publications.DeleteRegistry(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.publicationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) DeleteDestination(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DeleteDestinationParams) {
	if err := a.publications.DeleteDestination(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.publicationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toProfile(p publication.Profile) apigen.RegistryCredentialProfile {
	out := apigen.RegistryCredentialProfile{Id: p.ID, Name: p.Name, RegistryId: p.RegistryID, Type: apigen.RegistryCredentialProfileType(p.Type), Purpose: apigen.RegistryCredentialProfilePurpose(p.Purpose),
		Status: apigen.RegistryCredentialProfileStatus(p.Status), RepositoryScopes: ptr(p.Scopes)}
	if p.Username != "" {
		out.Username = ptr(p.Username)
	}
	if p.SecretID != "" {
		out.SecretRef = ptr(p.SecretID)
	}
	if p.LastTest != nil {
		out.LastTest = &apigen.ConnectionTest{}
		convert(p.LastTest, out.LastTest)
	}
	if p.Type == "ecr" {
		convert(p.ECR, &out.Ecr)
	}
	return out
}

func (a *API) ListRegistryCredentialProfiles(w http.ResponseWriter, r *http.Request, params apigen.ListRegistryCredentialProfilesParams) {
	list, err := a.publications.Profiles(r.Context(), principal(r))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	out := apigen.RegistryCredentialProfileList{Items: []apigen.RegistryCredentialProfile{}}
	for _, p := range list {
		out.Items = append(out.Items, toProfile(p))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x apigen.RegistryCredentialProfile) string { return x.Id }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateRegistryCredentialProfile(w http.ResponseWriter, r *http.Request, _ apigen.CreateRegistryCredentialProfileParams) {
	var in apigen.RegistryCredentialProfileCreate
	if !decodeBody(w, r, &in) {
		return
	}
	p := publication.Profile{Name: in.Name, RegistryID: in.RegistryId, Type: string(in.Type), Purpose: string(in.Purpose)}
	if in.Username != nil {
		p.Username = *in.Username
	}
	if in.SecretRef != nil {
		p.SecretID = *in.SecretRef
	}
	if in.RepositoryScopes != nil {
		p.Scopes = *in.RepositoryScopes
	}
	if in.Ecr != nil {
		convert(in.Ecr, &p.ECR)
	}
	out, err := a.publications.CreateProfile(r.Context(), principal(r), p, requestID(w))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toProfile(out))
}

func (a *API) UpdateRegistryCredentialProfile(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdateRegistryCredentialProfileParams) {
	var in apigen.RegistryCredentialProfileUpdate
	if !decodeBody(w, r, &in) {
		return
	}
	p := publication.Profile{Name: in.Name, Type: string(in.Type)}
	if in.Username != nil {
		p.Username = *in.Username
	}
	if in.SecretRef != nil {
		p.SecretID = *in.SecretRef
	}
	if in.RepositoryScopes != nil {
		p.Scopes = *in.RepositoryScopes
	}
	if in.Ecr != nil {
		convert(in.Ecr, &p.ECR)
	}
	out, err := a.publications.UpdateProfile(r.Context(), principal(r), id, p, requestID(w))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toProfile(out))
}

func (a *API) DeleteRegistryCredentialProfile(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DeleteRegistryCredentialProfileParams) {
	if err := a.publications.DeleteProfile(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.publicationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) TestRegistryCredentialProfile(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.TestRegistryCredentialProfileParams) {
	res, err := a.publications.TestProfile(r.Context(), principal(r), id)
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	out := apigen.ConnectionTest{Status: apigen.ConnectionTestStatus(res.Status), TestedAt: ptr(res.TestedAt)}
	if res.Detail != "" {
		out.Detail = ptr(res.Detail)
	}
	writeJSON(w, http.StatusOK, out)
}

func toDestination(d publication.Destination) apigen.Destination {
	return apigen.Destination{Id: d.ID, Name: d.Name, RegistryId: d.RegistryID, Repository: d.Repository, PushProfileId: d.PushProfileID, AcceptsRestricted: ptr(d.AcceptsRestricted),
		AllowedProjectIds: ptr(d.AllowedProjects), ProtectedTags: ptr(d.ProtectedTags), ImmutableTags: ptr(d.ImmutableTags), RequiresApproval: ptr(d.RequiresApproval),
		SignImages: ptr(d.SignImages), AttachProvenance: ptr(d.AttachProvenance), PromotionOnly: ptr(d.PromotionOnly)}
}

func (a *API) ListDestinations(w http.ResponseWriter, r *http.Request, params apigen.ListDestinationsParams) {
	list, err := a.publications.Destinations(r.Context(), principal(r))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	out := apigen.DestinationList{Items: []apigen.Destination{}}
	for _, d := range list {
		out.Items = append(out.Items, toDestination(d))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x apigen.Destination) string { return x.Id }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func fromDestination(in apigen.DestinationCreate) publication.Destination {
	d := publication.Destination{Name: in.Name, RegistryID: in.RegistryId, Repository: in.Repository, PushProfileID: in.PushProfileId,
		AcceptsRestricted: in.AcceptsRestricted != nil && *in.AcceptsRestricted, ImmutableTags: in.ImmutableTags != nil && *in.ImmutableTags, RequiresApproval: in.RequiresApproval != nil && *in.RequiresApproval,
		SignImages: in.SignImages != nil && *in.SignImages, AttachProvenance: in.AttachProvenance != nil && *in.AttachProvenance, PromotionOnly: in.PromotionOnly != nil && *in.PromotionOnly}
	if in.AllowedProjectIds != nil {
		d.AllowedProjects = *in.AllowedProjectIds
	}
	if in.ProtectedTags != nil {
		d.ProtectedTags = *in.ProtectedTags
	}
	return d
}

func (a *API) CreateDestination(w http.ResponseWriter, r *http.Request, _ apigen.CreateDestinationParams) {
	var in apigen.DestinationCreate
	if !decodeBody(w, r, &in) {
		return
	}
	out, err := a.publications.CreateDestination(r.Context(), principal(r), fromDestination(in), requestID(w))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toDestination(out))
}

func (a *API) UpdateDestination(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdateDestinationParams) {
	var in apigen.DestinationCreate
	if !decodeBody(w, r, &in) {
		return
	}
	out, err := a.publications.UpdateDestination(r.Context(), principal(r), id, fromDestination(in), requestID(w))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDestination(out))
}

func (a *API) ListProjectDestinations(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, params apigen.ListProjectDestinationsParams) {
	list, err := a.publications.ProjectDestinations(r.Context(), principal(r), operationRoles(r.Context()), projectID)
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	out := apigen.DestinationSummaryList{Items: []apigen.DestinationSummary{}}
	for _, d := range list {
		out.Items = append(out.Items, apigen.DestinationSummary{Id: d.ID, Name: d.Name, Repository: d.Repository, AcceptsRestricted: ptr(d.AcceptsRestricted), RequiresApproval: ptr(d.RequiresApproval)})
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x apigen.DestinationSummary) string { return x.Id }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type publicationView struct {
	ID                string             `json:"id"`
	Kind              string             `json:"kind"`
	ArtifactID        string             `json:"artifactId"`
	DestinationID     string             `json:"destinationId"`
	Tags              []string           `json:"tags"`
	State             string             `json:"state"`
	Reason            string             `json:"reason,omitempty"`
	GateDecisions     []publication.Gate `json:"gateDecisions"`
	DestinationDigest string             `json:"destinationDigest,omitempty"`
	DestinationRef    string             `json:"destinationRef,omitempty"`
	SourcePublication string             `json:"sourcePublicationId,omitempty"`
	ValidationRunID   string             `json:"validationRunId,omitempty"`
	SignatureRef      string             `json:"signatureRef,omitempty"`
	AttestationRef    string             `json:"attestationRef,omitempty"`
	SignatureStorage  string             `json:"signatureStorage,omitempty"`
	DeprecatedAt      *time.Time         `json:"deprecatedAt,omitempty"`
	DeprecationReason string             `json:"deprecationReason,omitempty"`
	RetiredAt         *time.Time         `json:"retiredAt,omitempty"`
	RetirementReason  string             `json:"retirementReason,omitempty"`
	RequestedBy       *apigen.Principal  `json:"requestedBy"`
	CreatedAt         time.Time          `json:"createdAt"`
	UpdatedAt         time.Time          `json:"updatedAt"`
}

func toPublication(p publication.Publication) publicationView {
	return publicationView{ID: p.ID, Kind: p.Kind, ArtifactID: p.ArtifactID, DestinationID: p.DestinationID, Tags: p.Tags, State: p.State, Reason: p.Reason, GateDecisions: p.Gates,
		DestinationDigest: p.ImageDigest, DestinationRef: p.DestinationRef, SourcePublication: p.SourcePublicationID, ValidationRunID: p.ValidationRunID,
		SignatureRef: p.SignatureRef, AttestationRef: p.AttestationRef, SignatureStorage: p.SignatureStorage, DeprecatedAt: p.DeprecatedAt, DeprecationReason: p.DeprecationReason, RetiredAt: p.RetiredAt, RetirementReason: p.RetirementReason,
		RequestedBy: &apigen.Principal{Id: p.RequestedBy, Type: apigen.PrincipalTypeUser}, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

func (a *API) ListPublications(w http.ResponseWriter, r *http.Request, params apigen.ListPublicationsParams) {
	limit, _ := pageParams(params.Limit, params.Cursor)
	list, err := a.publications.List(r.Context(), principal(r), operationRoles(r.Context()), limit)
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	out := struct {
		Items      []publicationView `json:"items"`
		NextCursor *string           `json:"nextCursor"`
	}{Items: []publicationView{}}
	for _, p := range list {
		out.Items = append(out.Items, toPublication(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) RequestPublication(w http.ResponseWriter, r *http.Request, params apigen.RequestPublicationParams) {
	var in apigen.PublicationRequest
	if !decodeBody(w, r, &in) {
		return
	}
	var tags []string
	if in.Tags != nil {
		tags = *in.Tags
	}
	var p publication.Publication
	var err error
	switch {
	case in.Kind == apigen.PublicationRequestKindPublish && in.ArtifactId != nil:
		p, err = a.publications.Request(r.Context(), principal(r), operationRoles(r.Context()), *in.ArtifactId, in.DestinationId, tags, params.IdempotencyKey, requestID(w))
	case in.Kind == apigen.PublicationRequestKindPromote && in.SourcePublicationId != nil:
		p, err = a.publications.Promote(r.Context(), principal(r), operationRoles(r.Context()), *in.SourcePublicationId, in.DestinationId, tags, params.IdempotencyKey, requestID(w))
	case in.Kind == apigen.PublicationRequestKindRollback && in.SourcePublicationId != nil:
		p, err = a.publications.Rollback(r.Context(), principal(r), operationRoles(r.Context()), *in.SourcePublicationId, in.DestinationId, tags, params.IdempotencyKey, requestID(w))
	default:
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", "publish needs artifactId; promote and rollback need sourcePublicationId")
		return
	}
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toPublication(p))
}

func (a *API) SetPublicationDeprecation(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.SetPublicationDeprecationParams) {
	var in apigen.PublicationDeprecation
	if !decodeBody(w, r, &in) {
		return
	}
	reason := ""
	if in.Reason != nil {
		reason = *in.Reason
	}
	p, err := a.publications.Deprecate(r.Context(), principal(r), operationRoles(r.Context()), id, in.Deprecated, reason, requestID(w))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPublication(p))
}

func (a *API) RetirePublication(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RetirePublicationParams) {
	var in apigen.RetirePublicationJSONBody
	if !decodeBody(w, r, &in) {
		return
	}
	p, err := a.publications.Retire(r.Context(), principal(r), operationRoles(r.Context()), id, in.Reason, requestID(w))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPublication(p))
}

func (a *API) GetPublication(w http.ResponseWriter, r *http.Request, id apigen.Id) {
	p, err := a.publications.Get(r.Context(), principal(r), operationRoles(r.Context()), id)
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPublication(p))
}

func (a *API) ListEcrRepositories(w http.ResponseWriter, r *http.Request, registryID apigen.Id, params apigen.ListEcrRepositoriesParams) {
	list, err := a.publications.ECRRepositories(r.Context(), principal(r), registryID, params.ProfileId)
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	out := struct {
		Items []publication.Repository `json:"items"`
	}{Items: list}
	if out.Items == nil {
		out.Items = []publication.Repository{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateEcrRepository(w http.ResponseWriter, r *http.Request, registryID apigen.Id, _ apigen.CreateEcrRepositoryParams) {
	var in apigen.CreateEcrRepositoryJSONBody
	if !decodeBody(w, r, &in) {
		return
	}
	immutable := in.ImmutableTags == nil || *in.ImmutableTags
	scan := in.ScanOnPush != nil && *in.ScanOnPush
	repo, err := a.publications.CreateECRRepository(r.Context(), principal(r), registryID, in.ProfileId, in.Name, immutable, scan, requestID(w))
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, repo)
}
