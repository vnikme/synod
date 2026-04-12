package internal

import (
	"log/slog"
	"net/http"
	"os"
	"strings"

	"google.golang.org/api/idtoken"
)

// OIDCAuthMiddleware returns middleware that validates Google OIDC tokens
// on internal endpoints. Cloud Tasks sends an OIDC token with each delivery;
// this middleware ensures only legitimately dispatched tasks can invoke
// /internal/* handlers.
//
// The token's audience is verified against the expectedAudience (typically
// ORCHESTRATOR_BASE_URL). If expectedSAEmail is non-empty, the token's
// email claim must also match (defense-in-depth).
//
// Set DISABLE_INTERNAL_AUTH=true to skip validation in local development.
func OIDCAuthMiddleware(expectedAudience, expectedSAEmail string) func(http.Handler) http.Handler {
	disabled := strings.EqualFold(os.Getenv("DISABLE_INTERNAL_AUTH"), "true")
	if disabled {
		slog.Warn("OIDC auth middleware disabled (DISABLE_INTERNAL_AUTH=true)")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if disabled {
				next.ServeHTTP(w, r)
				return
			}

			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				slog.Warn("auth: missing Authorization header", "path", r.URL.Path, "remote", r.RemoteAddr)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token == authHeader {
				// No "Bearer " prefix found
				slog.Warn("auth: malformed Authorization header", "path", r.URL.Path)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			payload, err := idtoken.Validate(r.Context(), token, expectedAudience)
			if err != nil {
				slog.Warn("auth: OIDC token validation failed",
					"path", r.URL.Path,
					"error", err,
				)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			// Optionally verify the caller's service account email
			if expectedSAEmail != "" {
				email, _ := payload.Claims["email"].(string)
				if email != expectedSAEmail {
					slog.Warn("auth: service account mismatch",
						"path", r.URL.Path,
						"expected", expectedSAEmail,
						"got", email,
					)
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}
