package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/nicholaswilde/rustdesk-build-worker/internal/api"
	"github.com/nicholaswilde/rustdesk-build-worker/internal/config"
	"github.com/nicholaswilde/rustdesk-build-worker/internal/s3"
	"github.com/nicholaswilde/rustdesk-build-worker/internal/worker"
)

func main() {
	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// API client
	apiClient := api.New(cfg.API.BaseURL, cfg.API.Token)

	// S3 client
	s3Client, err := s3.New(&cfg.S3)
	if err != nil {
		log.Fatalf("Failed to create S3 client: %v", err)
	}
	if err := s3Client.EnsureBucket(context.Background(), cfg.S3.Region); err != nil {
		log.Printf("Warning: S3 bucket check failed: %v", err)
	}

	// Worker
	w := worker.New(cfg, apiClient, s3Client)

	// Start HTTP server for versions/log proxying (in background)
	go func() {
		log.Printf("Worker HTTP server starting on %s", cfg.HTTP.Addr)
		if err := w.ServeHTTP(cfg.HTTP.Addr, cfg.API.Token); err != nil {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// Start polling loop
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Received shutdown signal")
		cancel()
	}()

	w.Run(ctx)
}
