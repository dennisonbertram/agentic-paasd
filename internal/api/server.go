package api

import (
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/paasd/paasd/internal/db"
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
	r.Use(jsonContentType)
	r.Use(maxBodySize(1 << 20))
	// Global concurrency limiter: cap in-flight requests to prevent goroutine exhaustion
	r.Use(chimw.Throttle(200))

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
		r.Use(middleware.Auth(s.store.StateDB, s.masterKey))
		rl := middleware.NewRateLimiter(100, 200)
		r.Use(rl.Middleware)
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

func (s *Server) ListenAndServe(addr string) error {
	log.Printf("starting server on %s", addr)
	if !s.devMode {
		log.Printf("HTTPS enforcement is ON. The server must be behind a TLS-terminating proxy (e.g. Traefik) that connects via loopback (127.0.0.1). X-Forwarded-Proto is only trusted from loopback RemoteAddr. If the proxy connects from a non-loopback address, requests will be rejected.")
	}
	return http.ListenAndServe(addr, s)
}

func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
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

// requireHTTPS rejects requests not arriving via TLS-terminating proxy.
// Only trusts X-Forwarded-Proto when RemoteAddr is loopback (i.e., from the
// local Traefik proxy). Direct external connections cannot spoof this header.
func requireHTTPS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopback(r.RemoteAddr) {
			// Request from trusted proxy — check forwarded proto
			proto := r.Header.Get("X-Forwarded-Proto")
			if proto != "https" {
				http.Error(w, `{"error":"HTTPS required"}`, http.StatusForbidden)
				return
			}
		} else {
			// Direct connection (not via proxy) — reject unless already TLS
			if r.TLS == nil {
				http.Error(w, `{"error":"HTTPS required"}`, http.StatusForbidden)
				return
			}
		}
		// Strip X-Forwarded headers from non-loopback to prevent spoofing
		if !isLoopback(r.RemoteAddr) {
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Forwarded-Proto")
			r.Header.Del("X-Real-Ip")
		}
		next.ServeHTTP(w, r)
	})
}

// stripForwardedHeaders removes proxy headers from direct (non-loopback) connections
// to prevent spoofing when the server is accidentally exposed.
func stripForwardedHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Forwarded-Proto")
			r.Header.Del("X-Real-Ip")
		}
		next.ServeHTTP(w, r)
	})
}

// trustedRealIP extracts client IP from X-Real-Ip or X-Forwarded-For only
// when the request comes from a trusted loopback proxy. Otherwise uses RemoteAddr.
// Traefik overwrites X-Real-Ip with the direct client IP, making it the most
// reliable source. Falls back to leftmost X-Forwarded-For entry.
func trustedRealIP(r *http.Request) string {
	if isLoopback(r.RemoteAddr) {
		// Prefer X-Real-Ip (set by Traefik to the direct client IP)
		if xri := r.Header.Get("X-Real-Ip"); xri != "" {
			return strings.TrimSpace(xri)
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if idx := strings.IndexByte(xff, ','); idx > 0 {
				return strings.TrimSpace(xff[:idx])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
