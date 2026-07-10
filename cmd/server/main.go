package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lohi-ai/agentray/internal/app"
	"github.com/lohi-ai/agentray/internal/config"
)

func main() {
	cfg := config.FromEnv()

	// Operator subcommands run against the same config, then exit (they do not
	// start the HTTP server). replay-dlq re-queues dead-lettered event batches.
	if len(os.Args) > 1 && os.Args[1] == "replay-dlq" {
		if err := replayDLQ(cfg); err != nil {
			log.Fatalf("replay-dlq: %v", err)
		}
		return
	}

	srv, err := app.New(context.Background(), cfg)
	if err != nil {
		log.Fatalf("start agentray: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(cfg.HTTPAddr)
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("http server: %v", err)
		}
	case <-stop:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}
