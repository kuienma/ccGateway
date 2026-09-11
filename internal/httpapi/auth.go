package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
)

const bearerPrefix = "Bearer "

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimSpace(r.Header.Get("x-api-key"))
		if provided == "" {
			authorization := strings.TrimSpace(r.Header.Get("Authorization"))
			if strings.HasPrefix(authorization, bearerPrefix) {
				provided = strings.TrimSpace(strings.TrimPrefix(authorization, bearerPrefix))
			}
		}
		if !secureEqual(provided, s.apiKey) {
			s.writeError(w, r, http.StatusUnauthorized, anthropic.ErrorTypeAuthentication, "网关认证失败")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func secureEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}
