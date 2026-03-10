package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/paasd/paasd/internal/api"
	"github.com/paasd/paasd/internal/builder"
	"github.com/paasd/paasd/internal/builds"
	"github.com/paasd/paasd/internal/db"
	"github.com/paasd/paasd/internal/docker"
	"github.com/paasd/paasd/internal/services"
)

func main() {
	port := flag.String("port", "8080", "HTTP port")
	listenAddr := flag.String("listen-addr", "", "Listen address (default: 127.0.0.1; use 0.0.0.0 to bind all interfaces)")
	dbPath := flag.String("db-path", "/var/lib/paasd/paasd.db", "Path to state SQLite database")
	masterKeyPath := flag.String("master-key-path", "/var/lib/paasd/master.key", "Path to master encryption key")
	devMode := flag.Bool("dev", false, "Development mode (disables HTTPS enforcement)")
	openRegistration := flag.Bool("open-registration", false, "Allow registration without bootstrap token (requires --dev)")
	flag.Parse()

	// Bootstrap token is always required unless --dev + --open-registration
	bootstrapToken := strings.TrimSpace(os.Getenv("PAASD_BOOTSTRAP_TOKEN"))
	if bootstrapToken == "" {
		if !*devMode {
			log.Fatalf("PAASD_BOOTSTRAP_TOKEN must be set (or use --dev --open-registration)")
		}
		if !*openRegistration {
			log.Fatalf("PAASD_BOOTSTRAP_TOKEN must be set. Use --open-registration with --dev to allow open registration.")
		}
		log.Printf("WARNING: open registration enabled — anyone can create tenants")
	} else if len(bootstrapToken) < 32 {
		log.Fatalf("PAASD_BOOTSTRAP_TOKEN must be at least 32 characters for brute-force resistance (got %d)", len(bootstrapToken))
	}

	if *openRegistration && !*devMode {
		log.Fatalf("--open-registration requires --dev")
	}

	// In production (non-dev) mode, refuse to bind to non-loopback addresses.
	// The server must be behind a TLS-terminating reverse proxy on loopback.
	if !*devMode && *listenAddr != "" && *listenAddr != "127.0.0.1" && *listenAddr != "::1" {
		log.Fatalf("FATAL: non-loopback listen address (%s) is not allowed in production mode. Use --dev for development or bind to 127.0.0.1 behind a reverse proxy.", *listenAddr)
	}
	// Warn in dev mode about non-loopback
	if *devMode && *listenAddr != "" && *listenAddr != "127.0.0.1" && *listenAddr != "::1" {
		log.Printf("WARNING: dev mode with non-loopback listen address (%s) disables HTTPS enforcement.", *listenAddr)
	}

	// Read master key
	masterKeyData, err := os.ReadFile(*masterKeyPath)
	if err != nil {
		log.Fatalf("failed to read master key from %s: %v\nGenerate one with: head -c 32 /dev/urandom | xxd -p -c 64 > %s", *masterKeyPath, err, *masterKeyPath)
	}
	masterKeyHex := strings.TrimSpace(string(masterKeyData))
	masterKey, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		log.Fatalf("master key in %s must be hex-encoded (got invalid hex): %v\nGenerate one with: head -c 32 /dev/urandom | xxd -p -c 64 > %s", *masterKeyPath, err, *masterKeyPath)
	}
	if len(masterKey) < 32 {
		log.Fatalf("master key in %s must be at least 32 bytes after hex decoding (got %d bytes from %d hex chars).\nGenerate one with: head -c 32 /dev/urandom | xxd -p -c 64 > %s", *masterKeyPath, len(masterKey), len(masterKeyHex), *masterKeyPath)
	}

	// Open databases
	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	// Create Docker client
	dockerClient, err := docker.NewClient()
	if err != nil {
		log.Fatalf("failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	// Verify gVisor runtime is available (fail closed in production)
	if err := dockerClient.VerifyGVisorRuntime(context.Background()); err != nil {
		if !*devMode {
			log.Fatalf("FATAL: %v. gVisor is required for container isolation in production.", err)
		}
		log.Printf("WARNING: %v. Containers will fail to start without gVisor.", err)
	} else {
		log.Printf("gVisor (runsc) runtime verified")
	}

	// Create Nixpacks builder and build manager
	nixBuilder, err := builder.NewBuilder("/var/lib/paasd/builds", "/usr/local/bin/nixpacks")
	if err != nil {
		log.Printf("WARNING: Nixpacks builder not available: %v", err)
	}

	var buildMgr *builds.Manager
	if nixBuilder != nil {
		// Create service manager early to get DeployImage function
		svcMgr := services.NewManager(store.StateDB, dockerClient, masterKey[:32])
		buildMgr = builds.NewManager(store.StateDB, nixBuilder, svcMgr.DeployImage)
	}

	// Create server
	srv := api.NewServer(api.ServerConfig{
		Store:            store,
		MasterKey:        masterKey[:32],
		DevMode:          *devMode,
		BootstrapToken:   bootstrapToken,
		OpenRegistration: *openRegistration,
		Docker:           dockerClient,
		BuildManager:     buildMgr,
	})

	// Default to 127.0.0.1 in ALL modes (loopback only).
	// Must explicitly pass --listen-addr=0.0.0.0 to bind all interfaces.
	addr := "127.0.0.1:" + *port
	if *listenAddr != "" {
		addr = *listenAddr + ":" + *port
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()
	log.Printf("paasd listening on %s", addr)
	if *devMode {
		log.Printf("WARNING: running in dev mode — HTTPS enforcement disabled")
	} else {
		log.Printf("HTTPS enforcement is ON. The server must be behind a TLS-terminating proxy (e.g. Traefik) that connects via loopback (127.0.0.1). X-Forwarded-Proto is only trusted from loopback RemoteAddr.")
	}

	<-done
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	log.Println("shutdown complete")
}
