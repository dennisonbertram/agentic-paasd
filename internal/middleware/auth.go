package middleware

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/paasd/paasd/internal/crypto"
)

type contextKey string

const TenantIDKey contextKey = "tenant_id"

func Auth(db *sql.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, `{"error":"missing authorization header"}`, http.StatusUnauthorized)
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || parts[0] != "Bearer" {
				http.Error(w, `{"error":"invalid authorization format"}`, http.StatusUnauthorized)
				return
			}
			token := parts[1]

			if len(token) < 8 {
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}
			prefix := token[:8]

			rows, err := db.Query(
				`SELECT ak.id, ak.tenant_id, ak.key_hash, t.status
				 FROM api_keys ak
				 JOIN tenants t ON t.id = ak.tenant_id
				 WHERE ak.key_prefix = ? AND ak.revoked_at IS NULL`,
				prefix,
			)
			if err != nil {
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			defer rows.Close()

			var matched bool
			var tenantID string
			for rows.Next() {
				var keyID, tid, keyHash, status string
				if err := rows.Scan(&keyID, &tid, &keyHash, &status); err != nil {
					continue
				}
				if status != "active" {
					continue
				}
				if crypto.VerifyPassword(keyHash, token) {
					matched = true
					tenantID = tid
					// Update last_used_at
					go func() {
						db.Exec("UPDATE api_keys SET last_used_at = ? WHERE id = ?", time.Now().Unix(), keyID)
					}()
					break
				}
			}

			if !matched {
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), TenantIDKey, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func GetTenantID(ctx context.Context) string {
	v, _ := ctx.Value(TenantIDKey).(string)
	return v
}
