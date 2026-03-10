package api

import (
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/paasd/paasd/internal/db"
	"github.com/paasd/paasd/internal/middleware"
)

type ServerConfig struct {
	Store          *db.Store
	MasterKey      []byte
	DevMode        bool
	BootstrapToken string
}

type Server struct {
	store          *db.Store
	masterKey      []byte
	devMode        bool
	bootstrapToken string
	router         chi.Router
}

func NewServer(cfg ServerConfig) *Server {
	s := &Server{
		store:          cfg.Store,
		masterKey:      cfg.MasterKey,
		devMode:        cfg.DevMode,
		bootstrapToken: cfg.BootstrapToken,
	}
	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	r := chi.NewRouter()

	r.Use(chimw.RequestID)
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(chimw.Timeout(30 * time.Second))
	r.Use(jsonContentType)
	r.Use(maxBodySize(1 << 20))
	// Global concurrency limiter: cap in-flight requests to prevent goroutine exhaustion
	r.Use(chimw.Throttle(200))

	// Enforce HTTPS via X-Forwarded-Proto (Traefik sets this).
	// Server binds to 127.0.0.1 by default, so direct access from
	// external networks is impossible without explicit --listen-addr override.
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
		log.Printf("WARNING: server is listening on plain HTTP. Ensure Traefik or another TLS-terminating proxy is in front of this service.")
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

// requireHTTPS rejects requests not arriving via TLS-terminating proxy.
// Combined with binding to 127.0.0.1 by default, this ensures the service
// is only accessible through Traefik which sets X-Forwarded-Proto.
func requireHTTPS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto := r.Header.Get("X-Forwarded-Proto")
		if proto != "https" {
			http.Error(w, `{"error":"HTTPS required"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
