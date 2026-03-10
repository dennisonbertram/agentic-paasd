package api

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/paasd/paasd/internal/db"
	"github.com/paasd/paasd/internal/httpx"
	"github.com/paasd/paasd/internal/middleware"
)

type ServerConfig struct {
	Store            *db.Store
	MasterKey        []byte
	DevMode          bool
	BootstrapToken   string
	OpenRegistration bool
}

type Server struct {
	store            *db.Store
	masterKey        []byte
	devMode          bool
	bootstrapToken   string
	openRegistration bool
	router           chi.Router
	authInvalidator  *middleware.AuthCacheInvalidator
}

func NewServer(cfg ServerConfig) *Server {
	s := &Server{
		store:            cfg.Store,
		masterKey:        cfg.MasterKey,
		devMode:          cfg.DevMode,
		bootstrapToken:   cfg.BootstrapToken,
		openRegistration: cfg.OpenRegistration,
	}
	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	r := chi.NewRouter()

	r.Use(chimw.RequestID)
	// chi.Logger logs method/path/status/latency only — never headers or body.
	// Authorization and bootstrap tokens are never logged.
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(chimw.Timeout(30 * time.Second))
	r.Use(maxBodySize(1 << 20))
	// Global concurrency limiter: cap in-flight requests to prevent goroutine exhaustion
	r.Use(chimw.Throttle(200))

	// Strip forwarded headers from non-loopback requests to prevent spoofing.
	// This runs unconditionally (including dev mode) as a defense-in-depth measure.
	r.Use(stripUntrustedForwardedHeaders)

	// Enforce HTTPS: only trust X-Forwarded-Proto from loopback (trusted proxy).
	// Server binds to 127.0.0.1 by default; even if --listen-addr overrides this,
	// the HTTPS check only trusts the header from loopback RemoteAddr.
	if !s.devMode {
		r.Use(requireHTTPS)
	}

	// Public routes
	r.Get("/v1/system/health", s.handleHealth)
	r.Post("/v1/tenants/register", s.handleTenantRegister)

	// Authenticated routes
	r.Group(func(r chi.Router) {
		authMW, authInvalidator := middleware.Auth(s.store.StateDB, s.masterKey)
		s.authInvalidator = authInvalidator
		r.Use(authMW)
		// Per-tenant rate limiter
		rl := middleware.NewRateLimiter(100, 200)
		r.Use(rl.Middleware)
		// Global aggregate rate limiter across all tenants (prevents multi-tenant abuse)
		globalRL := middleware.NewGlobalRateLimiter(500, 1000)
		r.Use(globalRL.Middleware)
		idem := middleware.NewIdempotencyStore()
		r.Use(idem.Middleware)

		r.Get("/v1/system/health/detailed", s.handleHealthDetailed)

		r.Get("/v1/tenant", s.handleTenantGet)
		r.Patch("/v1/tenant", s.handleTenantUpdate)
		r.Delete("/v1/tenant", s.handleTenantDelete)

		r.Post("/v1/auth/keys", s.handleKeyCreate)
		r.Get("/v1/auth/keys", s.handleKeyList)
		r.Delete("/v1/auth/keys/{keyID}", s.handleKeyRevoke)
	})

	s.router = r
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}


// writeError delegates to httpx.WriteError for consistent JSON error responses.
// All handlers in the api package should use this.
func writeError(w http.ResponseWriter, code int, message string) {
	httpx.WriteError(w, code, message)
}

// writeJSON delegates to httpx.WriteJSON for consistent JSON success responses.
// All handlers in the api package should use this.
func writeJSON(w http.ResponseWriter, code int, v any) {
	httpx.WriteJSON(w, code, v)
}

// writeDecodeError writes a 413 if the error is from MaxBytesReader, otherwise 400.
func writeDecodeError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid request body")
}

func maxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// isLoopback checks if the remote address is from a loopback interface.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// stripUntrustedForwardedHeaders removes proxy headers from non-loopback
// requests to prevent spoofing. Runs unconditionally (including dev mode).
func stripUntrustedForwardedHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Forwarded-Proto")
			r.Header.Del("X-Real-Ip")
		}
		next.ServeHTTP(w, r)
	})
}

// requireHTTPS rejects requests not arriving via TLS-terminating proxy.
// Only trusts X-Forwarded-Proto when RemoteAddr is loopback (i.e., from the
// local Traefik proxy). Direct external connections cannot spoof this header.
// Header stripping is handled by stripUntrustedForwardedHeaders above.
func requireHTTPS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopback(r.RemoteAddr) {
			proto := r.Header.Get("X-Forwarded-Proto")
			if proto != "https" {
				writeError(w, http.StatusForbidden, "HTTPS required")
				return
			}
		} else {
			if r.TLS == nil {
				writeError(w, http.StatusForbidden, "HTTPS required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}



// normalizeIP extracts a validated IP string from a value that may be
// "ip", "ip:port", or "[ip]:port". Returns "" if no valid IP is found.
func normalizeIP(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Try host:port split first
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return ""
}

// trustedRealIP extracts client IP from X-Real-Ip ONLY when the request comes
// from a trusted loopback proxy. X-Forwarded-For is NOT used because clients can
// spoof it and some proxy configs preserve the client-provided chain.
// Traefik always overwrites X-Real-Ip with the direct connection IP, making it
// the only trustworthy source. Falls back to RemoteAddr.
func trustedRealIP(r *http.Request) string {
	if isLoopback(r.RemoteAddr) {
		if ip := normalizeIP(r.Header.Get("X-Real-Ip")); ip != "" {
			return ip
		}
	}
	if ip := normalizeIP(r.RemoteAddr); ip != "" {
		return ip
	}
	return "unknown"
}
