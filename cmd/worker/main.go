package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/nicholaswilde/rustdesk-build-worker/internal/api"
	"github.com/nicholaswilde/rustdesk-build-worker/internal/config"
	"github.com/nicholaswilde/rustdesk-build-worker/internal/s3"
	"github.com/nicholaswilde/rustdesk-build-worker/internal/worker"
)

var configPath string

var rootCmd = &cobra.Command{
	Use:   "build-worker",
	Short: "RustDesk build worker — compiles and packages custom clients",
	Run:   run,
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "config.yaml", "path to config file")
}

func run(cmd *cobra.Command, args []string) {
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	apiClient := api.New(cfg.API.BaseURL, cfg.API.Token)

	s3Client, err := s3.New(&cfg.S3)
	if err != nil {
		log.Fatalf("Failed to create S3 client: %v", err)
	}
	if err := s3Client.EnsureBucket(context.Background(), cfg.S3.Region); err != nil {
		log.Printf("Warning: S3 bucket check failed: %v", err)
	}

	w := worker.New(cfg, apiClient, s3Client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Received shutdown signal")
		cancel()
	}()

	w.Run(ctx)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
