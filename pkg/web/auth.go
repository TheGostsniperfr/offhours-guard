package web

import (
	"context"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Authenticator checks the access token the Gateway's OIDC filter forwards.
// The Gateway already enforces login, but the pod is also reachable in-cluster
// (Gatus calls /health on it), so the token is verified here as well instead
// of being trusted because it is present.
type Authenticator struct {
	verifier      *oidc.IDTokenVerifier
	allowedGroups []string
}

func NewAuthenticator(issuer string, keySet oidc.KeySet, allowedGroups []string) *Authenticator {
	return &Authenticator{
		// Access tokens carry the realm's audience, not the Gateway client's ID.
		verifier:      oidc.NewVerifier(issuer, keySet, &oidc.Config{SkipClientIDCheck: true}),
		allowedGroups: allowedGroups,
	}
}

// NewKeycloakAuthenticator verifies tokens against the realm's published keys.
// jwksURL is empty to use Keycloak's standard location under the issuer.
func NewKeycloakAuthenticator(ctx context.Context, issuer, jwksURL string, allowedGroups []string) *Authenticator {
	if jwksURL == "" {
		jwksURL = strings.TrimSuffix(issuer, "/") + "/protocol/openid-connect/certs"
	}
	return NewAuthenticator(issuer, oidc.NewRemoteKeySet(ctx, jwksURL), allowedGroups)
}

type tokenClaims struct {
	Groups []string `json:"groups"`
}

// requireMember fails closed: a nil Authenticator means the issuer was not
// configured, and the dashboard stays unavailable rather than open.
func (a *Authenticator) requireMember(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a == nil {
			http.Error(w, "authentication is not configured", http.StatusServiceUnavailable)
			return
		}

		rawToken, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || rawToken == "" {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		token, err := a.verifier.Verify(r.Context(), rawToken)
		if err != nil {
			log.Printf("[WEB] Rejected token: %v", err)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		var claims tokenClaims
		if err := token.Claims(&claims); err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		if !slices.ContainsFunc(claims.Groups, func(group string) bool {
			return slices.Contains(a.allowedGroups, strings.TrimPrefix(group, "/"))
		}) {
			http.Error(w, "not a member of this project", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}
