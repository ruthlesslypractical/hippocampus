package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/embedding"
	"github.com/ruthlesslypractical/hippocampus/internal/mcp"
	"github.com/ruthlesslypractical/hippocampus/internal/memory"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("hippocampus-mcp v%s\n", config.Version)
		os.Exit(0)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("loading config: %s", err)
	}

	// Create embedder (nil if not configured)
	embedder := embedding.NewEmbedder(cfg.Ollama)

	store, err := memory.NewRedisStore(cfg.Redis, embedder)
	if err != nil {
		log.Fatalf("connecting to redis: %s", err)
	}
	defer store.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	server := mcp.NewServer(store, cfg)
	if err := server.Run(ctx); err != nil {
		log.Fatalf("server error: %s", err)
	}
}
