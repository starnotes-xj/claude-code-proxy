package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"claude-codex-proxy/internal/claudecodexproxy"
)

func main() {
	cfg, err := claudecodexproxy.LoadConfigFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	proxy := claudecodexproxy.New(cfg)
	log.Printf("claude-codex-proxy listening on %s -> %s", cfg.ListenAddr, cfg.BackendURL())

	if err := http.ListenAndServe(cfg.ListenAddr, proxy.Handler()); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}
