package middleware

import (
	"encoding/json"
	"net/http"
)

// writeJSONError writes a consistent JSON error response.
// Middleware can't import the api package, so this is a local helper
// that mirrors api.writeError for consistent error formatting.
func writeJSONError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
