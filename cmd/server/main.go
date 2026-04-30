package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"backend_mobile_app_go/internal/config"
	"backend_mobile_app_go/internal/db"
	"backend_mobile_app_go/internal/httpserver"
	"backend_mobile_app_go/internal/repository"
	"backend_mobile_app_go/internal/snapshot"
	"backend_mobile_app_go/internal/supabaseauth"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.SupabaseDBURL, cfg.MaxDBConns)
	if err != nil {
		log.Fatalf("init db pool: %v", err)
	}
	defer pool.Close()

	repo := repository.New(pool)

	snap := snapshot.New(repo, time.Duration(cfg.SnapshotRefreshMinutes)*time.Minute)
	warmCtx, warmCancel := context.WithTimeout(ctx, 30*time.Second)
	if err = snap.Refresh(warmCtx); err != nil {
		log.Printf("snapshot warm-up failed (will retry in background): %v", err)
	} else {
		log.Printf("snapshot ready: routes=%d stops=%d", snap.RoutesCount(), snap.StopsCount())
	}
	warmCancel()
	snap.StartBackgroundRefresh(ctx)

	authClient := supabaseauth.New(cfg.SupabaseURL, cfg.SupabaseAnonKey)
	if !authClient.Enabled() {
		log.Printf("supabase auth disabled: set SUPABASE_URL and SUPABASE_ANON_KEY to enable signup/login endpoints")
	}

	server := httpserver.New(cfg, repo, snap, authClient)

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           server.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("http server listening on :%s", cfg.Port)
		if serveErr := httpSrv.ListenAndServe(); serveErr != nil && serveErr != http.ErrServerClosed {
			log.Fatalf("http serve: %v", serveErr)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if shutdownErr := httpSrv.Shutdown(shutdownCtx); shutdownErr != nil {
		log.Printf("graceful shutdown failed: %v", shutdownErr)
	}
}
