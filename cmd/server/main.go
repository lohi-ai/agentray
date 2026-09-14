package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lohi-ai/agentray/internal/app"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

func main() {
	// The sandbox worker shares this binary: run_sql's untrusted SQL executes in
	// a child process of the API (see duckdb_sandbox.go), and the child is
	// started with none of the API's environment. It is dispatched before any
	// config is read, because the child must never need — or be able to leak —
	// the api's credentials, and it must never open the analytics file.
	if len(os.Args) > 1 && os.Args[1] == storage.SandboxWorkerArgv {
		if err := storage.RunSandboxWorker(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			log.Fatalf("%s: %v", storage.SandboxWorkerArgv, err)
		}
		return
	}

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
