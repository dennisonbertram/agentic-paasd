package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/paasd/paasd/internal/api"
	"github.com/paasd/paasd/internal/db"
)

func main() {
	port := flag.String("port", "8080", "HTTP port")
	dbPath := flag.String("db-path", "/var/lib/paasd/paasd.db", "Path to state SQLite database")
	masterKeyPath := flag.String("master-key-path", "/var/lib/paasd/master.key", "Path to master encryption key")
	flag.Parse()

	// Read master key
	masterKeyData, err := os.ReadFile(*masterKeyPath)
	if err != nil {
		log.Fatalf("failed to read master key: %v", err)
	}
	masterKey := []byte(strings.TrimSpace(string(masterKeyData)))
	if len(masterKey) < 32 {
		log.Fatalf("master key must be at least 32 bytes, got %d", len(masterKey))
	}

	// Open databases
	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	// Create server
	srv := api.NewServer(store, masterKey[:32])

	httpServer := &http.Server{
		Addr:         ":" + *port,
		Handler:      srv,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()
	log.Printf("paasd listening on :%s", *port)

	<-done
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	log.Println("shutdown complete")
}
