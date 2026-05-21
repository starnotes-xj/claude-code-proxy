package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	claudecodexproxy "claude-codex-proxy/internal"
)

func main() {
	cfg, err := claudecodexproxy.LoadConfigFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	proxy := claudecodexproxy.New(cfg)
	defer func() {
		if err := proxy.Close(); err != nil {
			log.Printf("close proxy error: %v", err)
		}
	}()

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: proxy.Handler(),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("claude-codex-proxy listening on %s -> %s", cfg.ListenAddr, cfg.BackendURL())

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}
