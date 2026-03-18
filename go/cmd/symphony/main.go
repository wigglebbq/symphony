package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/httpserver"
	"github.com/openai/symphony/go/internal/logger"
	"github.com/openai/symphony/go/internal/orchestrator"
)

func main() {
	port := flag.Int("port", -1, "HTTP observability port")
	flag.Parse()
	workflowPath := "WORKFLOW.md"
	if flag.NArg() > 0 {
		workflowPath = flag.Arg(0)
	}
	logr := logger.New()
	loader := config.NewLoader(workflowPath)
	orch, err := orchestrator.New(loader, logr)
	if err != nil {
		log.Fatalf("startup failed: %v", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cfg, _ := loader.Current()
	effectivePort := cfg.Server.Port
	if *port >= 0 {
		effectivePort = *port
	}
	if effectivePort >= 0 {
		if _, err := httpserver.Start(ctx, cfg.Server.Host, effectivePort, orch, logr); err != nil {
			log.Fatalf("http startup failed: %v", err)
		}
	}
	if err := orch.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("run failed: %v", err)
	}
}
