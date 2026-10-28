package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"samuori.invalid/samuori/internal/version"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/telemetry"
)

// Authenticator resolves the caller; ok is false for anonymous requests.
type Authenticator interface {
	Authenticate(r *http.Request) (p authz.Principal, ok bool, err error)
}

type rolesKey struct{}

// VersionHeader carries the controller's release on authenticated API responses.
const VersionHeader = "X-Samuori-Version"

func operationRoles(ctx context.Context) []authz.Role {
	r, _ := ctx.Value(rolesKey{}).([]authz.Role)
	return r
}

// guard enforces, before any handler runs: route match, authentication, coarse role check from x-samuori-roles,
// and request validation against the contract. Handlers still check resource scope.
func guard(next http.Handler, authn Authenticator, limit *limiter) (http.Handler, error) {
	spec, err := apigen.GetSwagger()
	if err != nil {
		return nil, err
	}
	return guardSpec(spec, next, authn, limit)
}

func guardSpec(spec *openapi3.T, next http.Handler, authn Authenticator, limit *limiter) (http.Handler, error) {
	spec.Servers = nil
	router, err := legacy.NewRouter(spec)
	if err != nil {
		return nil, err
	}
	deprecated, err := deprecations(spec)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookup := r.Clone(r.Context())
		lookup.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/v1")
		route, params, err := router.FindRoute(lookup)
		if errors.Is(err, routers.ErrPathNotFound) || errors.Is(err, routers.ErrMethodNotAllowed) || route == nil {
			next.ServeHTTP(w, r)
			return
		}
		op := route.Operation
		telemetry.SetOperation(r.Context(), op.OperationID)
		if d, ok := deprecated[op]; ok {
			d.apply(w.Header())
		}
		maxBody := bodyLimit(op)
		if r.ContentLength > maxBody {
			apigen.WriteProblem(w, http.StatusRequestEntityTooLarge, "too_large", "the request body is too large")
			return
		}
		body := &limitedBody{r: r.Body, left: maxBody}
		r.Body, lookup.Body = body, body

		ctx := r.Context()
		if op.Extensions["x-samuori-public"] != true {
			var p authz.Principal
			ok := false
			if authn != nil {
				p, ok, err = authn.Authenticate(r)
				if errors.Is(err, auth.ErrCSRF) {
					apigen.WriteProblem(w, http.StatusForbidden, "csrf", "missing or invalid CSRF token")
					return
				}
				if err != nil {
					apigen.WriteProblem(w, http.StatusUnauthorized, "unauthenticated", "authentication failed")
					return
				}
			}
			if !ok {
				apigen.WriteProblem(w, http.StatusUnauthorized, "unauthenticated", "sign in to use this operation")
				return
			}
			if p.ReadOnly && r.Method != http.MethodGet && r.Method != http.MethodHead {
				apigen.WriteProblem(w, http.StatusForbidden, "read_only_token", "this API token is read-only")
				return
			}
			roles, any := declaredRoles(op)
			if !any && !authz.HasAnyRole(p, roles) {
				apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "your roles do not permit this operation")
				return
			}
			if ok, retry := limit.allow("p:" + p.ID); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
				apigen.WriteProblem(w, http.StatusTooManyRequests, "rate_limited", "too many requests; slow down and retry")
				return
			}
			// Open portal tabs compare this with the version they loaded and offer a reload after an update.
			w.Header().Set(VersionHeader, version.Version)
			ctx = authz.WithPrincipal(ctx, p)
			ctx = context.WithValue(ctx, rolesKey{}, roles)
		}
		ctx = context.WithValue(ctx, operationKey{}, op)

		validated := lookup.WithContext(ctx)
		input := &openapi3filter.RequestValidationInput{
			Request: validated, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		if err := openapi3filter.ValidateRequest(ctx, input); err != nil {
			if body.exceeded {
				apigen.WriteProblem(w, http.StatusRequestEntityTooLarge, "too_large", "the request body is too large")
				return
			}
			apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", validationMessage(err))
			return
		}
		r.Body = validated.Body
		next.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

const defaultBodyLimit = 1 << 20

// bodyLimit is 1 MiB unless the operation declares x-samuori-max-body-bytes.
func bodyLimit(op *openapi3.Operation) int64 {
	if v, ok := op.Extensions["x-samuori-max-body-bytes"].(float64); ok && v > 0 {
		return int64(v)
	}
	return defaultBodyLimit
}

type limitedBody struct {
	r        io.ReadCloser
	left     int64
	exceeded bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		if n, _ := b.r.Read(one[:]); n > 0 {
			b.exceeded = true
			return 0, errBodyTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.r.Close() }

var errBodyTooLarge = errors.New("request body too large")

func declaredRoles(op *openapi3.Operation) ([]authz.Role, bool) {
	raw, _ := op.Extensions["x-samuori-roles"].([]any)
	var roles []authz.Role
	for _, v := range raw {
		s, _ := v.(string)
		if s == "any" {
			return nil, true
		}
		roles = append(roles, authz.Role(s))
	}
	return roles, false
}

func validationMessage(err error) string {
	var re *openapi3filter.RequestError
	if errors.As(err, &re) {
		return re.Error()
	}
	return "request does not match the API contract"
}
