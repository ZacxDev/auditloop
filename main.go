// Command auditloop is a generic crawler-based UX auditor. A single binary runs
// the web/API server and/or the background crawl worker, selected by
// AUDITLOOP_ROLE (web|worker|all).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ZacxDev/auditloop/handlers"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
)

func main() {
	cfg := config.Load()

	dsn := cfg.DatabasePath
	if cfg.DatabaseDriver == "postgres" || cfg.DatabaseDriver == "pgx" {
		dsn = cfg.DatabaseURL
	}
	database, err := db.Open(cfg.DatabaseDriver, dsn)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer database.Close()

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := handlers.OpenStore(rootCtx, cfg)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}

	router, err := handlers.NewRouter(rootCtx, cfg, database, store)
	if err != nil {
		log.Fatalf("router: %v", err)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		mode := "supabase-auth"
		if cfg.DevMode {
			mode = "DEV (auth bypassed)"
		}
		log.Printf("auditloop listening on :%s [role=%s auth=%s storage=%s]", cfg.Port, cfg.Role, mode, store.Backend())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	cancel() // stop the worker loop
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Println("auditloop stopped")
}
