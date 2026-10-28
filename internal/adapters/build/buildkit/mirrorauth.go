package buildkit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"

	"github.com/moby/buildkit/session/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MirrorAuth answers BuildKit's registry authentication for authorizing mirrors (ADR 0030). For a mirror host the
// session claims token authority, so BuildKit asks it for each token instead of calling the mirror's realm; the
// controller decides the scopes. Every other host is anonymous. Tokens go to BuildKit's resolver, never to build
// steps.
type MirrorAuth struct {
	auth.UnimplementedAuthServer
	Mirrors map[string]bool
	Token   MirrorToken
	key     ed25519.PrivateKey
}

// NewMirrorAuth uses a fresh authority key per build, so token authority never spans builds.
func NewMirrorAuth(mirrors []string, token MirrorToken) *MirrorAuth {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	m := &MirrorAuth{Mirrors: map[string]bool{}, Token: token, key: key}
	for _, h := range mirrors {
		m.Mirrors[strings.ToLower(h)] = true
	}
	return m
}

func (m *MirrorAuth) Register(server *grpc.Server) { auth.RegisterAuthServer(server, m) }

func (m *MirrorAuth) mirror(host string) bool { return m.Mirrors[strings.ToLower(host)] }

func (m *MirrorAuth) Credentials(context.Context, *auth.CredentialsRequest) (*auth.CredentialsResponse, error) {
	return &auth.CredentialsResponse{}, nil
}

func (m *MirrorAuth) GetTokenAuthority(_ context.Context, req *auth.GetTokenAuthorityRequest) (*auth.GetTokenAuthorityResponse, error) {
	if !m.mirror(req.Host) {
		return nil, status.Error(codes.Unimplemented, "no token authority for this host")
	}
	return &auth.GetTokenAuthorityResponse{PublicKey: m.key.Public().(ed25519.PublicKey)}, nil
}

func (m *MirrorAuth) VerifyTokenAuthority(_ context.Context, req *auth.VerifyTokenAuthorityRequest) (*auth.VerifyTokenAuthorityResponse, error) {
	if !m.mirror(req.Host) {
		return nil, status.Error(codes.Unimplemented, "no token authority for this host")
	}
	return &auth.VerifyTokenAuthorityResponse{Signed: append(ed25519.Sign(m.key, req.Payload), req.Payload...)}, nil
}

func (m *MirrorAuth) FetchToken(ctx context.Context, req *auth.FetchTokenRequest) (*auth.FetchTokenResponse, error) {
	if !m.mirror(req.Host) || m.Token == nil {
		return nil, status.Error(codes.PermissionDenied, "no token for this host")
	}
	token, expires, issued, err := m.Token(ctx, req.Host, req.Scopes)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "private image source authentication failed: "+err.Error())
	}
	return &auth.FetchTokenResponse{Token: token, ExpiresIn: expires, IssuedAt: issued.Unix()}, nil
}
