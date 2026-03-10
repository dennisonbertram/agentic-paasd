// Package httpx provides shared HTTP response helpers used by both the api
// and middleware packages to ensure consistent error formatting.
package httpx

import (
	"encoding/json"
	"log"
	"net/http"
)

// WriteError writes a consistent JSON error response with correct headers.
// All error responses across the codebase should use this function.
func WriteError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		log.Printf("httpx: failed to encode error response: %v", err)
	}
}

// WriteJSON writes a JSON success response with correct headers and status code.
// All success responses across the codebase should use this function.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("httpx: failed to encode JSON response: %v", err)
	}
}
