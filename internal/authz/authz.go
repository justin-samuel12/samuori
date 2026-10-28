// Package authz evaluates scoped role bindings. Every API operation and repository query consults it.
package authz

import (
	"context"
	"slices"
)

type Role string

const (
	Developer        Role = "developer"
	Maintainer       Role = "maintainer"
	Publisher        Role = "publisher"
	Operator         Role = "operator"
	SecurityReviewer Role = "security-reviewer"
	Administrator    Role = "administrator"
)

type ScopeType string

const (
	ScopeOrganization ScopeType = "organization"
	ScopeTeam         ScopeType = "team"
	ScopeProject      ScopeType = "project"
)

type Binding struct {
	Role      Role
	ScopeType ScopeType
	ScopeID   string
}

type Kind string

const (
	User    Kind = "user"
	Service Kind = "service"
	System  Kind = "system"
)

type Principal struct {
	ID       string
	OrgID    string
	Kind     Kind
	Bindings []Binding
	ReadOnly bool
}

// Resource locates an object in the tenancy tree; empty fields mean the object is not that specific.
type Resource struct {
	OrgID     string
	TeamID    string
	ProjectID string
}

func (b Binding) covers(p Principal, r Resource) bool {
	if r.OrgID != p.OrgID {
		return false
	}
	switch b.ScopeType {
	case ScopeOrganization:
		return b.ScopeID == r.OrgID
	case ScopeTeam:
		return r.TeamID != "" && b.ScopeID == r.TeamID
	case ScopeProject:
		return r.ProjectID != "" && b.ScopeID == r.ProjectID
	}
	return false
}

// Can reports whether p holds one of roles on a binding that covers r.
func Can(p Principal, roles []Role, r Resource) bool {
	for _, b := range p.Bindings {
		if slices.Contains(roles, b.Role) && b.covers(p, r) {
			return true
		}
	}
	return false
}

// HasAnyRole reports whether p holds any of roles at any scope; used before a resource is known.
func HasAnyRole(p Principal, roles []Role) bool {
	for _, b := range p.Bindings {
		if slices.Contains(roles, b.Role) {
			return true
		}
	}
	return false
}

func (p Principal) IsOrgAdmin() bool {
	return Can(p, []Role{Administrator}, Resource{OrgID: p.OrgID})
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
